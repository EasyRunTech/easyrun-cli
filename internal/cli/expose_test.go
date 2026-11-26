package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	agent "github.com/EasyRunTech/easyrun-tunnel-agent"
	"github.com/EasyRunTech/easyrun-tunnel-agent/protocol"
	"github.com/EasyRunTech/easyrun-tunnel-agent/wsconn"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
)

// ---------- panel palsu: rute tunnel publik (kontrak F2, rute token CLI) ----------

const tunnelHostSuffix = "-tunnel.contoh.test"

// gatewayInvalidTicketMsg — isi `error` 401 jabat tangan gerbang sungguhan
// untuk tiket tak dikenal/kedaluwarsa/terpakai (auth.ErrInvalidTicket di
// easyrun-tunnel): kalimat Indonesia, bukan kode mesin.
const gatewayInvalidTicketMsg = "tiket tidak valid atau kedaluwarsa"

type fakeTunnels struct {
	list     []api.Tunnel
	seq      int
	tickets  map[string]string // tiket → id tunnel; sekali pakai di gerbang palsu
	ticketN  int               // jumlah permintaan tiket
	deletes  []string
	agentURL string
	bodies   []map[string]any // badan POST /cli/tunnels

	createFail func(http.ResponseWriter) bool
	// ticketFail — n = urutan permintaan tiket (1-based); true = sudah dijawab.
	ticketFail func(w http.ResponseWriter, n int) bool
	deleteFail func(http.ResponseWriter) bool
}

func (p *fakePanel) tunnelRoutes(mux *http.ServeMux) {
	p.tun = &fakeTunnels{tickets: map[string]string{}, agentURL: "wss://gerbang.contoh.test" + protocol.AgentConnectPath}
	mux.HandleFunc("POST /api/v1/cli/tunnels", p.auth(p.handleTunnelCreate))
	mux.HandleFunc("GET /api/v1/cli/tunnels", p.auth(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		out := make([]map[string]any, 0, len(p.tun.list))
		for _, t := range p.tun.list {
			out = append(out, tunnelT(t))
		}
		ok(w, map[string]any{"tunnels": out})
	}))
	mux.HandleFunc("DELETE /api/v1/cli/tunnels/{id}", p.auth(p.handleTunnelDelete))
	mux.HandleFunc("POST /api/v1/cli/tunnels/{id}/ticket", p.auth(p.handleTunnelTicket))
}

// tunnelT — bentuk `T` persis kontrak (bukan tag struct klien).
func tunnelT(t api.Tunnel) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "host": t.Host, "url": t.URL,
		"ephemeral": t.Ephemeral, "disabled": t.Disabled, "created_at": t.CreatedAt,
	}
}

func okStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (p *fakePanel) addTunnel(name string, ephemeral bool) api.Tunnel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addTunnelLocked(name, ephemeral)
}

func (p *fakePanel) addTunnelLocked(name string, ephemeral bool) api.Tunnel {
	p.tun.seq++
	if name == "" {
		name = fmt.Sprintf("acak-%d", p.tun.seq)
	}
	host := name + tunnelHostSuffix
	t := api.Tunnel{
		ID:   fmt.Sprintf("00000000-0000-4000-8000-%012d", p.tun.seq),
		Name: name, Host: host, URL: "https://" + host,
		Ephemeral: ephemeral, CreatedAt: "2026-11-27T01:00:00Z",
	}
	p.tun.list = append(p.tun.list, t)
	return t
}

func (p *fakePanel) findTunnelLocked(id string) (api.Tunnel, int) {
	for i, t := range p.tun.list {
		if t.ID == id {
			return t, i
		}
	}
	return api.Tunnel{}, -1
}

func (p *fakePanel) handleTunnelCreate(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	p.mu.Lock()
	p.tun.bodies = append(p.tun.bodies, in)
	cf := p.tun.createFail
	p.mu.Unlock()
	if cf != nil && cf(w) {
		return
	}
	name, _ := in["name"].(string)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tun.list {
		if name != "" && t.Name == name {
			okStatus(w, http.StatusOK, map[string]any{"tunnel": tunnelT(t)})
			return
		}
	}
	t := p.addTunnelLocked(name, name == "")
	okStatus(w, http.StatusCreated, map[string]any{"tunnel": tunnelT(t)})
}

func (p *fakePanel) handleTunnelDelete(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	df := p.tun.deleteFail
	p.mu.Unlock()
	if df != nil && df(w) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id := r.PathValue("id")
	_, i := p.findTunnelLocked(id)
	if i < 0 {
		failReason(w, http.StatusNotFound, "tunnel tidak ditemukan", api.ReasonTunnelNotFound)
		return
	}
	p.tun.list = append(p.tun.list[:i], p.tun.list[i+1:]...)
	p.tun.deletes = append(p.tun.deletes, id)
	w.WriteHeader(http.StatusNoContent)
}

func (p *fakePanel) handleTunnelTicket(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.tun.ticketN++
	n, tf := p.tun.ticketN, p.tun.ticketFail
	p.mu.Unlock()
	if tf != nil && tf(w, n) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id := r.PathValue("id")
	if _, i := p.findTunnelLocked(id); i < 0 {
		failReason(w, http.StatusNotFound, "tunnel tidak ditemukan", api.ReasonTunnelNotFound)
		return
	}
	tk := fmt.Sprintf("tt-%d", n)
	p.tun.tickets[tk] = id
	okStatus(w, http.StatusCreated, map[string]any{"ticket": tk, "expires_in": 30, "agent_url": p.tun.agentURL})
}

func (p *fakePanel) tunnelState() (ticketN int, deletes []string, bodies []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tun.ticketN, append([]string(nil), p.tun.deletes...), append([]map[string]any(nil), p.tun.bodies...)
}

// ---------- gerbang palsu ----------

// fakeGateway — sisi gerbang kontrak kabel easyrun-tunnel-agent/protocol:
// terima WebSocket agen (Bearer tiket panel, sekali pakai), lalu jadi klien
// yamux yang membuka stream per "pengunjung".
type fakeGateway struct {
	p   *fakePanel
	srv *httptest.Server

	mu   sync.Mutex
	drop bool // putus segera setelah upgrade
	// reject — tolak jabat tangan bertanda (status, isi `error`) walau tiket sah.
	rejectStatus int
	rejectMsg    string
	headers      []http.Header
	accepted     int
	sessions     chan *yamux.Session
}

func newGateway(t *testing.T, p *fakePanel) *fakeGateway {
	t.Helper()
	g := &fakeGateway{p: p, sessions: make(chan *yamux.Session, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.AgentConnectPath, g.handle)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		g.srv.Close()
		for {
			select {
			case s := <-g.sessions:
				_ = s.Close()
			default:
				return
			}
		}
	})
	p.mu.Lock()
	p.tun.agentURL = "ws" + strings.TrimPrefix(g.srv.URL, "http") + protocol.AgentConnectPath
	p.mu.Unlock()
	return g
}

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	tk := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.p.mu.Lock()
	id, valid := g.p.tun.tickets[tk]
	delete(g.p.tun.tickets, tk)
	tun, _ := g.p.findTunnelLocked(id)
	g.p.mu.Unlock()
	g.mu.Lock()
	g.headers = append(g.headers, r.Header.Clone())
	drop := g.drop
	rejectStatus, rejectMsg := g.rejectStatus, g.rejectMsg
	g.mu.Unlock()
	if valid && rejectStatus != 0 {
		w.Header().Set(protocol.HeaderGatewayError, "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rejectStatus)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": rejectMsg})
		return
	}
	if !valid {
		w.Header().Set(protocol.HeaderGatewayError, "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": gatewayInvalidTicketMsg})
		return
	}
	hdr := http.Header{}
	hdr.Set(protocol.HeaderTunnelID, id)
	hdr.Set(protocol.HeaderTunnelHosts, tun.Host)
	hdr.Set(protocol.HeaderPublicScheme, "https")
	up := websocket.Upgrader{ReadBufferSize: protocol.WSChunkSize, WriteBufferSize: protocol.WSChunkSize}
	ws, err := up.Upgrade(w, r, hdr)
	if err != nil {
		return
	}
	nc := wsconn.New(ws)
	g.mu.Lock()
	g.accepted++
	g.mu.Unlock()
	if drop {
		_ = nc.Close()
		return
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.MaxStreamWindowSize = protocol.DefaultStreamWindow
	sess, err := yamux.Client(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	g.sessions <- sess
}

func (g *fakeGateway) state() (accepted int, headers []http.Header) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.accepted, append([]http.Header(nil), g.headers...)
}

// ---------- menjalankan expose di latar ----------

type exposeRun struct {
	out, errb *syncBuffer
	cancel    context.CancelFunc
	done      chan int
}

func (h *harness) startExpose(t *testing.T, args ...string) *exposeRun {
	t.Helper()
	x := &exposeRun{out: &syncBuffer{}, errb: &syncBuffer{}, done: make(chan int, 1)}
	h.a.Stdout, h.a.Stderr = x.out, x.errb
	h.a.ExposeBackoffMin, h.a.ExposeBackoffMax = 5*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	x.cancel = cancel
	go func() { x.done <- h.a.Run(ctx, append([]string{"expose"}, args...)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-x.done:
		case <-time.After(10 * time.Second):
			t.Error("expose tidak berhenti setelah dibatalkan")
		}
	})
	return x
}

// wait — kode keluar; gagal bila belum selesai dalam batas waktu.
func (x *exposeRun) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-x.done:
		x.done <- code // untuk cleanup
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("expose tidak selesai\nstdout:\n%s\nstderr:\n%s", x.out, x.errb)
		return -1
	}
}

func (x *exposeRun) stop(t *testing.T) int {
	t.Helper()
	x.cancel()
	return x.wait(t)
}

func (s *syncBuffer) contains(sub string) bool { return strings.Contains(s.String(), sub) }

func localApp(t *testing.T) (port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "halo dari laptop: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// visit — "pengunjung": satu GET HTTP/1.1 lewat stream yamux gerbang → agen
// → aplikasi lokal.
func visit(t *testing.T, sess *yamux.Session, host, path string) (int, string) {
	t.Helper()
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(st, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func (g *fakeGateway) nextSession(t *testing.T) *yamux.Session {
	t.Helper()
	select {
	case s := <-g.sessions:
		t.Cleanup(func() { _ = s.Close() })
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("agen tidak menyambung ke gerbang")
		return nil
	}
}

// ---------- uji ----------

// TestExposeUjungKeUjung — tunnel sementara: tiket dari panel → WSS ke
// gerbang (Bearer tiket) → pengunjung GET lewat stream sampai aplikasi
// lokal → Ctrl-C menghapus tunnel.
func TestExposeUjungKeUjung(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	port := localApp(t)

	x := h.startExpose(t, port, "--rewrite-host")
	sess := g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })

	status, body := visit(t, sess, "acak-1"+tunnelHostSuffix, "/cek")
	if status != http.StatusOK || body != "halo dari laptop: GET /cek" {
		t.Fatalf("jawaban lewat tunnel: %d %q", status, body)
	}
	if want := "https://acak-1" + tunnelHostSuffix + "  →  http://127.0.0.1:" + port; !x.out.contains(want) {
		t.Fatalf("stdout tanpa %q:\n%s", want, x.out)
	}

	_, hdrs := g.state()
	if len(hdrs) != 1 {
		t.Fatalf("sambungan ke gerbang: %d", len(hdrs))
	}
	if got := hdrs[0].Get("Authorization"); got != "Bearer tt-1" {
		t.Fatalf("Authorization ke gerbang = %q, mau tiket panel", got)
	}
	if got := hdrs[0].Get(protocol.HeaderHostRewrite); got != "127.0.0.1:"+port {
		t.Fatalf("%s = %q", protocol.HeaderHostRewrite, got)
	}
	if !strings.HasPrefix(hdrs[0].Get("User-Agent"), "easyrun/") {
		t.Fatalf("User-Agent = %q", hdrs[0].Get("User-Agent"))
	}
	if strings.Contains(fmt.Sprint(hdrs[0]), testToken) {
		t.Fatal("token CLI tidak boleh terkirim ke gerbang")
	}

	if code := x.stop(t); code != 0 {
		t.Fatalf("Ctrl-C harus keluar 0, dapat %d\n%s", code, x.errb)
	}
	_, deletes, bodies := h.p.tunnelState()
	if len(deletes) != 1 || deletes[0] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("tunnel sementara harus dihapus saat berhenti: %v", deletes)
	}
	if len(bodies) != 1 || len(bodies[0]) != 0 {
		t.Fatalf("tanpa --name badan harus {}: %v", bodies)
	}
	if !x.errb.contains("tunnel sementara acak-1 dihapus") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}

// TestExposeTanpaRewriteHost — tanpa --rewrite-host header Host asli
// pengunjung dipertahankan (header permintaan tidak dikirim).
func TestExposeTanpaRewriteHost(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	x := h.startExpose(t, localApp(t))
	g.nextSession(t)
	_, hdrs := g.state()
	if v := hdrs[0].Values(protocol.HeaderHostRewrite); len(v) != 0 {
		t.Fatalf("header %s tidak boleh dikirim: %v", protocol.HeaderHostRewrite, v)
	}
	x.stop(t)
}

// TestExposeNamaTetap — --name: tunnel tetap ada setelah berhenti; opsi
// boleh sebelum <port>; nama yang sudah ada dipakai ulang (200).
func TestExposeNamaTetap(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	existing := h.p.addTunnel("demo", false)
	g := newGateway(t, h.p)
	x := h.startExpose(t, "--name", "demo", "--host", "localhost", localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://demo" + tunnelHostSuffix) })
	if !x.out.contains("http://localhost:") {
		t.Fatalf("alamat lokal harus memakai --host:\n%s", x.out)
	}
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d", code)
	}
	_, deletes, bodies := h.p.tunnelState()
	if len(deletes) != 0 {
		t.Fatalf("tunnel bernama tidak boleh dihapus: %v", deletes)
	}
	if len(bodies) != 1 || bodies[0]["name"] != "demo" {
		t.Fatalf("badan POST /cli/tunnels: %v", bodies)
	}
	if h.p.n("POST /api/v1/cli/tunnels/"+existing.ID+"/ticket") == 0 {
		t.Fatal("tiket harus diminta untuk tunnel bernama yang sudah ada")
	}
	if !x.errb.contains("easyrun tunnels rm demo") {
		t.Fatalf("stderr harus menyebut cara menghapus:\n%s", x.errb)
	}
	// C2: 200 = tunnel yang sudah ada dipakai ulang — pengguna diberi tahu.
	if !x.errb.contains("memakai tunnel demo yang sudah ada (dibuat ") {
		t.Fatalf("stderr harus menyebut tunnel dipakai ulang:\n%s", x.errb)
	}
}

// TestExposeNamaBaruTanpaPesanPakaiUlang — 201 (nama baru) bukan pakai ulang.
func TestExposeNamaBaruTanpaPesanPakaiUlang(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	x := h.startExpose(t, "--name", "baru", localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://baru" + tunnelHostSuffix) })
	x.stop(t)
	if x.errb.contains("yang sudah ada") {
		t.Fatalf("tunnel baru tidak boleh disebut dipakai ulang:\n%s", x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 0 {
		t.Fatalf("tunnel bernama tidak boleh dihapus: %v", deletes)
	}
}

// TestExposeSambungUlangTiketBaru — gerbang memutus; setiap sambung ulang
// memakai tiket baru (tiket panel sekali pakai).
func TestExposeSambungUlangTiketBaru(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	g.drop = true
	x := h.startExpose(t, localApp(t))
	waitFor(t, "3 sambungan diterima gerbang", func() bool { n, _ := g.state(); return n >= 3 })
	x.stop(t)
	_, hdrs := g.state()
	seen := map[string]bool{}
	for _, hd := range hdrs {
		tk := hd.Get("Authorization")
		if seen[tk] {
			t.Fatalf("tiket dipakai ulang: %q", tk)
		}
		seen[tk] = true
	}
	if !x.errb.contains("menyambung ulang dalam") || !x.errb.contains("tersambung kembali") {
		t.Fatalf("pesan sambung ulang di stderr:\n%s", x.errb)
	}
	if x.out.contains("menyambung ulang") {
		t.Fatal("pesan sambung ulang tidak boleh di stdout")
	}
}

// TestExposeTiketDitolakBerhenti — kontrak F2: 401/403/404/409 dari panel
// saat meminta tiket sambung ulang = fatal; tunnel sementara tetap dihapus.
func TestExposeTiketDitolakBerhenti(t *testing.T) {
	cases := []struct {
		status       int
		msg, reason  string
		wantInStderr string
	}{
		{401, "token CLI sudah dicabut", "", "jalankan: easyrun login"},
		{403, "tunnel dinonaktifkan admin", api.ReasonTunnelRevoked, "tunnel ini dinonaktifkan admin EasyRun saat berjalan"},
		{404, "tunnel tidak ditemukan", api.ReasonTunnelNotFound, "tunnel ini sudah dihapus"},
		{409, "tunnel sedang dipakai perangkat lain", "", "tunnel sedang dipakai perangkat lain"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			g := newGateway(t, h.p)
			g.drop = true
			h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
				if n < 2 {
					return false
				}
				failReason(w, tc.status, tc.msg, tc.reason)
				return true
			}
			x := h.startExpose(t, localApp(t))
			if code := x.wait(t); code != 1 {
				t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
			}
			n, deletes, _ := h.p.tunnelState()
			if n != 2 {
				t.Fatalf("permintaan tiket = %d, mau berhenti setelah penolakan pertama", n)
			}
			if !x.errb.contains("easyrun: tunnel berhenti: ") || !x.errb.contains(tc.wantInStderr) {
				t.Fatalf("stderr tanpa %q:\n%s", tc.wantInStderr, x.errb)
			}
			if len(deletes) != 1 {
				t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
			}
		})
	}
}

// TestExposeTiketSementaraDicobaUlang — 429 dan 5xx dicoba ulang; 429
// menghormati Retry-After.
func TestExposeTiketSementaraDicobaUlang(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			g := newGateway(t, h.p)
			g.drop = true
			h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
				if n < 2 {
					return false
				}
				if status == 429 {
					w.Header().Set("Retry-After", "2")
					failReason(w, status, "terlalu sering meminta tiket", api.ReasonRateLimited)
				} else {
					fail(w, status, "panel sesaat bermasalah")
				}
				return true
			}
			x := h.startExpose(t, localApp(t))
			waitFor(t, "tiket diminta ulang", func() bool { n, _, _ := h.p.tunnelState(); return n >= 4 })
			if code := x.stop(t); code != 0 {
				t.Fatalf("kode %d\n%s", code, x.errb)
			}
			if status == 429 {
				h.clock.mu.Lock()
				sleeps := append([]time.Duration(nil), h.clock.sleeps...)
				h.clock.mu.Unlock()
				found := false
				for _, s := range sleeps {
					found = found || s == 2*time.Second
				}
				if !found {
					t.Fatalf("Retry-After 2 dtk tidak dihormati: %v", sleeps)
				}
				// C5: durasi tunggu sebenarnya diumumkan sebelum tidur.
				if !x.errb.contains("menunggu 2 detik sebelum menyambung ulang") {
					t.Fatalf("pesan tunggu Retry-After tidak dicetak:\n%s", x.errb)
				}
			}
		})
	}
}

// TestTicketErrorPemetaan — galat permintaan tiket → fatal atau dicoba ulang.
func TestTicketErrorPemetaan(t *testing.T) {
	cases := []struct {
		err   error
		fatal bool
	}{
		{&api.Error{Status: 400}, true},
		{&api.Error{Status: 401}, true},
		{&api.Error{Status: 403, Reason: api.ReasonTunnelNotAllowed}, true},
		{&api.Error{Status: 404}, true},
		{&api.Error{Status: 409}, true},
		{&api.Error{Status: 408}, false},
		{&api.Error{Status: 429}, false},
		{&api.Error{Status: 500}, false},
		{&api.Error{Status: 502}, false},
		{&api.Error{Status: 503}, false},
		{&api.NetError{Host: "api.contoh.test", Err: errors.New("batas waktu habis")}, false},
		{&api.RedirectError{Status: 302, Location: "https://lain.test/"}, true},
		{errors.New("respons tiket tunnel tidak lengkap"), false},
	}
	for _, tc := range cases {
		err := fmt.Errorf("ambil tiket: %w", ticketError(tc.err))
		var fe *agent.FatalError
		if got := errors.As(err, &fe); got != tc.fatal {
			t.Errorf("%v: fatal=%v, mau %v", tc.err, got, tc.fatal)
		}
		// Galat aslinya tetap terbaca untuk penjelasan ke pengguna.
		if !errors.Is(err, tc.err) {
			t.Errorf("%v: galat asli hilang dari rantai", tc.err)
		}
	}
	err := fmt.Errorf("ambil tiket: %w", ticketError(&api.Error{Status: 401, Message: "token CLI sudah dicabut"}))
	if s := explain(err); !strings.Contains(s, "easyrun login") {
		t.Fatalf("penjelasan 401: %q", s)
	}
}

func TestExposeArgumen(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	for _, args := range [][]string{
		{},
		{"abc"},
		{"0"},
		{"70000"},
		{"3000", "4000"},
		{"--bogus", "3000"},
		{"--host", "127.0.0.1:80", "3000"},
		{"--host", "", "3000"},
		{"--name"},
	} {
		if code := h.run(append([]string{"expose"}, args...)...); code != 2 {
			t.Errorf("expose %v: kode %d, mau 2\n%s", args, code, h.stderr)
		}
	}
	if h.p.n("POST /api/v1/cli/tunnels") != 0 {
		t.Fatal("argumen salah tidak boleh menghubungi panel")
	}
}

func TestExposeBelumMasuk(t *testing.T) {
	h := newHarness(t)
	if code := h.run("expose", "3000"); code != 1 || !strings.Contains(h.stderr.String(), "easyrun login") {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
}

// TestExposeAgentURLHarusWss — agent_url dari panel bukan wss (dan bukan
// loopback) ditolak sebelum menyambung; tunnel sementara tetap dihapus.
func TestExposeAgentURLHarusWss(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.tun.agentURL = "ws://gerbang.contoh.test" + protocol.AgentConnectPath
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d", code)
	}
	if !strings.Contains(h.stderr.String(), "agent_url dari server harus wss") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
	}
}

// TestExposeTiketPertamaDitolak — tiket pertama gagal: berhenti dengan
// penjelasan (belum pernah tersambung).
func TestExposeTiketPertamaDitolak(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.tun.ticketFail = func(w http.ResponseWriter, _ int) bool {
		failReason(w, 403, "akses tunnel tenant dimatikan", api.ReasonTunnelNotAllowed)
		return true
	}
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d", code)
	}
	if !strings.Contains(h.stderr.String(), "meminta tiket tunnel: tunnel publik belum diizinkan untuk tenant ini") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

func TestExposeBuatDitolak(t *testing.T) {
	cases := []struct {
		status      int
		msg, reason string
		want        string
	}{
		{409, "batas tunnel tenant tercapai", api.ReasonTunnelLimitReached, "easyrun tunnels rm <nama>"},
		{409, "nama tunnel sudah dipakai", api.ReasonTunnelNameTaken, "membuat tunnel: nama/alamat tunnel ini sudah dipakai — pilih nama lain lewat --name"},
		{400, "nama tunnel hanya boleh huruf kecil, angka, dan -", api.ReasonTunnelNameInvalid, "membuat tunnel: nama tunnel hanya boleh huruf kecil, angka, dan - — pilih nama lain lewat --name"},
		{400, "", api.ReasonTunnelNameInvalid, "nama tunnel tidak sah"},
		{403, "tunnel belum diizinkan untuk tenant ini", api.ReasonTunnelNotAllowed, "membuat tunnel: tunnel publik belum diizinkan untuk tenant ini — minta admin EasyRun"},
		{403, "hanya pembuat tunnel atau owner tenant yang boleh menghapusnya", api.ReasonTunnelForbidden, "membuat tunnel: tunnel bernama ini milik anggota lain di tenant ini — pilih nama lain lewat --name (owner org bisa menghapus tunnel itu lewat panel EasyRun)"},
		{403, "Tunnel belum tersedia", api.ReasonTunnelDisabled, "tunnel publik sedang dimatikan di platform"},
		{503, "", "", "tunnel publik belum tersedia di platform"},
		{503, "Tunnel publik sedang dimatikan", "", "Tunnel publik sedang dimatikan"},
	}
	for _, tc := range cases {
		h := newHarness(t)
		h.loggedIn(t)
		h.p.tun.createFail = func(w http.ResponseWriter) bool {
			failReason(w, tc.status, tc.msg, tc.reason)
			return true
		}
		if code := h.run("expose", "--name", "Demo!", "3000"); code != 1 {
			t.Fatalf("%d %s: kode %d", tc.status, tc.reason, code)
		}
		if !strings.Contains(h.stderr.String(), tc.want) {
			t.Fatalf("%d %s: stderr tanpa %q:\n%s", tc.status, tc.reason, tc.want, h.stderr)
		}
		if n, _, _ := h.p.tunnelState(); n != 0 {
			t.Fatal("tiket tidak boleh diminta bila tunnel gagal dibuat")
		}
	}
}

func TestTunnelsDaftar(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("tunnels"); code != 0 || !strings.Contains(h.stdout.String(), "Belum ada tunnel") {
		t.Fatalf("kode %d\n%s", code, h.stdout)
	}
	h.p.addTunnel("zeta", false)
	h.p.addTunnel("acak-2", true)
	h.p.mu.Lock()
	h.p.tun.list[0].Disabled = true
	h.p.mu.Unlock()
	if code := h.run("tunnels"); code != 0 {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
	out := h.stdout.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAMA") {
		t.Fatalf("tabel:\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "acak-2") || !strings.HasPrefix(lines[2], "zeta") {
		t.Fatalf("urut nama:\n%s", out)
	}
	for _, want := range []string{"https://zeta" + tunnelHostSuffix, "dinonaktifkan", "aktif", "ya", "tidak", "00000000-0000-4000-8000-000000000001"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tabel tanpa %q:\n%s", want, out)
		}
	}
}

func TestTunnelsRm(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	demo := h.p.addTunnel("demo", false)
	other := h.p.addTunnel("lain", false)

	if code := h.run("tunnels", "rm", "DEMO"); code != 0 {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "Tunnel demo dihapus") {
		t.Fatalf("stdout:\n%s", h.stdout)
	}
	if code := h.run("tunnels", "rm", strings.ToUpper(other.ID)); code != 0 {
		t.Fatalf("hapus lewat id: kode %d\n%s", code, h.stderr)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 2 || deletes[0] != demo.ID || deletes[1] != other.ID {
		t.Fatalf("DELETE: %v", deletes)
	}

	if code := h.run("tunnels", "rm", "tidak-ada"); code != 1 || !strings.Contains(h.stderr.String(), "tidak ditemukan — lihat: easyrun tunnels") {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
	for _, args := range [][]string{{"tunnels", "rm"}, {"tunnels", "rm", "a", "b"}, {"tunnels", "ls"}} {
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: kode %d, mau 2", args, code)
		}
	}

	h.p.addTunnel("milik-orang", false)
	h.p.tun.deleteFail = func(w http.ResponseWriter) bool {
		failReason(w, 403, "hanya pembuat atau owner/admin tenant yang boleh menghapus", api.ReasonTunnelForbidden)
		return true
	}
	if code := h.run("tunnels", "rm", "milik-orang"); code != 1 || !strings.Contains(h.stderr.String(), "menghapus tunnel milik-orang: hanya pembuat tunnel (atau owner tenant pribadi) yang boleh menghapusnya dari CLI — owner org bisa menghapusnya lewat panel EasyRun") {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
}

func TestExposeBantuan(t *testing.T) {
	h := newHarness(t)
	if code := h.run("expose", "--help"); code != 0 {
		t.Fatalf("kode %d", code)
	}
	for _, want := range []string{"--name NAMA", "--host HOST", "--rewrite-host", "Ctrl-C", "SIGTERM", "nohup easyrun expose", "tmux new -d", "--detach"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Fatalf("bantuan expose tanpa %q:\n%s", want, h.stdout)
		}
	}
	if code := h.run("--help"); code != 0 {
		t.Fatalf("kode %d", code)
	}
	for _, want := range []string{"easyrun expose <port>", "easyrun tunnels [rm <nama>]", "easyrun expose 3000"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Fatalf("bantuan tanpa %q:\n%s", want, h.stdout)
		}
	}
}

// TestExposeNamaSementaraSesiLainTidakDihapus — C1: --name menunjuk tunnel
// sementara yang sudah ada (200, dibuat sesi lain). Berhenti TIDAK boleh
// menghapusnya: hanya tunnel sementara yang dibuat perintah ini (201).
func TestExposeNamaSementaraSesiLainTidakDihapus(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	other := h.p.addTunnel("acak-lain", true)
	g := newGateway(t, h.p)
	x := h.startExpose(t, "--name", other.Name, localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://" + other.Host) })
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 0 {
		t.Fatalf("tunnel sementara milik sesi lain ikut dihapus: %v\n%s", deletes, x.errb)
	}
	if !x.errb.contains("memakai tunnel acak-lain yang sudah ada") || !x.errb.contains("dibuat sesi lain, jadi tidak dihapus") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
	if x.errb.contains("dihapus saat berhenti") {
		t.Fatalf("tidak boleh menjanjikan hapus saat berhenti:\n%s", x.errb)
	}
}

// TestExposeCtrlCSaatMembuat — C4: Ctrl-C datang saat POST /cli/tunnels
// berjalan (panel sudah menyimpan, jawaban belum sampai). Permintaan buat
// tidak ikut dibatalkan, jadi id diketahui dan tunnel sementara yang baru
// dibuat tetap dihapus; lalu keluar 0 tanpa meminta tiket.
func TestExposeCtrlCSaatMembuat(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	h.p.tun.createFail = func(http.ResponseWriter) bool {
		enterOnce.Do(func() { close(entered) })
		<-release
		return false // lanjut membuat seperti biasa
	}
	var stops sync.WaitGroup
	stops.Add(1)
	var stopOnce sync.Once
	h.a.StopSignals = func() { stopOnce.Do(stops.Done) }
	x := h.startExpose(t, "3000")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("POST /cli/tunnels tidak sampai ke panel")
	}
	x.cancel()
	// K2: pengguna diberi tahu sedang menunggu, dan perilaku sinyal bawaan
	// dikembalikan (Ctrl-C kedua keluar paksa) sebelum panel menjawab.
	waitFor(t, "pesan tunggu dicetak", func() bool {
		return x.errb.contains("menunggu jawaban panel (maks 10 detik) supaya tunnel sementara yang terlanjur dibuat bisa dihapus; Ctrl-C sekali lagi untuk keluar paksa")
	})
	stops.Wait()
	// Beri waktu klien membatalkan permintaannya (perilaku yang salah),
	// baru panel menjawab.
	select {
	case code := <-x.done:
		x.done <- code
	case <-time.After(200 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if code := x.wait(t); code != 0 {
		t.Fatalf("Ctrl-C harus keluar 0, dapat %d\n%s", code, x.errb)
	}
	n, deletes, _ := h.p.tunnelState()
	if len(deletes) != 1 || deletes[0] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("tunnel sementara yang baru dibuat harus dihapus: %v\n%s", deletes, x.errb)
	}
	if n != 0 {
		t.Fatalf("tiket tidak boleh diminta setelah Ctrl-C: %d", n)
	}
}

// TestExposeGerbangMenolakDibersihkan — C3: penolakan fatal gerbang (403
// bertanda) dicetak tanpa karakter kendali terminal.
func TestExposeGerbangMenolakDibersihkan(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	g.rejectStatus, g.rejectMsg = http.StatusForbidden, "token CLI sudah dicabut\x1b]0;judul palsu\x07 — jalankan easyrun login"
	x := h.startExpose(t, localApp(t))
	if code := x.wait(t); code != 1 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	errs := x.errb.String()
	if strings.ContainsAny(errs, "\x1b\x07") {
		t.Fatalf("karakter kendali lolos ke stderr: %q", errs)
	}
	if !strings.Contains(errs, "tunnel berhenti: ditolak gerbang tunnel: token CLI sudah dicabut]0;judul palsu — jalankan easyrun login") {
		t.Fatalf("stderr:\n%s", errs)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara buatan sendiri tetap dihapus: %v", deletes)
	}
}

func TestGatewayRejectText(t *testing.T) {
	cases := []struct {
		fe   *agent.FatalError
		want string
	}{
		{&agent.FatalError{Status: 401, Message: gatewayInvalidTicketMsg}, "gerbang tunnel menolak tiket sambungan: tiket tidak valid atau kedaluwarsa — ulangi perintahnya; bila tetap gagal, jalankan: easyrun login"},
		{&agent.FatalError{Status: 401, Message: ""}, "gerbang tunnel menolak tiket sambungan — ulangi perintahnya"},
		{&agent.FatalError{Status: 401, Message: "tiket\x1b[2J kedaluwarsa"}, "gerbang tunnel menolak tiket sambungan: tiket[2J kedaluwarsa — ulangi"},
		{&agent.FatalError{Status: 403, Message: ""}, "ditolak gerbang tunnel"},
		{&agent.FatalError{Status: 404, Message: "path\u202e salah"}, "gerbang tunnel menolak (HTTP 404): path salah"},
	}
	for _, tc := range cases {
		err := fmt.Errorf("ambil tiket: %w", tc.fe)
		if got := explainTunnel(opTunnelRun, err); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%+v: %q, mau berawalan %q", tc.fe, got, tc.want)
		}
	}
}

// TestTunnelHintErrorUnwrap — C6: galat asli terbaca lewat errors.As/Is.
func TestTunnelHintErrorUnwrap(t *testing.T) {
	ae := &api.Error{Status: 409, Reason: api.ReasonTunnelLimitReached}
	var got *api.Error
	if !errors.As(tunnelHintError{op: opTunnelCreate, err: ae}, &got) || got != ae {
		t.Fatal("errors.As *api.Error lewat tunnelHintError gagal")
	}
	if !errors.Is(tunnelHintError{err: fmt.Errorf("x: %w", context.Canceled)}, context.Canceled) {
		t.Fatal("errors.Is context.Canceled lewat tunnelHintError gagal")
	}
	if s := (tunnelHintError{op: opTunnelCreate, err: ae}).Error(); !strings.Contains(s, "easyrun tunnels rm <nama>") {
		t.Fatalf("petunjuk hilang: %q", s)
	}
}

// TestMatchTunnelIDLebihDulu — C7: nama berbentuk UUID tidak membuat id
// tunnel lain ambigu.
func TestMatchTunnelIDLebihDulu(t *testing.T) {
	victim := api.Tunnel{ID: "3f2a0000-0000-4000-8000-000000000001", Name: "korban"}
	squat := api.Tunnel{ID: "3f2a0000-0000-4000-8000-000000000002", Name: victim.ID}
	ts := []api.Tunnel{squat, victim}
	got, err := matchTunnel(ts, strings.ToUpper(victim.ID))
	if err != nil || got.ID != victim.ID {
		t.Fatalf("id persis harus menang: %+v %v", got, err)
	}
	if got, err := matchTunnel(ts, "korban"); err != nil || got.ID != victim.ID {
		t.Fatalf("nama: %+v %v", got, err)
	}
	if got, err := matchTunnel(ts, squat.ID); err != nil || got.ID != squat.ID {
		t.Fatalf("id tunnel kedua: %+v %v", got, err)
	}
}

// TestExplainTunnelSetiapReason — C9: setiap reason panel punya penjelasan
// Indonesia sendiri, sesuai langkahnya.
func TestExplainTunnelSetiapReason(t *testing.T) {
	cases := []struct {
		op     tunnelOp
		status int
		reason string
		retry  time.Duration
		want   string
	}{
		{opTunnelRun, 403, api.ReasonTunnelDisabled, 0, "tunnel publik sedang dimatikan di platform"},
		{opTunnelCreate, 403, api.ReasonTunnelNotAllowed, 0, "belum diizinkan untuk tenant ini — minta admin EasyRun"},
		{opTunnelCreate, 400, api.ReasonTunnelNameInvalid, 0, "nama tunnel tidak sah"},
		{opTunnelCreate, 409, api.ReasonTunnelNameTaken, 0, "nama/alamat tunnel ini sudah dipakai — pilih nama lain"},
		{opTunnelCreate, 409, api.ReasonTunnelLimitReached, 0, "batas jumlah tunnel tenant ini tercapai"},
		{opTunnelCreate, 403, api.ReasonTunnelForbidden, 0, "milik anggota lain di tenant ini — pilih nama lain lewat --name (owner org bisa menghapus tunnel itu lewat panel EasyRun)"},
		{opTunnelTicket, 403, api.ReasonTunnelForbidden, 0, "milik anggota lain di tenant ini — hanya pembuatnya yang boleh memakainya (owner org bisa menghapusnya lewat panel EasyRun)"},
		{opTunnelDelete, 403, api.ReasonTunnelForbidden, 0, "owner org bisa menghapusnya lewat panel EasyRun"},
		{opTunnelRun, 403, api.ReasonTunnelRevoked, 0, "dinonaktifkan admin EasyRun saat berjalan"},
		{opTunnelTicket, 403, api.ReasonTunnelRevoked, 0, "sedang dinonaktifkan admin EasyRun — hubungi admin EasyRun untuk menyalakannya lagi"},
		{opTunnelDelete, 403, api.ReasonTunnelRevoked, 0, "sedang dinonaktifkan admin EasyRun, jadi tidak bisa dihapus — hubungi admin EasyRun"},
		{opTunnelRun, 404, api.ReasonTunnelNotFound, 0, "sudah dihapus"},
		{opTunnelDelete, 404, api.ReasonTunnelNotFound, 0, "tunnel tidak ditemukan (mungkin sudah dihapus) — lihat: easyrun tunnels"},
		{opTunnelTicket, 429, api.ReasonRateLimited, 45 * time.Second, "terlalu sering meminta tiket tunnel — coba lagi dalam 45 detik"},
		{opTunnelTicket, 429, api.ReasonRateLimited, 0, "terlalu sering meminta tiket tunnel — coba lagi sebentar lagi"},
		{opTunnelCreate, 429, api.ReasonRateLimited, 0, "terlalu banyak permintaan ke panel — coba lagi sebentar lagi"},
		{opTunnelList, 429, api.ReasonRateLimited, 0, "terlalu banyak permintaan ke panel"},
		{opTunnelDelete, 429, api.ReasonRateLimited, 0, "terlalu banyak permintaan ke panel"},
	}
	for _, tc := range cases {
		err := &api.Error{Status: tc.status, Reason: tc.reason, RetryAfter: tc.retry, Message: ""}
		if got := explainTunnel(tc.op, err); !strings.Contains(got, tc.want) {
			t.Errorf("%d %s (op %d): %q tanpa %q", tc.status, tc.reason, tc.op, got, tc.want)
		}
	}
}

// TestExposeCtrlCSaatMembuatBernama — K2: dengan --name tunnel tetap ada
// setelah berhenti, jadi Ctrl-C saat POST /cli/tunnels langsung berhenti
// tanpa menunggu panel.
func TestExposeCtrlCSaatMembuatBernama(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	t.Cleanup(func() { close(release) })
	h.p.tun.createFail = func(http.ResponseWriter) bool {
		enterOnce.Do(func() { close(entered) })
		<-release
		return false
	}
	x := h.startExpose(t, "--name", "demo", "3000")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("POST /cli/tunnels tidak sampai ke panel")
	}
	x.cancel()
	if code := x.wait(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if x.errb.contains("menunggu jawaban panel") {
		t.Fatalf("--name tidak perlu menunggu panel:\n%s", x.errb)
	}
}

// TestExposeHapusSementaraDinonaktifkan — K3: DELETE saat berhenti dijawab
// 403 tunnel_revoked = selesai: catatan dinonaktifkan admin, tanpa saran rm.
func TestExposeHapusSementaraDinonaktifkan(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	h.p.tun.deleteFail = func(w http.ResponseWriter) bool {
		failReason(w, 403, "tunnel dinonaktifkan admin dan tidak bisa dihapus — hubungi admin EasyRun", api.ReasonTunnelRevoked)
		return true
	}
	x := h.startExpose(t, localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if !x.errb.contains("tunnel sementara acak-1 dinonaktifkan admin EasyRun, jadi tidak dihapus dari sini") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
	if x.errb.contains("easyrun tunnels rm") {
		t.Fatalf("tidak boleh menyarankan rm untuk tunnel yang dinonaktifkan:\n%s", x.errb)
	}
}

// TestExposeHapusSementaraGagalSaranRm — penolakan lain saat berhenti tetap
// menyebut perintah rm.
func TestExposeHapusSementaraGagalSaranRm(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	h.p.tun.deleteFail = func(w http.ResponseWriter) bool {
		fail(w, 500, "panel sesaat bermasalah")
		return true
	}
	x := h.startExpose(t, localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	x.stop(t)
	if !x.errb.contains("hapus nanti dengan: easyrun tunnels rm acak-1") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}

// TestExposeBatasTercapaiPetunjukDinonaktifkan — K5: tunnel_limit_reached
// dan daftar memuat tunnel yang dinonaktifkan admin → petunjuk bahwa tunnel
// itu ikut dihitung dan tidak bisa dihapus. Tanpa tunnel nonaktif: tanpa
// petunjuk itu.
func TestExposeBatasTercapaiPetunjukDinonaktifkan(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(fmt.Sprint(disabled), func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			h.p.addTunnel("lama", false)
			h.p.mu.Lock()
			h.p.tun.list[0].Disabled = disabled
			h.p.mu.Unlock()
			h.p.tun.createFail = func(w http.ResponseWriter) bool {
				failReason(w, 409, "batas tunnel tenant tercapai", api.ReasonTunnelLimitReached)
				return true
			}
			if code := h.run("expose", "3000"); code != 1 {
				t.Fatalf("kode %d\n%s", code, h.stderr)
			}
			errs := h.stderr.String()
			if !strings.Contains(errs, "batas jumlah tunnel tenant ini tercapai") {
				t.Fatalf("stderr:\n%s", errs)
			}
			const hint = "1 tunnel dinonaktifkan admin EasyRun dan tetap dihitung dalam batas (lama); tunnel itu tidak bisa dihapus"
			if got := strings.Contains(errs, hint); got != disabled {
				t.Fatalf("petunjuk tunnel nonaktif = %v, mau %v:\n%s", got, disabled, errs)
			}
		})
	}
}

// TestExposeGerbangTiketTidakValid — K4: 401 bertanda gerbang sungguhan
// berisi kalimat Indonesia; diteruskan ke pengguna apa adanya (dibersihkan).
func TestExposeGerbangTiketTidakValid(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	newGateway(t, h.p)
	// Tiket panel tidak dikenal gerbang palsu → 401 bertanda.
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		okStatus(w, http.StatusCreated, map[string]any{"ticket": "tt-asing", "expires_in": 30, "agent_url": h.p.tun.agentURL})
		return true
	}
	x := h.startExpose(t, "3000")
	if code := x.wait(t); code != 1 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if !x.errb.contains("tunnel berhenti: gerbang tunnel menolak tiket sambungan: tiket tidak valid atau kedaluwarsa — ulangi perintahnya") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}
