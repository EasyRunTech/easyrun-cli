package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestErrorFromResponseEnvelopePanel(t *testing.T) {
	now := time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Retry-After", "7")
	e := ErrorFromResponse(429, h, []byte(`{"success":false,"message":"terlalu sering","request_id":"rq-1"}`), now)
	if e.Status != 429 || e.Message != "terlalu sering" || e.RequestID != "rq-1" || e.RetryAfter != 7*time.Second {
		t.Fatalf("%+v", e)
	}
	h.Set("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat))
	if e := ErrorFromResponse(503, h, nil, now); e.RetryAfter != 90*time.Second {
		t.Fatalf("Retry-After tanggal: %v", e.RetryAfter)
	}
	if e := ErrorFromResponse(502, nil, []byte("<html>bad gateway</html>"), now); e.Message != "" {
		t.Fatalf("HTML tidak boleh jadi pesan: %q", e.Message)
	}
}

// Kontrak §4.1 menulis 400 {error: …}; envelope panel tidak punya field itu.
// Klien menerima semua bentuk yang mungkin dipakai panel.
func TestKodeAlurDeviceDariBerbagaiBentuk(t *testing.T) {
	cases := map[string]string{
		`{"error":"authorization_pending"}`:                                               CodeAuthorizationPending,
		`{"success":false,"message":"Menunggu persetujuan","data":{"error":"slow_down"}}`: CodeSlowDown,
		`{"success":false,"message":"x","code":"expired_token"}`:                          CodeExpiredToken,
		`{"success":false,"message":"access_denied"}`:                                     CodeAccessDenied,
		`{"success":false,"message":"device_code tidak valid"}`:                           "",
	}
	for body, want := range cases {
		if got := ErrorFromResponse(400, nil, []byte(body), time.Now()).Code; got != want {
			t.Errorf("%s → %q, ingin %q", body, got, want)
		}
	}
}

// data.reason penolakan 429 panel (common.SendErrorWithData + RetryData)
// terbaca sebagai Reason, terpisah dari Code. Panel lama tanpa data, data
// bukan objek, atau reason bukan string → "".
func TestReasonDariDataPanel(t *testing.T) {
	cases := map[string]string{
		`{"success":false,"message":"batas sesi SSH tercapai","data":{"reason":"session_limit"},"request_id":"rq"}`: ReasonSessionLimit,
		`{"success":false,"message":"batas sesi SSH tercapai","data":{"reason":"rate_limited"}}`:                    ReasonRateLimited,
		`{"success":false,"message":"x","data":{"reason":"Rate_Limited"}}`:                                          ReasonRateLimited,
		`{"success":false,"message":"batas sesi SSH tercapai"}`:                                                     "",
		`{"success":false,"message":"x","data":null}`:                                                               "",
		`{"success":false,"message":"x","data":[1]}`:                                                                "",
		`{"success":false,"message":"x","data":{"reason":5}}`:                                                       "",
		`{"success":false,"message":"x","data":{"next_change_at":"2026-11-12T00:00:00Z"}}`:                          "",
	}
	for body, want := range cases {
		e := ErrorFromResponse(429, nil, []byte(body), time.Now())
		if e.Reason != want || e.Code != "" {
			t.Errorf("%s → reason %q code %q, ingin reason %q", body, e.Reason, e.Code, want)
		}
	}
}

func TestCleanMembuangEscapeTerminal(t *testing.T) {
	in := "box \x1b]0;judul\x07rusak\x1b[31m merah\r\nbaris x"
	got := Clean(in)
	if strings.ContainsAny(got, "\x1b\x07\r\n ") {
		t.Fatalf("karakter kendali lolos: %q", got)
	}
	if long := Clean(strings.Repeat("a", 1000)); len([]rune(long)) != maxMessageLen+1 {
		t.Fatalf("tidak dipotong: %d", len([]rune(long)))
	}
}

func TestClientEnvelopeDanAuth(t *testing.T) {
	var gotAuth, gotUA, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotUA, gotCT = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/api/v1/cli/boxes":
			_, _ = w.Write([]byte(`{"success":true,"data":[{"id":"b1","name":"coba","hostname":"coba.easyrun.io","ssh_available":true}]}`))
		case "/api/v1/cli/me":
			// Tanpa envelope pun diterima.
			_, _ = w.Write([]byte(`{"user_id":"u1","email":"a@b.c","expires_at":"2026-12-11T16:10:00Z"}`))
		case "/api/v1/instances/b1/ssh/certificate":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["public_key"] != "ssh-ed25519 AAAA" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"certificate":"c","principal":"b1","host_alias":"coba.easyrun.io"}}`))
		case "/api/v1/cli/logout":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"success":false,"message":"token tidak berlaku"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "erc_x", UserAgent: "easyrun/uji"}
	ctx := context.Background()

	boxes, err := c.Boxes(ctx)
	if err != nil || len(boxes) != 1 || boxes[0].Hostname != "coba.easyrun.io" || !boxes[0].SSHAvailable {
		t.Fatalf("%+v %v", boxes, err)
	}
	if gotAuth != "Bearer erc_x" || gotUA != "easyrun/uji" {
		t.Fatalf("header: auth=%q ua=%q", gotAuth, gotUA)
	}
	if me, err := c.Me(ctx); err != nil || me.Email != "a@b.c" {
		t.Fatalf("%+v %v", me, err)
	}
	cert, err := c.Certificate(ctx, "b1", "ssh-ed25519 AAAA")
	if err != nil || cert.HostAlias != "coba.easyrun.io" || gotCT != "application/json" {
		t.Fatalf("%+v %v ct=%q", cert, err, gotCT)
	}
	err = c.Logout(ctx)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Message != "token tidak berlaku" || ae.Handshake {
		t.Fatalf("401: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Boxes(ctx); !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("tanpa token tidak boleh dikirim: %v", err)
	}
}

func TestClientServerMati(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := (&Client{BaseURL: url, Token: "x"}).Boxes(context.Background())
	var ne *NetError
	if !errors.As(err, &ne) {
		t.Fatalf("ingin NetError, dapat %T %v", err, err)
	}
}

func TestDecodeDataSuccessFalse(t *testing.T) {
	var out Box
	err := decodeData([]byte(`{"success":false,"message":"aneh"}`), &out)
	var ae *Error
	if !errors.As(err, &ae) || ae.Message != "aneh" {
		t.Fatalf("%v", err)
	}
	var list []Box
	for _, body := range []string{`{"success":true}`, `{"success":true,"data":null}`} {
		if err := decodeData([]byte(body), &list); err != nil || list != nil {
			t.Fatalf("%s: daftar kosong, dapat %v %v", body, list, err)
		}
	}
}

// Pengalihan tidak pernah diikuti — juga bila pemanggil memberi klien HTTP
// sendiri tanpa CheckRedirect — sehingga token Bearer tidak pernah sampai ke
// tujuan pengalihan; 3xx dilaporkan sebagai *RedirectError.
func TestPengalihanTidakDiikuti(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer target.Close()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/cli/boxes": // ke host lain
			http.Redirect(w, r, target.URL+"/api/v1/cli/boxes", http.StatusFound)
		case "/api/v1/instances/b1/ssh/ticket": // 307 mempertahankan metode + badan
			http.Redirect(w, r, target.URL+"/curi", http.StatusTemporaryRedirect)
		case "/api/v1/cli/me": // host yang sama, path lain
			w.Header().Set("Location", "/api/v1/cli/me2")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	if c := NewHTTPClient(); c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("NewHTTPClient harus menolak mengikuti pengalihan")
	}
	ctx := context.Background()
	for name, hc := range map[string]*http.Client{
		"bawaan":               nil,
		"NewHTTPClient":        NewHTTPClient(),
		"klien tanpa aturan":   {},
		"klien yang mengikuti": {CheckRedirect: func(*http.Request, []*http.Request) error { return nil }},
	} {
		c := &Client{BaseURL: srv.URL, Token: "erc_rahasia", HTTP: hc}
		calls := map[string]func() error{
			"boxes":  func() error { _, err := c.Boxes(ctx); return err },
			"ticket": func() error { _, err := c.Ticket(ctx, "b1"); return err },
			"me":     func() error { _, err := c.Me(ctx); return err },
		}
		want := map[string]int{"boxes": 302, "ticket": 307, "me": 301}
		for k, call := range calls {
			err := call()
			var re *RedirectError
			if !errors.As(err, &re) || re.Status != want[k] {
				t.Fatalf("%s/%s: ingin RedirectError %d, dapat %T %v", name, k, want[k], err, err)
			}
			if k == "boxes" && !strings.Contains(re.Location, target.URL) {
				t.Fatalf("Location %q", re.Location)
			}
			if strings.Contains(re.Error(), "erc_rahasia") {
				t.Fatalf("token tercetak: %q", re.Error())
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("tujuan pengalihan dihubungi: %q", leaked)
	}
	if hits != 12 {
		t.Fatalf("server asal %d permintaan, ingin 12", hits)
	}
}
