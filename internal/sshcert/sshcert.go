// Package sshcert — kunci SSH laptop (~/.easyrun/ssh/id_ed25519) dan
// sertifikat berumur pendek per box yang ditandatangani panel. Kunci privat
// tidak pernah dicetak dan tidak pernah dikirim ke mana pun; yang dikirim ke
// panel hanya kunci publiknya.
package sshcert

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// MinRemaining — sertifikat yang sisa umurnya kurang dari ini diperbarui
// (kontrak §6: "< 5 menit").
const MinRemaining = 5 * time.Minute

// KeyPair — kunci laptop yang siap dipakai.
type KeyPair struct {
	PublicKey     ssh.PublicKey
	AuthorizedKey string // "ssh-ed25519 AAAA… komentar", tanpa baris baru
}

// EnsureKey — muat kunci ed25519 di privPath; buat bila belum ada. Berkas .pub
// ditulis ulang bila hilang atau tidak cocok. Kunci yang izinnya terbuka
// ditolak (OpenSSH juga akan menolaknya).
func EnsureKey(privPath, pubPath, comment string) (*KeyPair, error) {
	pemBytes, err := os.ReadFile(privPath)
	if errors.Is(err, fs.ErrNotExist) {
		pemBytes, err = generate(privPath, comment)
	}
	if err != nil {
		return nil, err
	}
	if err := config.CheckPrivate(privPath); err != nil {
		return nil, fmt.Errorf("%w — hapus berkas itu (akan dibuat ulang otomatis) atau jalankan: chmod 600 %s", err, privPath)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("kunci %s memakai passphrase; easyrun butuh kunci khusus tanpa passphrase — hapus berkas itu supaya dibuat ulang", privPath)
		}
		return nil, fmt.Errorf("kunci %s rusak (hapus supaya dibuat ulang): %w", privPath, err)
	}
	pub := signer.PublicKey()
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		line += " " + comment
	}
	if cur, err := os.ReadFile(pubPath); err != nil || !samePublicKey(cur, pub) {
		if err := config.WriteFileAtomic(pubPath, []byte(line+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	return &KeyPair{PublicKey: pub, AuthorizedKey: line}, nil
}

func generate(privPath, comment string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	data := pem.EncodeToMemory(block)
	switch err := config.WriteFileExclusive(privPath, data, 0o600); {
	case err == nil:
		return data, nil
	case errors.Is(err, fs.ErrExist):
		// Proses lain baru saja membuatnya: pakai milik proses itu.
		return os.ReadFile(privPath)
	default:
		return nil, err
	}
}

func samePublicKey(authorizedLine []byte, pub ssh.PublicKey) bool {
	k, _, _, _, err := ssh.ParseAuthorizedKey(authorizedLine)
	return err == nil && bytes.Equal(k.Marshal(), pub.Marshal())
}

// ParseCert — sertifikat pengguna OpenSSH dari satu baris authorized_keys.
func ParseCert(data []byte) (*ssh.Certificate, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("sertifikat tidak bisa dibaca: %w", err)
	}
	cert, ok := k.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("berkas berisi kunci biasa, bukan sertifikat")
	}
	return cert, nil
}

// Want — syarat sertifikat yang masih bisa dipakai.
type Want struct {
	Principal string        // id box
	Key       ssh.PublicKey // kunci laptop
	Now       time.Time
	// MinRemaining — 0 = konstanta MinRemaining.
	MinRemaining time.Duration
}

// Check — nil bila sertifikat masih layak; selain itu alasannya (untuk pesan
// dan uji). ValidAfter tidak diperiksa: jam laptop yang sedikit mundur tidak
// boleh membuat sertifikat baru selalu dianggap rusak — box yang memutuskan.
func Check(cert *ssh.Certificate, w Want) error {
	if cert == nil {
		return errors.New("belum ada sertifikat")
	}
	if cert.CertType != ssh.UserCert {
		return errors.New("bukan sertifikat pengguna")
	}
	if !slices.Contains(cert.ValidPrincipals, w.Principal) {
		return fmt.Errorf("sertifikat untuk box lain (principal %q, ingin %q)", strings.Join(cert.ValidPrincipals, ","), w.Principal)
	}
	if w.Key != nil && !bytes.Equal(cert.Key.Marshal(), w.Key.Marshal()) {
		return errors.New("sertifikat untuk kunci lain")
	}
	if cert.ValidBefore != ssh.CertTimeInfinity {
		minRemaining := w.MinRemaining
		if minRemaining <= 0 {
			minRemaining = MinRemaining
		}
		exp := ExpiresAt(cert)
		if !w.Now.Before(exp) {
			return fmt.Errorf("sertifikat kedaluwarsa sejak %s", exp.Local().Format(time.DateTime))
		}
		if exp.Sub(w.Now) < minRemaining {
			return fmt.Errorf("sisa umur sertifikat %s (< %s)", exp.Sub(w.Now).Round(time.Second), minRemaining)
		}
	}
	return nil
}

// ExpiresAt — batas berlaku sertifikat (zero time untuk "selamanya").
func ExpiresAt(cert *ssh.Certificate) time.Time {
	if cert.ValidBefore == ssh.CertTimeInfinity || cert.ValidBefore > uint64(1<<63-1) {
		return time.Time{}
	}
	return time.Unix(int64(cert.ValidBefore), 0)
}
