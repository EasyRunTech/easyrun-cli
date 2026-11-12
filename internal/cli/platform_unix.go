//go:build !windows

package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// execSSH — ganti proses ini dengan ssh (tidak kembali bila berhasil), jadi
// sinyal, terminal, dan kode keluar langsung milik ssh.
func execSSH(path string, argv, env []string) (int, error) {
	return 1, syscall.Exec(path, argv, env)
}

// ignoreSIGPIPE — bila ssh menutup pipanya, tulisan ke stdout mengembalikan
// EPIPE (ditangani: WebSocket ditutup rapi) alih-alih membunuh proses.
func ignoreSIGPIPE() { signal.Ignore(syscall.SIGPIPE) }

// SignalContext — dibatalkan oleh Ctrl+C, SIGTERM, atau SIGHUP (ssh
// mengirim SIGHUP ke ProxyCommand saat selesai).
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
