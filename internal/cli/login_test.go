package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

func TestLoginAlurDevice(t *testing.T) {
	h := newHarness(t)
	h.p.tokenReplies = []func(http.ResponseWriter){tokenPending, tokenCode("slow_down"), tokenOK}
	if code := h.run("login"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	// Kontrak §7b P1: tautan (tanpa kode) dan kode dicetak terpisah, dengan
	// peringatan anti-penipuan; verification_uri_complete dari server
	// (panel lama) diabaikan.
	for _, want := range []string{
		"\n    " + h.p.srv.URL + "/cli/aktivasi\n",
		"\n    ABCD-EFGH\n",
		"Ketik kode ini sendiri di halaman tersebut. Jangan setujui kode yang dikirim orang lain.",
		"Berhasil masuk sebagai budi@contoh.com", "2026-12-11",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout tak memuat %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "kode=") {
		t.Errorf("kode ikut di tautan:\n%s", out)
	}
	if strings.Contains(out, testToken) || strings.Contains(h.stderr.String(), testToken) {
		t.Fatal("token tercetak")
	}
	if !reflect.DeepEqual(h.opened, []string{h.p.srv.URL + "/cli/aktivasi"}) {
		t.Fatalf("browser harus membuka verification_uri tanpa kode: %q", h.opened)
	}
	// interval 5 dtk, slow_down menambah 5 dtk (RFC 8628).
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}; !reflect.DeepEqual(h.clock.sleeps, want) {
		t.Fatalf("jeda polling %v, ingin %v", h.clock.sleeps, want)
	}
	if h.p.n("POST /api/v1/cli/token") != 3 || h.p.n("GET /api/v1/cli/me") != 1 {
		t.Fatalf("panggilan: %v", h.p.calls)
	}
	if h.p.deviceReq.ClientName != "laptop-budi" || h.p.deviceReq.ClientOS != "macOS "+runtime.GOARCH {
		t.Fatalf("device req: %+v", h.p.deviceReq)
	}
	c, err := h.d.LoadCredentials(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != testToken || c.Email != "budi@contoh.com" || c.TokenID != "tok-1" || c.APIURL != h.p.srv.URL {
		t.Fatalf("credentials: %+v", c)
	}
	if !c.ExpiresAt.Equal(time.Date(2026, 12, 11, 16, 10, 0, 0, time.UTC)) {
		t.Fatalf("expires_at dari /cli/me: %v", c.ExpiresAt)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(h.d.CredentialsPath()); fi.Mode().Perm() != 0o600 {
			t.Fatalf("credentials.json %04o", fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(h.d.ConfigPath()); err == nil {
		t.Fatal("config.json tidak boleh ditulis tanpa --api-url")
	}
}

func TestLoginGagal(t *testing.T) {
	cases := []struct {
		name    string
		replies []func(http.ResponseWriter)
		want    string
	}{
		{"ditolak", []func(http.ResponseWriter){tokenPending, tokenCode("access_denied")}, "login ditolak di panel"},
		{"kedaluwarsa di server", []func(http.ResponseWriter){tokenCode("expired_token")}, "kedaluwarsa"},
		{"kode tak dikenal", []func(http.ResponseWriter){func(w http.ResponseWriter) { fail(w, 400, "device_code tidak valid") }}, "device_code tidak valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.p.tokenReplies = c.replies
			if code := h.run("login"); code != 1 {
				t.Fatalf("kode %d", code)
			}
			if !strings.Contains(h.stderr.String(), c.want) {
				t.Fatalf("stderr %q tak memuat %q", h.stderr, c.want)
			}
			if _, err := h.d.LoadCredentials(true); err == nil {
				t.Fatal("token tersimpan padahal gagal")
			}
		})
	}
}

func TestLoginKedaluwarsaLokal(t *testing.T) {
	h := newHarness(t)
	h.p.device["expires_in"] = 12
	if code := h.run("login"); code != 1 || !strings.Contains(h.stderr.String(), "kedaluwarsa") {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	// 5 dtk + 5 dtk masih < 12 dtk → 2 kali tanya; jeda ke-3 melewati batas.
	if n := h.p.n("POST /api/v1/cli/token"); n != 2 {
		t.Fatalf("polling %d kali", n)
	}
}

func TestLoginCtrlC(t *testing.T) {
	h := newHarness(t)
	h.clock.cancelAt = 2
	if code := h.run("login"); code != 130 || !strings.Contains(h.stderr.String(), "dibatalkan") {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if _, err := h.d.LoadCredentials(true); err == nil {
		t.Fatal("token tersimpan padahal dibatalkan")
	}
}

func TestLoginServerSempatGagal(t *testing.T) {
	h := newHarness(t)
	h.p.tokenReplies = []func(http.ResponseWriter){
		func(w http.ResponseWriter) { fail(w, 502, "internal server error") },
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "20")
			fail(w, 429, "terlalu sering")
		},
		tokenOK,
	}
	if code := h.run("login"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "mencoba lagi") {
		t.Fatalf("stderr: %q", h.stderr)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 20 * time.Second}; !reflect.DeepEqual(h.clock.sleeps, want) {
		t.Fatalf("Retry-After 429 harus dihormati: %v", h.clock.sleeps)
	}
}

func TestLoginApiURLFlagDisimpan(t *testing.T) {
	h := newHarness(t)
	delete(h.env, config.EnvAPIURL)
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h.run("login", "--api-url", h.p.srv.URL+"/api/v1/"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	b, err := os.ReadFile(h.d.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	_ = json.Unmarshal(b, &cfg)
	if cfg.APIURL != h.p.srv.URL {
		t.Fatalf("config.json: %s", b)
	}
	// Perintah berikutnya memakai alamat tersimpan tanpa env.
	if code := h.run("boxes"); code != 0 {
		t.Fatalf("boxes: %d %s", code, h.stderr)
	}
}

func TestLoginTautanTidakAman(t *testing.T) {
	// verification_uri tidak aman → gagal, walau _complete (yang tidak lagi
	// dipakai) aman: tidak ada jatuh-balik ke tautan berkode.
	h := newHarness(t)
	h.p.device["verification_uri"] = "file:///etc/passwd"
	if code := h.run("login"); code != 1 || !strings.Contains(h.stderr.String(), "tidak aman") {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if len(h.opened) != 0 {
		t.Fatalf("browser dibuka: %q", h.opened)
	}
	// verification_uri yang (salah) memuat kode: kode dibuang dari tautan
	// yang dicetak dan yang dibuka; parameter lain tetap.
	h2 := newHarness(t)
	h2.p.device["verification_uri"] = h2.p.srv.URL + "/cli/aktivasi?kode=ABCD-EFGH&User_Code=ABCD-EFGH&lang=id"
	h2.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h2.run("login"); code != 0 || !reflect.DeepEqual(h2.opened, []string{h2.p.srv.URL + "/cli/aktivasi?lang=id"}) {
		t.Fatalf("kode %d opened %q", code, h2.opened)
	}
	if out := h2.stdout.String(); strings.Contains(strings.ToLower(out), "code=") || strings.Contains(out, "kode=") {
		t.Fatalf("kode ikut di tautan:\n%s", out)
	}
	// Tanpa user_code → gagal (tidak ada yang bisa diketik pengguna).
	h3 := newHarness(t)
	h3.p.device["user_code"] = "\x1b"
	if code := h3.run("login"); code != 1 {
		t.Fatalf("user_code kosong: kode %d", code)
	}
}

func TestLoginTanpaBrowser(t *testing.T) {
	h := newHarness(t)
	h.env[EnvNoBrowser] = "1"
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h.run("login"); code != 0 || len(h.opened) != 0 {
		t.Fatalf("kode %d opened %q", code, h.opened)
	}
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h.run("login", "--no-browser"); code != 0 || len(h.opened) != 0 {
		t.Fatalf("--no-browser: kode %d opened %q", code, h.opened)
	}
}

func TestLoginUlangMencabutTokenLama(t *testing.T) {
	h := newHarness(t)
	if err := h.d.SaveCredentials(config.Credentials{APIURL: h.p.srv.URL, AccessToken: "erc_lama"}); err != nil {
		t.Fatal(err)
	}
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h.run("login"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if !reflect.DeepEqual(h.p.logouts, []string{"erc_lama"}) {
		t.Fatalf("logout: %q", h.p.logouts)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if err := os.WriteFile(h.d.CertPath(boxID), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("logout"); code != 0 || !strings.Contains(h.stdout.String(), "dicabut") {
		t.Fatalf("kode %d: %s %s", code, h.stdout, h.stderr)
	}
	if !reflect.DeepEqual(h.p.logouts, []string{testToken}) {
		t.Fatalf("logout: %q", h.p.logouts)
	}
	if _, err := os.Stat(h.d.CredentialsPath()); err == nil {
		t.Fatal("credentials.json masih ada")
	}
	if _, err := os.Stat(h.d.CertPath(boxID)); err == nil {
		t.Fatal("sertifikat masih ada")
	}
	if code := h.run("logout"); code != 0 || !strings.Contains(h.stdout.String(), "Belum masuk") {
		t.Fatalf("logout kedua: %d %s", code, h.stdout)
	}
}

func TestLogoutServerMatiTetapHapusLokal(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.srv.Close()
	if code := h.run("logout"); code != 1 {
		t.Fatalf("kode %d", code)
	}
	if !strings.Contains(h.stderr.String(), "Perangkat CLI") {
		t.Fatalf("petunjuk cabut manual: %q", h.stderr)
	}
	if _, err := os.Stat(h.d.CredentialsPath()); err == nil {
		t.Fatal("token lokal harus tetap dihapus")
	}
}

func TestLogoutIzinTerbukaTetapDicabut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bit izin Unix")
	}
	h := newHarness(t)
	h.loggedIn(t)
	_ = os.Chmod(h.d.CredentialsPath(), 0o644)
	if code := h.run("logout"); code != 0 || len(h.p.logouts) != 1 {
		t.Fatalf("kode %d logouts %q: %s", code, h.p.logouts, h.stderr)
	}
}

func TestBoxesTabel(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("boxes"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("baris:\n%s", h.stdout)
	}
	if f := strings.Fields(lines[0]); !reflect.DeepEqual(f, []string{"NAMA", "HOSTNAME", "TENANT", "STATUS", "SSH", "ID"}) {
		t.Fatalf("kepala: %q", lines[0])
	}
	// Urut per tenant: "Agensi Dua" sebelum "Tenant Satu".
	if !strings.HasPrefix(lines[1], "web-lama") || !strings.Contains(lines[1], " tidak ") {
		t.Fatalf("baris 1: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "uji-box") || !strings.Contains(lines[2], "contoh-box.easyrun.io") || !strings.Contains(lines[2], " ya ") || !strings.HasSuffix(lines[2], boxID) {
		t.Fatalf("baris 2: %q", lines[2])
	}
}

func TestBoxesSesiTidakBerlaku(t *testing.T) {
	t.Run("belum masuk", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("boxes"); code != 1 || !strings.Contains(h.stderr.String(), "belum masuk — jalankan: easyrun login") {
			t.Fatalf("%d %s", code, h.stderr)
		}
		if h.p.total() != 0 {
			t.Fatal("tidak boleh ada permintaan")
		}
	})
	t.Run("dicabut di server", func(t *testing.T) {
		h := newHarness(t)
		if err := h.d.SaveCredentials(config.Credentials{APIURL: h.p.srv.URL, AccessToken: "erc_dicabut"}); err != nil {
			t.Fatal(err)
		}
		if code := h.run("boxes"); code != 1 || !strings.Contains(h.stderr.String(), "jalankan: easyrun login") {
			t.Fatalf("%d %s", code, h.stderr)
		}
	})
	t.Run("kedaluwarsa lokal", func(t *testing.T) {
		h := newHarness(t)
		h.loggedIn(t)
		h.clock.Advance(31 * 24 * time.Hour)
		if code := h.run("boxes"); code != 1 || !strings.Contains(h.stderr.String(), "kedaluwarsa") {
			t.Fatalf("%d %s", code, h.stderr)
		}
		if h.p.total() != 0 {
			t.Fatal("token kedaluwarsa tidak boleh dikirim")
		}
	})
	t.Run("token untuk server lain", func(t *testing.T) {
		h := newHarness(t)
		h.loggedIn(t)
		h.env[config.EnvAPIURL] = "https://server-lain.example"
		if code := h.run("boxes"); code != 1 || !strings.Contains(h.stderr.String(), "token tersimpan untuk") {
			t.Fatalf("%d %s", code, h.stderr)
		}
		if h.p.total() != 0 {
			t.Fatal("tidak boleh ada permintaan")
		}
	})
	t.Run("izin terbuka", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("bit izin Unix")
		}
		h := newHarness(t)
		h.loggedIn(t)
		_ = os.Chmod(h.d.CredentialsPath(), 0o640)
		if code := h.run("boxes"); code != 1 || !strings.Contains(h.stderr.String(), "chmod 600") || !strings.Contains(h.stderr.String(), "easyrun logout") {
			t.Fatalf("%d %s", code, h.stderr)
		}
		if h.p.total() != 0 {
			t.Fatal("token berizin terbuka tidak boleh dipakai")
		}
	})
	t.Run("daftar kosong", func(t *testing.T) {
		h := newHarness(t)
		h.loggedIn(t)
		h.p.mu.Lock()
		h.p.boxes = nil
		h.p.mu.Unlock()
		if code := h.run("boxes"); code != 0 || !strings.Contains(h.stdout.String(), "Belum ada box") {
			t.Fatalf("%d %s", code, h.stdout)
		}
	})
}

// Kontrak §7b P10: login dengan token baru membuang semua sertifikat box
// tersimpan (terikat token lama); `ssh-prepare` berikutnya meminta yang baru.
func TestLoginMembuangSertifikatLama(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	certs := func() int { return h.p.n("POST /instances/certificate") }
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 1 {
		t.Fatalf("ssh-prepare: kode %d sertifikat %d %s", code, certs(), h.stderr)
	}
	other := h.d.CertPath(otherID)
	if err := os.WriteFile(other, []byte("sertifikat box lain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ganti pengguna di laptop yang sama: token tersimpan milik orang lain.
	if err := h.d.SaveCredentials(config.Credentials{APIURL: h.p.srv.URL, AccessToken: "erc_pengguna_lain", UserID: "u-lain"}); err != nil {
		t.Fatal(err)
	}
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}
	if code := h.run("login"); code != 0 {
		t.Fatalf("login: kode %d %s", code, h.stderr)
	}
	for _, p := range []string{h.d.CertPath(boxID), other} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s masih ada setelah login (err %v)", p, err)
		}
	}
	if _, err := os.Stat(h.d.KeyPath()); err != nil {
		t.Fatalf("kunci laptop tidak boleh ikut dihapus: %v", err)
	}
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 2 {
		t.Fatalf("setelah login harus minta sertifikat baru: kode %d sertifikat %d %s", code, certs(), h.stderr)
	}
	// Sertifikat baru dipakai ulang seperti biasa (luring selama segar).
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 2 {
		t.Fatalf("sertifikat baru tidak dipakai ulang: kode %d sertifikat %d", code, certs())
	}
}

// Sertifikat lama tak bisa dihapus: login tetap menjalankan langkah lain yang
// aman — config.json menyimpan --api-url, token lama dicabut, identitas
// tersimpan — lalu keluar ≠ 0 dengan pesan yang menyebut berkasnya.
func TestLoginGagalMembuangSertifikatLama(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("butuh izin folder Unix dan bukan root")
	}
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("ssh-prepare", boxID); code != 0 {
		t.Fatalf("ssh-prepare: %d %s", code, h.stderr)
	}
	// Token tersimpan berbeda dari yang akan diterbitkan: harus dicabut.
	if err := h.d.SaveCredentials(config.Credentials{APIURL: h.p.srv.URL, AccessToken: "erc_lama"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.d.SSHDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.d.SSHDir(), 0o700) })
	delete(h.env, config.EnvAPIURL)
	h.p.tokenReplies = []func(http.ResponseWriter){tokenOK}

	code := h.run("login", "--api-url", h.p.srv.URL)
	errText := h.stderr.String()
	if code != 1 || !strings.Contains(errText, "sertifikat SSH lama tidak bisa dihapus") ||
		!strings.Contains(errText, "Hapus manual berkas "+filepath.Join(h.d.SSHDir(), "*-cert.pub")) {
		t.Fatalf("kode %d: %s", code, errText)
	}
	if !strings.Contains(h.stdout.String(), "Berhasil masuk sebagai budi@contoh.com") {
		t.Fatalf("stdout: %s", h.stdout)
	}
	b, err := os.ReadFile(h.d.ConfigPath())
	if err != nil {
		t.Fatalf("config.json harus tetap ditulis: %v", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.APIURL != h.p.srv.URL {
		t.Fatalf("config.json: %s", b)
	}
	if !reflect.DeepEqual(h.p.logouts, []string{"erc_lama"}) {
		t.Fatalf("token lama harus tetap dicabut: %q", h.p.logouts)
	}
	c, err := h.d.LoadCredentials(false)
	if err != nil || c.AccessToken != testToken || c.Email != "budi@contoh.com" {
		t.Fatalf("credentials: %+v %v", c, err)
	}
	if _, err := os.Stat(h.d.CertPath(boxID)); err != nil {
		t.Fatalf("sertifikat lama memang tidak terhapus di uji ini: %v", err)
	}
}
