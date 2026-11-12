package config

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAPIURL(t *testing.T) {
	ok := map[string]string{
		"https://api.easyrun.io":          "https://api.easyrun.io",
		"https://api.easyrun.io/":         "https://api.easyrun.io",
		"https://api.easyrun.io/api/v1":   "https://api.easyrun.io",
		"https://api.easyrun.io/api/v1/":  "https://api.easyrun.io",
		" https://x.example:8443/panel ":  "https://x.example:8443/panel",
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080",
		"http://localhost:3000/api/v1":    "http://localhost:3000",
		"http://[::1]:9000":               "http://[::1]:9000",
		"https://x.example/panel/api/v1/": "https://x.example/panel",
	}
	for in, want := range ok {
		got, err := NormalizeAPIURL(in)
		if err != nil || got != want {
			t.Errorf("%q → %q, %v; ingin %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "api.easyrun.io", "http://api.easyrun.io", "ftp://x.example",
		"https://user:pw@x.example", "https://x.example/?a=1", "https://x.example/#f",
		"javascript:alert(1)", "http://10.0.0.1",
	}
	for _, in := range bad {
		if got, err := NormalizeAPIURL(in); err == nil {
			t.Errorf("%q seharusnya ditolak, dapat %q", in, got)
		}
	}
}

func TestResolveAPIURLUrutan(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	cfg := Config{APIURL: "https://dari-config.example"}

	if got, _ := ResolveAPIURL("", getenv, Config{}); got != DefaultAPIURL {
		t.Fatalf("bawaan: %q", got)
	}
	if got, _ := ResolveAPIURL("", getenv, cfg); got != "https://dari-config.example" {
		t.Fatalf("config: %q", got)
	}
	env[EnvAPIURL] = "https://dari-env.example/api/v1"
	if got, _ := ResolveAPIURL("", getenv, cfg); got != "https://dari-env.example" {
		t.Fatalf("env menang atas config: %q", got)
	}
	if got, _ := ResolveAPIURL("https://dari-flag.example", getenv, cfg); got != "https://dari-flag.example" {
		t.Fatalf("flag menang atas env: %q", got)
	}
	env[EnvAPIURL] = "http://bukan-loopback.example"
	if _, err := ResolveAPIURL("", getenv, cfg); err == nil {
		t.Fatal("env http non-loopback harus ditolak")
	}
}

func TestLocate(t *testing.T) {
	p, err := Locate(func(k string) string {
		if k == EnvConfigDir {
			return "rel/dir"
		}
		return ""
	})
	if err != nil || !filepath.IsAbs(p) || !strings.HasSuffix(p, filepath.Join("rel", "dir")) {
		t.Fatalf("EASYRUN_CONFIG_DIR: %q %v", p, err)
	}
	p, err = Locate(func(string) string { return "" })
	if err != nil || filepath.Base(p) != ".easyrun" {
		t.Fatalf("bawaan ~/.easyrun: %q %v", p, err)
	}
}

func perm(t *testing.T, p string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bit izin Unix tidak berlaku di Windows")
	}
}

func TestCredentialsIzinDanAtomik(t *testing.T) {
	skipOnWindows(t)
	d := Dir{Path: filepath.Join(t.TempDir(), ".easyrun")}
	exp := time.Date(2026, 12, 11, 16, 10, 0, 0, time.UTC)
	in := Credentials{APIURL: "https://api.easyrun.io", AccessToken: "erc_rahasia", TokenID: "t1", ExpiresAt: exp}
	if err := d.SaveCredentials(in); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, d.Path); got != 0o700 {
		t.Fatalf("folder %04o, ingin 0700", got)
	}
	if got := perm(t, d.SSHDir()); got != 0o700 {
		t.Fatalf("folder ssh %04o, ingin 0700", got)
	}
	if got := perm(t, d.CredentialsPath()); got != 0o600 {
		t.Fatalf("credentials.json %04o, ingin 0600", got)
	}
	// Tidak ada berkas sementara yang tertinggal.
	entries, _ := os.ReadDir(d.Path)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("berkas sementara tertinggal: %s", e.Name())
		}
	}
	out, err := d.LoadCredentials(false)
	if err != nil {
		t.Fatal(err)
	}
	if out.AccessToken != in.AccessToken || !out.ExpiresAt.Equal(exp) || out.TokenID != "t1" {
		t.Fatalf("isi berubah: %+v", out)
	}

	// Timpa: izin tetap 0600 walau berkas lama sempat dibuka lebar.
	if err := os.Chmod(d.CredentialsPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = d.LoadCredentials(false)
	var pe *PermError
	if !errors.As(err, &pe) {
		t.Fatalf("izin 0644 harus ditolak, dapat %v", err)
	}
	if _, err := d.LoadCredentials(true); err != nil {
		t.Fatalf("logout tetap boleh membaca untuk mencabut: %v", err)
	}
	if err := d.SaveCredentials(in); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, d.CredentialsPath()); got != 0o600 {
		t.Fatalf("setelah ditimpa %04o, ingin 0600", got)
	}

	if err := d.RemoveCredentials(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.LoadCredentials(false); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("setelah dihapus: %v", err)
	}
	if err := d.RemoveCredentials(); err != nil {
		t.Fatalf("hapus dua kali harus sukses: %v", err)
	}
}

func TestEnsureMerapatkanFolder(t *testing.T) {
	skipOnWindows(t)
	root := filepath.Join(t.TempDir(), ".easyrun")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var warn bytes.Buffer
	d := Dir{Path: root, Warn: &warn}
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if got := perm(t, root); got != 0o700 {
		t.Fatalf("folder %04o, ingin 0700", got)
	}
	if !strings.Contains(warn.String(), "dirapatkan") {
		t.Fatalf("peringatan tidak muncul: %q", warn.String())
	}
}

func TestWriteFileExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kunci")
	if err := WriteFileExclusive(p, []byte("satu"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileExclusive(p, []byte("dua"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("tulisan kedua harus ErrExist, dapat %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "satu" {
		t.Fatalf("isi tertimpa: %q", b)
	}
}

func TestConfigDanSertifikat(t *testing.T) {
	d := Dir{Path: filepath.Join(t.TempDir(), "cfg")}
	c, err := d.LoadConfig()
	if err != nil || c.APIURL != "" {
		t.Fatalf("config kosong: %+v %v", c, err)
	}
	if err := d.SaveConfig(Config{APIURL: "https://x.example"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.LoadConfig(); c.APIURL != "https://x.example" {
		t.Fatalf("config: %+v", c)
	}
	for _, n := range []string{"a-cert.pub", "b-cert.pub"} {
		if err := os.WriteFile(filepath.Join(d.SSHDir(), n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(d.KeyPath(), []byte("kunci"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveCertificates(); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(d.SSHDir(), "*"))
	if len(left) != 1 || filepath.Base(left[0]) != "id_ed25519" {
		t.Fatalf("hanya sertifikat yang dihapus, sisa: %v", left)
	}
	if got := d.CertPath("abc"); filepath.Base(got) != "abc-cert.pub" {
		t.Fatalf("CertPath: %s", got)
	}
}
