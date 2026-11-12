package sshconf

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestEnsureIncludeBerkasBaru(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".ssh", "config")
	res, err := EnsureInclude(cfg, t.TempDir())
	if err != nil || !res.Created || !res.Inserted || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := readFile(t, cfg); got != IncludeLine+"\n" {
		t.Fatalf("isi %q", got)
	}
	if runtime.GOOS != "windows" {
		if m := perm(t, cfg); m != 0o600 {
			t.Fatalf("config %04o", m)
		}
		if m := perm(t, filepath.Dir(cfg)); m != 0o700 {
			t.Fatalf("~/.ssh %04o", m)
		}
	}
	// Idempoten.
	res, err = EnsureInclude(cfg, t.TempDir())
	if err != nil || res.Inserted || res.Created {
		t.Fatalf("panggilan kedua: %+v %v", res, err)
	}
}

func TestEnsureIncludeMenyisipkanSatuBarisSaja(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	orig := "# punya saya\nHost *\n    User root\n    ProxyJump x\n    ForwardAgent yes\n\nHost kerja\n    HostName 10.0.0.1\n"
	if err := os.WriteFile(cfg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureInclude(cfg, t.TempDir())
	if err != nil || !res.Inserted || res.Created || res.Backup != cfg+BackupSuffix {
		t.Fatalf("%+v %v", res, err)
	}
	if got := readFile(t, cfg); got != IncludeLine+"\n"+orig {
		t.Fatalf("selain satu baris di puncak, isi harus sama persis:\n%q", got)
	}
	if got := readFile(t, cfg+BackupSuffix); got != orig {
		t.Fatalf("cadangan %q", got)
	}
	if runtime.GOOS != "windows" {
		if m := perm(t, cfg); m != 0o644 {
			t.Fatalf("izin config berubah: %04o", m)
		}
		if m := perm(t, cfg+BackupSuffix); m != 0o600 {
			t.Fatalf("cadangan %04o", m)
		}
	}
	// Idempoten: tidak disisipi lagi, cadangan tidak disentuh.
	if res, err := EnsureInclude(cfg, t.TempDir()); err != nil || res.Inserted || res.Backup != "" {
		t.Fatalf("panggilan kedua: %+v %v", res, err)
	}
	if got := readFile(t, cfg); strings.Count(got, IncludeLine) != 1 {
		t.Fatalf("baris Include ganda:\n%s", got)
	}

	// Pengguna menghapus baris itu lalu mengubah berkasnya: disisipkan lagi,
	// tetapi cadangan tetap isi ASLI (dibuat sekali saja).
	edited := orig + "Host baru\n    User saya\n"
	_ = os.WriteFile(cfg, []byte(edited), 0o644)
	if res, err := EnsureInclude(cfg, t.TempDir()); err != nil || !res.Inserted || res.Backup != "" {
		t.Fatalf("sisip ulang: %+v %v", res, err)
	}
	if got := readFile(t, cfg); got != IncludeLine+"\n"+edited {
		t.Fatalf("sisip ulang: %q", got)
	}
	if got := readFile(t, cfg+BackupSuffix); got != orig {
		t.Fatalf("cadangan tertimpa: %q", got)
	}
}

func TestEnsureIncludeSudahDiAtas(t *testing.T) {
	cases := map[string]string{
		"baris pertama":            IncludeLine + "\nHost *\n    User root\n",
		"setelah komentar":         "# dotfiles\n\n" + IncludeLine + "\nHost *\n",
		"setelah Include lain":     "Include ~/.orbstack/ssh/config\n" + IncludeLine + "\nHost *\n",
		"bentuk = dan huruf kecil": "include=" + IncludePattern + "\nHost *\n",
		"dikutip, bersama pola":    `Include ~/.ssh/lain.conf "` + IncludePattern + "\"\nHost *\n",
		"berindentasi":             "  " + IncludeLine + "  \nHost *\n",
	}
	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config")
			_ = os.WriteFile(cfg, []byte(orig), 0o600)
			res, err := EnsureInclude(cfg, t.TempDir())
			if err != nil || res.Inserted || len(res.Stale) != 0 {
				t.Fatalf("%+v %v", res, err)
			}
			if got := readFile(t, cfg); got != orig {
				t.Fatalf("berkas berubah: %q", got)
			}
			if _, err := os.Stat(cfg + BackupSuffix); err == nil {
				t.Fatal("cadangan dibuat padahal tidak ada perubahan")
			}
		})
	}
}

func TestEnsureIncludeDiBawahHostBintang(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	orig := "Host *\n    User root\n" + IncludeLine + "\n"
	_ = os.WriteFile(cfg, []byte(orig), 0o600)
	res, err := EnsureInclude(cfg, t.TempDir())
	if err != nil || !res.Inserted || !reflect.DeepEqual(res.Stale, []int{3}) {
		t.Fatalf("%+v %v", res, err)
	}
	// Baris lama dibiarkan (tidak ada yang ditulis ulang); hanya disisipi.
	if got := readFile(t, cfg); got != IncludeLine+"\n"+orig {
		t.Fatalf("%q", got)
	}
	// Panggilan berikutnya: sudah di atas; baris lama tetap dilaporkan.
	res, err = EnsureInclude(cfg, t.TempDir())
	if err != nil || res.Inserted || !reflect.DeepEqual(res.Stale, []int{4}) {
		t.Fatalf("kedua: %+v %v", res, err)
	}
	// Direktif global sebelum Include kita (mis. ForwardAgent yes di luar
	// Host) akan menang → tidak dianggap "di atas".
	cfg2 := filepath.Join(t.TempDir(), "config")
	_ = os.WriteFile(cfg2, []byte("ForwardAgent yes\n"+IncludeLine+"\n"), 0o600)
	if res, err := EnsureInclude(cfg2, t.TempDir()); err != nil || !res.Inserted {
		t.Fatalf("global sebelum Include: %+v %v", res, err)
	}
}

func TestEnsureIncludeCRLFDanBOM(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	orig := "\xef\xbb\xbfHost *\r\n    User root\r\n"
	_ = os.WriteFile(cfg, []byte(orig), 0o600)
	if _, err := EnsureInclude(cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, cfg); got != "\xef\xbb\xbf"+IncludeLine+"\r\nHost *\r\n    User root\r\n" {
		t.Fatalf("%q", got)
	}
}

func TestEnsureIncludeSymlinkDotfiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "ssh_config")
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	_ = os.WriteFile(real, []byte("Host *\n    User root\n"), 0o644)
	cfg := filepath.Join(dir, ".ssh", "config")
	_ = os.MkdirAll(filepath.Dir(cfg), 0o700)
	if err := os.Symlink(real, cfg); err != nil {
		t.Fatal(err)
	}
	if res, err := EnsureInclude(cfg, t.TempDir()); err != nil || !res.Inserted {
		t.Fatalf("%+v %v", res, err)
	}
	if fi, err := os.Lstat(cfg); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink ~/.ssh/config diganti berkas biasa")
	}
	if got := readFile(t, real); !strings.HasPrefix(got, IncludeLine+"\nHost *\n") {
		t.Fatalf("berkas tujuan symlink: %q", got)
	}
	// Symlink putus → galat, bukan menimpa symlink.
	broken := filepath.Join(dir, "putus")
	_ = os.Symlink(filepath.Join(dir, "tidak-ada"), broken)
	if _, err := EnsureInclude(broken, t.TempDir()); err == nil {
		t.Fatal("symlink putus harus galat")
	}
}

func TestWriteBoxConf(t *testing.T) {
	sshDir := filepath.Join(t.TempDir(), ".ssh")
	p, existed, err := WriteBoxConf(sshDir, testID, "Host a\n")
	if err != nil || existed || p != filepath.Join(sshDir, "easyrun", testID+".conf") {
		t.Fatalf("%q %v %v", p, existed, err)
	}
	if _, existed, err = WriteBoxConf(sshDir, testID, "Host b\n"); err != nil || !existed {
		t.Fatalf("timpa: %v %v", existed, err)
	}
	if got := readFile(t, p); got != "Host b\n" {
		t.Fatalf("%q", got)
	}
	if runtime.GOOS != "windows" {
		if m := perm(t, p); m != 0o600 {
			t.Fatalf("%04o", m)
		}
		if m := perm(t, filepath.Dir(p)); m != 0o700 {
			t.Fatalf("folder %04o", m)
		}
	}
	if _, _, err := WriteBoxConf(sshDir, "../keluar", "x"); err == nil {
		t.Fatal("id dengan path harus ditolak")
	}
}

// Temuan review 8: Include lain di atas baris kita yang memuat `Host *`
// ber-opsi bentrok membuat baris kita disisipkan di baris 1; Include yang
// hanya untuk host tertentu (OrbStack) atau opsi tak berbahaya dibiarkan.
func TestEnsureIncludeIncludeLainYangMembayangi(t *testing.T) {
	cases := []struct {
		name, included string
		shadow         bool
	}{
		{"host tertentu", "Host orb\n    User orb\n    ProxyCommand orb-proxy\n", false},
		{"host bintang tak berbahaya", "Host *\n    UseKeychain yes\n    AddKeysToAgent yes\n    IdentityFile ~/.ssh/id_ed25519\n", false},
		{"host bintang User", "Host *\n    User root\n", true},
		{"di luar blok ProxyJump", "ProxyJump bastion\nHost lain\n    User x\n", true},
		{"match all ForwardAgent", "Match all\n    ForwardAgent yes\n", true},
		{"bersarang", "Include dalam.conf\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			sshDir := filepath.Join(home, ".ssh")
			_ = os.MkdirAll(sshDir, 0o700)
			_ = os.WriteFile(filepath.Join(sshDir, "lain.conf"), []byte(c.included), 0o600)
			_ = os.WriteFile(filepath.Join(sshDir, "dalam.conf"), []byte("Host *\n    User root\n"), 0o600)
			cfg := filepath.Join(sshDir, "config")
			orig := "Include ~/.ssh/lain.conf\n" + IncludeLine + "\n\nHost kerja\n    User budi\n"
			_ = os.WriteFile(cfg, []byte(orig), 0o600)
			res, err := EnsureInclude(cfg, home)
			if err != nil {
				t.Fatal(err)
			}
			got := readFile(t, cfg)
			if !c.shadow {
				if res.Inserted || got != orig || len(res.Shadowing) != 0 {
					t.Fatalf("harus dibiarkan: %+v\n%s", res, got)
				}
				return
			}
			if !res.Inserted || !strings.HasPrefix(got, IncludeLine+"\n") || got != IncludeLine+"\n"+orig {
				t.Fatalf("baris kita harus di baris 1: %+v\n%s", res, got)
			}
			if len(res.Shadowing) == 0 || !reflect.DeepEqual(res.Stale, []int{2}) {
				t.Fatalf("hasil %+v", res)
			}
			// Idempoten: jalan kedua tidak menyisipkan lagi.
			if res2, err := EnsureInclude(cfg, home); err != nil || res2.Inserted {
				t.Fatalf("jalan kedua: %+v %v", res2, err)
			}
		})
	}
}

// Temuan review 3: blok buatan easyrun untuk alias yang sama (box lain)
// dihapus; milik box ini dan berkas buatan tangan dibiarkan.
func TestRemoveStaleConfs(t *testing.T) {
	sshDir := t.TempDir()
	dir := filepath.Join(sshDir, IncludeDirName)
	_ = os.MkdirAll(dir, 0o700)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		_ = os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	mark := "# Ditulis ulang oleh `easyrun ssh-config uji --write`; perubahan manual akan tertimpa.\n"
	old := write("aaaa.conf", mark+"Match host uji exec \"easyrun ssh-prepare aaaa\"\n\nHost uji\n    HostName lama.easyrun.io\n")
	keep := write("ffff.conf", mark+"Host uji\n    HostName baru.easyrun.io\n")
	other := write("bbbb.conf", mark+"Host lain\n    HostName lain.easyrun.io\n")
	hand := write("00.conf", "Host UJI\n    User root\n")
	res, err := RemoveStaleConfs(sshDir, "ffff", "uji")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Removed, []string{old}) || !reflect.DeepEqual(res.Foreign, []string{hand}) {
		t.Fatalf("%+v", res)
	}
	for _, p := range []string{keep, other, hand} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s harus tetap ada", p)
		}
	}
	if _, err := RemoveStaleConfs(t.TempDir(), "ffff", "uji"); err != nil {
		t.Fatal("folder belum ada harus sukses")
	}
}
