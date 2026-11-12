//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// execSSH — Windows tidak punya exec: jalankan ssh sebagai proses anak dan
// teruskan kode keluarnya. Ctrl+C ditangkap SignalContext (proses ini tetap
// hidup) sementara ssh menerimanya sendiri dari konsol.
func execSSH(path string, argv, env []string) (int, error) {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func ignoreSIGPIPE() {}

// SignalContext — dibatalkan oleh Ctrl+C.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
