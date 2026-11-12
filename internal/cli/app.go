// Package cli — perintah `easyrun` (kontrak SSH box §6). Semua ketergantungan
// ke dunia luar (HTTP, jam, browser, exec ssh, stdin/stdout) bisa diganti
// lewat App supaya bisa diuji tanpa server sungguhan.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/version"
)

// EnvNoBrowser — isi apa pun supaya `easyrun login` tidak membuka browser.
const EnvNoBrowser = "EASYRUN_NO_BROWSER"

// App — satu kali jalan program.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer // untuk `tunnel`: aliran SSH, tidak boleh ada teks lain
	Stderr io.Writer

	Getenv     func(string) string
	Environ    func() []string
	Now        func() time.Time
	HTTP       *http.Client
	Sleep      func(context.Context, time.Duration) error
	Hostname   func() (string, error)
	Executable func() (string, error)
	LookPath   func(string) (string, error)
	// HomeDir — folder home ($HOME) untuk ~/.easyrun; nil = os.UserHomeDir.
	HomeDir func() (string, error)
	// SSHHomeDir — folder home yang dipakai OpenSSH untuk ~/.ssh/config dan
	// "~" di Include: home akun (passwd), bukan $HOME. nil = HomeDir.
	SSHHomeDir func() (string, error)
	// OpenBrowser — buka URL (sudah divalidasi https) di browser.
	OpenBrowser func(string) error
	// ExecSSH — jalankan ssh. Unix: menggantikan proses (tidak kembali bila
	// berhasil). Windows: proses anak, kembalikan kode keluarnya.
	ExecSSH func(path string, argv, env []string) (int, error)
	// PingTicks — sumber detak ping tunnel (uji); nil = 25 dtk.
	PingTicks <-chan time.Time
	// TunnelReadyTimeout, TunnelCloseGrace — uji; 0 = bawaan.
	TunnelReadyTimeout time.Duration
	TunnelCloseGrace   time.Duration
	// GOOS — sistem tujuan untuk cara mengutip path; bawaan runtime.GOOS.
	GOOS string

	// sshUser — hanya untuk uji ujung-ke-ujung dengan sshd lokal (sshd
	// non-root hanya melayani pengguna yang menjalankannya); "" = ubuntu.
	sshUser string

	// configDirOpt, apiURLOpt — opsi global --config-dir / --api-url (sebelum
	// nama perintah). Blok ssh-config membawanya ke ProxyCommand dan Match
	// exec, karena ssh/VS Code tidak selalu mewarisi env shell.
	configDirOpt string
	apiURLOpt    string
}

// New — App dengan ketergantungan sungguhan.
func New(stdin io.Reader, stdout, stderr io.Writer) *App {
	a := &App{
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
		Getenv:     os.Getenv,
		Environ:    os.Environ,
		Now:        time.Now,
		HTTP:       api.NewHTTPClient(),
		Sleep:      sleepCtx,
		Hostname:   os.Hostname,
		Executable: os.Executable,
		LookPath:   exec.LookPath,
		HomeDir:    os.UserHomeDir,
		SSHHomeDir: accountHomeDir,
		ExecSSH:    execSSH,
		GOOS:       runtime.GOOS,
	}
	a.OpenBrowser = a.openBrowser
	return a
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func userAgent() string { return "easyrun/" + version.Version }

type command struct {
	name  string
	usage string
	help  string
	more  string // keterangan tambahan untuk `easyrun <perintah> --help`
	run   func(a *App, ctx context.Context, args []string) int
}

var commands []command

func init() {
	commands = []command{
		{"login", "login [--api-url URL]", "masuk lewat browser (kode perangkat)",
			"  --api-url URL   alamat API panel; disimpan ke config.json (bawaan " + config.DefaultAPIURL + ")\n  --no-browser    jangan buka browser, cukup cetak tautannya\n", (*App).cmdLogin},
		{"logout", "logout", "keluar dan cabut token CLI di server", "", (*App).cmdLogout},
		{"boxes", "boxes", "daftar box yang bisa kamu akses", "", (*App).cmdBoxes},
		{"ssh", "ssh <box> [-- argumen ssh…]", "sambung SSH ke box",
			"  Sertifikat diperbarui otomatis bila sisa umurnya < 5 menit. Argumen setelah --\n  diteruskan ke ssh apa adanya, mis. easyrun ssh uji-box -- -L 8080:localhost:8080\n", (*App).cmdSSH},
		{"ssh-config", "ssh-config <box> [--write]", "blok ~/.ssh/config untuk box",
			"  Tanpa --write: cetak blok; taruh di ~/.ssh/config DI ATAS semua \"Host *\".\n  --write   tulis ~/.ssh/easyrun/<id>.conf dan satu baris Include di puncak\n            ~/.ssh/config (cadangan sekali: ~/.ssh/config.easyrun.bak).\n  Setelahnya: ssh <box>, scp, rsync, sftp, dan VS Code Remote-SSH langsung jalan.\n", (*App).cmdSSHConfig},
		{"ssh-prepare", "ssh-prepare <box>", "perbarui sertifikat SSH bila perlu", "  Dipanggil otomatis oleh blok Match exec dari ssh-config.\n", (*App).cmdSSHPrepare},
		{"tunnel", "tunnel <box>", "saluran SSH lewat HTTPS (ProxyCommand)", "  Dipanggil ssh sebagai ProxyCommand; stdin/stdout = aliran SSH. Bukan untuk diketik langsung.\n", (*App).cmdTunnel},
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "easyrun %s — program baris perintah EasyRun\n\nPemakaian:\n", version.Version)
	for _, c := range commands {
		fmt.Fprintf(w, "  easyrun %-30s %s\n", c.usage, c.help)
	}
	fmt.Fprintf(w, "  easyrun %-30s %s\n", "--version", "tampilkan versi")
	fmt.Fprintf(w, "  easyrun %-30s %s\n", "--self-update", "perbarui ke versi terbaru")
	fmt.Fprintf(w, "  easyrun %-30s %s\n", "--help", "tampilkan bantuan ini")
	fmt.Fprintf(w, `
<box> = nama, hostname, atau id dari `+"`easyrun boxes`"+`.

Contoh:
  easyrun login
  easyrun ssh uji-box
  easyrun ssh uji-box -- -L 8080:localhost:8080
  easyrun ssh-config uji-box --write   lalu: ssh uji-box, scp, VS Code

Opsi global (sebelum nama perintah):
  --config-dir DIR   folder konfigurasi (menggantikan %s)
  --api-url URL      alamat API panel (menggantikan %s)

Env: %s (alamat API, bawaan %s), %s (folder, bawaan ~/.easyrun), %s
`, config.EnvConfigDir, config.EnvAPIURL, config.EnvAPIURL, config.DefaultAPIURL, config.EnvConfigDir, EnvNoBrowser)
}

// Run — jalankan satu perintah; kembalikan kode keluar.
func (a *App) Run(ctx context.Context, args []string) int {
	if a.GOOS == "" {
		a.GOOS = runtime.GOOS
	}
	args, code, ok := a.globalOptions(args)
	if !ok {
		return code
	}
	if len(args) == 0 {
		a.usage(a.Stdout)
		return 0
	}
	switch args[0] {
	case "--version", "-v", "version":
		fmt.Fprintln(a.Stdout, "easyrun", version.Version)
		return 0
	case "--help", "-h", "help":
		a.usage(a.Stdout)
		return 0
	case "--self-update", "self-update":
		return a.selfUpdate()
	}
	for _, c := range commands {
		if c.name == args[0] {
			rest := args[1:]
			if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
				fmt.Fprintf(a.Stdout, "Pemakaian: easyrun %s\n  %s\n%s", c.usage, c.help, c.more)
				return 0
			}
			return c.run(a, ctx, rest)
		}
	}
	return a.unknown(args[0])
}

// globalOptions — ambil --config-dir / --api-url di depan nama perintah
// (bentuk "--opsi nilai" atau "--opsi=nilai"). ok=false → keluar dengan code.
func (a *App) globalOptions(args []string) (rest []string, code int, ok bool) {
	for len(args) > 0 {
		name, val, hasVal := strings.Cut(args[0], "=")
		var dst *string
		switch name {
		case "--config-dir":
			dst = &a.configDirOpt
		case "--api-url":
			dst = &a.apiURLOpt
		default:
			return args, 0, true
		}
		if !hasVal {
			if len(args) < 2 {
				fmt.Fprintf(a.Stderr, "easyrun: opsi %s butuh nilai\n", name)
				return nil, 2, false
			}
			val, args = args[1], args[2:]
		} else {
			args = args[1:]
		}
		if strings.TrimSpace(val) == "" {
			fmt.Fprintf(a.Stderr, "easyrun: opsi %s tidak boleh kosong\n", name)
			return nil, 2, false
		}
		*dst = val
	}
	return args, 0, true
}

// unknown — kontrak §6: perintah yang belum ada → "belum tersedia" (bisa jadi
// instruksi dari panel yang lebih baru dari program ini), bukan "tidak dikenal".
func (a *App) unknown(name string) int {
	kind := "perintah"
	if strings.HasPrefix(name, "-") {
		kind = "opsi"
	}
	fmt.Fprintf(a.Stderr, "easyrun: %s %q belum tersedia di easyrun %s.\n", kind, api.Clean(name), version.Version)
	if s := suggest(name); s != "" {
		fmt.Fprintf(a.Stderr, "Mungkin maksudmu: easyrun %s\n", s)
	}
	fmt.Fprintln(a.Stderr, "Lihat `easyrun --help`, atau perbarui dengan `easyrun --self-update`.")
	return 2
}

func suggest(name string) string {
	best, bestD := "", 3
	for _, c := range commands {
		if d := editDistance(name, c.name); d < bestD {
			best, bestD = c.name, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func (a *App) usageError(c string, msg string) int {
	for _, cmd := range commands {
		if cmd.name == c {
			fmt.Fprintf(a.Stderr, "easyrun: %s\nPemakaian: easyrun %s\n", msg, cmd.usage)
			return 2
		}
	}
	fmt.Fprintf(a.Stderr, "easyrun: %s\n", msg)
	return 2
}

// dir — folder konfigurasi, dibuat bila belum ada. Urutan: --config-dir →
// EASYRUN_CONFIG_DIR → ~/.easyrun.
func (a *App) dir() (config.Dir, error) {
	var p string
	var err error
	if a.configDirOpt != "" {
		p, err = filepath.Abs(a.configDirOpt)
	} else {
		p, err = config.Locate(a.Getenv, a.HomeDir)
	}
	if err != nil {
		return config.Dir{}, err
	}
	d := config.Dir{Path: p, Warn: a.Stderr}
	return d, d.Ensure()
}

// ---------- sesi ----------

type otherServerError struct{ have, want string }

func (e *otherServerError) Error() string {
	return fmt.Sprintf("token tersimpan untuk %s, sedangkan alamat API sekarang %s — jalankan: easyrun login", e.have, e.want)
}

type expiredError struct{ at time.Time }

func (e *expiredError) Error() string {
	return fmt.Sprintf("token CLI kedaluwarsa sejak %s — jalankan: easyrun login", e.at.Local().Format("2006-01-02 15:04 MST"))
}

// session — klien ber-token untuk alamat API aktif. Token tidak pernah
// dikirim ke server selain yang menerbitkannya.
func (a *App) session(d config.Dir) (*api.Client, error) {
	cfg, err := d.LoadConfig()
	if err != nil {
		return nil, err
	}
	base, err := config.ResolveAPIURL(a.apiURLOpt, a.Getenv, cfg)
	if err != nil {
		return nil, err
	}
	creds, err := d.LoadCredentials(false)
	if err != nil {
		return nil, err
	}
	have, err := config.NormalizeAPIURL(creds.APIURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.CredentialsPath(), err)
	}
	if have != base {
		return nil, &otherServerError{have: have, want: base}
	}
	if !creds.ExpiresAt.IsZero() && !a.Now().Before(creds.ExpiresAt) {
		return nil, &expiredError{at: creds.ExpiresAt}
	}
	return &api.Client{BaseURL: base, Token: creds.AccessToken, UserAgent: userAgent(), HTTP: a.HTTP}, nil
}

// ---------- pesan galat ----------

// fail — cetak "easyrun: <apa>: <penjelasan>" ke stderr, kembalikan 1.
func (a *App) fail(what string, err error) int {
	if what != "" {
		fmt.Fprintf(a.Stderr, "easyrun: %s: %s\n", what, explain(err))
	} else {
		fmt.Fprintf(a.Stderr, "easyrun: %s\n", explain(err))
	}
	return 1
}

// explain — kalimat yang bisa ditindaklanjuti untuk setiap jenis galat.
func explain(err error) string {
	var pe *config.PermError
	var ae *api.Error
	var ne *api.NetError
	var re *api.RedirectError
	switch {
	case errors.Is(err, config.ErrNotLoggedIn):
		return "belum masuk — jalankan: easyrun login"
	case errors.As(err, &pe):
		if filepath.Base(pe.Path) == "credentials.json" {
			return pe.Error() + ". Token mungkin sudah terbaca pengguna lain: jalankan `chmod 600 " + pe.Path + "`, lalu disarankan `easyrun logout` dan `easyrun login` lagi"
		}
		return err.Error()
	case errors.Is(err, context.Canceled):
		return "dibatalkan"
	case errors.As(err, &ne):
		return ne.Error() + " — periksa koneksi internet (alamat API bisa diganti lewat " + config.EnvAPIURL + ")"
	case errors.As(err, &re):
		return re.Error() + " — periksa alamat API (--api-url atau " + config.EnvAPIURL + "; bawaan " + config.DefaultAPIURL + ")"
	case errors.As(err, &ae):
		return explainAPI(ae)
	}
	return err.Error()
}

func explainAPI(e *api.Error) string {
	msg := e.Message
	with := func(base string) string {
		if msg != "" && !strings.EqualFold(msg, base) {
			return base + ": " + msg
		}
		return base
	}
	secs := int(e.RetryAfter.Round(time.Second) / time.Second)
	retry := ""
	if secs > 0 {
		retry = fmt.Sprintf(" — coba lagi dalam %d detik", secs)
	}
	switch {
	case e.Status == http.StatusUnauthorized && e.Handshake:
		// Jabat tangan WebSocket memakai tiket sekali pakai 30 detik, bukan
		// token: 401 di sini biasanya tiket kedaluwarsa/terpakai ("tiket SSH
		// tidak berlaku …"), lebih jarang token dicabut atau akses tenant
		// berubah tepat sesudah tiket diminta ("token CLI sudah dicabut …").
		// Panel yang tahu bedanya, jadi kalimatnya ditampilkan; mengulang
		// perintah meminta tiket baru, dan bila token memang tak berlaku,
		// permintaan tiket itu yang menyuruh login.
		if msg == "" {
			msg = "tiket sambungan SSH tidak berlaku atau sudah kedaluwarsa"
		}
		return msg + " — ulangi perintahnya; bila tetap gagal, jalankan: easyrun login"
	case e.Status == http.StatusUnauthorized:
		return "sesi CLI tidak berlaku (kedaluwarsa atau dicabut) — jalankan: easyrun login"
	case e.Status == http.StatusForbidden:
		return with("ditolak")
	case e.Status == http.StatusNotFound:
		return with("tidak ditemukan, atau kamu bukan anggota tenant pemiliknya")
	case e.Status == http.StatusConflict:
		// Kontrak §7b P14: panel menyebut alasannya sendiri (aplikasi tanpa
		// SSH, box belum siap, alamat sedang diganti, sedang diproses) —
		// diteruskan apa adanya, tanpa tebakan dari sisi CLI.
		if msg != "" {
			return msg
		}
		return "box belum bisa disambung saat ini (HTTP 409)"
	case e.Status == http.StatusTooManyRequests:
		if isSessionLimit(e) {
			// Panel tidak membedakan batas per box / tenant / platform.
			s := "batas sesi SSH tercapai (untuk box ini, tenant, atau platform); tutup sesi lain atau coba lagi "
			if secs > 0 {
				return s + fmt.Sprintf("dalam %d detik", secs)
			}
			return s + "nanti"
		}
		// Batas laju (terlalu sering meminta sertifikat/tiket, atau pembatas
		// per IP/token): kalimat panel sudah menyebutnya.
		if msg == "" {
			msg = "terlalu sering mengirim permintaan — coba lagi sebentar lagi"
		}
		if secs > 0 {
			return fmt.Sprintf("%s (tunggu %d detik)", msg, secs)
		}
		return msg
	case e.Status == http.StatusServiceUnavailable:
		if isSSHDisabled(e) {
			return "SSH sedang dimatikan di platform" + retry
		}
		if msg == "" {
			msg = "layanan belum tersedia"
		}
		return msg + retry
	case e.Status >= 500:
		s := fmt.Sprintf("server EasyRun sedang bermasalah (HTTP %d)", e.Status)
		if e.RequestID != "" {
			s += ", request_id " + e.RequestID
		}
		return s + retry
	}
	return e.Error()
}

// isSessionLimit — 429 karena batas jumlah sesi SSH bersamaan (orchestrator,
// diteruskan panel saat membuka WebSocket), bukan batas laju permintaan.
//
// Sumber utama: `data.reason` panel (session_limit | rate_limited); bila ada,
// reason menang atas kalimat — kalimat bisa berubah atau menyebut "batas sesi"
// untuk hal lain. Panel lama hanya mengirim kalimat ("batas sesi SSH tercapai
// …"), jadi tanpa reason (atau reason yang belum dikenal) kalimatnya ditebak.
func isSessionLimit(e *api.Error) bool {
	switch e.Reason {
	case api.ReasonSessionLimit:
		return true
	case api.ReasonRateLimited:
		return false
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "batas sesi") || strings.Contains(m, "session limit")
}

// sshDisabledMessage — jawaban 503 panel saat saklar SSH_ENABLED=false.
const sshDisabledMessage = "SSH belum tersedia"

func isSSHDisabled(e *api.Error) bool {
	return strings.EqualFold(e.Code, "ssh_disabled") ||
		strings.EqualFold(strings.TrimRight(e.Message, ". "), sshDisabledMessage)
}
