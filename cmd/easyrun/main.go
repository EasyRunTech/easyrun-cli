// easyrun — program baris perintah EasyRun.
//
// Fase ini baru berisi --version, --help, dan --self-update. Perintah SSH
// (login, ssh, tunnel, ssh-config) ditambahkan mengikuti rancangan 898d5d55.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/EasyRunTech/easyrun-cli/internal/version"
)

// installerURL — skrip pemasang yang sama dengan instruksi di README. Bisa
// diganti lewat env untuk mirror/uji lokal.
const installerURL = "https://raw.githubusercontent.com/EasyRunTech/easyrun-cli/main/install.sh"

const envInstallerURL = "EASYRUN_INSTALLER_URL"

func usage(w io.Writer) {
	fmt.Fprintf(w, `easyrun %s — program baris perintah EasyRun

Pemakaian:
  easyrun --version       tampilkan versi
  easyrun --self-update   perbarui ke versi terbaru
  easyrun --help          tampilkan bantuan ini

Segera hadir: easyrun login, easyrun ssh <box>, easyrun ssh-config <box>.
`, version.Version)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 0
	}
	switch args[0] {
	case "--version", "-v", "version":
		fmt.Fprintln(stdout, "easyrun", version.Version)
		return 0
	case "--help", "-h", "help":
		usage(stdout)
		return 0
	case "--self-update", "self-update":
		return selfUpdate(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "easyrun: perintah tidak dikenal: %s\n\n", args[0])
		usage(stderr)
		return 2
	}
}

// selfUpdate — jalankan skrip pemasang yang sama, dengan folder tujuan =
// folder tempat program ini berada, supaya versi lama tertimpa di tempatnya.
func selfUpdate(stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "easyrun: tidak bisa menemukan lokasi program:", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	url := installerURL
	if v := os.Getenv(envInstallerURL); v != "" {
		url = v
	}
	cmd := exec.Command("bash", "-c", `curl -fsSL "$1" | bash`, "easyrun-installer", url)
	cmd.Env = append(os.Environ(), "EASYRUN_INSTALL_DIR="+filepath.Dir(exe))
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, "easyrun: pembaruan gagal:", err)
		return 1
	}
	return 0
}
