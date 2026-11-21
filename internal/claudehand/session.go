package claudehand

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// forwardMethods — metode dari box yang boleh sampai ke `claude mcp serve`
// (kontrak P2 §2–3).
var forwardMethods = map[string]bool{
	"initialize": true, "notifications/initialized": true, "notifications/cancelled": true,
	"ping": true, "tools/list": true, "tools/call": true,
}

// requestMethods — metode permintaan (wajib ber-id); tanpa id dibuang.
var requestMethods = map[string]bool{"initialize": true, "ping": true, "tools/list": true, "tools/call": true}

var errClosed = errors.New("sambungan sudah ditutup")

// MaxLine — batas satu pesan JSON-RPC (box tidak bisa menghabiskan memori
// laptop dengan satu baris tanpa akhir).
const MaxLine = 64 << 20

// Server — satu proses `claude mcp serve` (atau pengganti uji).
type Server struct {
	In   io.WriteCloser // stdin server
	Out  io.Reader      // stdout server
	Stop func()         // hentikan dan tunggu; idempoten
}

// Stats — penghitung lintas sambungan (dibaca ringkasan `attach`).
type Stats struct {
	calls, denied atomic.Int64
	mu            sync.Mutex
	notes         []string
}

func (s *Stats) Calls() int64  { return s.calls.Load() }
func (s *Stats) Denied() int64 { return s.denied.Load() }

// Note — catatan untuk ringkasan (tanpa duplikat).
func (s *Stats) Note(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notes {
		if n == msg {
			return
		}
	}
	s.notes = append(s.notes, msg)
}

func (s *Stats) Notes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notes...)
}

// Config — satu sambungan tangan.
type Config struct {
	Policy *Policy
	Hello  []byte // baris hello, sudah diakhiri "\n"
	// Start — jalankan server; dipanggil saat pesan pertama dari box tiba
	// (bukan saat menunggu box), supaya putaran sambung ulang tidak
	// menyalakan claude berkali-kali.
	Start func() (*Server, error)
	Stats *Stats
	Audit *Audit
	// Log — baris untuk manusia (terminal mode `hand`); nil = dibuang.
	Log func(string)
	// OnReady — dipanggil sekali saat jawaban tools/list pertama terkirim ke
	// box (jabat tangan selesai).
	OnReady func()
}

// Result — akhir satu sambungan.
type Result struct {
	// Ready — jabat tangan sempat selesai.
	Ready bool
	// Replaced — jembatan memberi tahu tangan ini digantikan laptop lain.
	Replaced bool
	// ServerErr — server gagal dijalankan / berhenti sendiri.
	ServerErr error
	// LinkErr — sambungan ke box rusak (mis. baris terlalu panjang).
	LinkErr error
}

type session struct {
	cfg    Config
	linkMu sync.Mutex
	linkW  io.Writer
	srvMu  sync.Mutex
	srv    *Server
	srvQ   *queue
	srvErr error
	srvEnd chan struct{}
	closed bool

	listMu  sync.Mutex
	listIDs map[string]bool
	ready   atomic.Bool
	replace atomic.Bool
}

// Serve — jalankan satu sambungan: kirim hello, lalu teruskan JSON-RPC dua
// arah lewat penjaga sampai box/server menutup atau ctx selesai. Link TIDAK
// ditutup (milik pemanggil); server dihentikan sebelum kembali.
//
// Hello (bisa ±240 KiB) ditulis di goroutine: saluran ssh yang belum terbuka
// tidak membaca stdin, jadi tulisan itu bisa tertahan lama — Serve tetap
// kembali begitu ctx selesai. Penulisan yang tertahan lepas saat pemanggil
// menutup linkW (sesudah Serve kembali).
func Serve(ctx context.Context, linkR io.Reader, linkW io.Writer, cfg Config) Result {
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}
	s := &session{cfg: cfg, linkW: linkW, srvEnd: make(chan struct{}), listIDs: map[string]bool{}}
	helloDone := make(chan error, 1)
	go func() {
		s.linkMu.Lock()
		defer s.linkMu.Unlock()
		_, err := linkW.Write(cfg.Hello)
		helloDone <- err
	}()
	select {
	case <-ctx.Done():
		return Result{}
	case err := <-helloDone:
		if err != nil {
			return Result{LinkErr: err}
		}
	}
	boxEnd := make(chan error, 1)
	go func() { boxEnd <- s.fromBox(linkR) }()
	var res Result
	select {
	case <-ctx.Done():
	case err := <-boxEnd:
		res.LinkErr = err
	case <-s.srvEnd:
	}
	s.srvMu.Lock()
	srv, q := s.srv, s.srvQ
	res.ServerErr = s.srvErr
	s.srv = nil
	s.closed = true
	s.srvMu.Unlock()
	if q != nil {
		q.close()
	}
	if srv != nil {
		srv.Stop()
	}
	res.Ready = s.ready.Load()
	res.Replaced = s.replace.Load()
	return res
}

// log — satu baris untuk terminal. Hampir semua pesan memuat teks asal box
// (perintah, path, nama tool), jadi selalu dinetralkan di sini.
func (s *session) log(msg string) {
	if s.cfg.Log != nil {
		s.cfg.Log(Printable(msg))
	}
}

func (s *session) toLink(msg map[string]any) error {
	b, err := encodeLine(msg)
	if err != nil {
		return err
	}
	s.linkMu.Lock()
	defer s.linkMu.Unlock()
	_, err = s.linkW.Write(b)
	return err
}

// toServer — lewat antrean: pembaca box/server tidak pernah menunggu server
// membaca stdin-nya (dua pipa penuh bisa saling mengunci).
func (s *session) toServer(srv *Server, msg map[string]any) error {
	b, err := encodeLine(msg)
	if err != nil {
		return err
	}
	s.srvMu.Lock()
	q := s.srvQ
	s.srvMu.Unlock()
	if q == nil {
		return errClosed
	}
	return q.put(b)
}

// server — server yang sedang jalan, dinyalakan bila belum (sekali).
func (s *session) server() (*Server, error) {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	if s.srv != nil {
		return s.srv, nil
	}
	if s.closed {
		return nil, errClosed
	}
	if s.srvErr != nil {
		return nil, s.srvErr
	}
	srv, err := s.cfg.Start()
	if err != nil {
		s.srvErr = err
		close(s.srvEnd)
		return nil, err
	}
	s.srv = srv
	s.srvQ = newQueue(srv.In)
	go func() {
		err := s.fromServer(srv)
		s.srvMu.Lock()
		if s.srvErr == nil {
			if err == nil {
				err = errors.New("claude mcp serve berhenti")
			}
			s.srvErr = err
		}
		s.srvMu.Unlock()
		close(s.srvEnd)
	}()
	return srv, nil
}

func (s *session) deny(id any, why string) error {
	s.cfg.Stats.denied.Add(1)
	s.log("✗ ditolak: " + Short(why, 300))
	s.cfg.Audit.Log("tolak", why)
	return s.toLink(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "Ditolak penjaga laptop: " + why}},
		"isError": true,
	}})
}

// fromBox — box → penjaga → server.
func (s *session) fromBox(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			if perr := s.handleBox(line); perr != nil {
				return perr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *session) handleBox(line []byte) error {
	msg, ok := parseObject(line)
	if !ok {
		return nil
	}
	rawMethod, present := msg["method"]
	if !present || rawMethod == nil {
		return nil // jawaban atas permintaan server→klien: dijawab penjaga sendiri, tidak dipakai
	}
	method, hasMethod := rawMethod.(string)
	id, hasID := idOf(msg)
	if !hasMethod || !forwardMethods[method] {
		if method == "notifications/message" && replacedNotice(msg) {
			s.replace.Store(true)
			s.cfg.Audit.Log("digantikan", "jembatan: digantikan laptop lain")
		}
		if hasID {
			return s.toLink(map[string]any{"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": display(msg["method"]) + " ditolak penjaga"}})
		}
		return nil
	}
	if requestMethods[method] && !hasID {
		return nil
	}
	if method == "tools/call" {
		s.cfg.Stats.calls.Add(1)
		var params map[string]any
		switch p := msg["params"].(type) {
		case nil:
			params = map[string]any{}
		case map[string]any:
			params = p
		default:
			return s.deny(id, "params tools/call tidak valid")
		}
		name, isStr := params["name"].(string)
		if !isStr {
			name = display(params["name"])
		}
		var args map[string]any
		switch a := params["arguments"].(type) {
		case nil:
			args = map[string]any{}
		case map[string]any:
			args = a
		default:
			return s.deny(id, fmt.Sprintf("arguments %s tidak valid", display(params["name"])))
		}
		why, action := s.cfg.Policy.Check(name, args)
		if why != "" {
			return s.deny(id, why)
		}
		if name == "Bash" {
			s.log("$ " + Short(action, 300))
			s.cfg.Audit.Log("Bash", QuoteCommand(action))
		} else {
			s.log("✓ " + name + " " + action)
			s.cfg.Audit.Log(name, action)
		}
	}
	srv, err := s.server()
	if err != nil {
		if !errors.Is(err, errClosed) {
			s.log("! claude mcp serve gagal dijalankan: " + err.Error())
			s.cfg.Audit.Log("galat", "claude mcp serve gagal dijalankan: "+err.Error())
		}
		return nil
	}
	if method == "tools/list" {
		s.listMu.Lock()
		s.listIDs[idKey(id)] = true
		s.listMu.Unlock()
	}
	if err := s.toServer(srv, msg); err != nil {
		return nil // server mati: srvEnd yang mengakhiri sesi
	}
	return nil
}

// fromServer — server → penjaga → box.
func (s *session) fromServer(srv *Server) error {
	br := bufio.NewReaderSize(srv.Out, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			s.handleServer(srv, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *session) handleServer(srv *Server, line []byte) {
	msg, ok := parseObject(line)
	if !ok {
		return
	}
	id, hasID := idOf(msg)
	if m, present := msg["method"]; present && m != nil {
		if hasID { // permintaan server→klien: tolak di laptop
			_ = s.toServer(srv, map[string]any{"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32601, "message": "tidak didukung"}})
			return
		}
		if m == "notifications/tools/list_changed" {
			_ = s.toLink(msg)
		}
		return
	}
	isList := false
	if hasID {
		s.listMu.Lock()
		k := idKey(id)
		isList = s.listIDs[k]
		delete(s.listIDs, k)
		s.listMu.Unlock()
	}
	if isList {
		if _, isErr := msg["error"]; !isErr {
			msg["result"] = s.filterTools(msg["result"])
		}
	}
	if err := s.toLink(msg); err != nil {
		return
	}
	if isList && s.ready.CompareAndSwap(false, true) && s.cfg.OnReady != nil {
		s.cfg.OnReady()
	}
}

// filterTools — saring tools/list ke allowlist; peringatkan bila ada tool
// yang diharapkan hilang atau skemanya berubah (`claude mcp serve` tidak
// terdokumentasi dan bisa berubah kapan saja).
func (s *session) filterTools(v any) map[string]any {
	res, _ := v.(map[string]any)
	if res == nil {
		res = map[string]any{}
	}
	list, _ := res["tools"].([]any)
	got := []any{}
	have := map[string]bool{}
	for _, t := range list {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if !s.cfg.Policy.allowed(name) || have[name] {
			continue
		}
		have[name] = true
		got = append(got, tm)
		if fileTools[name] && !schemaHas(tm, "file_path") {
			s.warn(fmt.Sprintf("skema %s dari claude lokal tidak memuat file_path; panggilannya akan ditolak penjaga (perbarui easyrun)", name))
		}
		if name == "Bash" && !schemaHas(tm, "command") {
			s.warn("skema Bash dari claude lokal tidak memuat command (perbarui easyrun)")
		}
	}
	var missing []string
	for _, t := range s.cfg.Policy.Allowed() {
		if !have[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		s.warn("claude lokal tidak menyajikan: " + strings.Join(missing, ", "))
	}
	res["tools"] = got
	return res
}

func (s *session) warn(msg string) {
	s.log("! " + msg)
	s.cfg.Audit.Log("peringatan", msg)
	s.cfg.Stats.Note(msg)
}

func schemaHas(tool map[string]any, prop string) bool {
	sch, _ := tool["inputSchema"].(map[string]any)
	props, _ := sch["properties"].(map[string]any)
	_, ok := props[prop]
	return ok
}

// replacedNotice — pemberitahuan jembatan sebelum menutup tangan lama karena
// laptop lain tersambung:
// {"method":"notifications/message","params":{"level":"warning","logger":"easybox","data":"digantikan laptop lain","reason":"replaced"}}.
// `reason:"replaced"` = sinyal resmi (jembatan di image box); teks "digantikan"
// di `data` = cadangan untuk jembatan P1 yang belum mengirim `reason`.
func replacedNotice(msg map[string]any) bool {
	p, _ := msg["params"].(map[string]any)
	if r, _ := p["reason"].(string); r == "replaced" {
		return true
	}
	d, _ := p["data"].(string)
	return strings.Contains(strings.ToLower(d), "digantikan")
}

// parseObject — satu baris = tepat satu objek JSON. Kunci dibaca persis (map,
// bukan struct: decoder struct Go mencocokkan kunci tanpa beda huruf, beda
// dengan JSON.parse di claude — celah "PARAMS" vs "params"); kunci ganda →
// yang terakhir, sama dengan JSON.parse. Pesan diteruskan dalam bentuk hasil
// parse ulang ini, jadi server menerima persis yang diperiksa penjaga.
func parseObject(line []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return m, true
}

// idOf — id JSON-RPC; null dianggap tidak ada (seperti `rid is not None`).
func idOf(msg map[string]any) (any, bool) {
	id, ok := msg["id"]
	if !ok || id == nil {
		return nil, false
	}
	return id, true
}

func idKey(id any) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// readLine — satu baris tanpa "\n" (dan "\r"), paling panjang MaxLine.
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxLine {
			return nil, fmt.Errorf("pesan dari sambungan melebihi %d MiB", MaxLine>>20)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		buf = bytes.TrimRight(buf, "\r\n")
		return buf, err
	}
}

// queueMax — batas byte antre ke server; di atasnya penulis menunggu.
const queueMax = 256 << 20

// queue — penulis berantrean dengan satu goroutine penulis.
type queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  [][]byte
	size   int
	closed bool
	err    error
}

func newQueue(w io.Writer) *queue {
	q := &queue{}
	q.cond = sync.NewCond(&q.mu)
	go func() {
		for {
			q.mu.Lock()
			for len(q.items) == 0 && !q.closed {
				q.cond.Wait()
			}
			if len(q.items) == 0 {
				q.mu.Unlock()
				return
			}
			b := q.items[0]
			q.items = q.items[1:]
			q.size -= len(b)
			q.cond.Broadcast()
			q.mu.Unlock()
			if _, err := w.Write(b); err != nil {
				q.mu.Lock()
				q.err, q.closed, q.items = err, true, nil
				q.cond.Broadcast()
				q.mu.Unlock()
				return
			}
		}
	}()
	return q
}

func (q *queue) put(b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.size > queueMax && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		if q.err != nil {
			return q.err
		}
		return errClosed
	}
	q.items = append(q.items, b)
	q.size += len(b)
	q.cond.Broadcast()
	return nil
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.cond.Broadcast()
	q.mu.Unlock()
}
