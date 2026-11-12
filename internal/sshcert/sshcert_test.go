package sshcert

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/EasyRunTech/easyrun-cli/internal/sshtest"
)

func paths(t *testing.T) (string, string) {
	dir := filepath.Join(t.TempDir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "id_ed25519.pub")
}

func TestEnsureKeyMembuatSekaliDanStabil(t *testing.T) {
	priv, pub := paths(t)
	k1, err := EnsureKey(priv, pub, "easyrun-cli")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(priv); fi.Mode().Perm() != 0o600 {
			t.Fatalf("kunci privat %04o, ingin 0600", fi.Mode().Perm())
		}
	}
	if !strings.HasPrefix(k1.AuthorizedKey, "ssh-ed25519 ") || !strings.HasSuffix(k1.AuthorizedKey, " easyrun-cli") {
		t.Fatalf("baris publik: %q", k1.AuthorizedKey)
	}
	pemBytes, _ := os.ReadFile(priv)
	if !bytes.Contains(pemBytes, []byte("BEGIN OPENSSH PRIVATE KEY")) {
		t.Fatal("format kunci bukan OpenSSH")
	}
	if strings.Contains(k1.AuthorizedKey, "PRIVATE") {
		t.Fatal("baris publik memuat kunci privat")
	}

	k2, err := EnsureKey(priv, pub, "easyrun-cli")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1.PublicKey.Marshal(), k2.PublicKey.Marshal()) {
		t.Fatal("kunci berganti pada panggilan kedua")
	}
	// .pub hilang → ditulis ulang dari kunci privat.
	_ = os.Remove(pub)
	if _, err := EnsureKey(priv, pub, "easyrun-cli"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(pub)
	if strings.TrimSpace(string(got)) != k1.AuthorizedKey {
		t.Fatalf(".pub ditulis ulang salah: %q", got)
	}
}

func TestEnsureKeyBersamaanSatuKunci(t *testing.T) {
	priv, pub := paths(t)
	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	errs := make([]error, 8)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := EnsureKey(priv, pub, "easyrun-cli")
			errs[i] = err
			if err == nil {
				keys[i] = k.PublicKey.Marshal()
			}
		}(i)
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil {
			t.Fatalf("proses %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("proses %d memakai kunci berbeda", i)
		}
	}
}

func TestEnsureKeyMenolakIzinTerbukaDanRusak(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bit izin Unix")
	}
	priv, pub := paths(t)
	if _, err := EnsureKey(priv, pub, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(priv, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureKey(priv, pub, ""); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("izin 0644 harus ditolak dengan petunjuk: %v", err)
	}
	if err := os.WriteFile(priv, []byte("bukan kunci"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(priv, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureKey(priv, pub, ""); err == nil || !strings.Contains(err.Error(), "rusak") {
		t.Fatalf("kunci rusak: %v", err)
	}
}

func TestCheckSertifikat(t *testing.T) {
	ca := sshtest.NewCA(t)
	key := sshtest.NewKey(t)
	other := sshtest.NewKey(t)
	now := time.Date(2026, 11, 11, 23, 0, 0, 0, time.Local)
	const box = "7b4c1f0e-2a3d-4e5f-8a9b-0c1d2e3f4a5b"
	mk := func(pub ssh.PublicKey, principal string, before time.Time, typ uint32) *ssh.Certificate {
		c, err := ca.Sign(pub, principal, now.Add(-time.Minute), before, typ)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	want := Want{Principal: box, Key: key, Now: now}
	cases := []struct {
		name string
		cert *ssh.Certificate
		ok   bool
		why  string
	}{
		{"segar 1 jam", mk(key, box, now.Add(time.Hour), ssh.UserCert), true, ""},
		{"sisa tepat 5 menit", mk(key, box, now.Add(5*time.Minute), ssh.UserCert), true, ""},
		{"sisa 4m59s", mk(key, box, now.Add(5*time.Minute-time.Second), ssh.UserCert), false, "sisa umur"},
		{"kedaluwarsa", mk(key, box, now.Add(-time.Second), ssh.UserCert), false, "kedaluwarsa"},
		{"tepat habis", mk(key, box, now, ssh.UserCert), false, "kedaluwarsa"},
		{"principal lain", mk(key, "box-lain", now.Add(time.Hour), ssh.UserCert), false, "box lain"},
		{"kunci lain", mk(other, box, now.Add(time.Hour), ssh.UserCert), false, "kunci lain"},
		{"sertifikat host", mk(key, box, now.Add(time.Hour), ssh.HostCert), false, "bukan sertifikat pengguna"},
		{"tidak ada", nil, false, "belum ada"},
	}
	for _, c := range cases {
		err := Check(c.cert, want)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, ingin ok=%v", c.name, err, c.ok)
			continue
		}
		if !c.ok && !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: alasan %q tak memuat %q", c.name, err, c.why)
		}
	}

	inf := mk(key, box, now, ssh.UserCert)
	inf.ValidBefore = ssh.CertTimeInfinity
	if err := Check(inf, want); err != nil {
		t.Fatalf("tanpa batas waktu: %v", err)
	}
	if !ExpiresAt(inf).IsZero() {
		t.Fatal("ExpiresAt tanpa batas harus zero")
	}
	// Sertifikat baru dari server cukup belum kedaluwarsa.
	short := mk(key, box, now.Add(2*time.Minute), ssh.UserCert)
	if err := Check(short, Want{Principal: box, Key: key, Now: now, MinRemaining: time.Second}); err != nil {
		t.Fatalf("TTL pendek setelah refresh: %v", err)
	}
}

func TestParseCert(t *testing.T) {
	ca := sshtest.NewCA(t)
	key := sshtest.NewKey(t)
	now := time.Now()
	line := ca.SignLine(t, key, "b1", now, now.Add(time.Hour))
	c, err := ParseCert([]byte(line))
	if err != nil || c.ValidPrincipals[0] != "b1" {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := ParseCert(ssh.MarshalAuthorizedKey(key)); err == nil {
		t.Fatal("kunci biasa bukan sertifikat")
	}
	if _, err := ParseCert([]byte("sampah")); err == nil {
		t.Fatal("sampah harus galat")
	}
}
