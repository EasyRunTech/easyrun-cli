package claudehand

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// auditRotate — berkas audit dipindah ke <nama>.1 bila sudah sebesar ini.
	auditRotate = 10 << 20
	// auditCmdMax — perintah Bash dicatat paling panjang sekian karakter.
	auditCmdMax = 4000
)

// Audit — log audit lokal (0600): aksi + path/perintah, TIDAK PERNAH isi
// berkas atau keluaran perintah. Satu baris per peristiwa, diawali waktu
// lokal laptop dan id jalan (beberapa jalan bisa menulis berkas yang sama).
type Audit struct {
	Path string
	mu   sync.Mutex
	f    *os.File
	run  string
	now  func() time.Time
}

// OpenAudit — buka (atau buat) berkas audit untuk ditambahi.
func OpenAudit(path string, now func() time.Time) (*Audit, error) {
	if now == nil {
		now = time.Now
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= auditRotate {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("log audit %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
			_ = f.Chmod(0o600)
		}
	}
	var id [3]byte
	_, _ = rand.Read(id[:])
	return &Audit{Path: path, f: f, run: hex.EncodeToString(id[:]), now: now}, nil
}

// Log — satu peristiwa. detail diratakan menjadi satu baris.
func (a *Audit) Log(event, detail string) {
	if a == nil {
		return
	}
	line := a.now().Format("2006-01-02T15:04:05-07:00") + " [" + a.run + "] " + event
	if detail != "" {
		line += " " + oneLine(detail)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		_, _ = a.f.WriteString(line + "\n")
	}
}

// Close — tutup berkas.
func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	err := a.f.Close()
	a.f = nil
	return err
}

// QuoteCommand — perintah Bash untuk audit: dikutip (baris baru terlihat
// sebagai \n, tidak bisa memalsukan baris audit lain), dipotong bila panjang.
func QuoteCommand(cmd string) string {
	if r := []rune(cmd); len(r) > auditCmdMax {
		cmd = string(r[:auditCmdMax]) + "…"
	}
	return strconv.Quote(cmd)
}

func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

// Short — ringkas untuk terminal: spasi dirapatkan, dipotong n karakter
// (seperti short() di P1).
func Short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
