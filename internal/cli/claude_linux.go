//go:build linux

package cli

import "syscall"

// masterProcAttr — Linux: master ssh menerima SIGTERM bila proses easyrun
// mati mendadak (SIGKILL, panic), jadi tidak tertinggal memegang satu sesi
// SSH box. (Pdeathsig terikat pada thread induk; runtime Go tidak
// mengakhiri thread kecuali goroutine ber-LockOSThread selesai, dan easyrun
// tidak memakainya.)
func masterProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
