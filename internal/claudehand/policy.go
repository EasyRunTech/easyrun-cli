// Package claudehand — "tangan" laptop untuk sesi Claude Code di EasyBox
// (kontrak P2, brief 3b7f1d58): penjaga yang berdiri di antara jembatan MCP di
// box (`easybox-bridge`, lewat `easybox-hand-connect`) dan `claude mcp serve`
// lokal. Laptop TIDAK percaya box: hanya Bash/Read/Edit/Write (read-only: Read),
// path berkas wajib di dalam root (symlink diikuti), folder rahasia ditolak,
// Bash latar ditolak, permintaan server→klien dijawab di laptop.
//
// Semantiknya mengikuti penjaga rujukan P1 (python `laptop/easybox-hand`) —
// pesan tolak, urutan pemeriksaan, dan bentuk jawabannya sama — dengan
// beberapa pengetatan yang ditandai "lebih ketat dari P1".
package claudehand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Protocol — versi protokol tangan (baris hello `easybox_hand`).
const Protocol = 1

const (
	ModeFull     = "full"
	ModeReadOnly = "read-only"
)

var (
	// FullTools — allowlist mode penuh.
	FullTools = []string{"Bash", "Read", "Edit", "Write"}
	// ReadOnlyTools — allowlist mode read-only.
	ReadOnlyTools = []string{"Read"}
	fileTools     = map[string]bool{"Read": true, "Edit": true, "Write": true}

	// SecretDirs — relatif ke home: kontrak P2 §3 + tambahan P1
	// (.easybox-hand, .claude-personal). Config tangan sendiri dan folder
	// konfigurasi easyrun yang sebenarnya ditambahkan pemanggil lewat
	// PolicyConfig.ExtraSecrets.
	SecretDirs = []string{
		".ssh", ".aws", ".gnupg", "Library/Keychains", ".config/gh", ".kube", ".docker",
		".netrc", ".easyrun", ".easybox-hand", ".claude", ".claude.json", ".claude-personal",
		".password-store", ".config/gcloud", ".azure", ".npmrc", ".pypirc", ".git-credentials",
	}
)

// maxSymlinks — batas lompatan symlink saat mencari path nyata (seperti ELOOP).
const maxSymlinks = 64

// ErrRoot — root ditolak (/, home, atau folder rahasia).
var ErrRoot = errors.New("root ditolak")

// PolicyConfig — masukan NewPolicy.
type PolicyConfig struct {
	Root      string // folder proyek (biasanya cwd)
	Home      string // folder home pengguna
	ReadOnly  bool
	AllowHome bool
	// ExtraSecrets — path absolut lain yang diperlakukan sebagai folder
	// rahasia (folder konfigurasi easyrun, config tangan, CLAUDE_CONFIG_DIR).
	ExtraSecrets []string
}

// Policy — aturan penjaga untuk satu root.
type Policy struct {
	Root     string // path nyata
	Home     string // path nyata
	ReadOnly bool
	secrets  []string
}

// NewPolicy — periksa root dan susun daftar folder rahasia.
func NewPolicy(c PolicyConfig) (*Policy, error) {
	if c.Root == "" || c.Home == "" {
		return nil, errors.New("root dan home wajib diisi")
	}
	root, err := realAbs(c.Root)
	if err != nil {
		return nil, err
	}
	home, err := realAbs(c.Home)
	if err != nil {
		return nil, err
	}
	p := &Policy{Root: root, Home: home, ReadOnly: c.ReadOnly}
	add := func(s string) {
		if s == "" {
			return
		}
		s = filepath.Clean(s)
		for _, have := range p.secrets {
			if strings.EqualFold(have, s) {
				return
			}
		}
		p.secrets = append(p.secrets, s)
	}
	// Bentuk tekstual (seperti P1) DAN path nyatanya: ~/.ssh yang berupa
	// symlink ke ~/dotfiles/ssh tetap rahasia lewat target aslinya
	// (lebih ketat dari P1).
	for _, d := range SecretDirs {
		s := filepath.Join(home, filepath.FromSlash(d))
		add(s)
		if r, err := realpath(s); err == nil {
			add(r)
		}
	}
	for _, s := range c.ExtraSecrets {
		if s == "" {
			continue
		}
		if !filepath.IsAbs(s) {
			if abs, err := filepath.Abs(s); err == nil {
				s = abs
			}
		}
		add(s)
		if r, err := realpath(filepath.Clean(s)); err == nil {
			add(r)
		}
	}
	if isVolumeRoot(root) || (root == home && !c.AllowHome) {
		return nil, fmt.Errorf("%w: jalankan dari folder proyek, bukan / atau home (atau pakai --allow-home)", ErrRoot)
	}
	// Lebih ketat dari P1: root yang MEMUAT home (mis. /Users) membuka seluruh
	// home, jadi diperlakukan seperti home.
	if root != home && within(home, root) && !c.AllowHome {
		return nil, fmt.Errorf("%w: folder ini memuat folder home; jalankan dari folder proyek (atau pakai --allow-home)", ErrRoot)
	}
	for _, s := range p.secrets {
		if withinFold(root, s) {
			return nil, fmt.Errorf("%w: folder ini termasuk folder rahasia; ditolak", ErrRoot)
		}
	}
	return p, nil
}

// Mode — "full" atau "read-only".
func (p *Policy) Mode() string {
	if p.ReadOnly {
		return ModeReadOnly
	}
	return ModeFull
}

// Allowed — allowlist tool untuk mode ini.
func (p *Policy) Allowed() []string {
	if p.ReadOnly {
		return ReadOnlyTools
	}
	return FullTools
}

func (p *Policy) allowed(name string) bool {
	for _, t := range p.Allowed() {
		if t == name {
			return true
		}
	}
	return false
}

// Secrets — daftar folder rahasia (untuk uji dan pesan).
func (p *Policy) Secrets() []string { return append([]string(nil), p.secrets...) }

// Resolve — path berkas dari box → (path nyata, "") bila boleh, atau ("",
// alasan). Path relatif dianggap relatif ke root; komponen ".." ditolak
// SEBELUM dinormalkan; symlink diikuti walau targetnya belum ada; komponen
// yang belum ada (berkas baru di folder baru) ditempel apa adanya.
func (p *Policy) Resolve(v any) (string, string) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", "file_path kosong"
	}
	if strings.ContainsRune(s, 0) {
		return "", "path tidak valid"
	}
	if !filepath.IsAbs(s) {
		s = p.Root + string(filepath.Separator) + s
	}
	for _, part := range strings.Split(filepath.ToSlash(s), "/") {
		if part == ".." {
			return "", "path memuat '..'"
		}
	}
	full, err := realpath(filepath.Clean(s))
	if err != nil {
		return "", "path tidak bisa diperiksa: " + err.Error()
	}
	if !within(full, p.Root) {
		return "", "di luar root " + p.Root
	}
	for _, sec := range p.secrets {
		if withinFold(full, sec) {
			return "", "folder rahasia"
		}
	}
	return full, ""
}

// Check — periksa satu tools/call. deny "" = boleh; action = ringkasan untuk
// log (path nyata, atau perintah Bash).
func (p *Policy) Check(name string, args map[string]any) (deny, action string) {
	if !p.allowed(name) {
		return fmt.Sprintf("tool %s tidak diizinkan (mode %s; boleh: %s)", name, p.Mode(), strings.Join(p.Allowed(), ", ")), ""
	}
	if fileTools[name] {
		full, why := p.Resolve(args["file_path"])
		if why != "" {
			return fmt.Sprintf("%s %s: %s", name, display(args["file_path"]), why), ""
		}
		return "", full
	}
	if name == "Bash" {
		if truthy(args["run_in_background"]) {
			return "Bash run_in_background tidak didukung lewat EasyBox; jalankan di foreground", ""
		}
		if truthy(args["dangerouslyDisableSandbox"]) {
			return "Bash dangerouslyDisableSandbox ditolak", ""
		}
		cmd, _ := args["command"].(string)
		return "", cmd
	}
	return "", ""
}

// truthy — kebenaran ala Python (P1 memakai `if args.get(k)`): "false" sebagai
// string tetap benar, jadi ikut ditolak.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func display(v any) string {
	switch x := v.(type) {
	case nil:
		return "(kosong)"
	case string:
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// within — path == dir atau di bawahnya (peka huruf: arah aman, path dengan
// huruf berbeda dianggap di luar root).
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimRight(dir, string(filepath.Separator))+string(filepath.Separator))
}

// withinFold — seperti within tetapi tanpa beda huruf besar/kecil: di sistem
// berkas yang tidak peka huruf (bawaan macOS) ~/.SSH = ~/.ssh. Dipakai untuk
// folder rahasia (arah aman: lebih banyak ditolak).
func withinFold(path, dir string) bool {
	return within(strings.ToLower(path), strings.ToLower(dir))
}

func isVolumeRoot(p string) bool {
	return p == filepath.VolumeName(p)+string(filepath.Separator)
}

func realAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return realpath(abs)
}

// realpath — seperti os.path.realpath Python (tidak ketat): symlink diikuti
// walau targetnya belum ada (symlink "menggantung" ke luar root tetap
// ketahuan), komponen yang tidak ada ditempel apa adanya. Lingkaran symlink
// → galat (ditolak).
func realpath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path tidak absolut: %s", p)
	}
	sep := string(filepath.Separator)
	vol := filepath.VolumeName(p)
	out := vol + sep
	todo := splitPath(p[len(vol):])
	hops := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			out = filepath.Dir(out)
			continue
		}
		next := filepath.Join(out, c)
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			out = next
			continue
		}
		hops++
		if hops > maxSymlinks {
			return "", errors.New("terlalu banyak symlink")
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			tv := filepath.VolumeName(target)
			out = tv + sep
			target = target[len(tv):]
		}
		todo = append(splitPath(target), todo...)
	}
	return out, nil
}

func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}
