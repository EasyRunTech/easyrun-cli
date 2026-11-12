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

const (
	// clockWarn — selisih jam laptop vs server yang dilaporkan ke pengguna.
	clockWarn = 2 * time.Minute
	// clockNoise — selisih sekecil ini dianggap nol (resolusi header Date 1
	// dtk + waktu perjalanan) supaya clock.json tidak ditulis tiap kali.
	clockNoise = 2 * time.Second
)

// normalizeID — id box berbentuk UUID ditulis huruf kecil di semua tempat
// (nama berkas sertifikat, ProxyCommand, Match exec, principal): UUID tidak
// membedakan huruf besar/kecil, tetapi nama berkas di Linux membedakannya.
func normalizeID(id string) string {
	if sshconf.IsUUID(id) {
		return strings.ToLower(id)
	}
	return id
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
	b.ID = normalizeID(b.ID)
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
		return normalizeID(q), nil
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

// ensureCert — kontrak §6: perbarui bila belum ada, sisa < 5 menit, principal
// tidak cocok, atau untuk kunci lain. Sertifikat yang masih segar dipakai
// tanpa menghubungi server.
//
// Box menilai sertifikat dengan jam SERVER. Selisih jam laptop diukur dari
// header Date setiap kali sertifikat diterbitkan (clock.json), lalu:
//   - sertifikat yang BARU diterbitkan dinilai dengan jam server dari
//     jawaban itu — laptop yang jamnya maju tidak lagi membuat SSH tak
//     terpakai;
//   - sertifikat tersimpan dianggap segar hanya bila segar menurut jam
//     laptop DAN jam server perkiraan (laptop + selisih tersimpan). Laptop
//     yang jamnya mundur tidak lagi memakai sertifikat yang sudah habis
//     menurut box; selisih yang basi (jam baru dibetulkan) tidak pernah
//     membuat sertifikat habis dianggap segar — paling buruk diperbarui
//     lebih awal.
func (a *App) ensureCert(ctx context.Context, d config.Dir, getClient func() (*api.Client, error), boxID string) (string, error) {
	path := d.CertPath(boxID)
	kp, err := sshcert.EnsureKey(d.KeyPath(), d.PubKeyPath(), keyComment)
	if err != nil {
		return path, err
	}
	saved := d.LoadClockOffset()
	if data, err := os.ReadFile(path); err == nil {
		if cert, err := sshcert.ParseCert(data); err == nil {
			local := a.Now()
			fresh := func(now time.Time) bool {
				return sshcert.Check(cert, sshcert.Want{Principal: boxID, Key: kp.PublicKey, Now: now}) == nil
			}
			if fresh(local) && fresh(local.Add(saved.Offset())) {
				return path, nil
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return path, err
	}

	c, err := getClient()
	if err != nil {
		return path, err
	}
	resp, err := c.Certificate(ctx, boxID, kp.AuthorizedKey)
	if err != nil {
		return path, fmt.Errorf("meminta sertifikat SSH: %w", err)
	}
	cert, err := sshcert.ParseCert([]byte(strings.TrimSpace(resp.Certificate)))
	if err != nil {
		return path, fmt.Errorf("sertifikat dari server: %w", err)
	}
	now := a.Now().Add(saved.Offset())
	if !resp.ServerDate.IsZero() {
		now = resp.ServerDate
		a.recordClock(d, saved, resp.ServerDate)
	}
	// Sertifikat baru cukup belum kedaluwarsa (TTL server bisa < 5 menit).
	if err := sshcert.Check(cert, sshcert.Want{Principal: boxID, Key: kp.PublicKey, Now: now, MinRemaining: time.Second}); err != nil {
		return path, fmt.Errorf("sertifikat dari server tidak bisa dipakai: %w", err)
	}
	if resp.Principal != "" && !strings.EqualFold(resp.Principal, boxID) {
		return path, fmt.Errorf("sertifikat dari server untuk box lain (principal %q)", api.Clean(resp.Principal))
	}
	if err := config.WriteFileAtomic(path, []byte(strings.TrimSpace(resp.Certificate)+"\n"), 0o600); err != nil {
		return path, err
	}
	return path, nil
}

// recordClock — simpan selisih jam server − laptop (bila berubah berarti) dan
// beri tahu pengguna bila jam laptop melenceng jauh. Gagal menyimpan tidak
// menggagalkan sambungan.
func (a *App) recordClock(d config.Dir, saved config.ClockOffset, serverDate time.Time) {
	local := a.Now()
	off := serverDate.Sub(local).Round(time.Second)
	if off > -clockNoise && off < clockNoise {
		off = 0
	}
	if diff := off - saved.Offset(); diff <= -clockNoise || diff >= clockNoise {
		_ = d.SaveClockOffset(config.ClockOffset{OffsetSeconds: int64(off / time.Second), MeasuredAt: local})
	}
	if off <= -clockWarn || off >= clockWarn {
		dir := "lebih cepat"
		if off > 0 {
			dir = "lebih lambat"
		}
		mag := off
		if mag < 0 {
			mag = -mag
		}
		fmt.Fprintf(a.Stderr, "easyrun: peringatan: jam komputer ini %s %s dari jam server EasyRun; sertifikat SSH tetap dipakai, tetapi sebaiknya sinkronkan jam (pengaturan tanggal & waktu otomatis).\n", dir, mag)
	}
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

// sshParams — HostName selalu hostname dari /cli/boxes (host_alias di jawaban
// sertifikat tidak dipakai: isinya sama, dan memakai dua sumber membuat
// hasilnya berbeda antar-jalan). Folder konfigurasi selalu dibawa ke
// ProxyCommand/Match exec; alamat API hanya bila datang dari opsi/env (yang
// dari config.json sudah ikut lewat folder itu).
func (a *App) sshParams(d config.Dir, alias string, b api.Box) (sshconf.Params, error) {
	exe, err := a.exePath()
	if err != nil {
		return sshconf.Params{}, err
	}
	apiURL := ""
	if raw := a.apiURLOpt; raw != "" || a.Getenv(config.EnvAPIURL) != "" {
		if raw == "" {
			raw = a.Getenv(config.EnvAPIURL)
		}
		if apiURL, err = config.NormalizeAPIURL(raw); err != nil {
			return sshconf.Params{}, err
		}
	}
	return sshconf.Params{
		Alias:           alias,
		ID:              b.ID,
		Hostname:        b.Hostname,
		User:            a.sshUser,
		Exe:             exe,
		ConfigDir:       d.Path,
		APIURL:          apiURL,
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
	if _, err := a.ensureCert(ctx, d, func() (*api.Client, error) { return c, nil }, b.ID); err != nil {
		return a.fail("", err)
	}
	p, err := a.sshParams(d, q, b)
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
	p, err := a.sshParams(d, q, b)
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
// baris Include di puncak ~/.ssh/config (kontrak §7b P7). ~ = home akun yang
// dipakai OpenSSH (passwd), bukan $HOME — keduanya bisa berbeda di bawah
// `sudo -E`, di container, atau bila HOME diarahkan ke tempat lain.
func (a *App) writeSSHConfig(id, alias, block string) int {
	home, err := a.sshHome()
	if err != nil {
		return a.fail("ssh-config --write", fmt.Errorf("folder home tidak ditemukan: %w", err))
	}
	sshDir := filepath.Join(home, ".ssh")
	header := fmt.Sprintf("# Ditulis ulang oleh `easyrun ssh-config %s --write`; perubahan manual akan tertimpa.\n", alias)
	path, existed, err := sshconf.WriteBoxConf(sshDir, id, header+block)
	if err != nil {
		return a.fail("ssh-config --write", err)
	}
	// Blok lama untuk alias yang sama (box lain, mis. yang sudah dihapus lalu
	// dibuat ulang dengan nama sama) dimuat ssh lebih dulu bila nama
	// berkasnya lebih awal — dan menang.
	stale, staleErr := sshconf.RemoveStaleConfs(sshDir, id, alias)
	cfgPath := filepath.Join(sshDir, "config")
	res, err := sshconf.EnsureInclude(cfgPath, home)
	if err != nil {
		return a.fail("ssh-config --write", fmt.Errorf("blok box sudah ditulis ke %s, tetapi %s tidak bisa diperbarui: %w — tambahkan sendiri baris ini di PALING ATAS berkas itu: %s", path, cfgPath, err, sshconf.IncludeLine))
	}
	verb := "Ditulis"
	if existed {
		verb = "Diperbarui"
	}
	fmt.Fprintf(a.Stdout, "%s: %s\n", verb, path)
	for _, r := range stale.Removed {
		fmt.Fprintf(a.Stdout, "Dihapus: %s (blok lama untuk %s, box lain)\n", r, alias)
	}
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
	if len(res.Shadowing) > 0 {
		fmt.Fprintf(a.Stderr, "easyrun: catatan: berkas yang di-Include di atas baris easyrun menyetel opsi untuk semua host (%s); karena itu baris easyrun ditaruh di baris 1.\n", strings.Join(res.Shadowing, ", "))
	}
	if len(res.Stale) > 0 {
		fmt.Fprintf(a.Stderr, "easyrun: catatan: %s juga memuat baris %q di baris %s (bukan yang dipakai); dibiarkan, boleh dihapus.\n", cfgPath, sshconf.IncludeLine, joinInts(res.Stale))
	}
	for _, f := range stale.Foreign {
		fmt.Fprintf(a.Stderr, "easyrun: peringatan: %s juga mendefinisikan Host %s tetapi bukan buatan easyrun; bila namanya lebih awal, blok itu yang dipakai ssh — periksa atau hapus.\n", f, alias)
	}
	if staleErr != nil {
		fmt.Fprintf(a.Stderr, "easyrun: peringatan: blok lama untuk %s tidak bisa dibersihkan: %v\n", alias, staleErr)
	}
	if env, err := a.homeDir(); err == nil && filepath.Clean(env) != filepath.Clean(home) {
		fmt.Fprintf(a.Stderr, "easyrun: catatan: ssh membaca %s (home akun), bukan $HOME=%s; blok ditulis ke sana.\n", cfgPath, env)
	}
	fmt.Fprintf(a.Stdout, "Sekarang cukup: ssh %s  (juga scp, rsync, sftp, VS Code Remote-SSH)\n", alias)
	return 0
}

// homeDir — $HOME (App.HomeDir, bawaan os.UserHomeDir).
func (a *App) homeDir() (string, error) {
	if a.HomeDir != nil {
		return a.HomeDir()
	}
	return os.UserHomeDir()
}

// sshHome — home yang dipakai OpenSSH (App.SSHHomeDir; bila nil = homeDir).
func (a *App) sshHome() (string, error) {
	if a.SSHHomeDir != nil {
		if h, err := a.SSHHomeDir(); err == nil && h != "" {
			return h, nil
		}
	}
	return a.homeDir()
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
