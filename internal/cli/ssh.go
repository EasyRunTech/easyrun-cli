package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/sshcert"
	"github.com/EasyRunTech/easyrun-cli/internal/sshconf"
	"github.com/EasyRunTech/easyrun-cli/internal/tunnel"
)

// keyComment — komentar kunci laptop (terlihat di berkas .pub).
const keyComment = "easyrun-cli"

func getenvAny(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// ---------- resolusi <box> ----------

type boxNotFoundError struct{ query string }

func (e *boxNotFoundError) Error() string {
	return fmt.Sprintf("box %q tidak ditemukan di antara box yang bisa kamu akses — lihat: easyrun boxes", e.query)
}

type ambiguousBoxError struct {
	query   string
	matches []api.Box
}

func (e *ambiguousBoxError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q cocok dengan %d box — pakai hostname atau id:\n", e.query, len(e.matches))
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	for _, m := range e.matches {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", dash(m.Name), dash(m.Hostname), dash(m.TenantName), dash(m.ID))
	}
	_ = tw.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// matchBox — cocokkan <box> ke nama, hostname lengkap, label pertama hostname
// (mis. "contoh-box" untuk contoh-box.easyrun.io), atau id — tanpa beda huruf
// besar/kecil (kontrak §6 + §7a.5). Satu box dihitung sekali walau cocok di
// beberapa kolom; lebih dari satu box → ambiguousBoxError.
func matchBox(boxes []api.Box, q string) (api.Box, error) {
	var found []api.Box
	for _, b := range boxes {
		first, _, _ := strings.Cut(b.Hostname, ".")
		if strings.EqualFold(b.ID, q) || strings.EqualFold(b.Name, q) ||
			strings.EqualFold(b.Hostname, q) || (first != "" && strings.EqualFold(first, q)) {
			found = append(found, b)
		}
	}
	switch len(found) {
	case 0:
		return api.Box{}, &boxNotFoundError{query: q}
	case 1:
		return found[0], nil
	}
	sortBoxes(found)
	return api.Box{}, &ambiguousBoxError{query: q, matches: found}
}

// resolveBox — <box> → box lengkap lewat /cli/boxes, sudah divalidasi untuk
// dipakai di argumen ssh.
func (a *App) resolveBox(ctx context.Context, c *api.Client, q string) (api.Box, error) {
	if err := sshconf.ValidQuery(q); err != nil {
		return api.Box{}, err
	}
	boxes, err := c.Boxes(ctx)
	if err != nil {
		return api.Box{}, err
	}
	b, err := matchBox(boxes, q)
	if err != nil {
		return api.Box{}, err
	}
	if err := sshconf.ValidID(b.ID); err != nil {
		return api.Box{}, err
	}
	// ssh_available=false TIDAK ditolak di sini (kontrak §7b P14): panel
	// juga mengirim false bila status box sesaat tak terbaca. Permintaan
	// sertifikat/tiket berikutnya mendapat 409 dengan alasan sebenarnya.
	if b.Hostname == "" {
		return api.Box{}, fmt.Errorf("box %s belum punya hostname", dash(b.Name))
	}
	if err := sshconf.ValidHostname(b.Hostname); err != nil {
		return api.Box{}, err
	}
	return b, nil
}

func statusNote(b api.Box) string {
	if b.Status == "" {
		return ""
	}
	return " (status: " + dash(b.Status) + ")"
}

// boxID — id untuk ssh-prepare / tunnel. <box> berbentuk UUID dipakai
// langsung (tanpa memanggil /cli/boxes; server tetap memeriksa keanggotaan)
// supaya `Match exec` tetap luring selama sertifikat masih segar.
func (a *App) boxID(ctx context.Context, getClient func() (*api.Client, error), q string) (string, error) {
	if err := sshconf.ValidQuery(q); err != nil {
		return "", err
	}
	if sshconf.IsUUID(q) {
		return strings.ToLower(q), nil
	}
	c, err := getClient()
	if err != nil {
		return "", err
	}
	b, err := a.resolveBox(ctx, c, q)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// lazyClient — buat sesi hanya bila benar-benar perlu ke server.
func (a *App) lazyClient(d config.Dir) func() (*api.Client, error) {
	var c *api.Client
	var err error
	done := false
	return func() (*api.Client, error) {
		if !done {
			c, err = a.session(d)
			done = true
		}
		return c, err
	}
}

// ---------- sertifikat ----------

type certResult struct {
	path      string
	hostAlias string // dari panel bila baru diperbarui
	refreshed bool
}

// ensureCert — kontrak §6: perbarui bila belum ada, sisa < 5 menit, principal
// tidak cocok, atau untuk kunci lain. Sertifikat yang masih segar dipakai
// tanpa menghubungi server.
func (a *App) ensureCert(ctx context.Context, d config.Dir, getClient func() (*api.Client, error), boxID string) (certResult, error) {
	res := certResult{path: d.CertPath(boxID)}
	kp, err := sshcert.EnsureKey(d.KeyPath(), d.PubKeyPath(), keyComment)
	if err != nil {
		return res, err
	}
	if data, err := os.ReadFile(res.path); err == nil {
		if cert, err := sshcert.ParseCert(data); err == nil {
			if sshcert.Check(cert, sshcert.Want{Principal: boxID, Key: kp.PublicKey, Now: a.Now()}) == nil {
				return res, nil
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}

	c, err := getClient()
	if err != nil {
		return res, err
	}
	resp, err := c.Certificate(ctx, boxID, kp.AuthorizedKey)
	if err != nil {
		return res, fmt.Errorf("meminta sertifikat SSH: %w", err)
	}
	cert, err := sshcert.ParseCert([]byte(strings.TrimSpace(resp.Certificate)))
	if err != nil {
		return res, fmt.Errorf("sertifikat dari server: %w", err)
	}
	// Sertifikat baru cukup belum kedaluwarsa (TTL server bisa < 5 menit).
	if err := sshcert.Check(cert, sshcert.Want{Principal: boxID, Key: kp.PublicKey, Now: a.Now(), MinRemaining: time.Second}); err != nil {
		return res, fmt.Errorf("sertifikat dari server tidak bisa dipakai: %w", err)
	}
	if resp.Principal != "" && !strings.EqualFold(resp.Principal, boxID) {
		return res, fmt.Errorf("sertifikat dari server untuk box lain (principal %q)", api.Clean(resp.Principal))
	}
	if err := config.WriteFileAtomic(res.path, []byte(strings.TrimSpace(resp.Certificate)+"\n"), 0o600); err != nil {
		return res, err
	}
	res.refreshed = true
	if h := strings.TrimSpace(resp.HostAlias); h != "" && sshconf.ValidHostname(h) == nil {
		res.hostAlias = h
	}
	return res, nil
}

// ---------- path program ----------

// exePath — path absolut program ini untuk ProxyCommand / Match exec. Bila
// `easyrun` di PATH menunjuk berkas yang sama, path PATH itu yang dipakai:
// tetap benar setelah --self-update atau pembaruan lewat pengelola paket yang
// memakai symlink.
func (a *App) exePath() (string, error) {
	exe, err := a.Executable()
	if err != nil {
		return "", fmt.Errorf("lokasi program easyrun tidak diketahui: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	if a.LookPath != nil {
		if lp, err := a.LookPath("easyrun"); err == nil {
			if abs, err := filepath.Abs(lp); err == nil && sameFile(abs, exe) {
				return abs, nil
			}
		}
	}
	return exe, nil
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func (a *App) sshParams(d config.Dir, alias string, b api.Box, hostAlias string) (sshconf.Params, error) {
	exe, err := a.exePath()
	if err != nil {
		return sshconf.Params{}, err
	}
	host := b.Hostname
	if hostAlias != "" {
		host = hostAlias
	}
	return sshconf.Params{
		Alias:           alias,
		ID:              b.ID,
		Hostname:        host,
		User:            a.sshUser,
		Exe:             exe,
		IdentityFile:    d.KeyPath(),
		CertificateFile: d.CertPath(b.ID),
		KnownHostsFile:  d.KnownHostsPath(),
		GOOS:            a.GOOS,
	}, nil
}

// ---------- perintah ----------

func (a *App) cmdSSH(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.usageError("ssh", "nama box belum diisi")
	}
	q, extra := args[0], args[1:]
	if len(extra) > 0 && extra[0] == "--" {
		extra = extra[1:]
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	b, err := a.resolveBox(ctx, c, q)
	if err != nil {
		return a.fail("", err)
	}
	cr, err := a.ensureCert(ctx, d, func() (*api.Client, error) { return c, nil }, b.ID)
	if err != nil {
		return a.fail("", err)
	}
	p, err := a.sshParams(d, q, b, cr.hostAlias)
	if err != nil {
		return a.fail("", err)
	}
	argv, err := sshconf.Argv(p, extra)
	if err != nil {
		return a.fail("", err)
	}
	sshPath, err := a.LookPath("ssh")
	if err != nil {
		return a.fail("", errors.New("program ssh (OpenSSH) tidak ditemukan di PATH — pasang dulu: macOS sudah menyertakannya; Debian/Ubuntu: sudo apt install openssh-client; Windows: aktifkan fitur \"OpenSSH Client\""))
	}
	env := os.Environ()
	if a.Environ != nil {
		env = a.Environ()
	}
	code, err := a.ExecSSH(sshPath, argv, env)
	if err != nil {
		return a.fail("menjalankan ssh", err)
	}
	return code
}

func (a *App) cmdSSHPrepare(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return a.usageError("ssh-prepare", "butuh tepat satu nama box")
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("ssh-prepare", err)
	}
	getClient := a.lazyClient(d)
	id, err := a.boxID(ctx, getClient, args[0])
	if err != nil {
		return a.fail("ssh-prepare", err)
	}
	if _, err := a.ensureCert(ctx, d, getClient, id); err != nil {
		return a.fail("ssh-prepare", err)
	}
	return 0
}

func (a *App) cmdSSHConfig(ctx context.Context, args []string) int {
	write := false
	var pos []string
	for _, arg := range args {
		switch arg {
		case "--write", "-write", "-w":
			write = true
		default:
			if strings.HasPrefix(arg, "-") {
				return a.usageError("ssh-config", "opsi tidak dikenal: "+api.Clean(arg))
			}
			pos = append(pos, arg)
		}
	}
	if len(pos) != 1 {
		return a.usageError("ssh-config", "butuh tepat satu nama box")
	}
	q := pos[0]
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	b, err := a.resolveBox(ctx, c, q)
	if err != nil {
		return a.fail("", err)
	}
	if !b.SSHAvailable {
		fmt.Fprintf(a.Stderr, "easyrun: peringatan: panel saat ini menandai SSH belum tersedia untuk box %s%s; blok tetap dibuat, sambungan ditolak panel (dengan alasannya) sampai SSH tersedia.\n", dash(b.Name), statusNote(b))
	}
	p, err := a.sshParams(d, q, b, "")
	if err != nil {
		return a.fail("", err)
	}
	block, err := sshconf.ConfigBlock(p)
	if err != nil {
		return a.fail("", err)
	}
	alias := strings.ToLower(q)
	if !write {
		fmt.Fprint(a.Stdout, block)
		fmt.Fprintf(a.Stderr, `Taruh blok di atas di ~/.ssh/config, DI ATAS semua blok "Host *" / "Match all":
ssh memakai nilai pertama yang ditemukan, jadi blok di bawahnya (mis. ditempel
dengan >>) bisa kalah oleh User, ProxyJump, atau ForwardAgent milik "Host *".
Atau biarkan easyrun yang menulisnya:  easyrun ssh-config %s --write
Lalu cukup: ssh %s  (juga scp, rsync, sftp, VS Code Remote-SSH)
`, alias, alias)
		return 0
	}
	return a.writeSSHConfig(b.ID, alias, block)
}

// writeSSHConfig — `ssh-config --write`: ~/.ssh/easyrun/<id>.conf + satu
// baris Include di puncak ~/.ssh/config (kontrak §7b P7).
func (a *App) writeSSHConfig(id, alias, block string) int {
	homeDir := a.HomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return a.fail("ssh-config --write", fmt.Errorf("folder home tidak ditemukan: %w", err))
	}
	sshDir := filepath.Join(home, ".ssh")
	header := fmt.Sprintf("# Ditulis ulang oleh `easyrun ssh-config %s --write`; perubahan manual akan tertimpa.\n", alias)
	path, existed, err := sshconf.WriteBoxConf(sshDir, id, header+block)
	if err != nil {
		return a.fail("ssh-config --write", err)
	}
	cfgPath := filepath.Join(sshDir, "config")
	res, err := sshconf.EnsureInclude(cfgPath)
	if err != nil {
		return a.fail("ssh-config --write", fmt.Errorf("blok box sudah ditulis ke %s, tetapi %s tidak bisa diperbarui: %w — tambahkan sendiri baris ini di PALING ATAS berkas itu: %s", path, cfgPath, err, sshconf.IncludeLine))
	}
	verb := "Ditulis"
	if existed {
		verb = "Diperbarui"
	}
	fmt.Fprintf(a.Stdout, "%s: %s\n", verb, path)
	switch {
	case res.Created:
		fmt.Fprintf(a.Stdout, "Dibuat: %s berisi baris %q.\n", cfgPath, sshconf.IncludeLine)
	case res.Inserted:
		fmt.Fprintf(a.Stdout, "Ditambahkan di baris 1 %s: %q (baris lain tidak diubah).\n", cfgPath, sshconf.IncludeLine)
		if res.Backup != "" {
			fmt.Fprintf(a.Stdout, "Cadangan isi lama: %s\n", res.Backup)
		}
	default:
		fmt.Fprintf(a.Stdout, "%s sudah memuat baris Include di bagian atas; tidak diubah.\n", cfgPath)
	}
	if len(res.Stale) > 0 {
		fmt.Fprintf(a.Stderr, "easyrun: catatan: %s juga memuat baris %q di baris %s (di bawah blok lain); dibiarkan, boleh dihapus.\n", cfgPath, sshconf.IncludeLine, joinInts(res.Stale))
	}
	fmt.Fprintf(a.Stdout, "Sekarang cukup: ssh %s  (juga scp, rsync, sftp, VS Code Remote-SSH)\n", alias)
	return 0
}

func joinInts(n []int) string {
	s := make([]string, len(n))
	for i, v := range n {
		s[i] = fmt.Sprint(v)
	}
	return strings.Join(s, ", ")
}

// cmdTunnel — ProxyCommand. stdout = aliran SSH; semua pesan ke stderr.
func (a *App) cmdTunnel(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return a.usageError("tunnel", "butuh tepat satu nama box")
	}
	ignoreSIGPIPE()
	d, err := a.dir()
	if err != nil {
		return a.fail("tunnel", err)
	}
	getClient := a.lazyClient(d)
	id, err := a.boxID(ctx, getClient, args[0])
	if err != nil {
		return a.fail("tunnel", err)
	}
	c, err := getClient()
	if err != nil {
		return a.fail("tunnel", err)
	}
	t, err := c.Ticket(ctx, id)
	if err != nil {
		return a.fail("meminta tiket SSH", err)
	}
	wsURL, err := tunnel.URLWithTicket(t.WSURL, t.Ticket, c.BaseURL)
	if err != nil {
		return a.fail("tunnel", err)
	}
	conn, err := tunnel.Dial(ctx, wsURL, userAgent())
	if err != nil {
		return a.fail("membuka saluran SSH", err)
	}
	return tunnel.Pump(ctx, conn, tunnel.Options{
		Stdin:        a.Stdin,
		Stdout:       a.Stdout,
		Stderr:       a.Stderr,
		Ticks:        a.PingTicks,
		ReadyTimeout: a.TunnelReadyTimeout,
		CloseGrace:   a.TunnelCloseGrace,
	})
}
