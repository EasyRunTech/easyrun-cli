package cli

// Uji untuk temuan review rilis v0.2.0 (nomor = nomor temuan).

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// 1 — ssh/VS Code tidak mewarisi env shell: opsi global --config-dir dan
// --api-url (yang ditulis ssh-config ke ProxyCommand/Match exec) cukup.
func TestOpsiGlobalTanpaEnv(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.env = map[string]string{} // seperti VS Code yang tidak mewarisi env
	if code := h.run("ssh-prepare", boxID); code != 1 {
		t.Fatalf("tanpa env dan opsi harus gagal: %d", code)
	}
	if code := h.run("--config-dir", h.d.Path, "--api-url="+h.p.srv.URL, "ssh-prepare", boxID); code != 0 {
		t.Fatalf("dengan opsi global: %d %s", code, h.stderr)
	}
	if _, err := os.Stat(h.d.CertPath(boxID)); err != nil {
		t.Fatal("sertifikat tidak ditulis ke folder dari --config-dir")
	}
	if code := h.run("--config-dir"); code != 2 {
		t.Fatalf("--config-dir tanpa nilai: %d", code)
	}
}

// 3 — --write membersihkan blok lama untuk alias yang sama (box yang dihapus
// lalu dibuat ulang dengan nama sama), tetapi tidak menyentuh berkas yang
// bukan buatan easyrun.
func TestSSHConfigWriteHapusBlokLamaAliasSama(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("ssh-config", "uji-box", "--write"); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	dir := filepath.Join(h.home, ".ssh", "easyrun")
	oldConf := filepath.Join(dir, boxID+".conf")
	foreign := filepath.Join(dir, "00-tangan.conf")
	_ = os.WriteFile(foreign, []byte("Host uji-box\n    User root\n"), 0o600)

	const newID = "0a0a0a0a-1111-4222-8333-444444444444" // urut lebih awal dari boxID
	h.p.mu.Lock()
	h.p.boxes[0].ID = newID
	h.p.mu.Unlock()
	h.stdout.Reset()
	h.stderr.Reset()
	if code := h.run("ssh-config", "uji-box", "--write"); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	if _, err := os.Stat(oldConf); !os.IsNotExist(err) {
		t.Fatalf("blok box lama masih ada: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, newID+".conf")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stdout.String(), "Dihapus: "+oldConf) {
		t.Fatalf("stdout %q", h.stdout)
	}
	if _, err := os.Stat(foreign); err != nil || !strings.Contains(h.stderr.String(), foreign) {
		t.Fatalf("berkas bukan buatan easyrun harus dibiarkan dan dilaporkan: %v %q", err, h.stderr)
	}
}

// 9 — ~/.ssh mengikuti home akun (yang dibaca OpenSSH), bukan $HOME.
func TestSSHConfigWriteHomeAkun(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	acct := filepath.Join(t.TempDir(), "akun")
	h.a.SSHHomeDir = func() (string, error) { return acct, nil }
	if code := h.run("ssh-config", "uji-box", "--write"); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	if _, err := os.Stat(filepath.Join(acct, ".ssh", "config")); err != nil {
		t.Fatal("~/.ssh/config harus ditulis di home akun")
	}
	if _, err := os.Stat(filepath.Join(h.home, ".ssh")); !os.IsNotExist(err) {
		t.Fatal("$HOME/.ssh tidak boleh disentuh")
	}
	if !strings.Contains(h.stderr.String(), "home akun") {
		t.Fatalf("catatan beda home: %q", h.stderr)
	}
}

// 6 — jam laptop 61 menit lebih cepat: sertifikat baru tetap dipakai (dulu
// ditolak "kedaluwarsa") dan pengguna diberi tahu.
func TestJamLaptopLebihCepat(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.now = func() time.Time { return h.clock.Now().Add(-61 * time.Minute) }
	for i := 0; i < 2; i++ {
		h.stderr.Reset()
		if code := h.run("ssh-prepare", boxID); code != 0 {
			t.Fatalf("jalan %d: kode %d %s", i+1, code, h.stderr)
		}
		if !strings.Contains(h.stderr.String(), "lebih cepat 1h1m0s") {
			t.Fatalf("peringatan jam: %q", h.stderr)
		}
	}
	if off := h.d.LoadClockOffset().OffsetSeconds; off != -61*60 {
		t.Fatalf("selisih tersimpan %d", off)
	}
}

// 6 — jam laptop 30 menit lebih lambat: sertifikat yang menurut jam server
// tinggal 4 menit diperbarui (dulu dianggap masih 34 menit lalu dipakai
// sampai box menolaknya).
func TestJamLaptopLebihLambat(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.now = func() time.Time { return h.clock.Now().Add(30 * time.Minute) }
	if code := h.run("ssh-prepare", boxID); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	before := h.p.total()
	if code := h.run("ssh-prepare", boxID); code != 0 || h.p.total() != before {
		t.Fatalf("sertifikat segar diminta ulang: kode %d", code)
	}
	h.clock.Advance(56 * time.Minute)
	if code := h.run("ssh-prepare", boxID); code != 0 || h.p.total() == before {
		t.Fatalf("sertifikat yang hampir habis menurut jam server harus diperbarui: kode %d", code)
	}
}

// 6 — selisih tersimpan basi (jam laptop baru dibetulkan) tidak membuat
// sertifikat yang sudah habis dianggap segar.
func TestSelisihJamBasiTidakMemakaiSertifikatHabis(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("ssh-prepare", boxID); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	_ = h.d.SaveClockOffset(config.ClockOffset{OffsetSeconds: -2 * 3600})
	before := h.p.total()
	h.clock.Advance(56 * time.Minute)
	if code := h.run("ssh-prepare", boxID); code != 0 || h.p.total() == before {
		t.Fatalf("harus diperbarui menurut jam laptop: kode %d", code)
	}
}

// 11 — slow_down dengan data.interval dari panel: jeda berikutnya ikut panel.
func TestSlowDownMemakaiIntervalServer(t *testing.T) {
	h := newHarness(t)
	slow := func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"success":false,"message":"terlalu sering","data":{"error":"slow_down","interval":20}}`))
	}
	h.p.tokenReplies = []func(http.ResponseWriter){tokenPending, slow, tokenOK}
	if code := h.run("login"); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	if want := []time.Duration{5 * time.Second, 5 * time.Second, 20 * time.Second}; !reflect.DeepEqual(h.clock.sleeps, want) {
		t.Fatalf("jeda %v, ingin %v", h.clock.sleeps, want)
	}
}

// 15 — browser: sistem dan env dari App, bukan runtime/os langsung.
func TestOpenBrowserLewatApp(t *testing.T) {
	a := &App{GOOS: "linux", Getenv: func(string) string { return "" }}
	if err := a.openBrowser("https://panel.contoh.test/cli/aktivasi"); err == nil || !strings.Contains(err.Error(), "tampilan grafis") {
		t.Fatalf("tanpa DISPLAY harus menolak tanpa menjalankan apa pun: %v", err)
	}
}

// 10 — id UUID dinormalkan huruf kecil; id lain apa adanya.
func TestNormalizeID(t *testing.T) {
	if got := normalizeID(strings.ToUpper(boxID)); got != boxID {
		t.Fatalf("%q", got)
	}
	if got := normalizeID("Box-Lain"); got != "Box-Lain" {
		t.Fatalf("%q", got)
	}
	b := api.Box{ID: strings.ToUpper(boxID)}
	if normalizeID(b.ID) != boxID {
		t.Fatal("box dari server")
	}
}
