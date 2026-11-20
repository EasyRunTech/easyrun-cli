package cli

import (
	"context"
	"crypto/rand"
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

func randomName(n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// startMux — jalankan master (`-N`, BatchMode) dan tunggu sampai soketnya
// menjawab `-O check`. Master adalah proses anak: ditutup `close`.
func (a *App) startMux(ctx context.Context, c *boxConn, d config.Dir) (*sshMux, error) {
	path, err := controlPath(d)
	if err != nil {
		return nil, err
	}
	q, err := sshconf.QuoteConfigPath(path, a.GOOS)
	if err != nil {
		return nil, err
	}
	m := &sshMux{path: path, quoted: q, exited: make(chan struct{}), stderr: &tailBuffer{max: muxStderrLimit}}
	argv := append(append([]string(nil), c.base...),
		"-o", "ControlMaster=yes", "-o", "ControlPath="+q, "-o", "ControlPersist=no",
		"-o", "BatchMode=yes", "-N", "-T", c.host)
	m.cmd = exec.Command(c.sshPath, argv[1:]...)
	m.cmd.Env = c.env
	// stderr master (termasuk pesan `easyrun tunnel`) disimpan, bukan ke
	// layar: selama attach layar milik UI.
	m.cmd.Stderr = m.stderr
	if err := m.cmd.Start(); err != nil {
		return nil, fmt.Errorf("menjalankan ssh: %w", err)
	}
	go func() {
		_ = m.cmd.Wait()
		close(m.exited)
	}()
	deadline := time.Now().Add(muxReadyLimit)
	for {
		select {
		case <-m.exited:
			return nil, m.failure()
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

func (m *sshMux) failure() error {
	code := m.cmd.ProcessState.ExitCode()
	return fmt.Errorf("ssh berhenti (kode %d) sebelum sambungan siap%s", code, m.detail())
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
}
