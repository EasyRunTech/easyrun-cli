//go:build !linux

package cli

import "syscall"

// masterProcAttr — di luar Linux tidak ada Pdeathsig; master yatim
// dibersihkan reapMasters di jalan berikutnya.
func masterProcAttr() *syscall.SysProcAttr { return nil }
