//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// controlDirs — calon folder soket ControlMaster, dari yang paling disukai:
// <config>/cm, lalu folder sementara milik pengguna ini (TMPDIR, lalu /tmp)
// bila path folder config terlalu panjang untuk alamat soket unix.
func controlDirs(d config.Dir) []string {
	uid := strconv.Itoa(os.Getuid())
	dirs := []string{filepath.Join(d.Path, controlDirName), filepath.Join(os.TempDir(), "easyrun-"+uid)}
	if os.TempDir() != "/tmp" {
		dirs = append(dirs, filepath.Join("/tmp", "easyrun-"+uid))
	}
	return dirs
}

// ensurePrivateDir — folder 0700 milik pengguna ini, bukan symlink (folder
// di /tmp bisa disiapkan orang lain lebih dulu).
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("bukan folder")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s milik pengguna lain", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}
