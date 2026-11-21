//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// controlTempRoots — folder sementara calon induk folder soket (uji
// menggantinya supaya tidak menyentuh folder sementara sungguhan).
var controlTempRoots = func() []string {
	roots := []string{os.TempDir()}
	if os.TempDir() != "/tmp" {
		roots = append(roots, "/tmp")
	}
	return roots
}

// controlDirs — calon folder soket ControlMaster, dari yang paling disukai:
// <config>/cm, lalu folder sementara milik pengguna ini (TMPDIR, lalu /tmp)
// bila path folder config terlalu panjang untuk alamat soket unix.
func controlDirs(d config.Dir) []string {
	uid := strconv.Itoa(os.Getuid())
	dirs := []string{filepath.Join(d.Path, controlDirName)}
	for _, r := range controlTempRoots() {
		dirs = append(dirs, filepath.Join(r, "easyrun-"+uid))
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

// privateDirOK — folder soket easyrun milik pengguna ini: folder sungguhan
// (bukan symlink), 0700. Tidak dibuat bila belum ada.
func privateDirOK(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	return ownedByMe(fi)
}

func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func ownedSocket(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0 && ownedByMe(fi)
}

func ownedRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular() && ownedByMe(fi)
}

// socketDead — tidak ada yang mendengarkan di soket ini (sambungan ditolak);
// galat lain (mis. sibuk) dianggap hidup.
func socketDead(p string) bool {
	c, err := net.DialTimeout("unix", p, time.Second)
	if err == nil {
		_ = c.Close()
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// processAlive — proses MILIK PENGGUNA INI masih ada. EPERM (milik pengguna
// lain) = pid sudah dipakai ulang, bukan easyrun/ssh kita.
func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func terminatePID(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// processArgsContain — argv proses memuat salah satu teks (path soket acak
// = identitas master), supaya pid yang sudah dipakai ulang proses lain tidak
// ikut dihentikan.
func processArgsContain(pid int, subs ...string) bool {
	var args string
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		args = strings.ReplaceAll(string(b), "\x00", " ")
	} else {
		out, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		args = string(out)
	}
	for _, sub := range subs {
		if sub != "" && strings.Contains(args, sub) {
			return true
		}
	}
	return false
}
