// Package sshtest — CA SSH palsu untuk uji (hanya diimpor berkas _test.go,
// tidak ikut ke program).
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// CA — penanda tangan sertifikat pengguna, seperti SSH_USER_CA_PRIVATE_KEY.
type CA struct {
	Signer ssh.Signer
}

func NewCA(t testing.TB) *CA {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Signer: s}
}

// PublicLine — baris kunci publik CA (untuk TrustedUserCAKeys).
func (c *CA) PublicLine() []byte { return ssh.MarshalAuthorizedKey(c.Signer.PublicKey()) }

// Sign — sertifikat pengguna untuk pub, principal, berlaku [after, before).
func (c *CA) Sign(pub ssh.PublicKey, principal string, after, before time.Time, certType uint32) (*ssh.Certificate, error) {
	cert := &ssh.Certificate{
		Key:             pub,
		CertType:        certType,
		KeyId:           fmt.Sprintf("user:uji token:uji principal:%s", principal),
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(after.Unix()),
		ValidBefore:     uint64(before.Unix()),
		Permissions: ssh.Permissions{Extensions: map[string]string{
			"permit-pty":             "",
			"permit-port-forwarding": "",
		}},
	}
	if err := cert.SignCert(rand.Reader, c.Signer); err != nil {
		return nil, err
	}
	return cert, nil
}

// SignLine — Sign dalam bentuk satu baris authorized_keys (isi berkas -cert.pub).
func (c *CA) SignLine(t testing.TB, pub ssh.PublicKey, principal string, after, before time.Time) string {
	t.Helper()
	cert, err := c.Sign(pub, principal, after, before, ssh.UserCert)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(cert))
}

// NewKey — pasangan kunci ed25519 baru (kunci publiknya saja).
func NewKey(t testing.TB) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
