// Package config — folder ~/.easyrun: alamat API, token CLI, kunci dan
// sertifikat SSH. Semua berkas rahasia ditulis 0600 secara atomik dan DITOLAK
// dibaca bila izinnya terbuka untuk grup/pengguna lain (seperti OpenSSH).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// EnvConfigDir — ganti lokasi folder (uji, atau beberapa akun).
	EnvConfigDir = "EASYRUN_CONFIG_DIR"
	// EnvAPIURL — ganti alamat API panel tanpa menulis config.json.
	EnvAPIURL = "EASYRUN_API_URL"
	// DefaultAPIURL — alamat API panel produksi (kontrak SSH box §6).
	DefaultAPIURL = "https://api.easyrun.io"

	dirName         = ".easyrun"
	configFile      = "config.json"
	credentialsFile = "credentials.json"
	sshDirName      = "ssh"
	knownHostsFile  = "known_hosts"
	keyFile         = "id_ed25519"
	clockFile       = "clock.json"
	certSuffix      = "-cert.pub"

	dirPerm     fs.FileMode = 0o700
	privatePerm fs.FileMode = 0o600
	// apiPathSuffix — semua endpoint panel ada di bawah /api/v1; alamat API
	// yang disimpan adalah origin-nya. Bila pengguna menempelkan alamat yang
	// sudah berakhiran /api/v1, akhiran itu dibuang.
	apiPathSuffix = "/api/v1"
)

// ErrNotLoggedIn — belum ada credentials.json.
var ErrNotLoggedIn = errors.New("belum masuk")

// PermError — berkas rahasia bisa dibaca grup/pengguna lain.
type PermError struct {
	Path string
	Mode fs.FileMode
}

func (e *PermError) Error() string {
	return fmt.Sprintf("izin %s terlalu terbuka (%04o); berkas rahasia ini harus 0600", e.Path, e.Mode.Perm())
}

// Dir — folder konfigurasi easyrun.
type Dir struct {
	Path string
	// Warn — tujuan peringatan (selalu stderr; stdout bisa jadi aliran SSH).
	Warn io.Writer
}

// Locate — EASYRUN_CONFIG_DIR bila diisi, selain itu ~/.easyrun. homeDir
// nil = os.UserHomeDir.
func Locate(getenv func(string) string, homeDir func() (string, error)) (string, error) {
	if v := getenv(EnvConfigDir); v != "" {
		return filepath.Abs(v)
	}
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("folder home tidak ditemukan (isi %s): %w", EnvConfigDir, err)
	}
	return filepath.Join(home, dirName), nil
}

func (d Dir) ConfigPath() string      { return filepath.Join(d.Path, configFile) }
func (d Dir) CredentialsPath() string { return filepath.Join(d.Path, credentialsFile) }
func (d Dir) SSHDir() string          { return filepath.Join(d.Path, sshDirName) }
func (d Dir) KeyPath() string         { return filepath.Join(d.SSHDir(), keyFile) }
func (d Dir) PubKeyPath() string      { return d.KeyPath() + ".pub" }
func (d Dir) KnownHostsPath() string  { return filepath.Join(d.Path, knownHostsFile) }

// CertPath — sertifikat untuk satu box. Nama berkas memakai id box (bukan
// nama yang diketik) supaya satu box = satu sertifikat walau dipanggil lewat
// nama, hostname, atau id, dan tetap benar bila box diganti nama.
func (d Dir) CertPath(boxID string) string {
	return filepath.Join(d.SSHDir(), boxID+certSuffix)
}

// ClockPath — selisih jam laptop terhadap server (bukan rahasia).
func (d Dir) ClockPath() string { return filepath.Join(d.Path, clockFile) }

// Ensure — buat folder (dan ssh/) 0700; folder lama yang izinnya terbuka
// dirapatkan dengan peringatan.
func (d Dir) Ensure() error {
	for _, p := range []string{d.Path, d.SSHDir()} {
		if err := os.MkdirAll(p, dirPerm); err != nil {
			return fmt.Errorf("tidak bisa membuat folder %s: %w", p, err)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(p, dirPerm); err != nil {
				return fmt.Errorf("izin folder %s terlalu terbuka (%04o) dan tidak bisa diperbaiki: %w", p, fi.Mode().Perm(), err)
			}
			if d.Warn != nil {
				fmt.Fprintf(d.Warn, "easyrun: izin folder %s dirapatkan dari %04o ke 0700\n", p, fi.Mode().Perm())
			}
		}
	}
	return nil
}

// CheckPrivate — tolak berkas rahasia yang bisa dibaca/ditulis grup atau
// pengguna lain. Windows tidak memakai bit izin Unix, jadi dilewati.
func CheckPrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return &PermError{Path: path, Mode: fi.Mode()}
	}
	return nil
}

// Config — isi config.json.
type Config struct {
	APIURL string `json:"api_url,omitempty"`
}

func (d Dir) LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(d.ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s rusak: %w", d.ConfigPath(), err)
	}
	return c, nil
}

func (d Dir) SaveConfig(c Config) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	return writeJSON(d.ConfigPath(), c)
}

// Credentials — token CLI hasil `easyrun login`. APIURL mengikat token ke
// server yang menerbitkannya: token tidak pernah dikirim ke alamat lain.
type Credentials struct {
	APIURL      string    `json:"api_url"`
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type,omitempty"`
	TokenID     string    `json:"token_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	UserID      string    `json:"user_id,omitempty"`
	Email       string    `json:"email,omitempty"`
	ObtainedAt  time.Time `json:"obtained_at,omitzero"`
}

// LoadCredentials — ErrNotLoggedIn bila belum ada. allowInsecure hanya untuk
// `logout`: token yang izinnya terbuka tetap boleh dibaca untuk DICABUT.
func (d Dir) LoadCredentials(allowInsecure bool) (*Credentials, error) {
	p := d.CredentialsPath()
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if !allowInsecure {
		if err := CheckPrivate(p); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s rusak (jalankan easyrun login): %w", p, err)
	}
	if c.AccessToken == "" {
		return nil, ErrNotLoggedIn
	}
	return &c, nil
}

func (d Dir) SaveCredentials(c Credentials) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	return writeJSON(d.CredentialsPath(), c)
}

// RemoveCredentials — hapus token lokal; tidak ada berkas = sukses.
func (d Dir) RemoveCredentials() error {
	err := os.Remove(d.CredentialsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveCertificates — hapus semua sertifikat box (berumur pendek, terikat
// token). Kunci SSH dibiarkan: bukan rahasia server dan bisa dipakai lagi.
// Isi folder dibaca langsung (bukan filepath.Glob): path folder yang memuat
// karakter pola seperti "[" tidak boleh membuat sertifikat lama tertinggal.
func (d Dir) RemoveCertificates() error {
	entries, err := os.ReadDir(d.SSHDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), certSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(d.SSHDir(), e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ClockOffset — selisih jam server dikurangi jam laptop, diukur dari header
// Date saat sertifikat terakhir diterbitkan.
type ClockOffset struct {
	OffsetSeconds int64     `json:"offset_seconds"`
	MeasuredAt    time.Time `json:"measured_at,omitzero"`
}

// Offset — sebagai durasi.
func (c ClockOffset) Offset() time.Duration { return time.Duration(c.OffsetSeconds) * time.Second }

// LoadClockOffset — nol bila belum pernah diukur atau berkasnya rusak (jam
// laptop lalu dipercaya apa adanya).
func (d Dir) LoadClockOffset() ClockOffset {
	var c ClockOffset
	b, err := os.ReadFile(d.ClockPath())
	if err != nil || json.Unmarshal(b, &c) != nil {
		return ClockOffset{}
	}
	return c
}

func (d Dir) SaveClockOffset(c ClockOffset) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	return writeJSON(d.ClockPath(), c)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), privatePerm)
}

// writeTemp — tulis data ke berkas sementara di folder path (dibuat 0600 oleh
// os.CreateTemp lalu diberi perm), fsync, tutup. Kembalikan namanya; pemanggil
// yang memindahkan atau menghapusnya.
func writeTemp(path string, data []byte, perm fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// WriteFileAtomic — tulis ke berkas sementara di folder yang sama, fsync,
// lalu rename: pembaca tidak pernah melihat berkas setengah jadi, dan izin
// tidak pernah sempat terbuka.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	name, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// WriteFileExclusive — seperti WriteFileAtomic, tetapi GAGAL (fs.ErrExist)
// bila berkas tujuan sudah ada. Dipakai saat membuat kunci SSH supaya dua
// proses yang berjalan bersamaan (VS Code membuka beberapa sambungan) tidak
// saling menimpa kunci.
//
// Hard link = buat-bila-belum-ada yang atomik. Sistem berkas tanpa hard link
// jatuh ke O_EXCL: berkas tujuan lalu terlihat sesaat dalam keadaan setengah
// tertulis, jadi pembaca yang mendapat fs.ErrExist harus siap mengulang
// (lihat ReadFileSettled).
func WriteFileExclusive(path string, data []byte, perm fs.FileMode) error {
	name, err := writeTemp(path, data, perm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(name) }()
	err = os.Link(name, path)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return fs.ErrExist
	}
	f, ferr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if ferr != nil {
		if errors.Is(ferr, fs.ErrExist) {
			return fs.ErrExist
		}
		return ferr
	}
	if _, werr := f.Write(data); werr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return serr
	}
	return f.Close()
}

// settleTries, settleWait — ReadFileSettled mengulang paling lama ±2 detik.
const (
	settleTries = 40
	settleWait  = 50 * time.Millisecond
)

// ReadFileSettled — baca berkas yang baru saja dibuat proses lain lewat
// WriteFileExclusive: ulangi selama valid(isi) gagal (berkas masih setengah
// tertulis di sistem berkas tanpa hard link). Galat valid terakhir
// dikembalikan bila isinya tidak pernah lengkap.
func ReadFileSettled(path string, valid func([]byte) error) ([]byte, error) {
	var lastErr error
	for i := 0; i < settleTries; i++ {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if lastErr = valid(b); lastErr == nil {
			return b, nil
		}
		time.Sleep(settleWait)
	}
	return nil, lastErr
}

// NormalizeAPIURL — origin API panel yang aman dipakai: https, atau http
// HANYA ke loopback (uji lokal). Akhiran /api/v1 dan garis miring dibuang.
func NormalizeAPIURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("alamat API tidak valid: %q (contoh: %s)", raw, DefaultAPIURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("alamat API tidak boleh memuat user, query, atau fragment: %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("alamat API harus https (http hanya untuk localhost): %q", raw)
		}
	default:
		return "", fmt.Errorf("alamat API harus https: %q", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, apiPathSuffix)
	return u.Scheme + "://" + u.Host + strings.TrimRight(p, "/"), nil
}

// IsLoopbackHost — localhost / 127.0.0.0/8 / ::1.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ResolveAPIURL — urutan: flag → env EASYRUN_API_URL → config.json → bawaan.
func ResolveAPIURL(flagValue string, getenv func(string) string, cfg Config) (string, error) {
	for _, v := range []string{flagValue, getenv(EnvAPIURL), cfg.APIURL} {
		if strings.TrimSpace(v) != "" {
			return NormalizeAPIURL(v)
		}
	}
	return DefaultAPIURL, nil
}
