package claudehand

import (
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
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

// Start — nyalakan proses; Stop menutup stdin, menunggu sebentar, lalu
// menghentikannya.
func (c ServerCommand) Start() (*Server, error) {
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stderr = c.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	var once sync.Once
	return &Server{In: in, Out: out, Stop: func() {
		once.Do(func() {
			_ = in.Close()
			StopProcess(cmd.Process, exited, 2*time.Second)
		})
	}}, nil
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
