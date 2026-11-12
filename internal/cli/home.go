package cli

import (
	"os"
	"os/user"
	"runtime"
)

// accountHomeDir — home akun dari basis data pengguna (passwd / Directory
// Services): yang dipakai OpenSSH untuk ~/.ssh/config dan "~" di Include,
// bukan $HOME. Windows: OpenSSH Win32 memakai profil pengguna, sama dengan
// os.UserHomeDir. Gagal membaca akun → os.UserHomeDir.
func accountHomeDir() (string, error) {
	if runtime.GOOS != "windows" {
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			return u.HomeDir, nil
		}
	}
	return os.UserHomeDir()
}
