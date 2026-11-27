package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// tunnelT — bentuk `T` persis kontrak (bukan tag struct klien). Active nil =
// panel lama tanpa field aditif `active`/`last_seen_at`; selain itu keduanya
// selalu dikirim (`last_seen_at` null bila kosong).
func tunnelT(t api.Tunnel) map[string]any {
	m := map[string]any{
		"id": t.ID, "name": t.Name, "host": t.Host, "url": t.URL,
		"ephemeral": t.Ephemeral, "disabled": t.Disabled, "created_at": t.CreatedAt,
	}
	if t.Active != nil {
		m["active"] = *t.Active
		m["last_seen_at"] = nil
		if t.LastSeenAt != "" {
			m["last_seen_at"] = t.LastSeenAt
		}
	}
	return m
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
			// Panel sungguhan (createNamed): nama milik tunnel sementara =
			// 409 tunnel_name_taken (tak bisa diambil lewat nama), jadi 200
			// tidak pernah membawa ephemeral=true.
			if t.Ephemeral {
				failReason(w, http.StatusConflict, "nama tunnel sudah dipakai — pilih nama lain", api.ReasonTunnelNameTaken)
				return
			}
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
	// down — true = gerbang sedang restart: 503 tanpa tanda (seperti Ingress),
	// tiket tidak ditukar.
	down func() bool
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
	g.mu.Lock()
	down := g.down
	g.mu.Unlock()
	if down != nil && down() {
		http.Error(w, "gerbang sedang restart", http.StatusServiceUnavailable)
		return
	}
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
	// Jeda sambung ulang agen (waktu sungguhan) dipendekkan, kecuali uji
	// sudah mengisinya sendiri.
	if h.a.ExposeBackoffMin == 0 {
		h.a.ExposeBackoffMin, h.a.ExposeBackoffMax = 5*time.Millisecond, 50*time.Millisecond
	}
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

// haloLaptop — aplikasi lokal uji: menjawab metode dan path permintaan.
func haloLaptop(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "halo dari laptop: %s %s", r.Method, r.URL.Path)
}

// localApp — aplikasi lokal yang listen di 127.0.0.1 saja (httptest).
func localApp(t *testing.T) (port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(haloLaptop))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// localApp6 — aplikasi lokal yang HANYA listen di [::1], seperti Vite di
// Node >= 17 macOS dengan host bawaan "localhost". Dilewati bila IPv6
// loopback tidak tersedia, bila "localhost" di sistem ini tidak mencakup ::1,
// atau bila 127.0.0.1 dengan port yang sama kebetulan dipakai proses lain
// (uji ini harus membuktikan jalur ::1).
func localApp6(t *testing.T) (port string) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback tidak tersedia: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(haloLaptop))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), "localhost")
	if err != nil {
		t.Skipf("localhost tidak bisa di-resolve: %v", err)
	}
	has6 := false
	for _, a := range addrs {
		has6 = has6 || a == "::1"
	}
	if !has6 {
		t.Skipf("localhost di sistem ini tidak mencakup ::1: %v", addrs)
	}
	if c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", port), time.Second); err == nil {
		_ = c.Close()
		t.Skipf("127.0.0.1:%s dipakai proses lain", port)
	}
	return port
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
	// R6: --host bawaan "localhost" (bukan 127.0.0.1); aplikasi httptest
	// hanya listen di 127.0.0.1 dan tetap terjangkau tanpa peringatan.
	if want := "https://acak-1" + tunnelHostSuffix + "  →  http://localhost:" + port; !x.out.contains(want) {
		t.Fatalf("stdout tanpa %q:\n%s", want, x.out)
	}
	if x.errb.contains("belum ada aplikasi") {
		t.Fatalf("aplikasi di 127.0.0.1 harus terjangkau lewat localhost:\n%s", x.errb)
	}

	_, hdrs := g.state()
	if len(hdrs) != 1 {
		t.Fatalf("sambungan ke gerbang: %d", len(hdrs))
	}
	if got := hdrs[0].Get("Authorization"); got != "Bearer tt-1" {
		t.Fatalf("Authorization ke gerbang = %q, mau tiket panel", got)
	}
	if got := hdrs[0].Get(protocol.HeaderHostRewrite); got != "localhost:"+port {
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
	x := h.startExpose(t, "--name", "demo", "--host", "127.0.0.1", localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://demo" + tunnelHostSuffix) })
	if !x.out.contains("http://127.0.0.1:") {
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

// TestExposeTiketDitolakBerhenti — kontrak F2: 401/403/404 dari panel saat
// meminta tiket sambung ulang = fatal; tunnel sementara tetap dihapus. Hanya
// penolakan yang memang dikirim panel untuk tiket (tanpa 409).
func TestExposeTiketDitolakBerhenti(t *testing.T) {
	cases := []struct {
		status       int
		msg, reason  string
		wantInStderr string
	}{
		{401, "token CLI sudah dicabut", "", "jalankan: easyrun login"},
		{403, "tunnel dinonaktifkan admin", api.ReasonTunnelRevoked, "tunnel ini dinonaktifkan admin EasyRun saat berjalan"},
		{404, "tunnel tidak ditemukan", api.ReasonTunnelNotFound, "tunnel ini sudah dihapus"},
		{403, "tunnel ini milik anggota lain — hanya pembuat tunnel atau owner tenant yang boleh memakainya; pilih nama lain", api.ReasonTunnelForbidden, "hanya pembuatnya yang boleh memakainya"},
		{403, "tunnel belum diizinkan untuk tenant ini — hubungi admin EasyRun", api.ReasonTunnelNotAllowed, "tunnel publik belum diizinkan untuk tenant ini"},
		{403, "akses akun ini ke tenant tersebut sudah berubah — jalankan easyrun login", "membership_lost", "jalankan easyrun login"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status, tc.reason), func(t *testing.T) {
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
		// Panel tidak mengirim 409 untuk tiket; aturan umum 4xx = fatal.
		{&api.Error{Status: 409}, true},
		{&api.Error{Status: 408}, false},
		{&api.Error{Status: 429}, false},
		{&api.Error{Status: 500}, false},
		{&api.Error{Status: 502}, false},
		{&api.Error{Status: 503}, false},
		// Saklar tunnel mati = berhenti; 503 tanpa reason tetap dicoba ulang.
		{&api.Error{Status: 503, Reason: api.ReasonTunnelDisabled}, true},
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

// TestExposeRewriteHostDivalidasiSebelumMembuat — dengan --rewrite-host,
// HOST:PORT dikirim sebagai X-Easyrun-Host-Rewrite dan gerbang menolak
// nilai di luar nama host / IPv4 / [IPv6] (400 bertanda = fatal). --host
// seperti itu harus ditolak sebagai galat pemakaian yang menyebut --host,
// SEBELUM tunnel dibuat — bukan tunnel sementara dibuat lalu jabat tangan
// gagal dengan nama header internal. Tanpa --rewrite-host nilainya hanya
// dipakai dial lokal, jadi tetap diterima.
func TestExposeRewriteHostDivalidasiSebelumMembuat(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	// Pengaman: bila validasinya hilang, pembuatan tunnel ditolak panel
	// supaya uji gagal cepat (kode 1 + POST tercatat), tidak menyambung
	// ke gerbang tanpa batas waktu.
	h.p.tun.createFail = func(w http.ResponseWriter) bool {
		failReason(w, http.StatusForbidden, "akses tunnel tenant dimatikan", api.ReasonTunnelNotAllowed)
		return true
	}
	tolak := []string{
		"my_app",                 // garis bawah
		"fe80::1%lo0",            // zona IPv6
		"[fe80::1%lo0]",          // zona IPv6 berkurung
		"münchen.test",           // bukan ASCII
		strings.Repeat("a", 254), // melebihi 253 karakter
	}
	for _, host := range tolak {
		code := h.run("expose", "--rewrite-host", "--host", host, "3000")
		if code != 2 {
			t.Errorf("--host %q --rewrite-host: kode %d, mau 2\n%s", host, code, h.stderr)
			continue
		}
		if errs := h.stderr.String(); !strings.Contains(errs, "--host") || !strings.Contains(errs, "--rewrite-host") || strings.Contains(errs, protocol.HeaderHostRewrite) {
			t.Errorf("--host %q --rewrite-host: pesan harus menyebut --host dan --rewrite-host, bukan header internal:\n%s", host, errs)
		}
	}
	if n := h.p.n("POST /api/v1/cli/tunnels"); n != 0 {
		t.Fatalf("--host yang ditolak gerbang tidak boleh membuat tunnel: %d", n)
	}

	terima := []struct {
		args []string
		want string // nilai Host-Rewrite (localAddr)
	}{
		{[]string{"--rewrite-host", "3000"}, "localhost:3000"},
		{[]string{"--rewrite-host", "--host", "127.0.0.1", "3000"}, "127.0.0.1:3000"},
		{[]string{"--rewrite-host", "--host", "::1", "3000"}, "[::1]:3000"},
		{[]string{"--rewrite-host", "--host", "[::1]", "3000"}, "[::1]:3000"},
		{[]string{"--rewrite-host", "--host", "app-1.test", "3000"}, "app-1.test:3000"},
		// Tanpa --rewrite-host: hanya dial lokal (mis. nama di /etc/hosts).
		{[]string{"--host", "my_app", "3000"}, "my_app:3000"},
		{[]string{"--host", "fe80::1%lo0", "3000"}, "[fe80::1%lo0]:3000"},
	}
	for _, tc := range terima {
		x, code, ok := h.a.parseExposeArgs(tc.args)
		if !ok {
			t.Errorf("%v: ditolak (kode %d)\n%s", tc.args, code, h.stderr)
			continue
		}
		if got := x.localAddr(); got != tc.want {
			t.Errorf("%v: localAddr %q, mau %q", tc.args, got, tc.want)
		}
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
		// Saklar TUNNEL_ENABLED mati: panel menjawab 503 + tunnel_disabled.
		{503, "Tunnel belum tersedia", api.ReasonTunnelDisabled, "tunnel publik sedang dimatikan di platform"},
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
	for _, want := range []string{"--name NAMA", "--host HOST", "bawaan localhost", "::1", "--rewrite-host", "Ctrl-C", "SIGTERM", "nohup easyrun expose", "tmux new -d", "--detach"} {
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

// TestExposeNamaTunnelSementaraDitolak — --name menunjuk tunnel sementara
// yang sudah ada: panel menjawab 409 tunnel_name_taken (bukan 200), jadi
// tunnel itu tidak disambung, tidak dihapus, dan tiket tidak diminta.
func TestExposeNamaTunnelSementaraDitolak(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	other := h.p.addTunnel("acak-lain", true)
	if code := h.run("expose", "--name", other.Name, "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "membuat tunnel: nama/alamat tunnel ini sudah dipakai — pilih nama lain lewat --name") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
	if n, deletes, _ := h.p.tunnelState(); n != 0 || len(deletes) != 0 {
		t.Fatalf("tiket %d / hapus %v: tunnel sementara orang lain tidak boleh disentuh", n, deletes)
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
		// R4: 401 bertanda gerbang selalu soal tiket sambungan dan baru fatal
		// setelah dicoba ulang dengan tiket baru — saran ulangi perintah,
		// bukan login.
		{&agent.FatalError{Status: 401, Message: gatewayInvalidTicketMsg}, "gerbang tunnel berulang kali menolak tiket sambungan: tiket tidak valid atau kedaluwarsa — ulangi perintahnya"},
		{&agent.FatalError{Status: 401, Message: ""}, "gerbang tunnel berulang kali menolak tiket sambungan — ulangi perintahnya"},
		{&agent.FatalError{Status: 401, Message: "tiket\x1b[2J kedaluwarsa"}, "gerbang tunnel berulang kali menolak tiket sambungan: tiket[2J kedaluwarsa — ulangi perintahnya"},
		{&agent.FatalError{Status: 403, Message: ""}, "ditolak gerbang tunnel"},
		{&agent.FatalError{Status: 404, Message: "path\u202e salah"}, "gerbang tunnel menolak (HTTP 404): path salah"},
	}
	for _, tc := range cases {
		err := fmt.Errorf("ambil tiket: %w", tc.fe)
		got := explainTunnel(opTunnelRun, err)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%+v: %q, mau berawalan %q", tc.fe, got, tc.want)
		}
		if tc.fe.Status == http.StatusUnauthorized && (got != tc.want || strings.Contains(got, "easyrun login")) {
			t.Errorf("%+v: %q, mau persis %q (tanpa saran login)", tc.fe, got, tc.want)
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
		{opTunnelRun, 503, api.ReasonTunnelDisabled, 0, "tunnel publik sedang dimatikan di platform"},
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
// R4: setiap 401 dicoba ulang dengan tiket baru sampai batas agen, baru
// berhenti — dan pesannya menyarankan mengulang perintah, bukan login.
func TestExposeGerbangTiketTidakValid(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	// Tiket panel tidak dikenal gerbang palsu → 401 bertanda.
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		okStatus(w, http.StatusCreated, map[string]any{"ticket": fmt.Sprintf("tt-asing-%d", n), "expires_in": 30, "agent_url": h.p.tun.agentURL})
		return true
	}
	x := h.startExpose(t, "3000")
	if code := x.wait(t); code != 1 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if !x.errb.contains("tunnel berhenti: gerbang tunnel berulang kali menolak tiket sambungan: tiket tidak valid atau kedaluwarsa — ulangi perintahnya\n") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
	if x.errb.contains("easyrun login") {
		t.Fatalf("401 gerbang bukan soal login, jangan sarankan easyrun login:\n%s", x.errb)
	}
	// Tiket pertama + satu tiket baru untuk setiap percobaan ulang.
	wantTickets := agent.DefaultMaxTicketRejects + 1
	n, deletes, _ := h.p.tunnelState()
	if n != wantTickets {
		t.Fatalf("permintaan tiket = %d, mau %d (dicoba ulang dengan tiket baru sebelum berhenti)\n%s", n, wantTickets, x.errb)
	}
	_, hdrs := g.state()
	if len(hdrs) != wantTickets {
		t.Fatalf("sambungan ke gerbang = %d, mau %d", len(hdrs), wantTickets)
	}
	seen := map[string]bool{}
	for _, hd := range hdrs {
		tk := hd.Get("Authorization")
		if seen[tk] {
			t.Fatalf("tiket dipakai ulang: %q", tk)
		}
		seen[tk] = true
	}
	if len(deletes) != 1 {
		t.Fatalf("tunnel sementara buatan sendiri tetap dihapus: %v", deletes)
	}
}

// TestExposeGerbangTiketDitolakLaluPulih — R4: 401 bertanda gerbang yang
// sembuh dengan tiket baru (mis. tiket kedaluwarsa di jabat tangan lambat)
// tidak menghentikan expose: agen menyambung ulang dan pengunjung dilayani.
func TestExposeGerbangTiketDitolakLaluPulih(t *testing.T) {
	const rejected = agent.DefaultMaxTicketRejects - 1
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		if n > rejected {
			return false // tiket sah dari panel palsu
		}
		okStatus(w, http.StatusCreated, map[string]any{"ticket": fmt.Sprintf("tt-asing-%d", n), "expires_in": 30, "agent_url": h.p.tun.agentURL})
		return true
	}
	x := h.startExpose(t, localApp(t))
	sess := g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if status, body := visit(t, sess, "acak-1"+tunnelHostSuffix, "/cek"); status != http.StatusOK || body != "halo dari laptop: GET /cek" {
		t.Fatalf("jawaban lewat tunnel: %d %q", status, body)
	}
	if _, hdrs := g.state(); len(hdrs) != rejected+1 {
		t.Fatalf("sambungan ke gerbang = %d, mau %d", len(hdrs), rejected+1)
	}
	errs := x.errb.String()
	// Belum pernah siap: "gagal menyambung … mencoba lagi", bukan "terputus".
	if got := strings.Count(errs, "easyrun: gagal menyambung tunnel ("); got != rejected {
		t.Fatalf("pesan coba lagi = %d, mau %d:\n%s", got, rejected, errs)
	}
	if strings.Contains(errs, "terputus") {
		t.Fatalf("stderr:\n%s", errs)
	}
	if !strings.Contains(errs, gatewayInvalidTicketMsg) || strings.Contains(errs, "tunnel berhenti") {
		t.Fatalf("stderr:\n%s", errs)
	}
	if code := x.stop(t); code != 0 {
		t.Fatalf("Ctrl-C harus keluar 0, dapat %d\n%s", code, x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus saat berhenti: %v", deletes)
	}
}

// TestExposeBawaanMenjangkauAplikasiHanyaIPv6 — R6: tanpa --host, dev server
// yang hanya listen di ::1 (Vite di Node >= 17 macOS) tetap terjangkau:
// pengunjung dapat 200 dan tidak ada peringatan "belum ada aplikasi".
func TestExposeBawaanMenjangkauAplikasiHanyaIPv6(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	port := localApp6(t)
	x := h.startExpose(t, port, "--rewrite-host")
	sess := g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if status, body := visit(t, sess, "acak-1"+tunnelHostSuffix, "/cek"); status != http.StatusOK || body != "halo dari laptop: GET /cek" {
		t.Fatalf("jawaban lewat tunnel: %d %q\n%s", status, body, x.errb)
	}
	if x.errb.contains("belum ada aplikasi") {
		t.Fatalf("aplikasi di ::1 dianggap belum jalan:\n%s", x.errb)
	}
	if want := "  →  http://localhost:" + port; !x.out.contains(want) {
		t.Fatalf("stdout tanpa %q:\n%s", want, x.out)
	}
	if _, hdrs := g.state(); hdrs[0].Get(protocol.HeaderHostRewrite) != "localhost:"+port {
		t.Fatalf("%s = %q", protocol.HeaderHostRewrite, hdrs[0].Get(protocol.HeaderHostRewrite))
	}
	x.stop(t)
}

// TestExposePeringatanBelumAdaAplikasi — probe bawaan memakai alamat yang
// sama dengan agen: port kosong → peringatan menyebut localhost:PORT, tunnel
// tetap dibuka.
func TestExposePeringatanBelumAdaAplikasi(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	x := h.startExpose(t, port)
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if want := "easyrun: peringatan: belum ada aplikasi di localhost:" + port + ";"; !x.errb.contains(want) {
		t.Fatalf("stderr tanpa %q:\n%s", want, x.errb)
	}
	x.stop(t)
}

// ---------- review akhir F2 (fix5) ----------

// setTunnel — ubah tunnel palsu bernama name di bawah kunci panel.
func (p *fakePanel) setTunnel(name string, fn func(*api.Tunnel)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.tun.list {
		if p.tun.list[i].Name == name {
			fn(&p.tun.list[i])
		}
	}
}

// closedLoopbackPort — port 127.0.0.1 yang baru saja dilepas (tidak ada yang
// listen).
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// TestExposeTunnelSedangDipakaiPeringatan — C1: 200 (tunnel bernama yang
// sudah ada) dengan active=true → peringatan ke stderr SEBELUM menyambung
// (sesi bernama sama di perangkat lain saling mengganti di gerbang), lalu
// tetap tersambung. active=false atau panel lama tanpa field = tanpa
// peringatan.
func TestExposeTunnelSedangDipakaiPeringatan(t *testing.T) {
	yes, no := true, false
	const seen = "2026-11-27T02:30:00Z"
	cases := []struct {
		name     string
		active   *bool
		lastSeen string
		warn     bool
	}{
		{"aktif-dengan-waktu", &yes, seen, true},
		{"aktif-tanpa-waktu", &yes, "", true},
		{"tidak-aktif", &no, seen, false},
		{"panel-lama", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			h.p.addTunnel("demo", false)
			h.p.setTunnel("demo", func(tn *api.Tunnel) { tn.Active, tn.LastSeenAt = tc.active, tc.lastSeen })
			g := newGateway(t, h.p)
			x := h.startExpose(t, "--name", "demo", localApp(t))
			g.nextSession(t)
			waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://demo" + tunnelHostSuffix) })
			if code := x.stop(t); code != 0 {
				t.Fatalf("kode %d\n%s", code, x.errb)
			}
			errs := x.errb.String()
			const warn = "easyrun: peringatan: tunnel demo tampaknya sedang dipakai sesi lain"
			if got := strings.Contains(errs, warn); got != tc.warn {
				t.Fatalf("peringatan = %v, mau %v:\n%s", got, tc.warn, errs)
			}
			if !tc.warn {
				return
			}
			for _, want := range []string{"keduanya akan saling memutus", "hentikan salah satunya"} {
				if !strings.Contains(errs, want) {
					t.Fatalf("peringatan tanpa %q:\n%s", want, errs)
				}
			}
			if strings.Index(errs, warn) > strings.Index(errs, "Ctrl-C untuk berhenti") {
				t.Fatalf("peringatan harus dicetak sebelum menyambung:\n%s", errs)
			}
			wantSeen := "(terakhir aktif " + createdAt(seen) + ")"
			if got := strings.Contains(errs, wantSeen); got != (tc.lastSeen != "") {
				t.Fatalf("%q tercetak = %v, mau %v:\n%s", wantSeen, got, tc.lastSeen != "", errs)
			}
			if tc.lastSeen == "" && strings.Contains(errs, "terakhir aktif") {
				t.Fatalf("last_seen_at null tidak boleh menyebut waktu:\n%s", errs)
			}
		})
	}
}

// TestTunnelsKolomTersambung — C1: `easyrun tunnels` menampilkan field aditif
// active panel; panel lama (tanpa field) = "-".
func TestTunnelsKolomTersambung(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	yes, no := true, false
	h.p.addTunnel("a-aktif", false)
	h.p.addTunnel("b-diam", false)
	h.p.addTunnel("c-lama", false)
	h.p.setTunnel("a-aktif", func(tn *api.Tunnel) { tn.Active = &yes })
	h.p.setTunnel("b-diam", func(tn *api.Tunnel) { tn.Active = &no })
	if code := h.run("tunnels"); code != 0 {
		t.Fatalf("kode %d\n%s", code, h.stderr)
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("tabel:\n%s", h.stdout)
	}
	col := -1
	for i, f := range strings.Fields(lines[0]) {
		if f == "TERSAMBUNG" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("kolom TERSAMBUNG tidak ada:\n%s", h.stdout)
	}
	for i, want := range []string{"ya", "tidak", "-"} {
		if f := strings.Fields(lines[i+1]); len(f) <= col || f[col] != want {
			t.Fatalf("baris %d: TERSAMBUNG mau %q:\n%s", i+1, want, h.stdout)
		}
	}
}

// TestExposeBuatTenggatSementara — C2/N3: tanpa --name panel tidak menjawab
// POST /cli/tunnels → kalimat Indonesia (bukan "context deadline exceeded"
// atau galat jaringan) bahwa tunnel sementara mungkin sudah terbuat, cara
// memeriksa dan menghapusnya, exit 1. Dua jalan: batas klien biasa habis
// tanpa Ctrl-C, atau batas tunggu exposeCreateTimeout yang baru dihitung
// SETELAH Ctrl-C.
func TestExposeBuatTenggatSementara(t *testing.T) {
	for _, ctrlC := range []bool{false, true} {
		t.Run(fmt.Sprint("ctrl-c=", ctrlC), func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			const limit = 200 * time.Millisecond
			h.a.ExposeCreateTimeout = limit
			if !ctrlC {
				// Batas klien biasa (api.RequestTimeout), dipendekkan.
				h.a.HTTP = &http.Client{Timeout: 300 * time.Millisecond, Transport: h.p.srv.Client().Transport}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { close(release) })
			h.p.tun.createFail = func(http.ResponseWriter) bool {
				once.Do(func() { close(entered) })
				<-release
				return false
			}
			x := h.startExpose(t, "3000")
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("POST /cli/tunnels tidak sampai ke panel")
			}
			if ctrlC {
				// Panel lambat melewati exposeCreateTimeout: tanpa Ctrl-C itu
				// masih menunggu (bukan tenggat sendiri).
				time.Sleep(3 * limit / 2)
				select {
				case code := <-x.done:
					x.done <- code
					t.Fatalf("berhenti sebelum Ctrl-C (kode %d):\n%s", code, x.errb)
				default:
				}
				x.cancel()
			}
			if code := x.wait(t); code != 1 {
				t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
			}
			errs := x.errb.String()
			for _, want := range []string{
				"easyrun: membuat tunnel: panel tidak menjawab dalam 1 detik",
				"tunnel sementara mungkin sudah terbuat",
				"cek dengan: easyrun tunnels",
				"easyrun tunnels rm <nama>",
			} {
				if !strings.Contains(errs, want) {
					t.Fatalf("stderr tanpa %q:\n%s", want, errs)
				}
			}
			if got := strings.Contains(errs, "menunggu jawaban panel (maks 1 detik)"); got != ctrlC {
				t.Fatalf("pesan tunggu = %v, mau %v:\n%s", got, ctrlC, errs)
			}
			if strings.Contains(errs, "deadline") || strings.Contains(errs, "tunnel dihentikan") || strings.Contains(errs, "dihapus otomatis") || strings.Contains(errs, "periksa koneksi") {
				t.Fatalf("stderr:\n%s", errs)
			}
			if n, _, _ := h.p.tunnelState(); n != 0 {
				t.Fatalf("tiket tidak boleh diminta: %d", n)
			}
		})
	}
}

// TestExposeBuatLambatTanpaCtrlC — N3: tanpa --name dan tanpa Ctrl-C, panel
// yang sah lambat (lebih lama dari exposeCreateTimeout, mis. tenant terkena
// batas) tetap ditunggu sampai batas klien biasa — tunnel tersambung.
func TestExposeBuatLambatTanpaCtrlC(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.a.ExposeCreateTimeout = 50 * time.Millisecond
	h.p.tun.createFail = func(http.ResponseWriter) bool {
		time.Sleep(300 * time.Millisecond)
		return false
	}
	g := newGateway(t, h.p)
	x := h.startExpose(t, localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if x.errb.contains("tidak menjawab") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus saat berhenti: %v", deletes)
	}
}

// TestExposeNamaTanpaTenggatBuat — C2: dengan --name POST /cli/tunnels tidak
// memakai batas pendek tunnel sementara (cukup batas klien biasa, tetap bisa
// dibatalkan Ctrl-C): panel yang lambat tetap berakhir tersambung.
func TestExposeNamaTanpaTenggatBuat(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.a.ExposeCreateTimeout = 50 * time.Millisecond
	h.p.tun.createFail = func(http.ResponseWriter) bool {
		time.Sleep(300 * time.Millisecond)
		return false
	}
	g := newGateway(t, h.p)
	x := h.startExpose(t, "--name", "lambat", localApp(t))
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://lambat" + tunnelHostSuffix) })
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if x.errb.contains("deadline") || x.errb.contains("tidak menjawab") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}

// TestExposeTiketPertamaSementaraDicobaUlang — C3: sebelum tunnel pernah
// siap, tiket PERTAMA yang gagal sesaat (429, 5xx, jaringan) dicoba ulang
// dengan jeda (Retry-After dihormati), pesannya "gagal menyambung" (bukan
// "terputus"), lalu tunnel tetap tersambung.
func TestExposeTiketPertamaSementaraDicobaUlang(t *testing.T) {
	cases := []struct {
		name  string
		fails int
		reply func(http.ResponseWriter)
	}{
		{"429", 1, func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "2")
			failReason(w, 429, "terlalu sering meminta tiket tunnel — coba lagi sebentar lagi", api.ReasonRateLimited)
		}},
		{"500", 2, func(w http.ResponseWriter) { fail(w, 500, "panel sesaat bermasalah") }},
		{"503", 1, func(w http.ResponseWriter) { fail(w, 503, "pemeriksaan tiket tunnel sedang tidak tersedia") }},
		{"jaringan", 1, func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
				if n > tc.fails {
					return false
				}
				tc.reply(w)
				return true
			}
			g := newGateway(t, h.p)
			x := h.startExpose(t, localApp(t))
			sess := g.nextSession(t)
			waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
			if status, body := visit(t, sess, "acak-1"+tunnelHostSuffix, "/cek"); status != http.StatusOK || body != "halo dari laptop: GET /cek" {
				t.Fatalf("jawaban lewat tunnel: %d %q", status, body)
			}
			if code := x.stop(t); code != 0 {
				t.Fatalf("kode %d\n%s", code, x.errb)
			}
			errs := x.errb.String()
			if got := strings.Count(errs, "easyrun: gagal menyambung tunnel ("); got != tc.fails {
				t.Fatalf("pesan gagal menyambung = %d, mau %d:\n%s", got, tc.fails, errs)
			}
			if strings.Contains(errs, "terputus") || strings.Contains(errs, "meminta tiket tunnel:") {
				t.Fatalf("sebelum siap bukan terputus/fatal:\n%s", errs)
			}
			if tc.name == "429" {
				h.clock.mu.Lock()
				sleeps := append([]time.Duration(nil), h.clock.sleeps...)
				h.clock.mu.Unlock()
				if len(sleeps) == 0 || sleeps[0] != 2*time.Second {
					t.Fatalf("Retry-After 2 dtk tiket pertama tidak dihormati: %v", sleeps)
				}
			}
			if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
				t.Fatalf("tunnel sementara harus dihapus saat berhenti: %v", deletes)
			}
		})
	}
}

// slept — jumlah dan daftar Sleep jam palsu.
func (c *fakeClock) slept() (time.Duration, []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum time.Duration
	for _, d := range c.sleeps {
		sum += d
	}
	return sum, append([]time.Duration(nil), c.sleeps...)
}

// startBudgetSecs — exposeStartBudget dalam detik, seperti di pesan.
var startBudgetSecs = int(exposeStartBudget / time.Second)

// TestExposeTiketPertamaTerusGagalBerhenti — C3/N2: tiket pertama gagal
// sesaat terus-menerus → dicoba ulang sampai anggaran waktu exposeStartBudget
// habis (jeda terakhir dipangkas ke sisa anggaran, tidak lewat), lalu
// berhenti dengan pesan yang menyebut panel dan sebabnya; exit 1; tunnel
// sementara dihapus.
func TestExposeTiketPertamaTerusGagalBerhenti(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.tun.ticketFail = func(w http.ResponseWriter, _ int) bool {
		fail(w, 502, "panel sesaat bermasalah")
		return true
	}
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	sum, sleeps := h.clock.slept()
	if sum != exposeStartBudget {
		t.Fatalf("total jeda %v, mau persis anggaran %v: %v", sum, exposeStartBudget, sleeps)
	}
	n, deletes, _ := h.p.tunnelState()
	if n != len(sleeps)+1 {
		t.Fatalf("permintaan tiket = %d, mau %d (satu per jeda + yang pertama)", n, len(sleeps)+1)
	}
	if len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
	}
	u, _ := url.Parse(h.p.srv.URL)
	errs := h.stderr.String()
	want := fmt.Sprintf("easyrun: gagal menyambung tunnel: panel %s tidak memberi tiket dalam %d detik: ", u.Host, startBudgetSecs)
	if !strings.Contains(errs, want) || !strings.Contains(errs, "(HTTP 502)") {
		t.Fatalf("stderr tanpa %q:\n%s", want, errs)
	}
	if !strings.Contains(errs, fmt.Sprintf("(sisa waktu menyambung %d detik)", startBudgetSecs)) {
		t.Fatalf("pesan coba lagi harus menyebut sisa waktu:\n%s", errs)
	}
	if strings.Contains(errs, "terputus") || strings.Contains(errs, "percobaan") {
		t.Fatalf("stderr:\n%s", errs)
	}
}

// TestExposeStartJedaTidakMelewatiAnggaran — N2: Retry-After yang lebih
// panjang dari sisa anggaran dipangkas ke sisanya; percobaan terakhir tepat
// di batas anggaran.
func TestExposeStartJedaTidakMelewatiAnggaran(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	retry := exposeStartBudget * 5 / 6
	h.p.tun.ticketFail = func(w http.ResponseWriter, _ int) bool {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
		failReason(w, 429, "terlalu sering meminta tiket tunnel — coba lagi sebentar lagi", api.ReasonRateLimited)
		return true
	}
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	_, sleeps := h.clock.slept()
	if want := []time.Duration{retry, exposeStartBudget - retry}; fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Fatalf("jeda %v, mau %v (Retry-After dihormati, lalu dipangkas ke sisa anggaran)", sleeps, want)
	}
	if !strings.Contains(h.stderr.String(), "terlalu sering meminta tiket tunnel") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

// TestExposeGerbangTakTerjangkauBerhenti — C3/N2: gerbang tidak terjangkau
// sejak awal (DNS salah, gerbang mati) tidak lagi "terputus … menyambung
// ulang" selamanya: dicoba ulang sampai anggaran waktu habis, lalu berhenti
// dengan alamat gerbang + sebab, exit 1, tunnel sementara dihapus.
func TestExposeGerbangTakTerjangkauBerhenti(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	port := closedLoopbackPort(t)
	h.p.tun.agentURL = "ws://127.0.0.1:" + port + protocol.AgentConnectPath
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	errs := h.stderr.String()
	want := fmt.Sprintf("easyrun: gagal menyambung tunnel ke gerbang 127.0.0.1:%s dalam %d detik: ", port, startBudgetSecs)
	if !strings.Contains(errs, want) {
		t.Fatalf("stderr tanpa %q:\n%s", want, errs)
	}
	sum, sleeps := h.clock.slept()
	if sum != exposeStartBudget {
		t.Fatalf("total jeda %v, mau %v: %v", sum, exposeStartBudget, sleeps)
	}
	if got := strings.Count(errs, "mencoba lagi dalam"); got != len(sleeps) {
		t.Fatalf("pesan coba lagi = %d, mau %d:\n%s", got, len(sleeps), errs)
	}
	if strings.Contains(errs, "terputus") || strings.Contains(errs, "menyambung ulang") {
		t.Fatalf("belum pernah tersambung, jangan sebut terputus:\n%s", errs)
	}
	n, deletes, _ := h.p.tunnelState()
	if n != len(sleeps)+1 {
		t.Fatalf("permintaan tiket = %d, mau %d", n, len(sleeps)+1)
	}
	if len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
	}
}

// TestExposeGerbangRestartSaatMulai — N2: gerbang (satu replika) sedang
// restart/deploy saat expose dijalankan — 503 tanpa tanda selama sebagian
// anggaran — tetap berakhir tersambung, bukan gagal setelah beberapa detik.
func TestExposeGerbangRestartSaatMulai(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	t0 := h.clock.Now()
	downFor := exposeStartBudget / 3
	g.down = func() bool { return h.clock.Now().Sub(t0) < downFor }
	// Jeda bawaan agen: anggaran diuji dengan jam palsu, bukan waktu sungguhan.
	h.a.ExposeBackoffMin, h.a.ExposeBackoffMax = agent.DefaultBackoffMin, agent.DefaultBackoffMax
	x := h.startExpose(t, localApp(t))
	sess := g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	if status, body := visit(t, sess, "acak-1"+tunnelHostSuffix, "/cek"); status != http.StatusOK || body != "halo dari laptop: GET /cek" {
		t.Fatalf("jawaban lewat tunnel: %d %q", status, body)
	}
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if !x.errb.contains("easyrun: gagal menyambung tunnel (gerbang menjawab 503") || x.errb.contains("terputus") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}

// TestExposeTiketPertamaGagalLaluGerbangTolakTiket — N1: tiket pertama 503
// dua kali, lalu gerbang terus menolak tiket (401 bertanda): berhenti dengan
// penjelasan agen yang sama ("berulang kali menolak … ulangi perintahnya"),
// bukan "(401)" mentah dari jatah percobaan.
func TestExposeTiketPertamaGagalLaluGerbangTolakTiket(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	newGateway(t, h.p)
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		if n <= 2 {
			fail(w, 503, "panel sesaat bermasalah")
			return true
		}
		okStatus(w, http.StatusCreated, map[string]any{"ticket": fmt.Sprintf("tt-asing-%d", n), "expires_in": 30, "agent_url": h.p.tun.agentURL})
		return true
	}
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	errs := h.stderr.String()
	if !strings.Contains(errs, "easyrun: tunnel berhenti: gerbang tunnel berulang kali menolak tiket sambungan: "+gatewayInvalidTicketMsg+" — ulangi perintahnya\n") {
		t.Fatalf("stderr:\n%s", errs)
	}
	if n, _, _ := h.p.tunnelState(); n != 2+agent.DefaultMaxTicketRejects+1 {
		t.Fatalf("permintaan tiket = %d, mau %d", n, 2+agent.DefaultMaxTicketRejects+1)
	}
}

// TestExposeAnggaranHabisSaatGerbangTolakTiket — N1: anggaran habis tepat
// saat gerbang menolak tiket (401 bertanda): pesan akhir memakai penjelasan
// gerbang (gatewayRejectText: "… ulangi perintahnya"), bukan "(401)" mentah.
func TestExposeAnggaranHabisSaatGerbangTolakTiket(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	t0 := h.clock.Now()
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		if h.clock.Now().Sub(t0) < exposeStartBudget {
			fail(w, 503, "panel sesaat bermasalah")
			return true
		}
		okStatus(w, http.StatusCreated, map[string]any{"ticket": fmt.Sprintf("tt-asing-%d", n), "expires_in": 30, "agent_url": h.p.tun.agentURL})
		return true
	}
	if code := h.run("expose", "3000"); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, h.stderr)
	}
	gw, _ := url.Parse(g.srv.URL)
	errs := h.stderr.String()
	want := fmt.Sprintf("easyrun: gagal menyambung tunnel ke gerbang %s dalam %d detik: gerbang tunnel berulang kali menolak tiket sambungan: %s — ulangi perintahnya\n", gw.Host, startBudgetSecs, gatewayInvalidTicketMsg)
	if !strings.Contains(errs, want) || strings.Contains(errs, "(401)") {
		t.Fatalf("stderr tanpa %q:\n%s", want, errs)
	}
}

// TestExposeSetelahSiapSambungUlangTanpaBatas — C3: setelah tunnel pernah
// siap, gerbang yang hilang disambung ulang tanpa batas (tidak berhenti
// karena anggaran menyambung, yang hanya berlaku sebelum siap).
func TestExposeSetelahSiapSambungUlangTanpaBatas(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	x := h.startExpose(t, localApp(t))
	sess := g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	g.srv.Close()
	_ = sess.Close()
	const manyReconnects = 8
	waitFor(t, "sambung ulang berkali-kali", func() bool {
		return strings.Count(x.errb.String(), "tunnel terputus") > manyReconnects
	})
	select {
	case code := <-x.done:
		x.done <- code
		t.Fatalf("expose berhenti (kode %d) padahal pernah siap:\n%s", code, x.errb)
	default:
	}
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
}

// TestExposeGerbangPindah — C4: agent_url tiket sambung ulang berbeda dari
// gerbang yang sedang dipakai → CLI menyambung ke gerbang baru (bukan terus
// memanggil gerbang lama), pengunjung dilayani lewat gerbang baru.
func TestExposeGerbangPindah(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g1 := newGateway(t, h.p)
	x := h.startExpose(t, localApp(t))
	s1 := g1.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })
	g2 := newGateway(t, h.p) // panel kini menjawab agent_url gerbang kedua
	_ = s1.Close()
	s2 := g2.nextSession(t)
	if status, body := visit(t, s2, "acak-1"+tunnelHostSuffix, "/cek"); status != http.StatusOK || body != "halo dari laptop: GET /cek" {
		t.Fatalf("jawaban lewat gerbang baru: %d %q", status, body)
	}
	if n, _ := g1.state(); n != 1 {
		t.Fatalf("gerbang lama dipanggil lagi: %d sambungan", n)
	}
	u, _ := url.Parse(g2.srv.URL)
	if want := "easyrun: panel memindahkan tunnel ke gerbang " + u.Host + "; menyambung ke sana"; !x.errb.contains(want) {
		t.Fatalf("stderr tanpa %q:\n%s", want, x.errb)
	}
	if code := x.stop(t); code != 0 {
		t.Fatalf("kode %d\n%s", code, x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
	}
}

// TestExposeAgentURLBerikutnyaDivalidasi — C4: agent_url di tiket sambung
// ulang divalidasi seperti tiket pertama; bukan wss (dan bukan loopback) =
// berhenti dengan penjelasan, tunnel sementara dihapus.
func TestExposeAgentURLBerikutnyaDivalidasi(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	g.drop = true
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		if n < 2 {
			return false
		}
		okStatus(w, http.StatusCreated, map[string]any{"ticket": fmt.Sprintf("tt-x-%d", n), "expires_in": 30, "agent_url": "ws://gerbang-lain.contoh.test" + protocol.AgentConnectPath})
		return true
	}
	x := h.startExpose(t, localApp(t))
	if code := x.wait(t); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
	}
	if !x.errb.contains("easyrun: tunnel berhenti: agent_url dari server harus wss") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
	n, deletes, _ := h.p.tunnelState()
	if n != 2 {
		t.Fatalf("permintaan tiket = %d, mau berhenti di tiket kedua", n)
	}
	if len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
	}
}

// TestExposeNamaKosongDitolak — C5: --name yang diberikan tetapi kosong
// (atau hanya spasi) = galat pemakaian, bukan tunnel acak.
func TestExposeNamaKosongDitolak(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	// Pengaman: bila validasinya hilang, pembuatan ditolak cepat (kode 1).
	h.p.tun.createFail = func(w http.ResponseWriter) bool {
		failReason(w, http.StatusForbidden, "akses tunnel tenant dimatikan", api.ReasonTunnelNotAllowed)
		return true
	}
	for _, args := range [][]string{
		{"--name", "", "3000"},
		{"--name", "   ", "3000"},
		{"--name=", "3000"},
		{"3000", "--name", "\t"},
	} {
		code := h.run(append([]string{"expose"}, args...)...)
		if code != 2 || !strings.Contains(h.stderr.String(), "--name") {
			t.Errorf("expose %q: kode %d, mau 2 dengan pesan --name\n%s", args, code, h.stderr)
		}
	}
	if n := h.p.n("POST /api/v1/cli/tunnels"); n != 0 {
		t.Fatalf("--name kosong tidak boleh membuat tunnel: %d", n)
	}
}

// TestExposeBantuanSetelahPort — C6: -h/--help di mana pun (mis. setelah
// <port>) mencetak bantuan Indonesia yang sama dengan `easyrun expose
// --help`, exit 0, tanpa usage Inggris paket flag.
func TestExposeBantuanSetelahPort(t *testing.T) {
	h := newHarness(t)
	if code := h.run("expose", "--help"); code != 0 {
		t.Fatalf("kode %d", code)
	}
	want := h.stdout.String()
	for _, args := range [][]string{{"3000", "-h"}, {"3000", "--help"}, {"--name", "demo", "-help"}, {"-h", "3000"}} {
		code := h.run(append([]string{"expose"}, args...)...)
		if code != 0 || h.stdout.String() != want || h.stderr.Len() != 0 {
			t.Errorf("expose %q: kode %d\nstdout:\n%s\nstderr:\n%s", args, code, h.stdout, h.stderr)
		}
	}
	if code := h.run("expose", "--bogus", "3000"); code != 2 || strings.Contains(h.stderr.String(), "Usage of") || !strings.Contains(h.stderr.String(), "Pemakaian: easyrun expose") {
		t.Fatalf("opsi salah: kode %d\n%s", code, h.stderr)
	}
}

// TestWarnLocalDownDiamSaatBerhenti — C10: Ctrl-C saat memeriksa aplikasi
// lokal bukan "belum ada aplikasi".
func TestWarnLocalDownDiamSaatBerhenti(t *testing.T) {
	h := newHarness(t)
	x := exposeArgs{host: "127.0.0.1", port: closedLoopbackPort(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.a.warnLocalDown(ctx, x)
	if h.stderr.Len() != 0 {
		t.Fatalf("peringatan palsu setelah berhenti:\n%s", h.stderr)
	}
	h.a.warnLocalDown(context.Background(), x)
	if !strings.Contains(h.stderr.String(), "belum ada aplikasi di 127.0.0.1:"+x.port) {
		t.Fatalf("kontrol: peringatan harus tetap ada saat berjalan:\n%s", h.stderr)
	}
}

// ---------- E2E lokal: akun diblokir, saklar tunnel mati ----------

// bannedMsg — pesan 401 middleware RejectBanned panel.
const bannedMsg = "akun Anda diblokir"

// bannedReply — 401 RejectBanned: panel baru menambah data.reason
// user_banned; panel lama hanya pesannya.
func bannedReply(withReason bool) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if withReason {
			failReason(w, http.StatusUnauthorized, bannedMsg, "user_banned")
			return
		}
		fail(w, http.StatusUnauthorized, bannedMsg)
	}
}

// TestExplainAkunDiblokir — 401 user_banned (atau pesan persis panel lama)
// = kalimat CLI sendiri: akun diblokir, hubungi admin; tanpa saran login.
// 401 lain tetap menyuruh login.
func TestExplainAkunDiblokir(t *testing.T) {
	for _, e := range []*api.Error{
		{Status: 401, Message: bannedMsg, Reason: "user_banned"},
		{Status: 401, Message: "pesan lain", Reason: "user_banned"},
		{Status: 401, Message: bannedMsg},
	} {
		got := explain(fmt.Errorf("x: %w", e))
		if !strings.Contains(got, "akun ini diblokir di EasyRun") || !strings.Contains(got, "hubungi admin") || strings.Contains(got, "easyrun login") {
			t.Errorf("%+v: %q", e, got)
		}
	}
	if got := explain(&api.Error{Status: 401, Message: "token CLI sudah dicabut"}); !strings.Contains(got, "easyrun login") {
		t.Fatalf("401 biasa tetap menyarankan login: %q", got)
	}
}

// TestBoxesAkunDiblokir — berlaku untuk semua perintah, bukan hanya tunnel.
func TestBoxesAkunDiblokir(t *testing.T) {
	for _, withReason := range []bool{true, false} {
		h := newHarness(t)
		h.loggedIn(t)
		reply := bannedReply(withReason)
		h.p.authFail = func(w http.ResponseWriter) bool { reply(w); return true }
		if code := h.run("boxes"); code != 1 {
			t.Fatalf("kode %d\n%s", code, h.stderr)
		}
		if errs := h.stderr.String(); !strings.Contains(errs, "akun ini diblokir di EasyRun") || strings.Contains(errs, "easyrun login") {
			t.Fatalf("reason=%v stderr:\n%s", withReason, errs)
		}
	}
}

// TestExposeAkunDiblokirBerhenti — 401 user_banned saat tiket pertama
// maupun setelah tunnel pernah siap = fatal dengan kalimat akun diblokir,
// tanpa saran login; tunnel sementara tetap dicoba dihapus.
func TestExposeAkunDiblokirBerhenti(t *testing.T) {
	for _, afterReady := range []bool{false, true} {
		for _, withReason := range []bool{true, false} {
			t.Run(fmt.Sprintf("siap=%v/reason=%v", afterReady, withReason), func(t *testing.T) {
				h := newHarness(t)
				h.loggedIn(t)
				g := newGateway(t, h.p)
				g.drop = true
				reply := bannedReply(withReason)
				h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
					if afterReady && n < 2 {
						return false
					}
					reply(w)
					return true
				}
				x := h.startExpose(t, localApp(t))
				if code := x.wait(t); code != 1 {
					t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
				}
				errs := x.errb.String()
				if !strings.Contains(errs, "akun ini diblokir di EasyRun") || strings.Contains(errs, "easyrun login") || strings.Contains(errs, "mencoba lagi") {
					t.Fatalf("stderr:\n%s", errs)
				}
				if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
					t.Fatalf("tunnel sementara harus dihapus: %v", deletes)
				}
			})
		}
	}
}

// TestGatewayRejectTextKonsisten — penolakan tiket 403 bertanda gerbang
// membawa pesan panel saja (tanpa reason): akun diblokir dan saklar mati
// dijelaskan dengan kalimat CLI yang sama seperti penolakan panel.
func TestGatewayRejectTextKonsisten(t *testing.T) {
	ban := explainTunnel(opTunnelRun, fmt.Errorf("x: %w", &agent.FatalError{Status: 403, Message: "akun diblokir"}))
	if !strings.Contains(ban, "akun ini diblokir di EasyRun") || ban != explain(&api.Error{Status: 401, Reason: "user_banned"}) {
		t.Fatalf("akun diblokir lewat gerbang: %q", ban)
	}
	off := explainTunnel(opTunnelRun, fmt.Errorf("x: %w", &agent.FatalError{Status: 403, Message: "Tunnel belum tersedia"}))
	if want := explainTunnel(opTunnelRun, &api.Error{Status: 503, Reason: api.ReasonTunnelDisabled}); off != want {
		t.Fatalf("saklar mati lewat gerbang: %q, mau %q", off, want)
	}
}

// TestExposeSaklarMatiBerhenti — 503 tunnel_disabled pada tiket = berhenti
// (sebelum maupun sesudah siap), kalimat CLI saklar mati + saran menjalankan
// lagi nanti, exit 1, tunnel sementara dihapus. Teks mentah panel tidak
// dicetak, dan tidak ada coba lagi / sambung ulang.
func TestExposeSaklarMatiBerhenti(t *testing.T) {
	for _, afterReady := range []bool{false, true} {
		t.Run(fmt.Sprint("siap=", afterReady), func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			g := newGateway(t, h.p)
			g.drop = true
			h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
				if afterReady && n < 2 {
					return false
				}
				failReason(w, http.StatusServiceUnavailable, "Tunnel belum tersedia", api.ReasonTunnelDisabled)
				return true
			}
			x := h.startExpose(t, localApp(t))
			if code := x.wait(t); code != 1 {
				t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
			}
			errs := x.errb.String()
			if !strings.Contains(errs, "tunnel publik sedang dimatikan di platform — jalankan perintahnya lagi nanti") {
				t.Fatalf("stderr:\n%s", errs)
			}
			// Setelah siap: satu "menyambung ulang" dari putusnya gerbang palsu
			// (g.drop), lalu tiket 503 langsung menghentikan, tanpa ulang.
			wantTickets, wantReconnects := 1, 0
			if afterReady {
				wantTickets, wantReconnects = 2, 1
			}
			if strings.Contains(errs, "Tunnel belum tersedia") || strings.Contains(errs, "mencoba lagi") || strings.Count(errs, "menyambung ulang") != wantReconnects {
				t.Fatalf("stderr:\n%s", errs)
			}
			if n, deletes, _ := h.p.tunnelState(); n != wantTickets || len(deletes) != 1 {
				t.Fatalf("tiket %d (mau %d), hapus %v", n, wantTickets, deletes)
			}
		})
	}
}

// TestExposeSaklarMatiHapusDitolak — saklar mati juga menolak DELETE (503
// tunnel_disabled): pengguna diberi tahu tunnel sementara itu tertinggal
// dan kapan bisa dihapus.
func TestExposeSaklarMatiHapusDitolak(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	g.drop = true
	h.p.tun.ticketFail = func(w http.ResponseWriter, n int) bool {
		if n < 2 {
			return false
		}
		failReason(w, http.StatusServiceUnavailable, "Tunnel belum tersedia", api.ReasonTunnelDisabled)
		return true
	}
	h.p.tun.deleteFail = func(w http.ResponseWriter) bool {
		failReason(w, http.StatusServiceUnavailable, "Tunnel belum tersedia", api.ReasonTunnelDisabled)
		return true
	}
	x := h.startExpose(t, localApp(t))
	if code := x.wait(t); code != 1 {
		t.Fatalf("kode %d, mau 1\n%s", code, x.errb)
	}
	if !x.errb.contains("easyrun: catatan: tunnel sementara acak-1 tidak bisa dihapus selama tunnel publik dimatikan di platform; hapus setelah dinyalakan lagi: easyrun tunnels rm acak-1") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}
