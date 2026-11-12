package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// installerURL — skrip pemasang yang sama dengan instruksi di README. Bisa
// diganti lewat env untuk mirror/uji lokal.
const installerURL = "https://raw.githubusercontent.com/EasyRunTech/easyrun-cli/main/install.sh"

const envInstallerURL = "EASYRUN_INSTALLER_URL"

// selfUpdate — jalankan skrip pemasang yang sama, dengan folder tujuan =
// folder tempat program ini berada, supaya versi lama tertimpa di tempatnya.
func (a *App) selfUpdate() int {
	exe, err := a.Executable()
	if err != nil {
		fmt.Fprintln(a.Stderr, "easyrun: tidak bisa menemukan lokasi program:", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	url := installerURL
	if v := a.Getenv(envInstallerURL); v != "" {
		url = v
	}
	cmd := exec.Command("bash", "-c", `curl -fsSL "$1" | bash`, "easyrun-installer", url)
	env := os.Environ()
	if a.Environ != nil {
		env = a.Environ()
	}
	cmd.Env = append(env, "EASYRUN_INSTALL_DIR="+filepath.Dir(exe))
	cmd.Stdout, cmd.Stderr, cmd.Stdin = a.Stdout, a.Stderr, a.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(a.Stderr, "easyrun: pembaruan gagal:", err)
		return 1
	}
	return 0
}
