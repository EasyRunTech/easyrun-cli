package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/sshtest"
)

const (
	boxID     = "7b4c1f0e-2a3d-4e5f-8a9b-0c1d2e3f4a5b"
	otherID   = "11111111-2222-4333-8444-555555555555"
	testToken = "erc_token_uji_yang_rahasia"
	envMain   = "EASYRUN_CLI_TEST_MAIN"
)

// TestMain — biner uji juga bisa berperan sebagai program `easyrun` (dipanggil
// ssh sebagai ProxyCommand / Match exec pada uji ujung-ke-ujung).
func TestMain(m *testing.M) {
	if os.Getenv(envMain) == "1" {
		ctx, stop := SignalContext()
		code := New(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args[1:])
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakePanel — panel palsu dengan envelope persis seperti
// easyrun-panel/app/controller/common (success/data, success/message/request_id).
type fakePanel struct {
	t   *testing.T
	srv *httptest.Server
	ca  *sshtest.CA
	now func() time.Time

	mu           sync.Mutex
	calls        map[string]int
	boxes        []api.Box
	device       map[string]any
	deviceReq    api.DeviceRequest
	tokenReplies []func(http.ResponseWriter)
	logouts      []string
	certTTL      time.Duration
	certOverride func(pub ssh.PublicKey, id string) (ssh.PublicKey, string)
	hostAlias    string
	ticketFail   func(http.ResponseWriter) bool
	wsFail       func(http.ResponseWriter) bool
	ws           func(*websocket.Conn, *http.Request)
	tickets      map[string]string
	wsHeaders    []http.Header
}

func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": msg, "request_id": "rq-uji"})
}

// failReason — penolakan ber-`data.reason`, persis bentuk refuse() dan
// RateLimitPerIP panel (common.SendErrorWithData + common_utils.RetryData).
func failReason(w http.ResponseWriter, status int, msg, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false, "message": msg,
		"data":       map[string]string{"reason": reason},
		"request_id": "rq-uji",
	})
}

func newPanel(t *testing.T) *fakePanel {
	t.Helper()
	p := &fakePanel{
		t: t, ca: sshtest.NewCA(t), now: time.Now,
		calls:   map[string]int{},
		certTTL: time.Hour,
		tickets: map[string]string{},
		boxes: []api.Box{
			// Kontrak §7a.5: hostname = alamat gratis instance; label pertamanya
			// tidak harus sama dengan nama.
			{ID: boxID, Name: "uji-box", Hostname: "contoh-box.easyrun.io", TenantID: "t1", TenantName: "Tenant Satu", Status: "ready", SSHAvailable: true},
			{ID: otherID, Name: "web-lama", Hostname: "web-lama.easyrun.io", TenantID: "t2", TenantName: "Agensi Dua", Status: "ready", SSHAvailable: false},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/cli/device", p.handleDevice)
	mux.HandleFunc("POST /api/v1/cli/token", p.handleToken)
	mux.HandleFunc("GET /api/v1/cli/me", p.auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"user_id": "u-1", "email": "budi@contoh.com", "expires_at": "2026-12-11T16:10:00Z"})
	}))
	mux.HandleFunc("GET /api/v1/cli/boxes", p.auth(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		ok(w, p.boxes)
	}))
	mux.HandleFunc("POST /api/v1/cli/logout", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.logouts = append(p.logouts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		p.mu.Unlock()
		ok(w, map[string]any{})
	})
	mux.HandleFunc("POST /api/v1/instances/{id}/ssh/certificate", p.auth(p.handleCert))
	mux.HandleFunc("POST /api/v1/instances/{id}/ssh/ticket", p.auth(p.handleTicket))
	mux.HandleFunc("GET /api/v1/console/ws", p.handleWS)
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		key := r.Method + " " + r.URL.Path
		if strings.HasPrefix(r.URL.Path, "/api/v1/instances/") {
			key = r.Method + " " + "/instances/" + filepath.Base(r.URL.Path)
		}
		p.calls[key]++
		p.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	p.device = map[string]any{
		"device_code": "dc-rahasia", "user_code": "ABCD-EFGH",
		"verification_uri":          p.srv.URL + "/cli/aktivasi",
		"verification_uri_complete": p.srv.URL + "/cli/aktivasi?kode=ABCD-EFGH",
		"expires_in":                600, "interval": 5,
	}
	return p
}

func (p *fakePanel) n(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[key]
}

func (p *fakePanel) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, v := range p.calls {
		n += v
	}
	return n
}

func (p *fakePanel) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			fail(w, 401, "token CLI tidak berlaku")
			return
		}
		h(w, r)
	}
}

func (p *fakePanel) handleDevice(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = json.NewDecoder(r.Body).Decode(&p.deviceReq)
	ok(w, p.device)
}

func (p *fakePanel) handleToken(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in["device_code"] != "dc-rahasia" {
		fail(w, 400, "device_code tidak valid")
		return
	}
	p.mu.Lock()
	if len(p.tokenReplies) == 0 {
		p.mu.Unlock()
		tokenPending(w)
		return
	}
	reply := p.tokenReplies[0]
	p.tokenReplies = p.tokenReplies[1:]
	p.mu.Unlock()
	reply(w)
}

// Bentuk 400 dibuat bervariasi: panel mungkin memakai SendErrorWithData
// (data.error) atau badan gaya RFC 8628 ({"error": …}).
func tokenPending(w http.ResponseWriter) {
	w.WriteHeader(400)
	_, _ = w.Write([]byte(`{"success":false,"message":"menunggu persetujuan","data":{"error":"authorization_pending"}}`))
}
func tokenCode(code string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
	}
}
func tokenOK(w http.ResponseWriter) {
	ok(w, map[string]any{"access_token": testToken, "token_type": "Bearer", "expires_in": 2592000, "token_id": "tok-1"})
}

func (p *fakePanel) box(id string) (api.Box, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range p.boxes {
		if b.ID == id {
			return b, true
		}
	}
	return api.Box{}, false
}

func (p *fakePanel) handleCert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, found := p.box(id)
	if !found {
		fail(w, 404, "instance tidak ditemukan")
		return
	}
	if !b.SSHAvailable {
		fail(w, 409, "SSH tidak tersedia untuk instance ini")
		return
	}
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(in["public_key"]))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 {
		fail(w, 400, "public_key tidak valid")
		return
	}
	principal := id
	p.mu.Lock()
	override, ttl, alias := p.certOverride, p.certTTL, p.hostAlias
	p.mu.Unlock()
	if override != nil {
		pub, principal = override(pub, id)
	}
	now := p.now()
	cert, err := p.ca.Sign(pub, principal, now.Add(-time.Minute), now.Add(ttl), ssh.UserCert)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if alias == "" {
		alias = b.Hostname
	}
	// Jam server = jam palsu uji (panel sungguhan mengirim jamnya sendiri);
	// CLI mengukur selisih jam laptop dari header ini.
	w.Header().Set("Date", now.UTC().Format(http.TimeFormat))
	ok(w, map[string]any{
		"certificate": string(ssh.MarshalAuthorizedKey(cert)),
		"expires_at":  now.Add(ttl).UTC().Format(time.RFC3339),
		"principal":   principal,
		"host_alias":  alias,
	})
}

func (p *fakePanel) handleTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p.mu.Lock()
	tf := p.ticketFail
	p.mu.Unlock()
	if tf != nil && tf(w) {
		return
	}
	if _, found := p.box(id); !found {
		fail(w, 404, "instance tidak ditemukan")
		return
	}
	tk := "tk-" + id
	p.mu.Lock()
	p.tickets[tk] = id
	p.mu.Unlock()
	ok(w, map[string]any{"ticket": tk, "expires_in": 30, "ws_url": "ws" + strings.TrimPrefix(p.srv.URL, "http") + "/api/v1/console/ws"})
}

func (p *fakePanel) handleWS(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.wsHeaders = append(p.wsHeaders, r.Header.Clone())
	_, valid := p.tickets[r.URL.Query().Get("ticket")]
	delete(p.tickets, r.URL.Query().Get("ticket")) // sekali pakai
	wf, handler := p.wsFail, p.ws
	p.mu.Unlock()
	if wf != nil && wf(w) {
		return
	}
	if !valid {
		fail(w, 401, "tiket tidak berlaku")
		return
	}
	if r.Header.Get("Origin") != "" {
		fail(w, 403, "asal permintaan tidak diizinkan membuka konsol")
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	if handler != nil {
		handler(c, r)
	}
}

// ---------- App uji ----------

type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
	// cancelAt — Sleep ke-n (1-based) mengembalikan context.Canceled.
	cancelAt int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if c.cancelAt > 0 && len(c.sleeps) == c.cancelAt {
		return context.Canceled
	}
	c.t = c.t.Add(d)
	return nil
}

type execCall struct {
	path string
	argv []string
}

type harness struct {
	a       *App
	p       *fakePanel
	d       config.Dir
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	clock   *fakeClock
	env     map[string]string
	opened  []string
	execs   []execCall
	execRet int
	exe     string
	home    string // HOME uji: `ssh-config --write` tidak pernah menyentuh ~/.ssh asli
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p := newPanel(t)
	root := t.TempDir()
	h := &harness{
		p: p, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		clock: &fakeClock{t: time.Date(2026, 11, 11, 23, 0, 0, 0, time.Local)},
		d:     config.Dir{Path: filepath.Join(root, ".easyrun")},
		exe:   filepath.Join(root, "bin", "easyrun"),
		home:  filepath.Join(root, "home"),
	}
	p.now = h.clock.Now
	if err := os.MkdirAll(filepath.Dir(h.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.env = map[string]string{
		config.EnvConfigDir: h.d.Path,
		config.EnvAPIURL:    p.srv.URL,
	}
	h.a = &App{
		Stdin:  strings.NewReader(""),
		Stdout: h.stdout, Stderr: h.stderr,
		Getenv:   func(k string) string { return h.env[k] },
		Environ:  func() []string { return []string{"PATH=/usr/bin"} },
		Now:      h.clock.Now,
		HTTP:     p.srv.Client(),
		Sleep:    h.clock.Sleep,
		Hostname: func() (string, error) { return "laptop-budi.local", nil },
		Executable: func() (string, error) {
			return h.exe, nil
		},
		HomeDir: func() (string, error) { return h.home, nil },
		LookPath: func(name string) (string, error) {
			if name == "ssh" {
				return "/usr/bin/ssh", nil
			}
			return "", errors.New("tidak ada")
		},
		OpenBrowser: func(u string) error { h.opened = append(h.opened, u); return nil },
		ExecSSH: func(path string, argv, env []string) (int, error) {
			h.execs = append(h.execs, execCall{path, argv})
			return h.execRet, nil
		},
		GOOS: "darwin",
	}
	return h
}

// invocation — awal ProxyCommand/Match exec yang ditulis CLI di harness ini:
// program + --config-dir (selalu) + --api-url (karena alamat API uji datang
// dari env EASYRUN_API_URL).
func (h *harness) invocation() string {
	return h.exe + " --config-dir " + h.d.Path + " --api-url " + h.p.srv.URL
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return h.a.Run(context.Background(), args)
}

// loggedIn — tulis credentials.json seperti hasil login.
func (h *harness) loggedIn(t *testing.T) {
	t.Helper()
	err := h.d.SaveCredentials(config.Credentials{
		APIURL: h.p.srv.URL, AccessToken: testToken, TokenID: "tok-1",
		ExpiresAt: h.clock.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
}
