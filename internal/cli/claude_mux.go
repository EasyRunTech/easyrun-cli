package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/claudehand"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/sshconf"
)

const (
	// controlDirName — folder soket ControlMaster di bawah folder config.
	controlDirName = "cm"
	// controlNameLen — nama soket acak per jalan: dua `attach` bersamaan
	// tidak saling berbagi (atau saling menutup) master.
	controlNameLen = 10
	// controlTempSuffix — OpenSSH membuat soket sebagai "<path>.<16 acak>"
	// dulu lalu mengganti namanya.
	controlTempSuffix = 17
	// sunPathMax — batas alamat soket unix (macOS/BSD 104 byte termasuk NUL;
	// Linux 108); dipakai yang terketat.
	sunPathMax     = 104
	muxReadyLimit  = 90 * time.Second
	muxPoll        = 200 * time.Millisecond
	muxCheckLimit  = 10 * time.Second
	muxCloseLimit  = 10 * time.Second
	muxStderrLimit = 8 << 10
)

// sshMux — satu sambungan ssh master (ControlMaster) untuk semua saluran
// `easyrun claude`: version, ensure, tangan, dan UI berbagi satu sambungan =
// satu sesi SSH box (orchestrator membatasi 4 per instance).
type sshMux struct {
	path   string // ControlPath apa adanya
	quoted string // ControlPath sebagai nilai -o
	cmd    *exec.Cmd
	exited chan struct{}
	stderr *tailBuffer
	closed bool
}

// controlPath — path soket pendek: <config>/cm/<acak>; bila folder config
// terlalu panjang untuk alamat soket unix, folder sementara milik pengguna.
func controlPath(d config.Dir) (string, error) {
	name := randomName(controlNameLen)
	for _, dir := range controlDirs(d) {
		p := filepath.Join(dir, name)
		if len(p)+controlTempSuffix >= sunPathMax {
			continue
		}
		if err := ensurePrivateDir(dir); err != nil {
			continue
		}
		return p, nil
	}
	return "", fmt.Errorf("tidak ada folder pendek untuk soket ssh (alamat soket unix ≤ %d byte); pakai --config-dir dengan path lebih pendek", sunPathMax-1)
}

// controlAlphabet — huruf nama soket acak (tanpa l/o/0/1).
const controlAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

func randomName(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = controlAlphabet[int(b[i])%len(controlAlphabet)]
	}
	return string(b)
}

// startMux — jalankan master (`-N`, BatchMode) dan tunggu sampai soketnya
// menjawab `-O check`. Master adalah proses anak: ditutup `close`. Master
// yatim jalan sebelumnya dibersihkan dulu (reapMasters).
func (a *App) startMux(ctx context.Context, c *boxConn, d config.Dir) (*sshMux, error) {
	a.reapMasters(c, d)
	path, err := controlPath(d)
	if err != nil {
		return nil, err
	}
	q, err := sshconf.QuoteConfigPath(path, a.GOOS)
	if err != nil {
		return nil, err
	}
	// Pemilik dicatat SEBELUM soket ada: easyrun lain yang sedang
	// membersihkan tidak pernah melihat soket hidup tanpa pemilik.
	pidPath := path + controlPidSuffix
	if err := writeMuxOwner(pidPath, muxOwner{Owner: os.Getpid()}); err != nil {
		return nil, err
	}
	m := &sshMux{path: path, quoted: q, exited: make(chan struct{}), stderr: &tailBuffer{max: muxStderrLimit}}
	argv := append(append([]string(nil), c.base...),
		"-o", "ControlMaster=yes", "-o", "ControlPath="+q, "-o", "ControlPersist=no",
		"-o", "BatchMode=yes", "-N", "-T", c.host)
	m.cmd = exec.Command(c.sshPath, argv[1:]...)
	m.cmd.Env = c.env
	// Linux: master ikut berhenti (SIGTERM) bila easyrun mati mendadak.
	m.cmd.SysProcAttr = masterProcAttr()
	// stderr master (termasuk pesan `easyrun tunnel`) disimpan, bukan ke
	// layar: selama attach layar milik UI.
	m.cmd.Stderr = m.stderr
	if err := m.cmd.Start(); err != nil {
		_ = os.Remove(pidPath)
		return nil, fmt.Errorf("menjalankan ssh: %w", err)
	}
	_ = writeMuxOwner(pidPath, muxOwner{Owner: os.Getpid(), Master: m.cmd.Process.Pid})
	go func() {
		_ = m.cmd.Wait()
		close(m.exited)
	}()
	deadline := time.Now().Add(muxReadyLimit)
	for {
		select {
		case <-m.exited:
			err := m.failure()
			m.close(c)
			return nil, err
		case <-ctx.Done():
			m.close(c)
			return nil, ctx.Err()
		case <-time.After(muxPoll):
		}
		if _, err := os.Lstat(path); err == nil && m.check(c) {
			return m, nil
		}
		if time.Now().After(deadline) {
			m.close(c)
			return nil, fmt.Errorf("sambungan SSH ke box belum siap setelah %s%s", muxReadyLimit, m.detail())
		}
	}
}

func (m *sshMux) detail() string {
	if s := strings.TrimSpace(m.stderr.String()); s != "" {
		lines := strings.Split(s, "\n")
		if len(lines) > 6 {
			lines = lines[len(lines)-6:]
		}
		for i, l := range lines {
			lines[i] = api.Clean(l)
		}
		return ":\n  " + strings.Join(lines, "\n  ")
	}
	return ""
}

// muxError — master gagal dibuka.
type muxError struct {
	msg string
	// auth — sshd box menolak autentikasi ("Permission denied"), mis.
	// sertifikat kedaluwarsa atau akses dicabut: mengulang dengan bahan yang
	// sama tidak akan berhasil.
	auth bool
}

func (e *muxError) Error() string { return e.msg }

func (m *sshMux) failure() error {
	code := m.cmd.ProcessState.ExitCode()
	return &muxError{
		msg:  fmt.Sprintf("ssh berhenti (kode %d) sebelum sambungan siap%s", code, m.detail()),
		auth: strings.Contains(m.stderr.String(), "Permission denied"),
	}
}

// control — `ssh -O <op>` ke master ini.
func (m *sshMux) control(c *boxConn, op string, limit time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	x := exec.CommandContext(ctx, c.sshPath, "-o", "ControlPath="+m.quoted, "-O", op, c.host)
	x.Env = c.env
	return x.Run() == nil
}

func (m *sshMux) check(c *boxConn) bool { return m.control(c, "check", muxCheckLimit) }

// alive — master masih jalan dan belum ditutup.
func (m *sshMux) alive() bool { return m != nil && !m.closed && m.running() }

func (m *sshMux) running() bool {
	select {
	case <-m.exited:
		return false
	default:
		return true
	}
}

// close — `-O exit`, tunggu master keluar, lalu hentikan paksa bila perlu;
// soket sisa dihapus. Idempoten.
func (m *sshMux) close(c *boxConn) {
	if m == nil || m.closed {
		return
	}
	m.closed = true
	if m.running() {
		m.control(c, "exit", muxCloseLimit)
	}
	claudehand.StopProcess(m.cmd.Process, m.exited, 3*time.Second)
	_ = os.Remove(m.path)
	_ = os.Remove(m.path + controlPidSuffix)
}

// ---------- master yatim ----------

const (
	// controlPidSuffix — "<soket>.pid": pemilik master (pid easyrun) dan pid
	// master, supaya jalan berikutnya bisa membersihkan master yatim.
	controlPidSuffix = ".pid"
	reapControlLimit = 5 * time.Second
	reapExitWait     = 3 * time.Second
)

// muxOwner — isi <soket>.pid.
type muxOwner struct {
	Owner  int `json:"owner"`  // pid easyrun yang membuka master
	Master int `json:"master"` // pid ssh master (0 = belum dijalankan)
}

func writeMuxOwner(path string, o muxOwner) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// controlNameOK — nama soket buatan controlPath (randomName), bukan berkas
// lain (termasuk soket sementara "<nama>.<16 acak>" milik ssh yang sedang
// dibuat, dan berkas sementara WriteFileAtomic).
func controlNameOK(name string) bool {
	if len(name) != controlNameLen {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune(controlAlphabet, r) {
			return false
		}
	}
	return true
}

// reapMasters — bersihkan master yatim sebelum membuka master baru. Master
// `-N ControlPersist=no` tidak tahu easyrun-nya mati mendadak (SIGKILL,
// panic, laptop dimatikan paksa): ia tetap memegang satu dari 4 sesi SSH box
// dan soketnya tertinggal. Hanya folder soket easyrun milik pengguna ini
// (0700, bukan symlink) dan hanya nama buatan controlPath yang disentuh:
//
//   - <nama>.pid dengan pemilik yang masih hidup → dibiarkan (jalan lain);
//   - pemilik sudah mati → `ssh -O exit` lewat soketnya; bila master masih
//     hidup (mis. soketnya terhapus) dan argv-nya memuat path soket itu →
//     SIGTERM; lalu soket mati & pidfile dihapus;
//   - soket tanpa pidfile (easyrun versi lama) → dihapus hanya bila mati
//     (sambungan ditolak); yang masih menjawab dibiarkan.
func (a *App) reapMasters(c *boxConn, d config.Dir) {
	for _, dir := range controlDirs(d) {
		if !privateDirOK(dir) {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		owned := map[string]bool{}
		for _, e := range ents {
			if base, ok := strings.CutSuffix(e.Name(), controlPidSuffix); ok && controlNameOK(base) {
				owned[base] = true
				a.reapOwned(c, dir, base)
			}
		}
		for _, e := range ents {
			if name := e.Name(); controlNameOK(name) && !owned[name] {
				if p := filepath.Join(dir, name); ownedSocket(p) && socketDead(p) {
					_ = os.Remove(p)
				}
			}
		}
	}
}

func (a *App) reapOwned(c *boxConn, dir, base string) {
	pidPath := filepath.Join(dir, base+controlPidSuffix)
	sock := filepath.Join(dir, base)
	if !ownedRegular(pidPath) {
		return
	}
	raw, err := os.ReadFile(pidPath)
	var o muxOwner
	if err != nil || json.Unmarshal(raw, &o) != nil || o.Owner <= 0 {
		return // ragu (mis. sedang ditulis) → biarkan
	}
	if o.Owner == os.Getpid() || processAlive(o.Owner) {
		return
	}
	q, qerr := sshconf.QuoteConfigPath(sock, a.GOOS)
	sent := false
	if ownedSocket(sock) && qerr == nil {
		sent = (&sshMux{path: sock, quoted: q}).control(c, "exit", reapControlLimit)
	}
	if o.Master > 0 {
		for end := time.Now().Add(reapExitWait); sent && processAlive(o.Master) && time.Now().Before(end); {
			time.Sleep(100 * time.Millisecond)
		}
		// Master masih hidup (soketnya terhapus, atau tidak menjawab): hanya
		// dihentikan bila argv-nya memuat path soket acak ini.
		if processAlive(o.Master) && processArgsContain(o.Master, sock, q) {
			_ = terminatePID(o.Master)
		}
	}
	if ownedSocket(sock) && socketDead(sock) {
		_ = os.Remove(sock)
	}
	_ = os.Remove(pidPath)
}
