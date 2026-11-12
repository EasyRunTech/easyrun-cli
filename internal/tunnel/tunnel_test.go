package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
)

// lockedBuffer — stderr yang aman dibaca setelah Pump selesai.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// wsServer — server WebSocket palsu (menggantikan panel → orchestrator).
func wsServer(t *testing.T, handle func(c *websocket.Conn, r *http.Request)) string {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		handle(c, r)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/console/ws?ticket=t1"
}

func text(c *websocket.Conn, s string) { _ = c.WriteMessage(websocket.TextMessage, []byte(s)) }

func closeNormal(c *websocket.Conn) {
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

// drain — baca sampai klien menutup (balasan close).
func drain(c *websocket.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, url, "easyrun/uji")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// payload — semua nilai byte 0–255 lalu byte acak (deterministik).
func payload(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		if i < 256*4 {
			b[i] = byte(i)
		} else {
			b[i] = byte(r.UintN(256))
		}
	}
	return b
}

func TestBytePersisDuaArahMultiMB(t *testing.T) {
	up := payload(3<<20+12345, 1)   // laptop → box
	down := payload(5<<20+54321, 2) // box → laptop

	type serverResult struct {
		got            []byte
		maxFrame, text int
	}
	results := make(chan serverResult, 1)
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		var got bytes.Buffer
		maxFrame, textFromClient := 0, 0
		recvAll := make(chan struct{})
		text(c, `{"type":"ready"}`)
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			signaled := false
			for {
				mt, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				switch mt {
				case websocket.BinaryMessage:
					got.Write(data)
					maxFrame = max(maxFrame, len(data))
				case websocket.TextMessage:
					textFromClient++
				}
				if !signaled && got.Len() >= len(up) {
					signaled = true
					close(recvAll)
				}
			}
		}()
		rnd := rand.New(rand.NewPCG(3, 4))
		for off := 0; off < len(down); {
			n := min(1+int(rnd.UintN(200_000)), len(down)-off)
			if err := c.WriteMessage(websocket.BinaryMessage, down[off:off+n]); err != nil {
				t.Errorf("server tulis: %v", err)
				return
			}
			off += n
		}
		select {
		case <-recvAll:
		case <-time.After(20 * time.Second):
			t.Error("server tidak menerima seluruh payload")
		}
		text(c, `{"type":"exit","code":7}`)
		closeNormal(c)
		<-readerDone
		results <- serverResult{got.Bytes(), maxFrame, textFromClient}
	})

	stdinR, stdinW := io.Pipe()
	go func() {
		rnd := rand.New(rand.NewPCG(5, 6))
		for off := 0; off < len(up); {
			n := min(1+int(rnd.UintN(100_000)), len(up)-off)
			if _, err := stdinW.Write(up[off : off+n]); err != nil {
				return
			}
			off += n
		}
	}()
	defer stdinW.Close()

	var stdout bytes.Buffer
	var stderr lockedBuffer
	code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: &stdout, Stderr: &stderr})
	if code != 7 {
		t.Fatalf("kode %d, ingin 7 (stderr %q)", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), down) {
		t.Fatalf("stdout tidak persis: %d byte, ingin %d", stdout.Len(), len(down))
	}
	sr := <-results
	if !bytes.Equal(sr.got, up) {
		t.Fatalf("server menerima tidak persis: %d byte, ingin %d", len(sr.got), len(up))
	}
	if sr.maxFrame > ChunkSize || sr.maxFrame > 64<<10 {
		t.Fatalf("frame klien %d byte melebihi batas panel 64 KiB", sr.maxFrame)
	}
	if sr.text != 0 {
		t.Fatalf("klien mengirim %d frame teks tanpa detak ping", sr.text)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr harus kosong: %q", stderr.String())
	}
}

func TestPingMengikutiDetak(t *testing.T) {
	if PingInterval != 25*time.Second {
		t.Fatalf("PingInterval %s, kontrak 25 dtk", PingInterval)
	}
	ticks := make(chan time.Time)
	pings := make(chan string, 8)
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		n := 0
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				pings <- string(data)
				text(c, `{"type":"pong"}`)
				if n++; n == 3 {
					text(c, `{"type":"exit","code":0}`)
					closeNormal(c)
					drain(c)
					return
				}
			}
		}
	})
	done := make(chan int, 1)
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	go func() {
		done <- Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard, Ticks: ticks})
	}()

	select {
	case p := <-pings:
		t.Fatalf("ping terkirim tanpa detak: %q", p)
	case <-time.After(150 * time.Millisecond):
	}
	for i := 0; i < 3; i++ {
		ticks <- time.Now()
		select {
		case p := <-pings:
			if p != `{"type":"ping"}` {
				t.Fatalf("frame ping %q", p)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("ping ke-%d tidak sampai", i+1)
		}
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("kode %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pump tidak selesai")
	}
}

// Byte dari ssh (versi protokol dikirim seketika) tidak boleh dikirim sebelum
// box siap.
func TestStdinMenungguReady(t *testing.T) {
	times := make(chan [2]time.Time, 1)
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		var readySent, firstFrame time.Time
		frame := make(chan time.Time, 1)
		go func() {
			for {
				mt, _, err := c.ReadMessage()
				if err != nil {
					return
				}
				if mt == websocket.BinaryMessage {
					select {
					case frame <- time.Now():
					default:
					}
				}
			}
		}()
		time.Sleep(200 * time.Millisecond)
		readySent = time.Now()
		text(c, `{"type":"ready"}`)
		select {
		case firstFrame = <-frame:
		case <-time.After(5 * time.Second):
		}
		times <- [2]time.Time{readySent, firstFrame}
		text(c, `{"type":"exit","code":0}`)
		closeNormal(c)
		time.Sleep(100 * time.Millisecond)
	})
	stdinR, stdinW := io.Pipe()
	go func() { _, _ = stdinW.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n")) }()
	defer stdinW.Close()
	if code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard}); code != 0 {
		t.Fatalf("kode %d", code)
	}
	tm := <-times
	readySent, firstFrame := tm[0], tm[1]
	if firstFrame.IsZero() || firstFrame.Before(readySent) {
		t.Fatalf("frame pertama %v sebelum ready %v", firstFrame, readySent)
	}
}

func TestReadyTimeout(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) { drain(c) })
	var stderr lockedBuffer
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard, Stderr: &stderr, ReadyTimeout: 150 * time.Millisecond})
	if code != 1 || !strings.Contains(stderr.String(), "tidak siap") {
		t.Fatalf("kode %d stderr %q", code, stderr.String())
	}
}

func TestFrameKendaliDanKodeKeluar(t *testing.T) {
	cases := []struct {
		name   string
		frames []string // "" = tutup normal; "!" = putus mendadak; "~" = tutup 1001
		code   int
		stdout string
		stderr []string
	}{
		{"exit 0 + notice", []string{`{"type":"notice","code":"max_duration","message":"Sesi mencapai batas 12 jam"}`, "b:halo", `{"type":"exit","code":0}`, ""}, 0, "halo", []string{"batas 12 jam"}},
		{"exit 3", []string{`{"type":"exit","code":3}`, ""}, 3, "", nil},
		{"exit 255", []string{`{"type":"exit","code":255}`, ""}, 255, "", nil},
		{"exit di luar rentang", []string{`{"type":"exit","code":300}`, ""}, 1, "", nil},
		{"exit tanpa kode", []string{`{"type":"exit"}`, ""}, 0, "", nil},
		{"exit kode rusak", []string{`{"type":"exit","code":"x"}`, ""}, 1, "", nil},
		{"error", []string{`{"type":"error","message":"exec gagal"}`, ""}, 1, "", []string{"easyrun: exec gagal\n"}},
		// Kontrak §7b P11: error selalu ≠ 0, walau orchestrator lalu mengirim exit 0.
		{"error lalu exit 0", []string{`{"type":"error","message":"image aplikasi ini belum mendukung SSH; perbarui image di katalog"}`, `{"type":"exit","code":0}`, ""}, 1, "",
			[]string{"easyrun: image aplikasi ini belum mendukung SSH; perbarui image di katalog\n"}},
		{"error lalu exit 127", []string{`{"type":"error","message":"x"}`, `{"type":"exit","code":127}`, ""}, 127, "", []string{"easyrun: x\n"}},
		{"error tanpa pesan", []string{`{"type":"error"}`, ""}, 1, "", []string{"tanpa keterangan"}},
		{"notice tanpa pesan lalu server berhenti", []string{`{"type":"notice","code":"server_restarting"}`, "~"}, 1, "", []string{"dimulai ulang", "server berhenti"}},
		{"frame asing diabaikan", []string{`{"type":"masa-depan"}`, `bukan json`, `{"type":"ready"}`, "b:\x00\xff", `{"type":"exit","code":0}`, ""}, 0, "\x00\xff", nil},
		{"tutup tanpa exit", []string{"b:abc", ""}, 1, "abc", []string{"ditutup server"}},
		{"putus mendadak", []string{"b:abc", "!"}, 1, "abc", []string{"sambungan"}},
		{"escape terminal dibuang", []string{`{"type":"error","message":"\u001b]0;x\u0007jahat"}`, ""}, 1, "", []string{"easyrun: ]0;xjahat\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
				text(c, `{"type":"ready"}`)
				for _, f := range tc.frames {
					switch {
					case f == "":
						closeNormal(c)
						drain(c)
						return
					case f == "~":
						_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, ""))
						drain(c)
						return
					case f == "!":
						_ = c.UnderlyingConn().Close()
						return
					case strings.HasPrefix(f, "b:"):
						_ = c.WriteMessage(websocket.BinaryMessage, []byte(f[2:]))
					default:
						text(c, f)
					}
				}
			})
			var stdout bytes.Buffer
			var stderr lockedBuffer
			stdinR, stdinW := io.Pipe()
			defer stdinW.Close()
			code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: &stdout, Stderr: &stderr, CloseGrace: time.Second})
			if code != tc.code {
				t.Fatalf("kode %d, ingin %d (stderr %q)", code, tc.code, stderr.String())
			}
			if stdout.String() != tc.stdout {
				t.Fatalf("stdout %q, ingin %q — stdout hanya untuk byte SSH", stdout.String(), tc.stdout)
			}
			for _, s := range tc.stderr {
				if !strings.Contains(stderr.String(), s) {
					t.Fatalf("stderr %q tak memuat %q", stderr.String(), s)
				}
			}
			if strings.ContainsAny(stderr.String(), "\x1b\x07") {
				t.Fatalf("escape lolos ke stderr: %q", stderr.String())
			}
		})
	}
}

func TestStdinHabisMenutupRapi(t *testing.T) {
	type result struct {
		data      string
		closeCode int
	}
	res := make(chan result, 1)
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		var got bytes.Buffer
		for {
			mt, data, err := c.ReadMessage()
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				res <- result{got.String(), ce.Code}
				return
			}
			if err != nil {
				res <- result{got.String(), -1}
				return
			}
			if mt == websocket.BinaryMessage {
				got.Write(data)
			}
		}
	})
	code := Pump(context.Background(), dial(t, url), Options{Stdin: strings.NewReader("halo box"), Stdout: io.Discard, CloseGrace: 2 * time.Second})
	if code != 0 {
		t.Fatalf("kode %d", code)
	}
	if r := <-res; r.data != "halo box" || r.closeCode != websocket.CloseNormalClosure {
		t.Fatalf("server: data %q, kode tutup %d", r.data, r.closeCode)
	}
}

func TestServerDiamDianggapPutus(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		drain(c)
	})
	var stderr lockedBuffer
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard, Stderr: &stderr, IdleTimeout: 200 * time.Millisecond})
	if code != 1 || !strings.Contains(stderr.String(), "tidak ada balasan") {
		t.Fatalf("kode %d stderr %q", code, stderr.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestStdoutTertutupMenutupRapi(t *testing.T) {
	closed := make(chan int, 1)
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		_ = c.WriteMessage(websocket.BinaryMessage, []byte("x"))
		for {
			_, _, err := c.ReadMessage()
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				closed <- ce.Code
				return
			}
			if err != nil {
				closed <- -1
				return
			}
		}
	})
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	if code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: brokenWriter{}}); code != 0 {
		t.Fatalf("kode %d", code)
	}
	if c := <-closed; c != websocket.CloseNormalClosure {
		t.Fatalf("server menerima kode tutup %d", c)
	}
}

func TestDibatalkan(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		drain(c)
	})
	ctx, cancel := context.WithCancel(context.Background())
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	time.AfterFunc(100*time.Millisecond, cancel)
	if code := Pump(ctx, dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard, CloseGrace: time.Second}); code != 1 {
		t.Fatalf("kode %d", code)
	}
}

func TestDialHeaderTanpaOrigin(t *testing.T) {
	hdr := make(chan *http.Request, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr <- r.Clone(context.Background())
		c, err := up.Upgrade(w, r, nil)
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	u, err := URLWithTicket("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/console/ws", "tiket+/=", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := dial(t, u)
	c.Close()
	r := <-hdr
	_, hasOrigin := r.Header["Origin"]
	origin, ua, ticket := r.Header.Get("Origin"), r.Header.Get("User-Agent"), r.URL.Query().Get("ticket")
	if hasOrigin || origin != "" {
		t.Fatalf("header Origin terkirim: %q", origin)
	}
	if ua != "easyrun/uji" || ticket != "tiket+/=" {
		t.Fatalf("ua=%q ticket=%q", ua, ticket)
	}
}

func TestDialDitolakSebelumUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("ticket") {
		case "409":
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"success":false,"message":"instance sedang ganti alamat"}`))
		case "429":
			w.Header().Set("Retry-After", "12")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"success":false,"message":"batas sesi SSH tercapai"}`))
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"success":false,"message":"tiket tidak berlaku"}`))
		}
	}))
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?ticket="
	cases := map[string]func(*api.Error) bool{
		"409": func(e *api.Error) bool { return e.Status == 409 && e.Message == "instance sedang ganti alamat" },
		"429": func(e *api.Error) bool { return e.Status == 429 && e.RetryAfter == 12*time.Second },
		"x":   func(e *api.Error) bool { return e.Status == 401 },
	}
	for tk, ok := range cases {
		_, err := Dial(context.Background(), base+tk, "easyrun/uji")
		var ae *api.Error
		if !errors.As(err, &ae) || !ok(ae) || !ae.Handshake {
			t.Errorf("%s: %v (handshake %v)", tk, err, ae != nil && ae.Handshake)
		}
	}
}

func TestURLWithTicket(t *testing.T) {
	ok := map[[2]string]string{
		{"wss://konsol.easyrun.io/api/v1/console/ws", "https://api.easyrun.io"}:     "wss://konsol.easyrun.io/api/v1/console/ws?ticket=T",
		{"wss://konsol.easyrun.io/ws?mode=ssh", "https://api.easyrun.io"}:           "wss://konsol.easyrun.io/ws?mode=ssh&ticket=T",
		{"/api/v1/console/ws", "https://api.easyrun.io"}:                            "wss://api.easyrun.io/api/v1/console/ws?ticket=T",
		{"ws://127.0.0.1:9/ws", "http://127.0.0.1:9"}:                               "ws://127.0.0.1:9/ws?ticket=T",
		{"wss://konsol.easyrun.io/ws?ticket=lama", "https://api.easyrun.io"}:        "wss://konsol.easyrun.io/ws?ticket=T",
		{"/ws", "http://localhost:8080"}:                                            "ws://localhost:8080/ws?ticket=T",
		{"wss://konsol.easyrun.io/api/v1/console/ws", "https://api.easyrun.io/x/y"}: "wss://konsol.easyrun.io/api/v1/console/ws?ticket=T",
	}
	for in, want := range ok {
		got, err := URLWithTicket(in[0], "T", in[1])
		if err != nil || got != want {
			t.Errorf("%q → %q, %v; ingin %q", in[0], got, err, want)
		}
	}
	for _, bad := range []string{"ws://konsol.easyrun.io/ws", "https://konsol.easyrun.io/ws", "file:///etc/passwd", "javascript:x"} {
		if got, err := URLWithTicket(bad, "T", "https://api.easyrun.io"); err == nil {
			t.Errorf("%q harus ditolak, dapat %q", bad, got)
		}
	}
}

// orderLog — stdout dan stderr dari Pump dalam urutan tulisnya.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *orderLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type taggedWriter struct {
	tag string
	l   *orderLog
}

func (w taggedWriter) Write(b []byte) (int, error) {
	w.l.add(w.tag + string(b))
	return len(b), nil
}

// Frame error (kontrak §7b P11): pesan tercetak SEBELUM Pump kembali (jadi
// sebelum proses keluar dan ssh melihat stdout tertutup), kode ≠ 0, klien
// sendiri yang meminta tutup — tidak menunggu server yang diam sampai batas
// idle, dan baik sebelum maupun sesudah "ready".
func TestErrorTercetakSebelumSelesaiDanTidakMenunggu(t *testing.T) {
	for _, withReady := range []bool{true, false} {
		name := "sesudah ready"
		if !withReady {
			name = "sebelum ready"
		}
		t.Run(name, func(t *testing.T) {
			gotClose := make(chan int, 1)
			url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
				if withReady {
					text(c, `{"type":"ready"}`)
					_ = c.WriteMessage(websocket.BinaryMessage, []byte("SSH-2.0-"))
				}
				text(c, `{"type":"error","message":"image aplikasi ini belum mendukung SSH; perbarui image di katalog"}`)
				// Server sengaja TIDAK menutup; hanya mencatat close dari klien.
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						var ce *websocket.CloseError
						if errors.As(err, &ce) {
							gotClose <- ce.Code
						} else {
							gotClose <- -1
						}
						return
					}
				}
			})
			var log orderLog
			stdinR, stdinW := io.Pipe()
			defer stdinW.Close()
			start := time.Now()
			code := Pump(context.Background(), dial(t, url), Options{
				Stdin: stdinR, Stdout: taggedWriter{"stdout:", &log}, Stderr: taggedWriter{"stderr:", &log},
				CloseGrace: 3 * time.Second, IdleTimeout: 30 * time.Second, ReadyTimeout: 20 * time.Second,
			})
			log.add("selesai")
			if code == 0 {
				t.Fatalf("kode 0 setelah frame error")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("menunggu %s; klien harus menutup sendiri setelah error", d)
			}
			ev := log.list()
			want := "stderr:easyrun: image aplikasi ini belum mendukung SSH; perbarui image di katalog\n"
			idx := slices.Index(ev, want)
			if idx < 0 || ev[len(ev)-1] != "selesai" {
				t.Fatalf("urutan %q", ev)
			}
			for _, e := range ev {
				if strings.HasPrefix(e, "stderr:") && e != want {
					t.Fatalf("pesan tambahan yang menyesatkan: %q (semua: %q)", e, ev)
				}
			}
			select {
			case c := <-gotClose:
				if c != websocket.CloseNormalClosure {
					t.Fatalf("server menerima tutup %d, ingin 1000", c)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("klien tidak mengirim close setelah error")
			}
		})
	}
}

// Temuan review 2: koneksi putus DI TENGAH frame biner adalah sesi putus
// (kode 1 + pesan), bukan "ssh menutup ujung bacanya" (kode 0 diam-diam).
func TestPutusDiTengahFrameBukanTutupRapi(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		w, err := c.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return
		}
		// Lebih besar dari buffer tulis: fragmen awal (bukan final) sudah
		// terkirim, lalu koneksi diputus sebelum frame selesai.
		_, _ = w.Write(payload(256<<10, 3))
		_ = c.UnderlyingConn().Close()
	})
	var stderr lockedBuffer
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	code := Pump(context.Background(), dial(t, url), Options{Stdin: stdinR, Stdout: io.Discard, Stderr: &stderr})
	if code != 1 || !strings.Contains(stderr.String(), "terputus") {
		t.Fatalf("kode %d stderr %q", code, stderr.String())
	}
}

// Temuan review 4: setelah frame exit, ping server tidak boleh memperpanjang
// batas tunggu penutupan (CloseGrace) — tunnel tetap selesai.
func TestSetelahExitPingTidakMemperpanjang(t *testing.T) {
	stop := make(chan struct{})
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		text(c, `{"type":"exit","code":3}`)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
					return
				}
			}
		}
	})
	defer close(stop)
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	done := make(chan int, 1)
	go func() {
		done <- Pump(context.Background(), dial(t, url), Options{
			Stdin: stdinR, Stdout: io.Discard, Stderr: io.Discard,
			CloseGrace: 200 * time.Millisecond, IdleTimeout: 5 * time.Second,
		})
	}()
	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("kode %d, ingin 3", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel tidak selesai: ping server terus memperpanjang batas tunggu setelah exit")
	}
}

// slowWriter — stdout yang tertahan sekali (ssh dihentikan sementara).
type slowWriter struct {
	once sync.Once
	d    time.Duration
	buf  lockedBuffer
}

func (w *slowWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { time.Sleep(w.d) })
	return w.buf.Write(b)
}

// Temuan review 5: waktu tertahan menulis ke stdout tidak dihitung sebagai
// server diam.
func TestStdoutTertahanBukanServerDiam(t *testing.T) {
	url := wsServer(t, func(c *websocket.Conn, r *http.Request) {
		text(c, `{"type":"ready"}`)
		_ = c.WriteMessage(websocket.BinaryMessage, []byte("satu"))
		time.Sleep(50 * time.Millisecond)
		_ = c.WriteMessage(websocket.BinaryMessage, []byte("dua"))
		text(c, `{"type":"exit","code":0}`)
		closeNormal(c)
		drain(c)
	})
	var stderr lockedBuffer
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	w := &slowWriter{d: 600 * time.Millisecond}
	code := Pump(context.Background(), dial(t, url), Options{
		Stdin: stdinR, Stdout: w, Stderr: &stderr,
		IdleTimeout: 200 * time.Millisecond, CloseGrace: time.Second,
	})
	if code != 0 || w.buf.String() != "satudua" || stderr.String() != "" {
		t.Fatalf("kode %d stdout %q stderr %q", code, w.buf.String(), stderr.String())
	}
}
