//go:build windows

package claudehand

import (
	"os"
	"runtime"
)

func terminate(p *os.Process) error { return p.Kill() }

// OSString — Windows tidak punya uname.
func OSString() string { return "Windows " + runtime.GOARCH }
