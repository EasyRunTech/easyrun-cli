package claudehand

import (
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EnvConfigDir — config Claude Code; tangan memakai folder terpisah.
const EnvConfigDir = "CLAUDE_CONFIG_DIR"

// DroppedEnv — token yang TIDAK diwariskan ke `claude mcp serve` (kontrak P2
// §4): server jalan tanpa login, jadi tidak butuh dan tidak boleh memegangnya.
var DroppedEnv = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"}

// ServerEnv — env untuk `claude mcp serve`: base tanpa DroppedEnv, dengan
// CLAUDE_CONFIG_DIR = configDir.
func ServerEnv(base []string, configDir string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		drop := k == EnvConfigDir
		for _, d := range DroppedEnv {
			if k == d {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, EnvConfigDir+"="+configDir)
}

// ServerCommand — cara menjalankan `claude mcp serve`.
type ServerCommand struct {
	Argv   []string // mis. {"/path/claude", "mcp", "serve"}
	Dir    string   // cwd = root
	Env    []string // sudah lewat ServerEnv
	Stderr io.Writer
}

// serverDrainGrace — setelah proses keluar, pembaca diberi waktu sekian untuk
// menghabiskan ekor stdout sebelum pipa ditutup paksa (cucu yang mewarisi
// stdout bisa menahan pipa tetap terbuka).
const serverDrainGrace = 2 * time.Second

// Start — nyalakan proses; Stop menutup stdin, menunggu sebentar, lalu
// menghentikannya.
//
// Stdout memakai pipa milik sendiri (os.Pipe), bukan cmd.StdoutPipe: Wait
// menutup StdoutPipe begitu proses keluar, padahal penjaga mungkin belum
// membaca ekor keluarannya (dokumentasi os/exec: Wait tidak boleh dipanggil
// sebelum semua pembacaan selesai) — ekor hilang dan pembaca mendapat
// "file already closed".
func (c ServerCommand) Start() (*Server, error) {
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stderr = c.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	_ = pw.Close() // salinan milik anak tetap terbuka sampai ia keluar
	out := &pipeReader{f: pr}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
		time.AfterFunc(serverDrainGrace, out.close)
	}()
	var once sync.Once
	return &Server{In: in, Out: out, Stop: func() {
		once.Do(func() {
			_ = in.Close()
			StopProcess(cmd.Process, exited, 2*time.Second)
			out.close()
		})
	}}, nil
}

// pipeReader — ujung baca stdout server. Ditutup oleh kita sendiri (Stop,
// atau habis masa tenggang setelah proses keluar) → pembaca menerima EOF,
// bukan galat "file already closed".
type pipeReader struct {
	f      *os.File
	once   sync.Once
	closed atomic.Bool
}

func (p *pipeReader) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if err != nil && p.closed.Load() {
		err = io.EOF
	}
	return n, err
}

func (p *pipeReader) close() {
	p.once.Do(func() {
		p.closed.Store(true)
		_ = p.f.Close()
	})
}

// StopProcess — tunggu proses keluar sendiri selama grace, lalu minta
// berhenti (SIGTERM; Windows: kill), lalu bunuh.
func StopProcess(p *os.Process, exited <-chan struct{}, grace time.Duration) {
	if p == nil {
		return
	}
	select {
	case <-exited:
		return
	case <-time.After(grace):
	}
	_ = terminate(p)
	select {
	case <-exited:
		return
	case <-time.After(3 * time.Second):
	}
	_ = p.Kill()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
	}
}

// Terminate — minta proses berhenti dengan sopan (SIGTERM; Windows: kill).
func Terminate(p *os.Process) error { return terminate(p) }

// LocalUser — nama pengguna laptop (seperti getpass.getuser()).
func LocalUser(getenv func(string) string) string {
	for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
		if v := getenv(k); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
