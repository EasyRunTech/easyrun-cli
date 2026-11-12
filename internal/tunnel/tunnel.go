// Package tunnel — saluran SSH lewat WebSocket panel (kontrak SSH box §3/§6).
//
// stdin → frame BINER, frame BINER → stdout, byte apa adanya. Frame TEKS JSON
// adalah kendali: klien mengirim {"type":"ping"} berkala; server mengirim
// ready, pong, notice, exit {code}, error {message}. stdout HANYA berisi byte
// SSH — semua pesan ke stderr.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

const (
	// PingInterval — kontrak §6: ping tiap 25 dtk.
	PingInterval = 25 * time.Second
	// ChunkSize — ukuran maksimal satu frame ke server. Panel menutup
	// koneksi bila frame klien > 64 KiB; 32 KiB memberi ruang aman.
	ChunkSize = 32 << 10
	// maxServerFrame — batas frame dari server (panel meneruskan ≤ 1 MiB).
	maxServerFrame = 4 << 20
	// maxControlFrame — frame teks kendali selalu kecil.
	maxControlFrame = 64 << 10

	defaultReadyTimeout = 30 * time.Second
	defaultIdleTimeout  = 90 * time.Second
	defaultCloseGrace   = 5 * time.Second
	handshakeTimeout    = 20 * time.Second
	writeWait           = 10 * time.Second
)

var pingFrame = []byte(`{"type":"ping"}`)

// URLWithTicket — ws_url dari panel + ?ticket=. wss wajib; ws hanya ke
// loopback (uji lokal). ws_url relatif diselesaikan terhadap alamat API.
func URLWithTicket(wsURL, ticket, apiBase string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("ws_url dari server tidak valid: %w", err)
	}
	if !u.IsAbs() {
		base, err := url.Parse(apiBase)
		if err != nil {
			return "", err
		}
		u = base.ResolveReference(u)
		switch u.Scheme {
		case "https":
			u.Scheme = "wss"
		case "http":
			u.Scheme = "ws"
		}
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !config.IsLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("ws_url dari server harus wss: %q", api.Clean(wsURL))
		}
	default:
		return "", fmt.Errorf("ws_url dari server harus wss: %q", api.Clean(wsURL))
	}
	q := u.Query()
	q.Set("ticket", ticket)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Dial — buka WebSocket. Header yang dikirim hanya User-Agent: TANPA Origin
// (panel menolak tiket ssh yang membawa Origin selain panel). Jabat tangan
// yang ditolak dikembalikan sebagai *api.Error dari envelope panel, bertanda
// Handshake.
func Dial(ctx context.Context, wsURL, userAgent string) (*websocket.Conn, error) {
	d := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: handshakeTimeout,
	}
	h := http.Header{}
	if userAgent != "" {
		h.Set("User-Agent", userAgent)
	}
	conn, resp, err := d.DialContext(ctx, wsURL, h)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			// gorilla tidak pernah mengikuti pengalihan; laporkan seperti
			// klien API supaya pesannya sama.
			if rerr := api.RedirectFromResponse(resp.StatusCode, resp.Header); rerr != nil {
				return nil, rerr
			}
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			}
			e := api.ErrorFromResponse(resp.StatusCode, resp.Header, body, time.Now())
			e.Handshake = true
			return nil, e
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		host := wsURL
		if u, perr := url.Parse(wsURL); perr == nil {
			host = u.Host
		}
		return nil, &api.NetError{Host: host, Err: err}
	}
	return conn, nil
}

// Options — pengaturan Pump. Nilai nol = bawaan kontrak.
type Options struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Ticks — sumber detak ping; nil = time.Ticker(PingInterval).
	Ticks <-chan time.Time
	// ReadyTimeout — batas menunggu frame "ready" sebelum stdin diteruskan.
	ReadyTimeout time.Duration
	// IdleTimeout — server dianggap hilang bila tak ada frame apa pun
	// (termasuk ping/pong) selama ini.
	IdleTimeout time.Duration
	// CloseGrace — setelah stdin habis atau frame exit: batas menunggu server
	// menutup koneksi.
	CloseGrace time.Duration
}

type pump struct {
	conn *websocket.Conn
	o    Options

	wmu sync.Mutex // gorilla: satu penulis frame data dalam satu waktu

	readyOnce sync.Once
	ready     chan struct{}

	mu         sync.Mutex
	exitCode   *int
	gotError   bool
	localClose bool
	// closeBy — setelah frame exit/error: batas akhir menunggu server
	// menutup. Begitu diisi, ping/pong/frame berikutnya tidak lagi
	// memperpanjang batas baca (alive).
	closeBy time.Time
}

// Pump — salurkan sampai sesi berakhir; kembalikan kode keluar.
//
//	exit {code}            → code
//	error {message}        → ≠ 0 (pesan apa adanya ke stderr), walau
//	                         sesudahnya datang exit 0
//	stdin habis            → kirim close 1000, tunggu server, → 0
//	koneksi putus/ditutup  → 1 (pesan ke stderr)
//	ctx dibatalkan         → kirim close, → 1
//
// Pump tidak pernah kembali selagi pembaca frame masih bisa menulis pesan ke
// stderr: pesan easyrun selalu tercetak SEBELUM proses keluar, jadi sebelum
// ssh melihat stdout tertutup dan mencetak "Connection closed …" miliknya.
func Pump(ctx context.Context, conn *websocket.Conn, o Options) int {
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = defaultReadyTimeout
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = defaultIdleTimeout
	}
	if o.CloseGrace <= 0 {
		o.CloseGrace = defaultCloseGrace
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Ticks == nil {
		t := time.NewTicker(PingInterval)
		defer t.Stop()
		o.Ticks = t.C
	}
	p := &pump{conn: conn, o: o, ready: make(chan struct{})}
	defer conn.Close()

	conn.SetReadLimit(maxServerFrame)
	p.alive()
	conn.SetPingHandler(func(data string) error {
		p.alive()
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil
		}
		return err
	})
	conn.SetPongHandler(func(string) error { p.alive(); return nil })

	readDone := make(chan int, 1)
	go func() { readDone <- p.readLoop() }()

	stop := make(chan struct{})
	defer close(stop)
	go p.pingLoop(stop)

	stdinDone := make(chan struct{})
	go p.stdinLoop(stdinDone, stop)

	readyTimer := time.NewTimer(o.ReadyTimeout)
	defer readyTimer.Stop()
	readyC := readyTimer.C
	readySel := (<-chan struct{})(p.ready) // nil setelah terpilih sekali

	for {
		select {
		case code := <-readDone:
			return code
		case <-readySel:
			readyTimer.Stop()
			readyC, readySel = nil, nil
		case <-readyC:
			p.mu.Lock()
			p.localClose = true
			p.mu.Unlock()
			p.closeWith(websocket.CloseNormalClosure, "ready timeout")
			p.stopReader(readDone)
			if code, ok := p.known(); ok {
				return code // frame error sempat datang: pesannya sudah tercetak
			}
			fmt.Fprintf(o.Stderr, "easyrun: box tidak siap dalam %s; coba lagi sebentar lagi\n", o.ReadyTimeout)
			return 1
		case <-stdinDone:
			p.mu.Lock()
			p.localClose = true
			p.mu.Unlock()
			p.closeWith(websocket.CloseNormalClosure, "")
			return p.waitServer(readDone, 0)
		case <-ctx.Done():
			p.mu.Lock()
			p.localClose = true
			p.mu.Unlock()
			p.closeWith(websocket.CloseGoingAway, "dibatalkan")
			p.waitServer(readDone, 1)
			return 1
		}
	}
}

// waitServer — tunggu server menutup (byte yang masih dalam perjalanan tetap
// diteruskan ke stdout), paling lama CloseGrace.
func (p *pump) waitServer(readDone <-chan int, fallback int) int {
	t := time.NewTimer(p.o.CloseGrace)
	defer t.Stop()
	select {
	case code := <-readDone:
		return code
	case <-t.C:
		p.stopReader(readDone)
		return p.result(fallback)
	}
}

// readerStopWait — batas menunggu pembaca frame selesai setelah koneksi
// ditutup paksa (NextReader langsung gagal; ini hanya pengaman).
const readerStopWait = time.Second

// stopReader — tutup koneksi lalu tunggu readLoop selesai, supaya pesan
// yang sedang ditulisnya ke stderr tidak terpotong oleh proses yang keluar.
func (p *pump) stopReader(readDone <-chan int) {
	_ = p.conn.Close()
	t := time.NewTimer(readerStopWait)
	defer t.Stop()
	select {
	case <-readDone:
	case <-t.C:
	}
}

// alive — ada tanda hidup dari server (frame, ping, pong) atau stdout baru
// saja menerima data: perpanjang batas baca IdleTimeout — kecuali sesi sudah
// diakhiri server (closeBy), yang batasnya tetap.
func (p *pump) alive() {
	p.mu.Lock()
	by := p.closeBy
	p.mu.Unlock()
	if by.IsZero() {
		by = time.Now().Add(p.o.IdleTimeout)
	}
	_ = p.conn.SetReadDeadline(by)
}

// closeSoon — server mengakhiri sesi (exit/error): tunggu penutupannya paling
// lama CloseGrace sejak saat ini, tidak diperpanjang lagi.
func (p *pump) closeSoon() {
	p.mu.Lock()
	if p.closeBy.IsZero() {
		p.closeBy = time.Now().Add(p.o.CloseGrace)
	}
	by := p.closeBy
	p.mu.Unlock()
	_ = p.conn.SetReadDeadline(by)
}

// outWriter — stdout (aliran ke ssh). Galat TULIS ditandai supaya bisa
// dibedakan dari galat BACA jaringan di tengah frame (keduanya keluar dari
// io.Copy yang sama). Setiap tulisan yang berhasil memperpanjang batas baca:
// waktu tertahan di stdout (ssh dihentikan sementara dengan ~^Z, atau lambat
// menguras) bukan tanda server hilang — selama itu frame ping server memang
// tidak terbaca.
type outWriter struct {
	p   *pump
	err error
}

func (w *outWriter) Write(b []byte) (int, error) {
	n, err := w.p.o.Stdout.Write(b)
	if err != nil {
		w.err = err
		return n, err
	}
	w.p.alive()
	return n, nil
}

func (p *pump) write(mt int, data []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return p.conn.WriteMessage(mt, data)
}

func (p *pump) closeWith(code int, text string) {
	_ = p.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(writeWait))
}

func (p *pump) pingLoop(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case _, ok := <-p.o.Ticks:
			if !ok {
				return
			}
			if p.write(websocket.TextMessage, pingFrame) != nil {
				return
			}
		}
	}
}

// stdinLoop — baru mulai setelah "ready": byte yang ditulis ssh sebelum box
// tersambung menunggu di pipa, tidak pernah dibuang.
func (p *pump) stdinLoop(done chan<- struct{}, stop <-chan struct{}) {
	select {
	case <-p.ready:
	case <-stop:
		return
	}
	buf := make([]byte, ChunkSize)
	for {
		n, err := p.o.Stdin.Read(buf)
		if n > 0 {
			if werr := p.write(websocket.BinaryMessage, buf[:n]); werr != nil {
				return // koneksi rusak; readLoop yang melapor
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(p.o.Stderr, "easyrun: gagal membaca stdin: %v\n", err)
			}
			close(done)
			return
		}
	}
}

func (p *pump) readLoop() int {
	out := &outWriter{p: p}
	for {
		mt, r, err := p.conn.NextReader()
		if err != nil {
			return p.ended(err)
		}
		p.alive()
		switch mt {
		case websocket.BinaryMessage:
			if _, err := io.Copy(out, r); err != nil {
				if out.err == nil {
					// Galat BACA di tengah frame: koneksi reset atau batas
					// waktu — sesi putus, bukan ditutup ssh.
					return p.ended(err)
				}
				// ssh sudah menutup ujung bacanya: tidak ada lagi yang
				// menunggu byte — tutup rapi.
				p.mu.Lock()
				p.localClose = true
				p.mu.Unlock()
				p.closeWith(websocket.CloseNormalClosure, "")
				return p.result(0)
			}
		case websocket.TextMessage:
			data, err := io.ReadAll(io.LimitReader(r, maxControlFrame))
			if err != nil {
				return p.ended(err)
			}
			p.control(data)
		}
	}
}

type controlFrame struct {
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (p *pump) control(data []byte) {
	var f controlFrame
	if json.Unmarshal(data, &f) != nil {
		return // kompatibilitas maju: abaikan
	}
	switch f.Type {
	case "ready":
		p.readyOnce.Do(func() { close(p.ready) })
	case "pong":
	case "notice":
		fmt.Fprintf(p.o.Stderr, "easyrun: %s\n", noticeText(f))
	case "exit":
		code := exitCodeOf(f.Code)
		p.mu.Lock()
		p.exitCode = &code
		p.mu.Unlock()
		// Server akan menutup; jangan menunggu selamanya (ping server tidak
		// boleh memperpanjangnya lagi — lihat alive).
		p.closeSoon()
	case "error":
		// Kontrak §7b P11: kalimat server (mis. "image aplikasi ini belum
		// mendukung SSH; …") dicetak apa adanya — hanya karakter kendali
		// yang dibuang — lalu sesi diakhiri dengan kode ≠ 0.
		msg := api.Clean(f.Message)
		if msg == "" {
			msg = "server mengirim galat tanpa keterangan"
		}
		fmt.Fprintf(p.o.Stderr, "easyrun: %s\n", msg)
		p.mu.Lock()
		p.gotError = true
		p.mu.Unlock()
		// Sesi sudah gagal: minta server menutup sekarang, jangan menunggu.
		p.closeWith(websocket.CloseNormalClosure, "")
		p.closeSoon()
	}
}

func noticeText(f controlFrame) string {
	if m := api.Clean(f.Message); m != "" {
		return m
	}
	var code string
	_ = json.Unmarshal(f.Code, &code)
	switch code {
	case "server_restarting":
		return "server EasyRun sedang dimulai ulang; sambungan SSH akan putus — sambung lagi sebentar lagi"
	case "max_duration":
		return "sesi SSH mencapai batas durasi maksimum dan akan ditutup"
	case "":
		return "pemberitahuan dari server"
	default:
		return "pemberitahuan dari server: " + api.Clean(code)
	}
}

// exitCodeOf — kode keluar 0–255; selain itu (atau rusak) → 1. Tanpa kode → 0.
func exitCodeOf(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return 1
	}
	if n < 0 || n > 255 || n != float64(int(n)) {
		return 1
	}
	return int(n)
}

// known — kode akhir yang sudah pasti dari frame server. Frame error
// selalu ≠ 0: exit 0 sesudahnya tidak menghapusnya.
func (p *pump) known() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.gotError:
		if p.exitCode != nil && *p.exitCode != 0 {
			return *p.exitCode, true
		}
		return 1, true
	case p.exitCode != nil:
		return *p.exitCode, true
	}
	return 0, false
}

// result — kode akhir yang sudah diketahui, atau fallback.
func (p *pump) result(fallback int) int {
	if code, ok := p.known(); ok {
		return code
	}
	return fallback
}

// ended — koneksi berakhir dengan err dari NextReader.
func (p *pump) ended(err error) int {
	if code, ok := p.known(); ok {
		return code
	}
	p.mu.Lock()
	local := p.localClose
	p.mu.Unlock()
	if local {
		return 0
	}
	var ce *websocket.CloseError
	var ne net.Error
	switch {
	case errors.As(err, &ce):
		fmt.Fprintf(p.o.Stderr, "easyrun: sambungan ditutup server (%s)\n", closeText(ce))
	case errors.As(err, &ne) && ne.Timeout():
		fmt.Fprintf(p.o.Stderr, "easyrun: tidak ada balasan dari server selama %s; sambungan dianggap putus\n", p.o.IdleTimeout)
	default:
		fmt.Fprintf(p.o.Stderr, "easyrun: sambungan terputus: %v\n", err)
	}
	return 1
}

func closeText(ce *websocket.CloseError) string {
	var s string
	switch ce.Code {
	case websocket.CloseNormalClosure:
		s = "normal"
	case websocket.CloseGoingAway:
		s = "server berhenti"
	case websocket.CloseMessageTooBig:
		s = "frame terlalu besar"
	case websocket.CloseInternalServerErr:
		s = "galat server"
	case websocket.CloseAbnormalClosure:
		s = "terputus mendadak"
	case websocket.ClosePolicyViolation:
		s = "ditolak server"
	default:
		s = fmt.Sprintf("kode %d", ce.Code)
	}
	if t := api.Clean(ce.Text); t != "" && !strings.EqualFold(t, s) {
		s += ": " + t
	}
	return s
}
