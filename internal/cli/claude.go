package cli

// `easyrun claude …` — Claude Code "otak" berjalan di EasyBox (pane herdr,
// login Claude di box), berkas & perintah dieksekusi di laptop ini lewat MCP
// (kontrak P2, brief 3b7f1d58):
//
//	claude di box ⇄ easybox-bridge (soket unix) ⇄ easybox-hand-connect
//	  ⇄ ssh exec (satu ControlMaster) ⇄ PENJAGA (internal/claudehand)
//	  ⇄ `claude mcp serve` lokal di cwd laptop (tanpa login)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/claudehand"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/sshconf"
	"github.com/EasyRunTech/easyrun-cli/internal/version"
)

const (
	// claudeDefaultSession — sesi herdr bawaan (kontrak P2 "Nama").
	claudeDefaultSession = "default"
	// easyboxProtocol — versi `easybox_protocol` yang dipahami program ini.
	easyboxProtocol = 1
	// handConnectWait — `easybox-hand-connect --wait`: detik menunggu soket
	// jembatan (claude di pane masih menyala).
	handConnectWait = 60
	// claudeInstallCmd — installer resmi Claude Code (kontrak P2: tidak
	// dibundel di image; dipasang pengguna).
	claudeInstallCmd = "curl -fsSL https://claude.ai/install.sh | bash"

	claudeAuditFile = "claude-audit.log"
	claudeHandDir   = "claude-hand"

	boxCmdTimeout    = 60 * time.Second
	ensureCmdTimeout = 120 * time.Second
	claudeProbeLimit = 20 * time.Second
	handReadyWait    = 5 * time.Second
	handBackoffMax   = 30 * time.Second
	// handStableAfter — sambungan yang bertahan selama ini dianggap sehat:
	// jeda sambung ulang kembali ke 1 detik.
	handStableAfter = 30 * time.Second
)

// paneNameRe — <pane> dan <sesi> (kontrak P2 "Nama" + Amandemen 6): karakter
// pertama huruf/angka (menolak "-h", ".", "..", ".x"), sisanya huruf/angka/._-,
// total 1–40, dicocokkan UTUH (Go tanpa (?m): "$" = akhir teks). Ikut masuk
// perintah jarak jauh, jadi hanya karakter yang aman tanpa kutip.
var paneNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

const claudeMore = `
  easyrun claude <box> ls
      daftar pane claude di box: sesi, jalan?, laptop yang tersambung
  easyrun claude <box> [<sesi>:]<pane> attach [--read-only] [--allow-home]
      buka pane claude (herdr) di terminal ini; berkas & perintahnya
      dijalankan di folder ini lewat penjaga laptop. Keluar/lepas dari
      herdr = tangan dilepas, sambungan ditutup, ringkasan dicetak.
  easyrun claude <box> [<sesi>:]<pane> hand [--read-only] [--allow-home]
      tangan saja (setiap aksi dicetak di sini) — untuk yang melihat pane
      lewat Konsol di browser. Ctrl-C = lepas. Putus = disambung ulang
      otomatis (sertifikat diperbarui bila perlu), kecuali pane sudah
      dipegang tangan lain atau autentikasi SSH ditolak berulang.
  easyrun claude <box> <pane> stop
      hentikan claude di pane itu

  <pane>, <sesi>: diawali huruf/angka, sisanya huruf/angka/._- (maks 40);
                 sesi bawaan "default" (= sesi yang dibuka Konsol mode herdr).
  --read-only    box hanya boleh Read (tanpa Bash/Edit/Write)
  --allow-home   izinkan folder home sebagai root (tidak disarankan)

  Root = folder tempat perintah dijalankan (bukan / atau home). Penjaga
  laptop hanya mengizinkan Bash/Read/Edit/Write; path berkas wajib di dalam
  root (symlink diikuti), folder rahasia (~/.ssh, ~/.aws, ~/.easyrun,
  ~/.claude, …) ditolak, Bash latar ditolak. Perintah Bash TIDAK dibatasi
  ke root: pakai --read-only bila ragu. Setiap aksi (path/perintah, tanpa
  isi berkas) dicatat di <folder config>/claude-audit.log.

  Butuh: Claude Code di laptop (tanpa login) dan di box (sudah login), box
  versi terbaru. Satu sambungan SSH (ControlMaster) dipakai bersama untuk
  semua saluran, jadi dihitung satu sesi SSH box.
`

type claudeArgs struct {
	box, session, pane, action string
	readOnly, allowHome        bool
}

func parseClaudeArgs(args []string) (claudeArgs, error) {
	var ca claudeArgs
	var pos []string
	for _, a := range args {
		switch a {
		case "--read-only":
			ca.readOnly = true
		case "--allow-home":
			ca.allowHome = true
		default:
			if strings.HasPrefix(a, "-") {
				return ca, fmt.Errorf("opsi tidak dikenal: %s", api.Clean(a))
			}
			pos = append(pos, a)
		}
	}
	switch {
	case len(pos) == 0:
		return ca, errors.New("nama box belum diisi")
	case len(pos) == 1:
		return ca, errors.New("aksi belum diisi: ls, atau <pane> attach|hand|stop")
	case len(pos) == 2 && pos[1] == "ls":
		ca.box, ca.action = pos[0], "ls"
	case len(pos) == 2:
		return ca, fmt.Errorf("aksi untuk pane %s belum diisi: attach, hand, atau stop", api.Clean(pos[1]))
	case len(pos) == 3:
		ca.box, ca.action = pos[0], pos[2]
		ca.session, ca.pane = claudeDefaultSession, pos[1]
		if s, p, ok := strings.Cut(pos[1], ":"); ok {
			ca.session, ca.pane = s, p
		}
		if !paneNameRe.MatchString(ca.session) || !paneNameRe.MatchString(ca.pane) {
			return ca, fmt.Errorf("nama %q tidak valid: [<sesi>:]<pane> — tiap nama diawali huruf/angka, sisanya huruf/angka/._-, maks 40 karakter", api.Clean(pos[1]))
		}
	default:
		return ca, errors.New("argumen terlalu banyak")
	}
	switch ca.action {
	case "ls", "stop":
		if ca.readOnly || ca.allowHome {
			return ca, errors.New("--read-only/--allow-home hanya untuk attach dan hand")
		}
	case "attach", "hand":
	default:
		return ca, fmt.Errorf("aksi %q tidak dikenal: ls, attach, hand, atau stop", api.Clean(ca.action))
	}
	if err := sshconf.ValidQuery(ca.box); err != nil {
		return ca, err
	}
	return ca, nil
}

func (ca claudeArgs) target() string { return ca.session + ":" + ca.pane }

// ---------- perintah ----------

func (a *App) cmdClaude(ctx context.Context, args []string) int {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return a.Run(ctx, []string{"claude", "--help"})
		}
	}
	ca, err := parseClaudeArgs(args)
	if err != nil {
		return a.usageError("claude", err.Error())
	}
	hands := ca.action == "attach" || ca.action == "hand"
	if hands && a.GOOS == "windows" {
		return a.fail("", errors.New("`easyrun claude … attach/hand` belum didukung di Windows (OpenSSH Windows tidak punya ControlMaster); pakai macOS/Linux/WSL"))
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	var local *claudeLocal
	if hands {
		// Pemeriksaan lokal dulu: gagal di sini tidak menyentuh jaringan.
		if local, err = a.claudePreflight(ca, d); err != nil {
			return a.fail("", err)
		}
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	b, err := a.resolveBox(ctx, c, ca.box)
	if err != nil {
		return a.fail("", err)
	}
	if _, err := a.ensureCert(ctx, d, func() (*api.Client, error) { return c, nil }, b.ID); err != nil {
		return a.fail("", err)
	}
	conn, err := a.newBoxConn(d, ca.box, b)
	if err != nil {
		return a.fail("", err)
	}
	switch ca.action {
	case "ls":
		return a.claudeLs(ctx, conn, ca)
	case "stop":
		return a.claudeStop(ctx, conn, ca)
	}
	return a.claudeHands(ctx, conn, d, ca, local, b)
}

// claudeLocal — hasil pemeriksaan laptop.
type claudeLocal struct {
	claude  string // path claude lokal
	version string
	policy  *claudehand.Policy
	handCfg string // CLAUDE_CONFIG_DIR untuk `claude mcp serve`
}

func (a *App) getwd() (string, error) {
	if a.Getwd != nil {
		return a.Getwd()
	}
	return os.Getwd()
}

func (a *App) stdinIsTerminal() bool {
	if a.StdinIsTerminal != nil {
		return a.StdinIsTerminal()
	}
	f, ok := a.Stdin.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// claudePreflight — root sah, `claude` lokal ada dan `claude mcp serve`
// jalan, terminal ada (attach).
func (a *App) claudePreflight(ca claudeArgs, d config.Dir) (*claudeLocal, error) {
	cwd, err := a.getwd()
	if err != nil {
		return nil, fmt.Errorf("folder kerja tidak terbaca: %w", err)
	}
	home, err := a.homeDir()
	if err != nil {
		return nil, fmt.Errorf("folder home tidak ditemukan: %w", err)
	}
	handCfg := filepath.Join(d.Path, claudeHandDir)
	pol, err := claudehand.NewPolicy(claudehand.PolicyConfig{
		Root: cwd, Home: home, ReadOnly: ca.readOnly, AllowHome: ca.allowHome,
		ExtraSecrets: []string{d.Path, handCfg, a.Getenv(claudehand.EnvConfigDir)},
	})
	if err != nil {
		return nil, err
	}
	if ca.action == "attach" && !a.stdinIsTerminal() {
		return nil, fmt.Errorf("attach butuh terminal interaktif; tanpa terminal pakai: easyrun claude %s %s hand", ca.box, ca.target())
	}
	claude, err := a.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("Claude Code tidak ditemukan di laptop ini (dibutuhkan untuk menjalankan berkas & perintah di sini; tidak perlu login) — pasang: %s", claudeInstallCmd)
	}
	env := a.environ()
	ctx, cancel := context.WithTimeout(context.Background(), claudeProbeLimit)
	defer cancel()
	probe := exec.CommandContext(ctx, claude, "mcp", "serve", "--help")
	probe.Env = env
	if out, err := probe.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("`claude mcp serve` tidak jalan di laptop ini (%v): %s — perbarui Claude Code (%s)", err, api.Clean(string(out)), claudeInstallCmd)
	}
	vctx, vcancel := context.WithTimeout(context.Background(), claudeProbeLimit)
	defer vcancel()
	vc := exec.CommandContext(vctx, claude, "--version")
	vc.Env = env
	out, _ := vc.Output()
	if err := os.MkdirAll(handCfg, 0o700); err != nil {
		return nil, fmt.Errorf("folder %s: %w", handCfg, err)
	}
	return &claudeLocal{claude: claude, version: api.Clean(string(out)), policy: pol, handCfg: handCfg}, nil
}

func (a *App) environ() []string {
	if a.Environ != nil {
		return a.Environ()
	}
	return os.Environ()
}

// ---------- sambungan ke box ----------

// boxConn — cara menjalankan perintah di box lewat ssh sistem.
type boxConn struct {
	a       *App
	sshPath string
	env     []string
	base    []string // "ssh" + opsi easyrun (tanpa tujuan)
	host    string
	mux     *sshMux // nil = satu sambungan per perintah
}

func (a *App) newBoxConn(d config.Dir, alias string, b api.Box) (*boxConn, error) {
	p, err := a.sshParams(d, alias, b)
	if err != nil {
		return nil, err
	}
	argv, err := sshconf.ArgvWith(p, nil, nil)
	if err != nil {
		return nil, err
	}
	sshPath, err := a.LookPath("ssh")
	if err != nil {
		return nil, errors.New("program ssh (OpenSSH) tidak ditemukan di PATH — pasang dulu: macOS sudah menyertakannya; Debian/Ubuntu: sudo apt install openssh-client")
	}
	return &boxConn{a: a, sshPath: sshPath, env: a.environ(), base: argv[:len(argv)-1], host: argv[len(argv)-1]}, nil
}

// remote — perintah jarak jauh (nama sudah divalidasi; boxBin hanya uji).
func (c *boxConn) remote(cmd ...string) []string {
	out := append([]string(nil), cmd...)
	if c.a.boxBin != "" {
		out[0] = c.a.boxBin + "/" + out[0]
	}
	return out
}

// argv — ssh lengkap untuk satu perintah box (lewat master bila ada).
func (c *boxConn) argv(tty bool, cmd ...string) []string {
	argv := append([]string(nil), c.base...)
	if c.mux != nil {
		argv = append(argv, "-o", "ControlMaster=no", "-o", "ControlPath="+c.mux.quoted)
	}
	// Tanpa "Shared connection … closed." / peringatan pty di layar.
	argv = append(argv, "-o", "LogLevel=ERROR")
	if tty {
		argv = append(argv, "-t")
	} else {
		argv = append(argv, "-T")
	}
	argv = append(argv, c.host)
	return append(argv, c.remote(cmd...)...)
}

// boxResult — keluaran satu perintah box.
type boxResult struct {
	code   int
	stdout []byte
	stderr string
}

// run — jalankan perintah box tanpa stdin; stdout = JSON satu baris, stderr =
// pesan manusia (ditampilkan hanya bila gagal).
func (c *boxConn) run(parent context.Context, timeout time.Duration, cmd ...string) (boxResult, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	argv := c.argv(false, cmd...)
	x := exec.CommandContext(ctx, c.sshPath, argv[1:]...)
	x.Env = c.env
	var out bytes.Buffer
	errb := &tailBuffer{max: 8 << 10}
	x.Stdout, x.Stderr = &out, errb
	err := x.Run()
	res := boxResult{stdout: out.Bytes(), stderr: errb.String()}
	// Dibatalkan pemanggil (Ctrl-C, UI keluar) ≠ batas waktu perintah ini:
	// explain() mencetak "dibatalkan", bukan "tidak selesai dalam …".
	if perr := parent.Err(); err != nil && perr != nil {
		return res, fmt.Errorf("perintah `%s` di box: %w", strings.Join(cmd, " "), perr)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("perintah `%s` di box tidak selesai dalam %s", strings.Join(cmd, " "), timeout)
		}
	default:
		return res, fmt.Errorf("menjalankan ssh: %w", err)
	}
	return res, nil
}

// boxCmdError — galat umum perintah box (bukan kode khusus perintahnya).
func boxCmdError(cmd string, r boxResult) error {
	msg := strings.TrimSpace(api.Clean(r.stderr))
	switch {
	case r.code == 127 || strings.Contains(r.stderr, "command not found"):
		return errors.New("box ini belum punya perintah `easybox` (dibutuhkan `easyrun claude`) — perbarui box ke versi terbaru dari panel")
	case r.code == 255:
		if msg == "" {
			msg = "tanpa keterangan"
		}
		return fmt.Errorf("sambungan SSH ke box gagal: %s", msg)
	}
	if msg != "" {
		return fmt.Errorf("`%s` di box gagal (kode %d): %s", cmd, r.code, msg)
	}
	return fmt.Errorf("`%s` di box gagal (kode %d)", cmd, r.code)
}

// lastJSON — baris JSON terakhir di stdout (kontrak: satu baris).
func lastJSON(out []byte, v any) error {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") {
			return json.Unmarshal([]byte(l), v)
		}
	}
	return errors.New("tidak ada JSON di keluaran")
}

// ---------- easybox version / ensure / ls / stop ----------

type easyboxVersion struct {
	Protocol       int     `json:"easybox_protocol"`
	Version        string  `json:"easybox_version"`
	ClaudePath     *string `json:"claude_path"`
	ClaudeVersion  *string `json:"claude_version"`
	ClaudeLoggedIn bool    `json:"claude_logged_in"`
	HerdrVersion   *string `json:"herdr_version"`
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (a *App) easyboxVersion(ctx context.Context, conn *boxConn, ca claudeArgs) (easyboxVersion, error) {
	var v easyboxVersion
	r, err := conn.run(ctx, boxCmdTimeout, "easybox", "version")
	if err != nil {
		return v, err
	}
	if r.code != 0 {
		return v, boxCmdError("easybox version", r)
	}
	if err := lastJSON(r.stdout, &v); err != nil {
		return v, fmt.Errorf("jawaban `easybox version` tidak bisa dibaca (%v) — perbarui box ke versi terbaru dari panel", err)
	}
	switch {
	case v.Protocol > easyboxProtocol:
		return v, fmt.Errorf("box memakai protokol easybox %d, easyrun %s baru mengenal %d — perbarui: easyrun --self-update", v.Protocol, version.Version, easyboxProtocol)
	case v.Protocol < easyboxProtocol:
		return v, fmt.Errorf("box memakai protokol easybox %d (butuh %d) — perbarui box ke versi terbaru dari panel", v.Protocol, easyboxProtocol)
	case v.ClaudePath == nil:
		return v, fmt.Errorf("Claude Code belum terpasang di box. Pasang sekali:\n  easyrun ssh %s\n  %s\nlalu login dengan `claude` dan ulangi perintah ini", ca.box, claudeInstallCmd)
	case !v.ClaudeLoggedIn:
		return v, fmt.Errorf("Claude Code di box belum login. Login sekali:\n  easyrun ssh %s\n  claude            (ikuti langkah login, lalu /exit)\nlalu ulangi perintah ini", ca.box)
	case ca.action == "attach" && v.HerdrVersion == nil:
		return v, fmt.Errorf("herdr tidak ada di box — perbarui box ke versi terbaru dari panel (atau pakai: easyrun claude %s %s hand)", ca.box, ca.target())
	}
	return v, nil
}

type easyboxEnsure struct {
	Session string `json:"session"`
	Pane    string `json:"pane"`
	State   string `json:"state"`
}

func (a *App) easyboxEnsure(ctx context.Context, conn *boxConn, ca claudeArgs) (easyboxEnsure, error) {
	var e easyboxEnsure
	r, err := conn.run(ctx, ensureCmdTimeout, "easybox", "ensure", ca.session, ca.pane)
	if err != nil {
		return e, err
	}
	box := strings.TrimSpace(api.Clean(r.stderr))
	switch r.code {
	case 0:
		if err := lastJSON(r.stdout, &e); err != nil {
			return e, fmt.Errorf("jawaban `easybox ensure` tidak bisa dibaca: %v", err)
		}
		return e, nil
	case 4:
		return e, fmt.Errorf("Claude Code belum terpasang di box — easyrun ssh %s, lalu: %s", ca.box, claudeInstallCmd)
	case 5:
		return e, fmt.Errorf("Claude Code di box belum login — easyrun ssh %s, lalu jalankan `claude` dan ikuti login", ca.box)
	case 6:
		return e, errors.New("herdr tidak tersedia di box — perbarui box ke versi terbaru dari panel")
	case 7:
		if ca.action == "hand" {
			// Jembatan sudah hidup (claude dijalankan di luar herdr): tangan
			// tetap bisa tersambung.
			return easyboxEnsure{Session: ca.session, Pane: ca.pane, State: "running"}, nil
		}
		return e, fmt.Errorf("pane %s sudah menjalankan claude di luar herdr, jadi tidak bisa dibuka di sini — hentikan dulu (easyrun claude %s %s stop) atau sambungkan tangan saja (… %s hand)", ca.pane, ca.box, ca.pane, ca.target())
	case 2:
		return e, fmt.Errorf("box menolak argumen `easybox ensure` (versi box/easyrun tidak cocok?): %s", box)
	}
	return e, boxCmdError("easybox ensure", r)
}

type easyboxLaptop struct {
	Host        string `json:"host"`
	User        string `json:"user"`
	Root        string `json:"root"`
	Mode        string `json:"mode"`
	ConnectedAt string `json:"connected_at"`
}

type easyboxPane struct {
	Pane          string         `json:"pane"`
	Session       *string        `json:"session"`
	BridgeRunning bool           `json:"bridge_running"`
	StartedAt     *string        `json:"started_at"`
	Laptop        *easyboxLaptop `json:"laptop"`
}

func (a *App) easyboxLs(ctx context.Context, conn *boxConn) ([]easyboxPane, error) {
	r, err := conn.run(ctx, boxCmdTimeout, "easybox", "ls")
	if err != nil {
		return nil, err
	}
	if r.code != 0 {
		return nil, boxCmdError("easybox ls", r)
	}
	var out struct {
		Panes []easyboxPane `json:"panes"`
	}
	if err := lastJSON(r.stdout, &out); err != nil {
		return nil, fmt.Errorf("jawaban `easybox ls` tidak bisa dibaca: %v", err)
	}
	return out.Panes, nil
}

func (a *App) claudeLs(ctx context.Context, conn *boxConn, ca claudeArgs) int {
	panes, err := a.easyboxLs(ctx, conn)
	if err != nil {
		return a.fail("", err)
	}
	if len(panes) == 0 {
		fmt.Fprintf(a.Stdout, "Belum ada pane claude di box ini. Mulai: easyrun claude %s <pane> attach\n", ca.box)
		return 0
	}
	sort.SliceStable(panes, func(i, j int) bool { return panes[i].Pane < panes[j].Pane })
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PANE\tSESI\tJALAN\tMULAI\tLAPTOP\tROOT\tMODE\tSEJAK")
	for _, p := range panes {
		run := "tidak"
		if p.BridgeRunning {
			run = "ya"
		}
		laptop, root, mode, since := "-", "-", "-", "-"
		if l := p.Laptop; l != nil {
			laptop = dash(l.Host)
			if l.User != "" {
				laptop = dash(l.User) + "@" + laptop
			}
			root, mode, since = dash(l.Root), dash(l.Mode), dash(l.ConnectedAt)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dash(p.Pane), dash(strOr(p.Session)), run, dash(strOr(p.StartedAt)), laptop, root, mode, since)
	}
	return boolCode(tw.Flush() == nil)
}

func (a *App) claudeStop(ctx context.Context, conn *boxConn, ca claudeArgs) int {
	r, err := conn.run(ctx, boxCmdTimeout, "easybox", "stop", ca.pane)
	if err != nil {
		return a.fail("", err)
	}
	if r.code != 0 {
		return a.fail("", boxCmdError("easybox stop", r))
	}
	var out struct {
		Pane    string `json:"pane"`
		Stopped bool   `json:"stopped"`
	}
	if err := lastJSON(r.stdout, &out); err != nil {
		return a.fail("", fmt.Errorf("jawaban `easybox stop` tidak bisa dibaca: %v", err))
	}
	if out.Stopped {
		fmt.Fprintf(a.Stdout, "Claude di pane %s dihentikan.\n", ca.pane)
	} else {
		fmt.Fprintf(a.Stdout, "Tidak ada claude yang berjalan di pane %s.\n", ca.pane)
	}
	return 0
}

// ---------- attach / hand ----------

func (a *App) claudeHands(ctx context.Context, conn *boxConn, d config.Dir, ca claudeArgs, local *claudeLocal, b api.Box) int {
	// Tangan (goroutine) dan alur utama sama-sama menulis ke stderr; UI ssh
	// tetap menerima stderr asli (terminal).
	uiStderr := a.Stderr
	a.Stderr = &lockedWriter{w: uiStderr}
	defer func() { a.Stderr = uiStderr }()
	fmt.Fprintf(a.Stderr, "easyrun: menyambung ke %s…\n", b.Hostname)
	mux, err := a.startMux(ctx, conn, d)
	if err != nil {
		return a.fail("membuka sambungan SSH", err)
	}
	conn.mux = mux
	defer func() { conn.mux.close(conn) }()

	ver, err := a.easyboxVersion(ctx, conn, ca)
	if err != nil {
		return a.fail("", err)
	}
	ens, err := a.easyboxEnsure(ctx, conn, ca)
	if err != nil {
		return a.fail("", err)
	}
	state := "sudah berjalan"
	if ens.State == "started" {
		state = "baru dinyalakan"
	}
	fmt.Fprintf(a.Stderr, "easyrun: box: easybox %s, Claude Code %s; pane %s %s.\n", dash(ver.Version), dash(strOr(ver.ClaudeVersion)), ca.target(), state)
	if panes, err := a.easyboxLs(ctx, conn); err == nil {
		for _, p := range panes {
			if p.Pane == ca.pane && p.Laptop != nil {
				fmt.Fprintf(a.Stderr, "easyrun: catatan: pane %s sedang tersambung ke laptop %s (%s) sejak %s; laptop itu akan dilepas (tangan terbaru menang).\n", ca.pane, dash(p.Laptop.Host), dash(p.Laptop.Root), dash(p.Laptop.ConnectedAt))
			}
		}
	}

	auditPath := filepath.Join(d.Path, claudeAuditFile)
	audit, err := claudehand.OpenAudit(auditPath, a.Now)
	if err != nil {
		return a.fail("", err)
	}
	defer audit.Close()
	pol := local.policy
	audit.Log("mulai", fmt.Sprintf("box=%s pane=%s root=%s mode=%s aksi=%s guard=easyrun-cli/%s", b.Hostname, ca.target(), pol.Root, pol.Mode(), ca.action, version.Version))
	stats := &claudehand.Stats{}
	h := &handLoop{
		a: a, conn: conn, ca: ca, local: local, stats: stats, audit: audit,
		ready: make(chan struct{}), d: d, restartMux: ca.action == "hand",
		// Sertifikat bawaan berumur 1 jam: master yang dibuka ulang (mode
		// hand) butuh sertifikat segar. ensureCert hanya menghubungi panel
		// bila sisa < 5 menit; kredensial dibaca ulang (login di terminal
		// lain ikut terpakai).
		renewCert: func(ctx context.Context) error {
			_, err := a.ensureCert(ctx, d, func() (*api.Client, error) { return a.session(d) }, b.ID)
			return err
		},
	}
	if ca.action == "hand" {
		// Lapis kedua: teks asal box sudah dinetralkan claudehand, tetapi
		// setiap baris ke terminal tetap tanpa karakter kontrol mentah.
		h.log = func(s string) {
			fmt.Fprintf(a.Stderr, "%s %s\n", a.Now().Format("15:04:05"), claudehand.PrintableLines(s))
		}
	}
	handCtx, stopHand := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		h.run(handCtx)
	}()

	code := 0
	if ca.action == "hand" {
		fmt.Fprintf(a.Stderr, "easyrun: tangan laptop untuk pane %s di %s — root %s (mode %s). Ctrl-C untuk lepas.\n", ca.target(), b.Hostname, pol.Root, pol.Mode())
		select {
		case <-ctx.Done():
		case <-loopDone:
			code = 1
		}
	} else {
		select {
		case <-h.ready:
			fmt.Fprintf(a.Stderr, "easyrun: tangan laptop tersambung — root %s (mode %s).\n", pol.Root, pol.Mode())
		case <-time.After(handReadyWait):
			fmt.Fprintf(a.Stderr, "easyrun: tangan laptop masih menyambung di latar (claude di pane mungkin sedang menyala).\n")
		case <-loopDone:
			for _, n := range h.notes() {
				fmt.Fprintf(a.Stderr, "easyrun: peringatan: tangan laptop tidak tersambung: %s\n", claudehand.PrintableLines(n))
			}
		case <-ctx.Done():
		}
		if ctx.Err() == nil {
			fmt.Fprintf(a.Stderr, "easyrun: membuka pane %s; keluar/lepas dari herdr untuk selesai.\n", ca.target())
			code = a.runClaudeUI(ctx, conn, ca, uiStderr)
		}
	}
	stopHand()
	<-loopDone
	audit.Log("selesai", fmt.Sprintf("panggilan=%d ditolak=%d", stats.Calls(), stats.Denied()))
	notes := h.notes()
	if !h.wasReady() {
		// attach: selama UI terbuka galat tangan tidak tampil di layar; tanpa
		// ini ringkasan hanya "0 panggilan tool" tanpa alasan.
		if last := h.lastTrouble(); last != "" {
			notes = append(notes, "tangan laptop tidak pernah tersambung; galat terakhir: "+last)
		}
	}
	a.claudeSummary(stats, notes, auditPath)
	return code
}

// runClaudeUI — `ssh -t … easybox attach-ui <sesi> <pane>` di terminal ini.
func (a *App) runClaudeUI(ctx context.Context, conn *boxConn, ca claudeArgs, stderr io.Writer) int {
	argv := conn.argv(true, "easybox", "attach-ui", ca.session, ca.pane)
	cmd := exec.Command(conn.sshPath, argv[1:]...)
	cmd.Env = conn.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(a.Stderr, "easyrun: menjalankan ssh: %v\n", err)
		return 1
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
		// SIGTERM, bukan SIGKILL: ssh mengembalikan mode terminal sendiri.
		claudehand.StopProcess(cmd.Process, exited, 0)
	}
	code := cmd.ProcessState.ExitCode()
	switch code {
	case 0:
		return 0
	case -1: // dihentikan sinyal
		if ctx.Err() != nil {
			return 0
		}
		return 1
	case 2:
		fmt.Fprintln(a.Stderr, "easyrun: box menolak membuka pane (butuh terminal, atau argumen tidak cocok dengan versi box).")
	case 6:
		fmt.Fprintln(a.Stderr, "easyrun: herdr tidak tersedia di box — perbarui box ke versi terbaru dari panel.")
	case 8:
		fmt.Fprintf(a.Stderr, "easyrun: pane %s tidak ditemukan di sesi herdr %s.\n", ca.pane, ca.session)
	case 255:
		fmt.Fprintln(a.Stderr, "easyrun: sambungan SSH ke box terputus.")
	}
	return code
}

func (a *App) claudeSummary(stats *claudehand.Stats, notes []string, auditPath string) {
	fmt.Fprintf(a.Stderr, "easyrun: tangan dilepas — box tidak lagi punya akses ke laptop ini. %d panggilan tool, %d ditolak penjaga. Log audit: %s\n", stats.Calls(), stats.Denied(), auditPath)
	for _, n := range append(stats.Notes(), notes...) {
		fmt.Fprintf(a.Stderr, "easyrun: catatan: %s\n", claudehand.PrintableLines(n))
	}
}

// ---------- putaran tangan ----------

const (
	// handAuthFailMax — master yang dibuka ulang ditolak autentikasinya
	// sekian kali berturut-turut (walau sertifikat sudah diperbarui) →
	// tangan berhenti, bukan mencoba tanpa akhir.
	handAuthFailMax = 3
	// selfLearnTries/selfLearnPause — `easybox ls` sesudah jabat tangan untuk
	// mencatat connected_at sambungan ini (jembatan menulis keadaannya tepat
	// sesudah jabat tangan, jadi bisa sedikit tertinggal).
	selfLearnTries = 3
	selfLearnPause = 300 * time.Millisecond
)

type handLoop struct {
	a          *App
	conn       *boxConn
	d          config.Dir
	ca         claudeArgs
	local      *claudeLocal
	stats      *claudehand.Stats
	audit      *claudehand.Audit
	log        func(string) // terminal (hand); nil = attach (layar milik UI)
	ready      chan struct{}
	readyOnce  sync.Once
	restartMux bool
	renewCert  func(context.Context) error // nil = tidak diperbarui (uji)

	// Hanya disentuh goroutine putaran (run → once).
	me     easyboxLaptop // identitas tangan ini di `easybox ls` (dari hello)
	selfAt *string       // connected_at sambungan terakhir tangan ini; nil = tidak diketahui

	mu      sync.Mutex
	noted   []string
	trouble string // galat terakhir (ringkasan bila tidak pernah tersambung)
}

func (h *handLoop) note(msg string) {
	h.mu.Lock()
	for _, n := range h.noted {
		if n == msg {
			h.mu.Unlock()
			return
		}
	}
	h.noted = append(h.noted, msg)
	h.mu.Unlock()
	h.say("! " + msg)
}

func (h *handLoop) notes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.noted...)
}

// fail — catat galat terakhir dan tampilkan (mode hand).
func (h *handLoop) fail(msg string) {
	h.setTrouble(msg)
	h.say("! " + msg)
}

func (h *handLoop) setTrouble(msg string) {
	h.mu.Lock()
	h.trouble = msg
	h.mu.Unlock()
}

func (h *handLoop) lastTrouble() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trouble
}

// wasReady — jabat tangan pernah selesai di jalan ini.
func (h *handLoop) wasReady() bool {
	select {
	case <-h.ready:
		return true
	default:
		return false
	}
}

func (h *handLoop) say(msg string) {
	if h.log != nil {
		h.log(msg)
	}
}

func (h *handLoop) hello() ([]byte, error) {
	host, _ := h.a.Hostname()
	hi := claudehand.BuildHello(h.local.policy, claudehand.HelloInfo{
		Host: host, User: claudehand.LocalUser(h.a.Getenv), OS: claudehand.OSString(),
		ClaudeVersion: h.local.version, Guard: "easyrun-cli/" + version.Version,
	})
	// Jembatan menyalin host/user/root/mode hello apa adanya ke `easybox ls`.
	h.me = easyboxLaptop{Host: hi.Host, User: hi.User, Root: hi.Root, Mode: hi.Mode}
	return hi.Line()
}

// run — sambung, layani, sambung ulang dengan jeda bertambah (1 dtk … 30
// dtk) selama ctx hidup. Berhenti tanpa sambung ulang bila digantikan laptop
// lain, pane sudah dipegang tangan lain, box menolak argumen, atau
// autentikasi SSH ditolak berulang.
func (h *handLoop) run(ctx context.Context) {
	backoff := time.Second
	missing := false
	authFails := 0
	pause := func() bool {
		h.say(fmt.Sprintf("menyambung ulang dalam %s…", backoff))
		ok := sleepOrDone(ctx, backoff)
		backoff = min(backoff*2, handBackoffMax)
		return ok
	}
	for ctx.Err() == nil {
		if !h.conn.mux.alive() {
			if !h.restartMux {
				h.note("sambungan SSH ke box terputus")
				return
			}
			h.say("sambungan SSH terputus; membuka ulang…")
			m, err := h.reopen(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				h.fail(err.Error())
				if !authFailure(err) {
					authFails = 0
				} else if authFails++; authFails >= handAuthFailMax {
					h.note(fmt.Sprintf("autentikasi SSH ke box gagal %d kali berturut-turut (sertifikat sudah diperiksa/diperbarui); tangan berhenti — bila perlu `easyrun login`, lalu ulangi: easyrun claude %s %s %s. Galat terakhir: %s",
						authFails, h.ca.box, h.ca.target(), h.ca.action, claudehand.Short(err.Error(), 300)))
					return
				}
				if !pause() {
					return
				}
				continue
			}
			authFails = 0
			h.conn.mux.close(h.conn)
			h.conn.mux = m
		}
		// Sambung ulang setelah pernah tersambung: pemberitahuan "digantikan"
		// bisa hilang (laptop tidur, jaringan putus) — jangan merebut kembali
		// pane yang kini dipegang tangan lain.
		if h.wasReady() {
			who, err := h.otherHolder(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				h.fail("memeriksa pemegang pane: " + explain(err))
				if !pause() {
					return
				}
				continue
			}
			if who != "" {
				h.note(fmt.Sprintf("pane %s sekarang dipegang %s; tangan ini berhenti supaya tidak merebutnya kembali (ulangi perintah ini bila memang ingin mengambil alih)", h.ca.target(), who))
				return
			}
		}
		start := h.a.Now()
		res, code, err := h.once(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			h.fail(err.Error())
		case res.Replaced:
			h.note(fmt.Sprintf("pane %s diambil alih laptop lain (tangan terbaru menang); tangan ini berhenti", h.ca.target()))
			return
		case code == 2:
			h.note("box menolak argumen easybox-hand-connect (versi box/easyrun tidak cocok?)")
			return
		case code == 3:
			h.setTrouble(fmt.Sprintf("belum ada claude yang berjalan di pane %s", h.ca.pane))
			if !missing {
				h.say(fmt.Sprintf("belum ada claude yang berjalan di pane %s; menunggu…", h.ca.pane))
			}
			missing = true
		case res.ServerErr != nil:
			h.note("claude mcp serve lokal berhenti: " + res.ServerErr.Error())
		case res.LinkErr != nil:
			h.fail("sambungan tangan: " + res.LinkErr.Error())
		case res.Ready:
			h.say("tangan terputus dari box")
		case code != 255: // 255: once sudah mencatat pesan ssh
			h.setTrouble(fmt.Sprintf("box menutup sambungan tangan sebelum jabat tangan selesai (kode %d; lihat log jembatan di box)", code))
		}
		if res.Ready || h.a.Now().Sub(start) >= handStableAfter {
			backoff = time.Second
			missing = false
		}
		if !pause() {
			return
		}
	}
}

// reopen — perbarui sertifikat dulu, baru buka master baru: master dengan
// sertifikat kedaluwarsa ditolak "Permission denied" berulang tanpa akhir.
func (h *handLoop) reopen(ctx context.Context) (*sshMux, error) {
	if h.renewCert != nil {
		if err := h.renewCert(ctx); err != nil {
			return nil, &certError{err: err}
		}
	}
	return h.a.startMux(ctx, h.conn, h.d)
}

// certError — sertifikat SSH tidak bisa diperbarui.
type certError struct{ err error }

func (e *certError) Error() string { return "memperbarui sertifikat SSH: " + explain(e.err) }
func (e *certError) Unwrap() error { return e.err }

// authFailure — galat yang tidak sembuh dengan mencoba lagi: sshd menolak
// autentikasi, atau sertifikat tidak bisa diminta karena sesi CLI / akses ke
// box tidak berlaku.
func authFailure(err error) bool {
	var me *muxError
	if errors.As(err, &me) {
		return me.auth
	}
	var ce *certError
	if !errors.As(err, &ce) {
		return false
	}
	var ee *expiredError
	var ae *api.Error
	switch {
	case errors.Is(err, config.ErrNotLoggedIn), errors.As(err, &ee):
		return true
	case errors.As(err, &ae):
		return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Status == http.StatusNotFound
	}
	return false
}

// paneLaptop — laptop yang memegang pane ini menurut `easybox ls` (nil =
// tidak ada).
func (h *handLoop) paneLaptop(ctx context.Context) (*easyboxLaptop, error) {
	panes, err := h.a.easyboxLs(ctx, h.conn)
	if err != nil {
		return nil, err
	}
	for _, p := range panes {
		if p.Pane == h.ca.pane {
			return p.Laptop, nil
		}
	}
	return nil, nil
}

func (h *handLoop) sameLaptop(l *easyboxLaptop) bool {
	return l.Host == h.me.Host && l.User == h.me.User && l.Root == h.me.Root && l.Mode == h.me.Mode
}

// otherHolder — "" bila pane bebas, atau masih tercatat atas sambungan tangan
// ini sendiri (jembatan belum sadar sambungan lamanya putus); selain itu
// keterangan pemegang lain. Identitas = host/user/root/mode hello tangan ini
// + connected_at sambungan terakhirnya (dicatat learnSelf): laptop & folder
// yang sama dari proses easyrun lain tetap terhitung "lain".
func (h *handLoop) otherHolder(ctx context.Context) (string, error) {
	l, err := h.paneLaptop(ctx)
	if err != nil || l == nil {
		return "", err
	}
	same := h.sameLaptop(l)
	if same && (h.selfAt == nil || l.ConnectedAt == *h.selfAt) {
		return "", nil
	}
	who := dash(l.Host)
	if l.User != "" {
		who = dash(l.User) + "@" + who
	}
	if same {
		who += " (laptop & folder ini, dari proses easyrun lain)"
	}
	return fmt.Sprintf("%s, root %s, sejak %s", who, dash(l.Root), dash(l.ConnectedAt)), nil
}

// learnSelf — catat connected_at sambungan ini dari `easybox ls` (lewat
// master yang sama) sesudah jabat tangan. Gagal → tidak diketahui, dan
// otherHolder membandingkan host/user/root/mode saja.
func (h *handLoop) learnSelf(ctx context.Context, ended <-chan struct{}) {
	h.selfAt = nil
	for i := 0; i < selfLearnTries; i++ {
		if l, err := h.paneLaptop(ctx); err == nil && l != nil && h.sameLaptop(l) {
			at := l.ConnectedAt
			h.selfAt = &at
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ended:
			return
		case <-time.After(selfLearnPause):
		}
	}
}

func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// once — satu saluran `easybox-hand-connect <pane> --wait 60` lewat master.
func (h *handLoop) once(ctx context.Context) (claudehand.Result, int, error) {
	hello, err := h.hello()
	if err != nil {
		return claudehand.Result{}, 0, err
	}
	argv := h.conn.argv(false, "easybox-hand-connect", h.ca.pane, "--wait", strconv.Itoa(handConnectWait))
	cmd := exec.Command(h.conn.sshPath, argv[1:]...)
	cmd.Env = h.conn.env
	errb := &tailBuffer{max: 8 << 10}
	cmd.Stderr = errb
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return claudehand.Result{}, 0, err
	}
	// Pipa stdout milik sendiri: Wait tidak menutupnya selagi penjaga membaca.
	pr, pw, err := os.Pipe()
	if err != nil {
		return claudehand.Result{}, 0, err
	}
	defer pr.Close()
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return claudehand.Result{}, 0, fmt.Errorf("menjalankan ssh: %w", err)
	}
	_ = pw.Close()
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	h.audit.Log("sambung", "pane="+h.ca.target())
	srvCmd := claudehand.ServerCommand{
		Argv: []string{h.local.claude, "mcp", "serve"},
		Dir:  h.local.policy.Root,
		Env:  claudehand.ServerEnv(h.conn.env, h.local.handCfg),
	}
	ready := make(chan struct{})
	served := make(chan struct{})
	var res claudehand.Result
	go func() {
		defer close(served)
		res = claudehand.Serve(ctx, pr, stdin, claudehand.Config{
			Policy: h.local.policy, Hello: hello, Start: srvCmd.Start,
			Stats: h.stats, Audit: h.audit, Log: h.log,
			OnReady: func() {
				h.audit.Log("tersambung", "pane="+h.ca.target())
				h.say(fmt.Sprintf("tersambung ke %s — root %s (mode %s)", h.ca.target(), h.local.policy.Root, h.local.policy.Mode()))
				h.readyOnce.Do(func() { close(h.ready) })
				close(ready)
			},
		})
	}()
	select {
	case <-served:
	case <-ready:
		// Di goroutine putaran (bukan di OnReady): master tidak pernah
		// diganti selagi `easybox ls` ini memakainya.
		h.learnSelf(ctx, served)
		<-served
	}
	_ = stdin.Close()
	claudehand.StopProcess(cmd.Process, exited, 3*time.Second)
	code := cmd.ProcessState.ExitCode()
	h.audit.Log("lepas", fmt.Sprintf("pane=%s kode=%d", h.ca.target(), code))
	if code == 255 {
		msg := strings.TrimSpace(api.Clean(errb.String()))
		if msg == "" {
			msg = "berhenti (kode 255) tanpa keterangan"
		}
		h.fail("ssh: " + msg)
	}
	return res, code, nil
}

// lockedWriter — satu penulis pada satu waktu (baris log tidak bercampur).
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// tailBuffer — simpan hanya max byte terakhir (stderr proses latar).
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
