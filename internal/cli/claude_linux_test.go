//go:build linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMasterIkutMatiBersamaEasyrun — temuan 8 (Linux): easyrun yang mati
// mendadak (SIGKILL) tidak meninggalkan master: master menerima SIGTERM
// (Pdeathsig) dan menghapus soketnya.
func TestMasterIkutMatiBersamaEasyrun(t *testing.T) {
	if a := masterProcAttr(); a == nil || a.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("masterProcAttr: %+v", a)
	}
	ch := newClaudeHarness(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(ch.bin, "pdeath-parent")
	if err := os.Symlink(self, parent); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(ch.tmpRoot, "pdeathsock")
	cmd := exec.Command(parent, filepath.Join(ch.bin, "ssh"), sock)
	cmd.Env = ch.a.environ()
	out, _ := cmd.Output() // induk mati karena SIGKILL
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid master: %q %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if st, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || st.Signal() != syscall.SIGKILL {
		t.Fatalf("induk harus mati karena SIGKILL: %v", cmd.ProcessState)
	}
	waitFor(t, "master berhenti setelah easyrun mati", func() bool {
		_, err := os.Lstat(sock)
		return errors.Is(err, os.ErrNotExist)
	})
}
