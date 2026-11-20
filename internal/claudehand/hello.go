package claudehand

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ClaudeMDFiles — instruksi proyek yang dikirim ke box (relatif ke root).
var ClaudeMDFiles = []string{"CLAUDE.md", ".claude/CLAUDE.md", "CLAUDE.local.md"}

const (
	// ClaudeMDMax — karakter per berkas (kontrak P2 §1).
	ClaudeMDMax = 20000
	claudeMDCut = "\n…(dipotong)"
	// HelloMaxBytes — jembatan P1 membaca baris hello paling banyak 256 KiB
	// (262144 byte) lalu menolak tangan; sisakan ruang.
	HelloMaxBytes = 240 << 10
)

// Hello — baris pertama dari laptop (kontrak P2 §1).
type Hello struct {
	EasyboxHand   int               `json:"easybox_hand"`
	Host          string            `json:"host"`
	User          string            `json:"user"`
	OS            string            `json:"os"`
	Root          string            `json:"root"`
	Mode          string            `json:"mode"`
	ClaudeVersion string            `json:"claude_version"`
	ClaudeMD      map[string]string `json:"claude_md"`
	Guard         string            `json:"guard"`
}

// HelloInfo — data laptop untuk hello.
type HelloInfo struct {
	Host, User, OS, ClaudeVersion, Guard string
}

// BuildHello — hello untuk policy ini. CLAUDE.md dibaca lewat jail yang sama
// dengan tool berkas (lebih ketat dari P1: CLAUDE.md berupa symlink ke luar
// root atau ke folder rahasia tidak ikut terkirim).
func BuildHello(p *Policy, info HelloInfo) Hello {
	h := Hello{
		EasyboxHand: Protocol, Host: info.Host, User: info.User, OS: info.OS,
		Root: p.Root, Mode: p.Mode(), ClaudeVersion: info.ClaudeVersion,
		ClaudeMD: map[string]string{}, Guard: info.Guard,
	}
	for _, rel := range ClaudeMDFiles {
		path := filepath.Join(p.Root, filepath.FromSlash(rel))
		full, why := p.Resolve(path)
		if why != "" {
			continue
		}
		txt, err := readText(full, ClaudeMDMax)
		if err != nil {
			continue
		}
		h.ClaudeMD[path] = txt
	}
	return h
}

// readText — isi berkas sebagai UTF-8 sah (byte rusak → U+FFFD, seperti
// errors="replace"), paling banyak max karakter + penanda dipotong.
func readText(path string, max int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", os.ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(max+1)*utf8.UTFMax))
	if err != nil {
		return "", err
	}
	s := strings.ToValidUTF8(string(raw), "�")
	return cutRunes(s, max), nil
}

func cutRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + claudeMDCut
		}
		n++
	}
	return s
}

// Line — hello sebagai satu baris JSON (dengan "\n"), tanpa escape HTML, dan
// dijamin < HelloMaxBytes: bila terlalu besar, CLAUDE.md terbesar dipotong
// separuh berulang kali.
func (h Hello) Line() ([]byte, error) {
	md := make(map[string]string, len(h.ClaudeMD))
	for k, v := range h.ClaudeMD {
		md[k] = v
	}
	h.ClaudeMD = md
	for i := 0; ; i++ {
		b, err := encodeLine(h)
		if err != nil {
			return nil, err
		}
		if len(b) <= HelloMaxBytes || i > 40 {
			return b, nil
		}
		big, size := "", -1
		for k, v := range h.ClaudeMD {
			if len(v) > size {
				big, size = k, len(v)
			}
		}
		if size <= 0 {
			return b, nil
		}
		v := strings.TrimSuffix(h.ClaudeMD[big], claudeMDCut)
		h.ClaudeMD[big] = cutRunes(v, utf8.RuneCountInString(v)/2)
		if !strings.HasSuffix(h.ClaudeMD[big], claudeMDCut) {
			h.ClaudeMD[big] += claudeMDCut
		}
	}
}

// encodeLine — JSON satu baris + "\n" tanpa escape <, >, & (seperti
// json.dumps ensure_ascii=False di P1).
func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
