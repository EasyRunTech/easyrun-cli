//go:build !windows

package claudehand

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// OSString — "Darwin 24.6.0 arm64" (seperti platform.system/release/machine).
func OSString() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	f := func(b []byte) string { return strings.TrimRight(string(b), "\x00") }
	return f(u.Sysname[:]) + " " + f(u.Release[:]) + " " + f(u.Machine[:])
}
