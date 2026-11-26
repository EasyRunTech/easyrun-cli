package cli

import (
	"os"
	"os/signal"
)

// RestoreInterrupt — kembalikan perilaku bawaan Ctrl-C saja (proses langsung
// berhenti), dipakai `expose` setelah sinyal berhenti pertama lewat
// App.StopSignals. SIGTERM dan SIGHUP sengaja tetap tertangkap
// SignalContext: zsh mengirim dua SIGHUP saat jendela terminal ditutup, dan
// SIGHUP kedua tidak boleh membunuh proses sebelum tunnel sementara yang
// terlanjur dibuat dihapus.
func RestoreInterrupt() { signal.Reset(os.Interrupt) }
