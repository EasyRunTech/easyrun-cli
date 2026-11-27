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
// mengirim SIGHUP ke ProxyCommand saat selesai). Fungsi stop-nya
// mengembalikan perilaku sinyal bawaan.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), exitSignals(signal.Ignored)...)
}

// exitSignals — sinyal yang membatalkan SignalContext. SIGHUP yang sudah
// diabaikan sejak awal (nohup) tidak didaftarkan: signal.Notify memasang
// handler yang membatalkan "abaikan" itu, sehingga `nohup easyrun expose … &`
// ikut berhenti saat terminal ditutup.
func exitSignals(ignored func(os.Signal) bool) []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}
