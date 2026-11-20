//go:build windows

package cli

import (
	"errors"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// Windows: OpenSSH Win32 tidak punya ControlMaster; attach/hand ditolak
// lebih awal di cmdClaude.
func controlDirs(config.Dir) []string { return nil }

// controlTempRoots — padanan unix (tidak dipakai di Windows).
var controlTempRoots = func() []string { return nil }

func ensurePrivateDir(string) error { return errors.New("tidak didukung di Windows") }

// Pembersih master yatim tidak dipakai di Windows (controlDirs kosong).
func privateDirOK(string) bool                        { return false }
func ownedSocket(string) bool                         { return false }
func ownedRegular(string) bool                        { return false }
func socketDead(string) bool                          { return false }
func processAlive(int) bool                           { return false }
func terminatePID(int) error                          { return errors.New("tidak didukung di Windows") }
func processArgsContain(pid int, subs ...string) bool { return false }
