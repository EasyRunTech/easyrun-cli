package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/sshcert"
	"github.com/EasyRunTech/easyrun-cli/internal/sshtest"
)

func TestMatchBox(t *testing.T) {
	boxes := []api.Box{
		{ID: boxID, Name: "uji-box", Hostname: "contoh-box.easyrun.io"},
		{ID: otherID, Name: "web-lama", Hostname: "web-lama.easyrun.io"},
		{ID: "33333333-2222-4333-8444-555555555555", Name: "kembar", Hostname: "kembar-a.easyrun.io", TenantName: "A"},
		{ID: "44444444-2222-4333-8444-555555555555", Name: "kembar", Hostname: "kembar-b.easyrun.io", TenantName: "B"},
		{ID: "55555555-2222-4333-8444-555555555555", Name: "contoh-box", Hostname: "lain.easyrun.io"},
	}
	// Kontrak §6 + §7a.5: nama, hostname lengkap, label pertama hostname, id.
	unique := map[string]string{
		"uji-box":               boxID,
		"UJI-BOX":               boxID,
		"contoh-box.easyrun.io": boxID,
		"CONTOH-BOX.EASYRUN.IO": boxID,
		strings.ToUpper(boxID):  boxID,
		"web-lama":              otherID, // nama = label pertama: tetap satu box
		"web-lama.easyrun.io":   otherID,
		"kembar-a":              "33333333-2222-4333-8444-555555555555",
	}
	for q, want := range unique {
		b, err := matchBox(boxes, q)
		if err != nil || b.ID != want {
			t.Errorf("%q → %q, %v; ingin %q", q, b.ID, err, want)
		}
	}
	// "contoh-box" = label pertama box 1 DAN nama box 5 → ambigu.
	for q, n := range map[string]int{"kembar": 2, "contoh-box": 2} {
		_, err := matchBox(boxes, q)
		var ae *ambiguousBoxError
		if !errors.As(err, &ae) || len(ae.matches) != n {
			t.Errorf("%q harus ambigu (%d), dapat %v", q, n, err)
		}
	}
	_, err := matchBox(boxes, "tidak-ada")
	var nf *boxNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("tidak ditemukan: %v", err)
	}
}

func TestSSHAmbiguMendaftarPilihan(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.boxes = append(h.p.boxes, api.Box{ID: "33333333-2222-4333-8444-555555555555", Name: "contoh-box", Hostname: "contoh-box-2.easyrun.io", TenantName: "Agensi Dua", SSHAvailable: true})
	if code := h.run("ssh", "contoh-box"); code != 1 {
		t.Fatalf("kode %d", code)
	}
	for _, want := range []string{"cocok dengan 2 box", "contoh-box.easyrun.io", "contoh-box-2.easyrun.io", boxID} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr tak memuat %q:\n%s", want, h.stderr)
		}
	}
	if len(h.execs) != 0 {
		t.Fatal("ssh dijalankan padahal ambigu")
	}
}

func readCert(t *testing.T, path string) *ssh.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sshcert.ParseCert(b)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSSHPrepare(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	certs := func() int { return h.p.n("POST /instances/certificate") }

	if code := h.run("ssh-prepare", boxID); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if certs() != 1 || h.p.n("GET /api/v1/cli/boxes") != 0 {
		t.Fatalf("id UUID tidak perlu daftar box: %v", h.p.calls)
	}
	if h.stdout.Len() != 0 || h.stderr.Len() != 0 {
		t.Fatalf("ssh-prepare harus diam bila sukses: %q %q", h.stdout, h.stderr)
	}
	c := readCert(t, h.d.CertPath(boxID))
	if !slices.Equal(c.ValidPrincipals, []string{boxID}) {
		t.Fatalf("principal %v", c.ValidPrincipals)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{h.d.KeyPath(), h.d.CertPath(boxID)} {
			if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s %04o", p, fi.Mode().Perm())
			}
		}
	}

	// Masih segar → tanpa server sama sekali, bahkan tanpa token.
	before := h.p.total()
	_ = h.d.RemoveCredentials()
	if code := h.run("ssh-prepare", boxID); code != 0 || h.p.total() != before {
		t.Fatalf("segar harus luring: kode %d, permintaan %d → %d (%s)", code, before, h.p.total(), h.stderr)
	}
	h.loggedIn(t)

	// Sisa 4 menit → diperbarui.
	h.clock.Advance(56 * time.Minute)
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 2 {
		t.Fatalf("hampir habis: kode %d sertifikat %d", code, certs())
	}
	// Sisa 6 menit dari sertifikat baru → tidak diperbarui.
	h.clock.Advance(54 * time.Minute)
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 2 {
		t.Fatalf("sisa 6 menit: kode %d sertifikat %d", code, certs())
	}
	// Kedaluwarsa → diperbarui.
	h.clock.Advance(2 * time.Hour)
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 3 {
		t.Fatalf("kedaluwarsa: kode %d sertifikat %d", code, certs())
	}

	// Berkas berisi sertifikat untuk box lain → diperbarui.
	kp, err := sshcert.EnsureKey(h.d.KeyPath(), h.d.PubKeyPath(), keyComment)
	if err != nil {
		t.Fatal(err)
	}
	now := h.clock.Now()
	wrong := h.p.ca.SignLine(t, kp.PublicKey, otherID, now, now.Add(time.Hour))
	if err := os.WriteFile(h.d.CertPath(boxID), []byte(wrong), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 4 {
		t.Fatalf("principal salah: kode %d sertifikat %d", code, certs())
	}
	if c := readCert(t, h.d.CertPath(boxID)); c.ValidPrincipals[0] != boxID {
		t.Fatalf("principal %v", c.ValidPrincipals)
	}
	// Sertifikat untuk kunci lain (kunci laptop diganti) → diperbarui.
	other := h.p.ca.SignLine(t, sshtest.NewKey(t), boxID, now, now.Add(time.Hour))
	_ = os.WriteFile(h.d.CertPath(boxID), []byte(other), 0o600)
	if code := h.run("ssh-prepare", boxID); code != 0 || certs() != 5 {
		t.Fatalf("kunci lain: kode %d sertifikat %d", code, certs())
	}
}

func TestSSHPrepareLewatNamaDanHostname(t *testing.T) {
	for _, q := range []string{"uji-box", "contoh-box", "contoh-box.easyrun.io"} {
		h := newHarness(t)
		h.loggedIn(t)
		if code := h.run("ssh-prepare", q); code != 0 {
			t.Fatalf("%q: kode %d %s", q, code, h.stderr)
		}
		if h.p.n("GET /api/v1/cli/boxes") != 1 || h.p.n("POST /instances/certificate") != 1 {
			t.Fatalf("%q: %v", q, h.p.calls)
		}
		if _, err := os.Stat(h.d.CertPath(boxID)); err != nil {
			t.Fatalf("%q: sertifikat tidak ditulis ke berkas id", q)
		}
	}
}

func TestSSHPrepareSertifikatServerDitolak(t *testing.T) {
	cases := map[string]func(ssh.PublicKey, string) (ssh.PublicKey, string){
		"box lain":   func(k ssh.PublicKey, id string) (ssh.PublicKey, string) { return k, otherID },
		"kunci lain": func(k ssh.PublicKey, id string) (ssh.PublicKey, string) { return sshtest.NewKey(t), id },
	}
	for want, override := range cases {
		h := newHarness(t)
		h.loggedIn(t)
		h.p.certOverride = override
		if code := h.run("ssh-prepare", boxID); code != 1 || !strings.Contains(h.stderr.String(), want) {
			t.Fatalf("%s: kode %d %s", want, code, h.stderr)
		}
		if _, err := os.Stat(h.d.CertPath(boxID)); err == nil {
			t.Fatalf("%s: sertifikat salah tetap disimpan", want)
		}
	}
	// TTL server < 5 menit tetap dipakai (tidak berputar minta terus).
	h := newHarness(t)
	h.loggedIn(t)
	h.p.certTTL = 2 * time.Minute
	if code := h.run("ssh-prepare", boxID); code != 0 {
		t.Fatalf("TTL pendek: %d %s", code, h.stderr)
	}
}

func TestSSHPrepareGagal(t *testing.T) {
	h := newHarness(t)
	if code := h.run("ssh-prepare", boxID); code != 1 || !strings.Contains(h.stderr.String(), "easyrun login") {
		t.Fatalf("tanpa token & sertifikat: %d %s", code, h.stderr)
	}
	h.loggedIn(t)
	if code := h.run("ssh-prepare", "99999999-2222-4333-8444-555555555555"); code != 1 || !strings.Contains(h.stderr.String(), "bukan anggota tenant") {
		t.Fatalf("404: %d %s", code, h.stderr)
	}
	// 409: kalimat panel diteruskan apa adanya (kontrak §7b P14).
	if code := h.run("ssh-prepare", otherID); code != 1 || !strings.Contains(h.stderr.String(), "SSH tidak tersedia untuk instance ini") {
		t.Fatalf("409: %d %s", code, h.stderr)
	}
	if code := h.run("ssh-prepare"); code != 2 {
		t.Fatalf("tanpa argumen: %d", code)
	}
}

func TestSSHArgvDanExec(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	// Temuan review 12: host_alias jawaban sertifikat diabaikan — HostName
	// selalu dari /cli/boxes, HostKeyAlias = id box.
	h.p.hostAlias = "alamat-lama.easyrun.io"
	h.execRet = 42
	if code := h.run("ssh", "CONTOH-BOX.easyrun.io", "--", "-L", "8080:localhost:80", "uptime"); code != 42 {
		t.Fatalf("kode keluar ssh harus diteruskan: %d %s", code, h.stderr)
	}
	if len(h.execs) != 1 {
		t.Fatalf("exec: %v", h.execs)
	}
	e := h.execs[0]
	if e.path != "/usr/bin/ssh" || e.argv[0] != "ssh" {
		t.Fatalf("exec %q %q", e.path, e.argv)
	}
	argv := strings.Join(e.argv, "\n")
	for _, want := range []string{
		"ProxyCommand=" + h.invocation() + " tunnel " + boxID,
		"IdentityFile=" + h.d.KeyPath(),
		"CertificateFile=" + h.d.CertPath(boxID),
		"IdentitiesOnly=yes",
		"HostKeyAlias=" + boxID,
		"UserKnownHostsFile=" + h.d.KnownHostsPath(),
		"StrictHostKeyChecking=accept-new",
		"ServerAliveInterval=30",
		"ForwardAgent=no",
		"ForwardX11=no",
		"User=ubuntu",
	} {
		if !strings.Contains(argv, "\n"+want+"\n") {
			t.Errorf("argv tak memuat %q", want)
		}
	}
	if tail := e.argv[len(e.argv)-4:]; !slices.Equal(tail, []string{"contoh-box.easyrun.io", "-L", "8080:localhost:80", "uptime"}) {
		t.Fatalf("ekor argv %q", tail)
	}
	if _, err := os.Stat(h.d.CertPath(boxID)); err != nil {
		t.Fatal("sertifikat belum disiapkan sebelum exec")
	}
	// Tanpa "--" juga diteruskan.
	h.execs = nil
	if h.run("ssh", "uji-box", "whoami"); len(h.execs) != 1 || h.execs[0].argv[len(h.execs[0].argv)-1] != "whoami" {
		t.Fatalf("tanpa --: %v", h.execs)
	}
}

func TestSSHMenolak(t *testing.T) {
	cases := []struct {
		name  string
		box   string
		setup func(*harness)
		want  string
	}{
		{"nama bersisip shell", "x;touch /tmp/pwned", nil, "tidak valid"},
		{"nama seperti opsi", "-oProxyCommand=id", nil, "tidak valid"},
		// ssh_available=false: panel yang memutuskan (409 + alasannya).
		{"ssh tidak tersedia", "web-lama", nil, "SSH tidak tersedia untuk instance ini"},
		{"tidak ditemukan", "entah", nil, "tidak ditemukan"},
		{"hostname server aneh", "uji-box", func(h *harness) { h.p.boxes[0].Hostname = "evil host;id" }, "hostname box dari server tidak valid"},
		{"id server aneh", "uji-box", func(h *harness) { h.p.boxes[0].ID = "$(id)" }, "id box dari server tidak valid"},
		{"tanpa program ssh", "uji-box", func(h *harness) {
			h.a.LookPath = func(string) (string, error) { return "", errors.New("tidak ada") }
		}, "OpenSSH"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			if c.setup != nil {
				c.setup(h)
			}
			if code := h.run("ssh", c.box); code != 1 {
				t.Fatalf("kode %d", code)
			}
			if !strings.Contains(h.stderr.String(), c.want) {
				t.Fatalf("stderr %q tak memuat %q", h.stderr, c.want)
			}
			if len(h.execs) != 0 {
				t.Fatal("ssh dijalankan")
			}
		})
	}
}

func TestSSHConfig(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	if code := h.run("ssh-config", "contoh-box.easyrun.io"); code != 0 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{
		`Match host contoh-box.easyrun.io exec "` + h.invocation() + ` ssh-prepare ` + boxID + `"`,
		"\nHost contoh-box.easyrun.io\n",
		"    HostName contoh-box.easyrun.io\n",
		"    User ubuntu\n",
		"    IdentityFile " + h.d.KeyPath() + "\n",
		"    CertificateFile " + h.d.CertPath(boxID) + "\n",
		"    IdentitiesOnly yes\n",
		"    ProxyCommand " + h.invocation() + " tunnel " + boxID + "\n",
		"    HostKeyAlias " + boxID + "\n",
		"    UserKnownHostsFile " + h.d.KnownHostsPath() + "\n",
		"    StrictHostKeyChecking accept-new\n",
		"    ServerAliveInterval 30\n",
		"    ForwardAgent no\n",
		"    ForwardX11 no\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("blok tak memuat %q:\n%s", want, out)
		}
	}
	// Kontrak §7b P7: tidak lagi menyarankan ">> ~/.ssh/config" (blok di
	// bawah kalah oleh "Host *"); petunjuk ke stderr, stdout hanya blok.
	errText := h.stderr.String()
	if !strings.Contains(errText, `DI ATAS semua blok "Host *"`) || !strings.Contains(errText, "easyrun ssh-config contoh-box.easyrun.io --write") {
		t.Fatalf("petunjuk ke stderr: %q", errText)
	}
	if strings.Contains(errText+out, ">> ~/.ssh/config") {
		t.Fatalf("masih menyarankan >> ~/.ssh/config: %q", errText)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".ssh")); err == nil {
		t.Fatal("tanpa --write tidak boleh menulis ~/.ssh")
	}
	// Alias mengikuti yang diketik.
	if h.run("ssh-config", "Uji-Box"); !strings.Contains(h.stdout.String(), "\nHost uji-box\n") {
		t.Fatalf("alias nama:\n%s", h.stdout)
	}
	// ssh_available=false: blok tetap dibuat, dengan peringatan (panel yang
	// menolak sambungan beserta alasannya).
	if code := h.run("ssh-config", "web-lama"); code != 0 || !strings.Contains(h.stdout.String(), "\nHost web-lama\n") ||
		!strings.Contains(h.stderr.String(), "peringatan: panel saat ini menandai SSH belum tersedia untuk box web-lama (status: ready)") {
		t.Fatalf("ssh_available=false: kode %d\n%s\n%s", code, h.stdout, h.stderr)
	}
}

func TestExePathMemakaiPathDiPATH(t *testing.T) {
	h := newHarness(t)
	link := h.exe + "-link"
	if err := os.Symlink(h.exe, link); err != nil {
		t.Skip("symlink tidak didukung")
	}
	h.a.Executable = func() (string, error) { return h.exe, nil }
	h.a.LookPath = func(string) (string, error) { return link, nil }
	if got, _ := h.a.exePath(); got != link {
		t.Fatalf("path PATH yang menunjuk berkas sama harus dipakai: %q", got)
	}
	h.a.LookPath = func(string) (string, error) { return "/bin/sh", nil }
	if got, _ := h.a.exePath(); got != h.exe {
		t.Fatalf("easyrun lain di PATH tidak boleh dipakai: %q", got)
	}
}

// ---------- tunnel lewat perintah ----------

func TestTunnelUjungKeUjung(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	h.p.ws = func(c *websocket.Conn, r *http.Request) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"ready"}`))
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				for i, b := range data { // huruf besar ASCII; byte lain apa adanya
					if b >= 'a' && b <= 'z' {
						data[i] = b - 32
					}
				}
				_ = c.WriteMessage(websocket.BinaryMessage, data)
				_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"exit","code":5}`))
				_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				_, _, _ = c.ReadMessage()
				return
			}
		}
	}
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	go func() { _, _ = stdinW.Write([]byte("halo\x00\xff")) }()
	h.a.Stdin = stdinR
	if code := h.run("tunnel", "uji-box"); code != 5 {
		t.Fatalf("kode %d: %s", code, h.stderr)
	}
	if h.stdout.String() != "HALO\x00\xff" {
		t.Fatalf("stdout %q", h.stdout.String())
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr %q", h.stderr)
	}
	hdr := h.p.wsHeaders[0]
	if _, has := hdr["Origin"]; has {
		t.Fatal("Origin terkirim")
	}
	if hdr.Get("User-Agent") != "easyrun/dev" {
		t.Fatalf("User-Agent %q", hdr.Get("User-Agent"))
	}
	if h.p.n("POST /instances/ticket") != 1 {
		t.Fatalf("tiket: %v", h.p.calls)
	}
}

func TestTunnelDitolak(t *testing.T) {
	cases := []struct {
		name   string
		ticket func(http.ResponseWriter) bool
		ws     func(http.ResponseWriter) bool
		want   []string
	}{
		// Kontrak §7b P14: 409 = kalimat panel apa adanya (bukan klaim
		// "aplikasi tanpa SSH"); 429 tiket = batas laju; 429 WebSocket =
		// batas sesi; 503 "SSH belum tersedia" = saklar platform mati.
		{"409", func(w http.ResponseWriter) bool {
			fail(w, 409, "alamat box sedang diganti — SSH bisa dipakai setelah proses selesai")
			return true
		}, nil, []string{"easyrun: meminta tiket SSH: alamat box sedang diganti — SSH bisa dipakai setelah proses selesai\n"}},
		{"409 tanpa pesan", func(w http.ResponseWriter) bool { w.WriteHeader(409); return true }, nil, []string{"HTTP 409"}},
		{"429 batas laju", func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "60")
			fail(w, 429, "terlalu sering membuka SSH — coba lagi sebentar lagi")
			return true
		}, nil, []string{"terlalu sering membuka SSH", "tunggu 60 detik"}},
		{"429 per IP tanpa Retry-After", func(w http.ResponseWriter) bool {
			fail(w, 429, "terlalu banyak percobaan, coba lagi beberapa saat lagi")
			return true
		}, nil, []string{"terlalu banyak percobaan"}},
		// data.reason (panel baru) = sumber utama jenis 429; kalimat hanya
		// cadangan untuk panel lama.
		{"429 batas laju (reason)", func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "60")
			failReason(w, 429, "terlalu sering membuka SSH — coba lagi sebentar lagi", "rate_limited")
			return true
		}, nil, []string{"easyrun: meminta tiket SSH: terlalu sering membuka SSH — coba lagi sebentar lagi (tunggu 60 detik)\n"}},
		{"429 reason rate_limited menang atas kalimat berisi batas sesi", func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "20")
			failReason(w, 429, "batas sesi permintaan tercapai — coba lagi sebentar lagi", "rate_limited")
			return true
		}, nil, []string{"easyrun: meminta tiket SSH: batas sesi permintaan tercapai — coba lagi sebentar lagi (tunggu 20 detik)\n"}},
		{"ws 429 reason rate_limited menang atas kalimat berisi batas sesi", nil, func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "20")
			failReason(w, 429, "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti", "rate_limited")
			return true
		}, []string{"easyrun: membuka saluran SSH: batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti (tunggu 20 detik)\n"}},
		// 401 panggilan API ber-token = sesi CLI; 401 jabat tangan WebSocket
		// = biasanya tiket 30 detik (kalimat panel dipertahankan, ulang dulu).
		{"401", func(w http.ResponseWriter) bool { fail(w, 401, "token dicabut"); return true }, nil,
			[]string{"easyrun: meminta tiket SSH: sesi CLI tidak berlaku (kedaluwarsa atau dicabut) — jalankan: easyrun login\n"}},
		{"ws 401 tiket kedaluwarsa", nil, func(w http.ResponseWriter) bool {
			fail(w, 401, "tiket SSH tidak berlaku atau sudah kedaluwarsa — jalankan perintah easyrun sekali lagi")
			return true
		}, []string{"easyrun: membuka saluran SSH: tiket SSH tidak berlaku atau sudah kedaluwarsa — jalankan perintah easyrun sekali lagi — ulangi perintahnya; bila tetap gagal, jalankan: easyrun login\n"}},
		{"ws 401 token dicabut", nil, func(w http.ResponseWriter) bool {
			fail(w, 401, "token CLI sudah dicabut — jalankan easyrun login")
			return true
		}, []string{"easyrun: membuka saluran SSH: token CLI sudah dicabut — jalankan easyrun login — ulangi perintahnya; bila tetap gagal, jalankan: easyrun login\n"}},
		{"ws 401 kalimat berisi escape", nil, func(w http.ResponseWriter) bool {
			fail(w, 401, "\x1b]0;judul\x07tiket\x1b[31m tidak berlaku")
			return true
		}, []string{"membuka saluran SSH: ]0;judultiket[31m tidak berlaku — ulangi perintahnya"}},
		{"ws 401 tanpa pesan", nil, func(w http.ResponseWriter) bool { w.WriteHeader(401); return true },
			[]string{"easyrun: membuka saluran SSH: tiket sambungan SSH tidak berlaku atau sudah kedaluwarsa — ulangi perintahnya; bila tetap gagal, jalankan: easyrun login\n"}},
		{"503 saklar mati", func(w http.ResponseWriter) bool { fail(w, 503, "SSH belum tersedia"); return true }, nil,
			[]string{"easyrun: meminta tiket SSH: SSH sedang dimatikan di platform\n"}},
		{"503 lain", func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "5")
			fail(w, 503, "layanan SSH sedang dimulai ulang — coba lagi sebentar lagi")
			return true
		}, nil, []string{"layanan SSH sedang dimulai ulang", "5 detik"}},
		{"500", func(w http.ResponseWriter) bool { fail(w, 500, "internal server error"); return true }, nil, []string{"HTTP 500", "rq-uji"}},
		{"ws 403", nil, func(w http.ResponseWriter) bool { fail(w, 403, "asal permintaan tidak diizinkan"); return true },
			[]string{"membuka saluran SSH", "ditolak", "asal permintaan tidak diizinkan"}},
		{"302 tidak diikuti", func(w http.ResponseWriter) bool {
			w.Header().Set("Location", "https://lain.example/curi")
			w.WriteHeader(http.StatusFound)
			return true
		}, nil, []string{"HTTP 302", "lain.example", "tidak diikuti", "periksa alamat API"}},
		{"ws 302 tidak diikuti", nil, func(w http.ResponseWriter) bool {
			w.Header().Set("Location", "https://lain.example/ws")
			w.WriteHeader(http.StatusFound)
			return true
		}, []string{"membuka saluran SSH", "HTTP 302", "tidak diikuti"}},
		{"ws 429 batas sesi", nil, func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "30")
			fail(w, 429, "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti")
			return true
		}, []string{"membuka saluran SSH: batas sesi SSH tercapai (untuk box ini, tenant, atau platform); tutup sesi lain atau coba lagi dalam 30 detik\n"}},
		{"ws 429 batas sesi tanpa Retry-After", nil, func(w http.ResponseWriter) bool {
			fail(w, 429, "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti")
			return true
		}, []string{"tutup sesi lain atau coba lagi nanti\n"}},
		{"ws 429 batas sesi (reason)", nil, func(w http.ResponseWriter) bool {
			w.Header().Set("Retry-After", "30")
			failReason(w, 429, "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti", "session_limit")
			return true
		}, []string{"membuka saluran SSH: batas sesi SSH tercapai (untuk box ini, tenant, atau platform); tutup sesi lain atau coba lagi dalam 30 detik\n"}},
		{"ws 429 batas sesi (reason) walau kalimatnya lain", nil, func(w http.ResponseWriter) bool {
			failReason(w, 429, "terlalu sering membuka SSH — coba lagi sebentar lagi", "session_limit")
			return true
		}, []string{"membuka saluran SSH: batas sesi SSH tercapai (untuk box ini, tenant, atau platform); tutup sesi lain atau coba lagi nanti\n"}},
		{"ws 409", nil, func(w http.ResponseWriter) bool {
			fail(w, 409, "SSH belum bisa dibuka — box belum siap atau sedang diproses")
			return true
		}, []string{"membuka saluran SSH: SSH belum bisa dibuka — box belum siap atau sedang diproses\n"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.loggedIn(t)
			h.p.ticketFail, h.p.wsFail = c.ticket, c.ws
			if code := h.run("tunnel", boxID); code != 1 {
				t.Fatalf("kode %d", code)
			}
			for _, w := range c.want {
				if !strings.Contains(h.stderr.String(), w) {
					t.Errorf("stderr %q tak memuat %q", h.stderr, w)
				}
			}
			// Jenis 429 tidak tertukar, dan 409 tidak menebak alasannya.
			// Penanda batas sesi = kalimat CLI sendiri, bukan kata "batas
			// sesi" (kalimat panel untuk batas laju boleh memuatnya).
			errText := h.stderr.String()
			sessionText := "untuk box ini, tenant, atau platform"
			rateLimited := strings.Contains(c.name, "batas laju") || strings.Contains(c.name, "rate_limited")
			switch {
			case rateLimited && strings.Contains(errText, sessionText),
				!rateLimited && strings.Contains(c.name, "batas sesi") && (strings.Contains(errText, "terlalu sering") || strings.Contains(errText, "tunggu")),
				strings.HasPrefix(c.name, "409") && strings.Contains(errText, "tidak tersedia"),
				strings.HasPrefix(c.name, "ws 401") && strings.Contains(errText, "sesi CLI tidak berlaku"),
				c.name == "401" && strings.Contains(errText, "ulangi perintahnya"),
				strings.ContainsAny(errText, "\x1b\x07"):
				t.Errorf("pesan menyesatkan: %q", errText)
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout harus kosong (aliran SSH): %q", h.stdout)
			}
		})
	}
}

// isSessionLimit: data.reason menang atas kalimat; tanpa reason (panel lama)
// atau reason yang belum dikenal, kalimat yang ditebak.
func TestJenis429ReasonMenangAtasKalimat(t *testing.T) {
	cases := []struct {
		reason, msg string
		want        bool
	}{
		{api.ReasonSessionLimit, "batas sesi SSH tercapai", true},
		{api.ReasonSessionLimit, "too many requests", true},
		{api.ReasonRateLimited, "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti", false},
		{api.ReasonRateLimited, "session limit reached", false},
		{"", "batas sesi SSH tercapai — tutup sesi lain atau coba lagi nanti", true},
		{"", "terlalu sering membuka SSH", false},
		{"alasan_baru", "batas sesi SSH tercapai", true},
		{"alasan_baru", "terlalu sering", false},
	}
	for _, c := range cases {
		if got := isSessionLimit(&api.Error{Status: 429, Reason: c.reason, Message: c.msg}); got != c.want {
			t.Errorf("reason %q kalimat %q → %v, ingin %v", c.reason, c.msg, got, c.want)
		}
	}
}

func TestExplainTidakMembocorkanToken(t *testing.T) {
	for _, err := range []error{
		&api.Error{Status: 401, Message: "x"},
		&api.NetError{Host: "api.easyrun.io", Err: errors.New("dial tcp: i/o timeout")},
		fmt.Errorf("bungkus: %w", &api.Error{Status: 409, Message: "status bukan ready"}),
	} {
		if s := explain(err); strings.Contains(s, testToken) || s == "" {
			t.Fatalf("%q", s)
		}
	}
}

// sshG — `ssh -G` sungguhan: opsi akhir yang dipakai OpenSSH.
func sshG(t *testing.T, args ...string) map[string][]string {
	t.Helper()
	c := exec.Command("ssh", append([]string{"-G"}, args...)...)
	c.Stdin = strings.NewReader("")
	out, err := c.Output()
	if err != nil {
		var ee *exec.ExitError
		stderr := ""
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("ssh -G %q: %v\n%s", args, err, stderr)
	}
	got := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			got[k] = append(got[k], v)
		}
	}
	return got
}

func first(m map[string][]string, k string) string {
	if len(m[k]) == 0 {
		return ""
	}
	return m[k][0]
}

// TestSSHConfigWriteMengalahkanHostBintang — kontrak §7b P7, dibuktikan
// dengan OpenSSH sungguhan (ssh -G) di HOME sementara: ~/.ssh/config yang
// sudah punya `Host *` dengan User root, ProxyJump x, ForwardAgent yes.
//   - cara lama (blok ditempel di bawah, `>>`): Host * MENANG — ini masalahnya;
//   - `ssh-config --write`: blok box menang; host lain tetap memakai Host *.
func TestSSHConfigWriteMengalahkanHostBintang(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil || runtime.GOOS == "windows" {
		t.Skip("butuh OpenSSH unix")
	}
	h := newHarness(t)
	h.loggedIn(t)
	sshDir := filepath.Join(h.home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	userCfg := filepath.Join(sshDir, "config")
	orig := "# konfigurasi pengguna\nHost *\n    User root\n    ProxyJump x\n    ForwardAgent yes\n    ForwardX11 yes\n"
	if err := os.WriteFile(userCfg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Kontrol: blok di bawah Host * (saran lama ">> ~/.ssh/config").
	if code := h.run("ssh-config", "contoh-box"); code != 0 {
		t.Fatalf("ssh-config: %d %s", code, h.stderr)
	}
	appended := filepath.Join(t.TempDir(), "config-lama")
	_ = os.WriteFile(appended, []byte(orig+"\n"+h.stdout.String()), 0o600)
	if got := sshG(t, "-F", appended, "contoh-box"); first(got, "user") != "root" || first(got, "forwardagent") != "yes" {
		t.Fatalf("kontrol tidak menunjukkan masalahnya (user %q forwardagent %q)", first(got, "user"), first(got, "forwardagent"))
	}

	// --write
	if code := h.run("ssh-config", "contoh-box", "--write"); code != 0 {
		t.Fatalf("--write: %d %s", code, h.stderr)
	}
	conf := filepath.Join(sshDir, "easyrun", boxID+".conf")
	confText := readText(t, conf)
	if !strings.Contains(confText, "\nHost contoh-box\n") || !strings.Contains(confText, "    ForwardAgent no\n") {
		t.Fatalf("berkas box:\n%s", confText)
	}
	if fi, _ := os.Stat(conf); fi.Mode().Perm() != 0o600 {
		t.Fatalf("berkas box %04o", fi.Mode().Perm())
	}
	if got := readText(t, userCfg); got != "Include ~/.ssh/easyrun/*.conf\n"+orig {
		t.Fatalf("~/.ssh/config harus = satu baris Include + isi lama persis:\n%q", got)
	}
	if fi, _ := os.Stat(userCfg); fi.Mode().Perm() != 0o644 {
		t.Fatalf("izin ~/.ssh/config berubah: %04o", fi.Mode().Perm())
	}
	if got := readText(t, userCfg+".easyrun.bak"); got != orig {
		t.Fatalf("cadangan: %q", got)
	}
	for _, want := range []string{"Ditulis: " + conf, "Ditambahkan di baris 1", "Cadangan isi lama", "ssh contoh-box"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("stdout tak memuat %q:\n%s", want, h.stdout)
		}
	}

	// OpenSSH mengembangkan "~" dari passwd (bukan $HOME), jadi untuk
	// membaca HOME uji, "~/" di baris Include diganti path HOME uji pada
	// SALINAN berkas; isi lainnya persis yang ditulis --write.
	written := readText(t, userCfg)
	probe := filepath.Join(sshDir, "config.uji")
	_ = os.WriteFile(probe, []byte(strings.Replace(written, "Include ~/", "Include "+h.home+"/", 1)), 0o600)
	got := sshG(t, "-F", probe, "contoh-box")
	want := map[string]string{
		"user":         "ubuntu",
		"hostname":     "contoh-box.easyrun.io",
		"forwardagent": "no",
		"forwardx11":   "no",
		"proxycommand": h.invocation() + " tunnel " + boxID,
	}
	for k, v := range want {
		if first(got, k) != v {
			t.Errorf("%s = %q, ingin %q", k, got[k], v)
		}
	}
	if pj := first(got, "proxyjump"); pj != "" && pj != "none" {
		t.Errorf("ProxyJump dari Host * masih menang: %q", pj)
	}
	// Host lain tidak tersentuh: tetap memakai Host * pengguna.
	if other := sshG(t, "-F", probe, "server-lain.example"); first(other, "user") != "root" || first(other, "forwardagent") != "yes" {
		t.Errorf("host lain berubah: user %q forwardagent %q", first(other, "user"), first(other, "forwardagent"))
	}

	// Idempoten: --write lagi tidak mengubah ~/.ssh/config maupun cadangan.
	if code := h.run("ssh-config", "contoh-box", "-w"); code != 0 {
		t.Fatalf("--write kedua: %d %s", code, h.stderr)
	}
	if again := readText(t, userCfg); again != written {
		t.Fatalf("--write kedua mengubah ~/.ssh/config:\n%q", again)
	}
	if !strings.Contains(h.stdout.String(), "Diperbarui: "+conf) || !strings.Contains(h.stdout.String(), "tidak diubah") {
		t.Fatalf("stdout kedua:\n%s", h.stdout)
	}
	if got := readText(t, userCfg+".easyrun.bak"); got != orig {
		t.Fatalf("cadangan tertimpa: %q", got)
	}
}

func TestSSHConfigWriteTanpaConfigDanOpsiSalah(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	// ~/.ssh belum ada sama sekali.
	if code := h.run("ssh-config", "--write", "uji-box"); code != 0 {
		t.Fatalf("kode %d %s", code, h.stderr)
	}
	cfg := filepath.Join(h.home, ".ssh", "config")
	if got := readText(t, cfg); got != "Include ~/.ssh/easyrun/*.conf\n" {
		t.Fatalf("%q", got)
	}
	if runtime.GOOS != "windows" {
		for p, m := range map[string]os.FileMode{cfg: 0o600, filepath.Dir(cfg): 0o700, filepath.Join(h.home, ".ssh", "easyrun"): 0o700} {
			if fi, _ := os.Stat(p); fi.Mode().Perm() != m {
				t.Errorf("%s %04o, ingin %04o", p, fi.Mode().Perm(), m)
			}
		}
	}
	if _, err := os.Stat(cfg + ".easyrun.bak"); err == nil {
		t.Fatal("cadangan dibuat untuk berkas yang belum ada")
	}
	if code := h.run("ssh-config", "uji-box", "--tulis"); code != 2 || !strings.Contains(h.stderr.String(), "opsi tidak dikenal") {
		t.Fatalf("opsi salah: %d %s", code, h.stderr)
	}
	if code := h.run("ssh-config", "--write"); code != 2 {
		t.Fatalf("tanpa box: %d", code)
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
