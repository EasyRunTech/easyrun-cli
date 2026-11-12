// Package api — klien HTTP untuk endpoint CLI panel EasyRun (kontrak SSH box
// §4). Envelope panel: sukses `{success:true, data}`, gagal `{success:false,
// message, request_id}` (sebagian penolakan menambah `data`, mis.
// `data.reason` pada 429). Badan tanpa envelope (gaya RFC 8628) juga diterima.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// apiPrefix — semua endpoint panel ada di bawah /api/v1.
	apiPrefix = "/api/v1"
	// maxBody — badan respons lebih dari ini dianggap rusak.
	maxBody = 1 << 20
	// RequestTimeout — batas satu permintaan HTTP.
	RequestTimeout = 30 * time.Second
	// maxMessageLen — pesan server dipotong sepanjang ini sebelum dicetak.
	maxMessageLen = 300
)

// Kode galat alur device (RFC 8628 §3.5).
const (
	CodeAuthorizationPending = "authorization_pending"
	CodeSlowDown             = "slow_down"
	CodeExpiredToken         = "expired_token"
	CodeAccessDenied         = "access_denied"
)

var deviceCodes = map[string]bool{
	CodeAuthorizationPending: true, CodeSlowDown: true,
	CodeExpiredToken: true, CodeAccessDenied: true,
}

// Alasan penolakan 429 yang bisa dibaca mesin: `data.reason` envelope panel
// (penolakan sertifikat/tiket SSH, pembatas laju, dan jabat tangan WebSocket).
// Panel lama tidak mengirimnya — Error.Reason lalu kosong.
const (
	// ReasonRateLimited — terlalu sering meminta; ulangi setelah Retry-After.
	ReasonRateLimited = "rate_limited"
	// ReasonSessionLimit — batas sesi SSH bersamaan tercapai.
	ReasonSessionLimit = "session_limit"
)

// Client — klien panel. Token kosong = permintaan tanpa auth.
type Client struct {
	BaseURL   string // origin API (tanpa /api/v1), sudah dinormalisasi
	Token     string
	UserAgent string
	HTTP      *http.Client
}

// NewHTTPClient — klien dengan batas waktu wajar; proxy dari env tetap
// berlaku (http.DefaultTransport). Pengalihan tidak pernah diikuti.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: RequestTimeout, CheckRedirect: noRedirect}
}

// noRedirect — permintaan panel membawa token Bearer; mengikuti pengalihan
// (ke host lain, ke http, atau ke path lain di host yang sama) bisa
// mengirimkannya ke tempat yang tidak dimaksud. Respons 3xx dikembalikan apa
// adanya lalu dilaporkan sebagai *RedirectError.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// RedirectError — server menjawab 3xx; easyrun tidak mengikutinya.
type RedirectError struct {
	Status   int
	Location string // sudah dibersihkan dari karakter kendali
}

func (e *RedirectError) Error() string {
	to := ""
	if e.Location != "" {
		to = " ke " + e.Location
	}
	return fmt.Sprintf("server mengalihkan permintaan (HTTP %d%s); pengalihan tidak diikuti supaya token tidak terkirim ke alamat lain", e.Status, to)
}

// RedirectFromResponse — *RedirectError untuk respons 3xx, nil selain itu.
func RedirectFromResponse(status int, header http.Header) error {
	if status < 300 || status > 399 {
		return nil
	}
	e := &RedirectError{Status: status}
	if header != nil {
		e.Location = Clean(header.Get("Location"))
	}
	return e
}

// Error — penolakan dari server (status ≥ 400).
type Error struct {
	Status     int
	Message    string // sudah dibersihkan dari karakter kendali
	Code       string // kode mesin bila ada (mis. authorization_pending)
	Reason     string // data.reason bila ada (ReasonRateLimited / ReasonSessionLimit), huruf kecil
	RequestID  string
	RetryAfter time.Duration
	// Handshake — penolakan jabat tangan WebSocket (/console/ws?ticket=),
	// bukan panggilan API ber-token. 401 di sana soal tiket 30 detik.
	Handshake bool
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" && e.Code != msg {
		msg += " (" + e.Code + ")"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

// NetError — server tidak bisa dihubungi (DNS, TLS, batas waktu, ...).
type NetError struct {
	Host string
	Err  error
}

func (e *NetError) Error() string {
	return fmt.Sprintf("tidak bisa menghubungi %s: %v", e.Host, e.Err)
}
func (e *NetError) Unwrap() error { return e.Err }

type envelope struct {
	Success   *bool           `json:"success"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Error     json.RawMessage `json:"error"`
	Code      json.RawMessage `json:"code"`
	RequestID string          `json:"request_id"`
}

// ErrorFromResponse — bangun *Error dari respons gagal (juga dipakai untuk
// jabat tangan WebSocket yang ditolak).
func ErrorFromResponse(status int, header http.Header, body []byte, now time.Time) *Error {
	e := &Error{Status: status}
	if header != nil {
		e.RetryAfter = ParseRetryAfter(header.Get("Retry-After"), now)
	}
	var env envelope
	if json.Unmarshal(body, &env) == nil {
		e.Message = Clean(env.Message)
		e.RequestID = Clean(env.RequestID)
		e.Code = firstCode(env)
		e.Reason = dataReason(env)
	} else if s := strings.TrimSpace(string(body)); s != "" && !strings.HasPrefix(s, "<") {
		e.Message = Clean(s)
	}
	return e
}

// firstCode — kode mesin dari tempat mana pun panel menaruhnya: `error`
// tingkat atas (RFC 8628), `data.error` (SendErrorWithData panel), `code`,
// atau `message` yang isinya persis salah satu kode alur device.
func firstCode(env envelope) string {
	if s := rawString(env.Error); s != "" {
		return s
	}
	if len(env.Data) > 0 {
		var d struct {
			Error json.RawMessage `json:"error"`
			Code  json.RawMessage `json:"code"`
		}
		if json.Unmarshal(env.Data, &d) == nil {
			if s := rawString(d.Error); s != "" {
				return s
			}
			if s := rawString(d.Code); s != "" {
				return s
			}
		}
	}
	if s := rawString(env.Code); s != "" {
		return s
	}
	if m := strings.TrimSpace(env.Message); deviceCodes[m] {
		return m
	}
	return ""
}

// dataReason — `data.reason` envelope panel (SendErrorWithData dengan
// RetryData), huruf kecil; "" bila tidak ada atau `data` bukan objek.
func dataReason(env envelope) string {
	if len(env.Data) == 0 {
		return ""
	}
	var d struct {
		Reason json.RawMessage `json:"reason"`
	}
	if json.Unmarshal(env.Data, &d) != nil {
		return ""
	}
	return strings.ToLower(rawString(d.Reason))
}

func rawString(r json.RawMessage) string {
	var s string
	if len(r) == 0 || json.Unmarshal(r, &s) != nil {
		return ""
	}
	return Clean(s)
}

// ParseRetryAfter — detik atau tanggal HTTP; 0 bila kosong/rusak.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

// Clean — buang karakter kendali (cegah sisipan escape terminal dari teks
// server) dan potong panjangnya.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxMessageLen {
		s = string(r[:maxMessageLen]) + "…"
	}
	return s
}

// do — kirim permintaan JSON, buka envelope, isi out dengan `data` (atau
// seluruh badan bila tanpa envelope).
func (c *Client) do(ctx context.Context, method, path string, in, out any, auth bool) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := c.BaseURL + apiPrefix + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if auth {
		if c.Token == "" {
			return &Error{Status: http.StatusUnauthorized, Message: "belum masuk"}
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = NewHTTPClient()
	}
	// Salinan dangkal: Transport (dan pool koneksinya) tetap dipakai bersama,
	// tetapi pengalihan tidak pernah diikuti, apa pun klien yang diberikan.
	noFollow := *hc
	noFollow.CheckRedirect = noRedirect
	resp, err := noFollow.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &NetError{Host: hostOf(c.BaseURL), Err: unwrapURLError(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return &NetError{Host: hostOf(c.BaseURL), Err: err}
	}
	if len(raw) > maxBody {
		return fmt.Errorf("respons %s %s terlalu besar", method, path)
	}
	if err := RedirectFromResponse(resp.StatusCode, resp.Header); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return ErrorFromResponse(resp.StatusCode, resp.Header, raw, time.Now())
	}
	if out == nil {
		return nil
	}
	return decodeData(raw, out)
}

func decodeData(raw []byte, out any) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Success != nil {
		if !*env.Success {
			return &Error{Status: http.StatusOK, Message: Clean(env.Message), Code: firstCode(env)}
		}
		// Panel mengirim slice nil sebagai "data": null (atau tanpa data):
		// hasilnya nilai kosong; pemanggil memeriksa field wajibnya sendiri.
		if len(env.Data) == 0 || string(env.Data) == "null" {
			return nil
		}
		raw = env.Data
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("respons server tidak sesuai kontrak: %w", err)
	}
	return nil
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		var ne net.Error
		if errors.As(ue.Err, &ne) && ne.Timeout() {
			return errors.New("batas waktu habis")
		}
		return ue.Err
	}
	return err
}

// ---------- endpoint ----------

// DeviceRequest — badan POST /cli/device.
type DeviceRequest struct {
	ClientName string `json:"client_name"`
	ClientOS   string `json:"client_os"`
}

// DeviceCode — jawaban POST /cli/device. `verification_uri_complete` sengaja
// tidak dibaca (kontrak §7b P1): tautan berisi kode bisa dikirim penipu ke
// korban; pengguna harus mengetik kode dari terminalnya sendiri.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Token — jawaban POST /cli/token.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenID     string `json:"token_id"`
}

// Me — jawaban GET /cli/me.
type Me struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
}

// Box — satu baris GET /cli/boxes.
type Box struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Hostname     string `json:"hostname"`
	TenantID     string `json:"tenant_id"`
	TenantName   string `json:"tenant_name"`
	Status       string `json:"status"`
	SSHAvailable bool   `json:"ssh_available"`
}

// Certificate — jawaban POST /instances/:id/ssh/certificate.
type Certificate struct {
	Certificate string `json:"certificate"`
	ExpiresAt   string `json:"expires_at"`
	Principal   string `json:"principal"`
	HostAlias   string `json:"host_alias"`
}

// Ticket — jawaban POST /instances/:id/ssh/ticket.
type Ticket struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
	WSURL     string `json:"ws_url"`
}

func (c *Client) StartDevice(ctx context.Context, r DeviceRequest) (*DeviceCode, error) {
	var out DeviceCode
	if err := c.do(ctx, http.MethodPost, "/cli/device", r, &out, false); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" {
		return nil, errors.New("respons /cli/device tidak lengkap")
	}
	return &out, nil
}

// PollToken — satu kali tanya POST /cli/token. Menunggu persetujuan →
// *Error{Status:400, Code:authorization_pending}.
func (c *Client) PollToken(ctx context.Context, deviceCode string) (*Token, error) {
	var out Token
	if err := c.do(ctx, http.MethodPost, "/cli/token", map[string]string{"device_code": deviceCode}, &out, false); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("respons /cli/token tanpa access_token")
	}
	return &out, nil
}

func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.do(ctx, http.MethodGet, "/cli/me", nil, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Boxes(ctx context.Context) ([]Box, error) {
	var out []Box
	if err := c.do(ctx, http.MethodGet, "/cli/boxes", nil, &out, true); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/cli/logout", nil, nil, true)
}

func (c *Client) Certificate(ctx context.Context, boxID, publicKey string) (*Certificate, error) {
	var out Certificate
	path := "/instances/" + url.PathEscape(boxID) + "/ssh/certificate"
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"public_key": publicKey}, &out, true); err != nil {
		return nil, err
	}
	if out.Certificate == "" {
		return nil, errors.New("respons sertifikat kosong")
	}
	return &out, nil
}

func (c *Client) Ticket(ctx context.Context, boxID string) (*Ticket, error) {
	var out Ticket
	path := "/instances/" + url.PathEscape(boxID) + "/ssh/ticket"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{}, &out, true); err != nil {
		return nil, err
	}
	if out.Ticket == "" || out.WSURL == "" {
		return nil, errors.New("respons tiket tidak lengkap")
	}
	return &out, nil
}
