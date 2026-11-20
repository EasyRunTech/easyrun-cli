package claudehand

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixture — home palsu berisi proyek, folder luar, dan folder rahasia.
type fixture struct {
	home, root, outside string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{
		home:    filepath.Join(base, "home"),
		root:    filepath.Join(base, "home", "proj"),
		outside: filepath.Join(base, "home", "luar"),
	}
	for _, d := range []string{f.root + "/sub", f.outside, f.home + "/.ssh", f.home + "/dotfiles/aws", f.home + "/.easyrun"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, f.root+"/a.txt", "isi-dalam\n")
	write(t, f.outside+"/b.txt", "rahasia-luar\n")
	write(t, f.home+"/.ssh/id_ed25519", "KUNCI\n")
	return f
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) policy(t *testing.T, mut func(*PolicyConfig)) *Policy {
	t.Helper()
	c := PolicyConfig{Root: f.root, Home: f.home}
	if mut != nil {
		mut(&c)
	}
	p, err := NewPolicy(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRootDitolak(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	cases := []struct {
		name string
		c    PolicyConfig
		want string
	}{
		{"garis miring", PolicyConfig{Root: "/", Home: f.home}, "bukan / atau home"},
		{"home", PolicyConfig{Root: f.home, Home: f.home}, "bukan / atau home"},
		{"induk home", PolicyConfig{Root: filepath.Dir(f.home), Home: f.home}, "memuat folder home"},
		{"folder rahasia", PolicyConfig{Root: f.home + "/.ssh", Home: f.home}, "folder rahasia"},
		{"rahasia huruf besar", PolicyConfig{Root: f.home + "/.SSH", Home: f.home}, "folder rahasia"},
		{"config easyrun", PolicyConfig{Root: f.home + "/.easyrun", Home: f.home}, "folder rahasia"},
		{"rahasia tambahan", PolicyConfig{Root: f.outside, Home: f.home, ExtraSecrets: []string{f.outside}}, "folder rahasia"},
	}
	for _, c := range cases {
		_, err := NewPolicy(c.c)
		if err == nil || !errors.Is(err, ErrRoot) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: galat %v, ingin memuat %q", c.name, err, c.want)
		}
	}
	if _, err := NewPolicy(PolicyConfig{Root: f.home, Home: f.home, AllowHome: true}); err != nil {
		t.Errorf("--allow-home harus mengizinkan home: %v", err)
	}
	if _, err := NewPolicy(PolicyConfig{Root: f.root, Home: f.home}); err != nil {
		t.Errorf("folder proyek harus boleh: %v", err)
	}
}

func TestResolveJail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	if err := os.Symlink(f.outside, f.root+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside+"/belum-ada.txt", f.root+"/gantung"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", f.root+"/relatif"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../luar", f.root+"/naik"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root+"/putar2", f.root+"/putar1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root+"/putar1", f.root+"/putar2"); err != nil {
		t.Fatal(err)
	}
	p := f.policy(t, nil)
	ok := []struct{ in, want string }{
		{f.root + "/a.txt", f.root + "/a.txt"},
		{"a.txt", f.root + "/a.txt"},
		{f.root + "/sub/baru/n.txt", f.root + "/sub/baru/n.txt"}, // berkas baru di folder baru
		{f.root + "/relatif/x", f.root + "/sub/x"},
		{f.root, f.root},
		{f.root + "/./a.txt", f.root + "/a.txt"},
	}
	for _, c := range ok {
		got, why := p.Resolve(c.in)
		if why != "" || got != c.want {
			t.Errorf("Resolve(%q) = %q, %q; ingin %q", c.in, got, why, c.want)
		}
	}
	bad := []struct {
		in   any
		want string
	}{
		{f.outside + "/b.txt", "di luar root"},
		{f.root + "/link/b.txt", "di luar root"},
		{f.root + "/gantung", "di luar root"}, // symlink menggantung ke luar root
		{f.root + "/naik/b.txt", "di luar root"},
		{f.root + "/sub/../../luar/b.txt", "'..'"},
		{"sub/../a.txt", "'..'"},
		{f.home + "/.ssh/id_ed25519", "di luar root"},
		{"", "file_path kosong"},
		{nil, "file_path kosong"},
		{42, "file_path kosong"},
		{f.root + "/a\x00b", "path tidak valid"},
		{f.root + "/putar1", "terlalu banyak symlink"},
		{"/etc/passwd", "di luar root"},
	}
	for _, c := range bad {
		got, why := p.Resolve(c.in)
		if got != "" || !strings.Contains(why, c.want) {
			t.Errorf("Resolve(%v) = %q, %q; ingin tolak %q", c.in, got, why, c.want)
		}
	}
}

func TestResolveRahasiaDiDalamRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	cfgDir := filepath.Join(f.home, "lain", "cfg-easyrun")
	write(t, cfgDir+"/credentials.json", "{}")
	// ~/.aws → ~/dotfiles/aws (dotfiles): rahasia lewat target aslinya juga.
	if err := os.Symlink(f.home+"/dotfiles/aws", f.home+"/.aws"); err != nil {
		t.Fatal(err)
	}
	write(t, f.home+"/dotfiles/aws/credentials", "AKIA")
	p := f.policy(t, func(c *PolicyConfig) {
		c.Root, c.AllowHome = f.home, true
		c.ExtraSecrets = []string{cfgDir}
	})
	for _, in := range []string{
		f.home + "/.ssh/id_ed25519",
		f.home + "/.SSH/id_ed25519", // macOS tidak peka huruf
		f.home + "/.ssh",
		f.home + "/.claude.json",
		f.home + "/.claude/settings.json",
		f.home + "/.config/gh/hosts.yml",
		f.home + "/Library/Keychains/login.keychain-db",
		f.home + "/dotfiles/aws/credentials",
		f.home + "/.aws/credentials",
		cfgDir + "/credentials.json",
	} {
		if got, why := p.Resolve(in); why != "folder rahasia" {
			t.Errorf("Resolve(%q) = %q, %q; ingin folder rahasia", in, got, why)
		}
	}
	if got, why := p.Resolve(f.home + "/.sshx"); why != "" || got == "" {
		t.Errorf(".sshx bukan rahasia: %q %q", got, why)
	}
}

func TestCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	p := f.policy(t, nil)
	in := f.root + "/a.txt"
	cases := []struct {
		name string
		args map[string]any
		deny string
		act  string
	}{
		{"Read", map[string]any{"file_path": in}, "", in},
		{"Edit", map[string]any{"file_path": in, "old_string": "a", "new_string": "b"}, "", in},
		{"Write", map[string]any{"file_path": f.root + "/baru/x.txt", "content": "x"}, "", f.root + "/baru/x.txt"},
		{"Write", map[string]any{"file_path": f.outside + "/x"}, "Write " + f.outside + "/x: di luar root " + f.root, ""},
		{"Read", map[string]any{}, "Read (kosong): file_path kosong", ""},
		{"Bash", map[string]any{"command": "pwd"}, "", "pwd"},
		{"Bash", map[string]any{"command": "sleep 1", "run_in_background": true}, "Bash run_in_background tidak didukung lewat EasyBox; jalankan di foreground", ""},
		{"Bash", map[string]any{"command": "x", "run_in_background": "false"}, "run_in_background", ""}, // seperti Python: string tak kosong = benar
		{"Bash", map[string]any{"command": "x", "run_in_background": false}, "", "x"},
		{"Bash", map[string]any{"command": "x", "dangerouslyDisableSandbox": true}, "Bash dangerouslyDisableSandbox ditolak", ""},
		{"Workflow", map[string]any{"script": "x"}, "tool Workflow tidak diizinkan (mode full; boleh: Bash, Read, Edit, Write)", ""},
		{"Agent", nil, "tidak diizinkan", ""},
		{"read", map[string]any{"file_path": in}, "tidak diizinkan", ""},
	}
	for _, c := range cases {
		deny, act := p.Check(c.name, c.args)
		if (c.deny == "") != (deny == "") || !strings.Contains(deny, c.deny) || act != c.act {
			t.Errorf("Check(%s, %v) = %q, %q; ingin %q, %q", c.name, c.args, deny, act, c.deny, c.act)
		}
	}
	ro := f.policy(t, func(c *PolicyConfig) { c.ReadOnly = true })
	for _, n := range []string{"Bash", "Edit", "Write"} {
		if deny, _ := ro.Check(n, map[string]any{"file_path": in, "command": "id"}); !strings.Contains(deny, "tidak diizinkan (mode read-only; boleh: Read)") {
			t.Errorf("read-only %s: %q", n, deny)
		}
	}
	if deny, _ := ro.Check("Read", map[string]any{"file_path": in}); deny != "" {
		t.Errorf("read-only Read harus boleh: %q", deny)
	}
}

func TestHelloCLAUDEmd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	write(t, f.root+"/CLAUDE.md", "Aturan proyek: pakai tab. <b>&</b>\n")
	write(t, f.root+"/CLAUDE.local.md", strings.Repeat("é", ClaudeMDMax+5))
	// .claude/CLAUDE.md berupa symlink ke kunci SSH: tidak boleh terkirim.
	if err := os.MkdirAll(f.root+"/.claude", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.home+"/.ssh/id_ed25519", f.root+"/.claude/CLAUDE.md"); err != nil {
		t.Fatal(err)
	}
	p := f.policy(t, nil)
	h := BuildHello(p, HelloInfo{Host: "laptop", User: "budi", OS: "Darwin 24 arm64", ClaudeVersion: "2.1.329 (Claude Code)", Guard: "easyrun-cli/dev"})
	line, err := h.Line()
	if err != nil {
		t.Fatal(err)
	}
	s := string(line)
	for _, want := range []string{`"easybox_hand":1`, `"guard":"easyrun-cli/dev"`, `"mode":"full"`, `"root":"` + f.root + `"`, `<b>&</b>`, "…(dipotong)"} {
		if !strings.Contains(s, want) {
			t.Errorf("hello tidak memuat %q", want)
		}
	}
	if strings.Contains(s, "KUNCI") || strings.Contains(s, "\\u003c") {
		t.Errorf("hello membocorkan symlink rahasia atau meng-escape HTML: kunci=%v esc=%v", strings.Contains(s, "KUNCI"), strings.Contains(s, "\\u003c"))
	}
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Error("hello harus tepat satu baris")
	}
	if got := []rune(h.ClaudeMD[f.root+"/CLAUDE.local.md"]); len(got) != ClaudeMDMax+len([]rune(claudeMDCut)) {
		t.Errorf("CLAUDE.local.md dipotong ke %d karakter", len(got))
	}
}

func TestHelloDibatasiUkuranJembatan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	f := newFixture(t)
	// Karakter kontrol di-escape jadi \u00XX (6 byte): 3 × 20000 bisa > 256 KiB.
	big := strings.Repeat("\x01", ClaudeMDMax)
	for _, n := range ClaudeMDFiles {
		write(t, filepath.Join(f.root, n), big)
	}
	h := BuildHello(f.policy(t, nil), HelloInfo{Guard: "easyrun-cli/dev"})
	line, err := h.Line()
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > HelloMaxBytes {
		t.Fatalf("hello %d byte > %d", len(line), HelloMaxBytes)
	}
	if len(h.ClaudeMD) != 3 {
		t.Fatalf("Line tidak boleh mengubah Hello asli")
	}
}
