//go:build windows

package cli

import (
	"errors"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// Windows: OpenSSH Win32 tidak punya ControlMaster; attach/hand ditolak
// lebih awal di cmdClaude.
func controlDirs(config.Dir) []string { return nil }

func ensurePrivateDir(string) error { return errors.New("tidak didukung di Windows") }
