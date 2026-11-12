package sshconf

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testID = "7b4c1f0e-2a3d-4e5f-8a9b-0c1d2e3f4a5b"

func TestValidQuery(t *testing.T) {
	for _, q := range []string{"uji-box", "Uji_Box", "uji-box.easyrun.io", testID, "a", "box1.2"} {
		if err := ValidQuery(q); err != nil {
			t.Errorf("%q harus diterima: %v", q, err)
		}
	}
	bad := []string{
		"", "-oProxyCommand=touch x", "--", "a b", "a;b", "$(id)", "`id`", "a|b", "a&b",
		"a*", "a?", "!a", "a,b", "a'b", `a"b`, "a\\b", "a\nb", "ä", "a/b", "a%h",
		strings.Repeat("a", 254),
	}
	for _, q := range bad {
		if err := ValidQuery(q); err == nil {
			t.Errorf("%q harus ditolak", q)
		}
	}
}

func TestValidHostnameDanID(t *testing.T) {
	for _, h := range []string{"coba.easyrun.io", "a", "x-1.y-2.z"} {
		if ValidHostname(h) != nil {
			t.Errorf("%q harus diterima", h)
		}
	}
	for _, h := range []string{"", "-a.b", "a-.b", "a..b", "a b", "a;b", "a_b.c", strings.Repeat("a", 64) + ".b"} {
		if ValidHostname(h) == nil {
			t.Errorf("%q harus ditolak", h)
		}
	}
	if ValidID(testID) != nil || ValidID("-x") == nil || ValidID("a b") == nil || ValidID("") == nil {
		t.Fatal("ValidID")
	}
	if !IsUUID(testID) || !IsUUID(strings.ToUpper(testID)) || IsUUID("uji-box") {
		t.Fatal("IsUUID")
	}
}

func TestQuoteExe(t *testing.T) {
	ok := []struct{ in, goos, want string }{
		{"/usr/local/bin/easyrun", "darwin", "/usr/local/bin/easyrun"},
		{"/Users/Budi Santoso/bin/easyrun", "darwin", "'/Users/Budi Santoso/bin/easyrun'"},
		{"/opt/a$(id)/easyrun", "linux", "'/opt/a$(id)/easyrun'"},
		{"/opt/100%/easyrun", "linux", "'/opt/100%%/easyrun'"},
		{"/opt/100% x/easyrun", "linux", "'/opt/100%% x/easyrun'"},
		{`C:\Tools\easyrun.exe`, "windows", `C:\Tools\easyrun.exe`},
		{`C:\Program Files\EasyRun\easyrun.exe`, "windows", `"C:\Program Files\EasyRun\easyrun.exe"`},
	}
	for _, c := range ok {
		got, err := QuoteExe(c.in, c.goos)
		if err != nil || got != c.want {
			t.Errorf("%q/%s → %q, %v; ingin %q", c.in, c.goos, got, err, c.want)
		}
	}
	for _, in := range []string{"/opt/it's/easyrun", `/opt/a\b/easyrun`, "/opt/a\"b/easyrun", "/opt/a\nb/easyrun"} {
		if got, err := QuoteExe(in, "linux"); err == nil {
			t.Errorf("%q harus ditolak, dapat %q", in, got)
		}
	}
}

func params(exe, dir string) Params {
	return Params{
		Alias:           "uji-box",
		ID:              testID,
		Hostname:        "uji-box.easyrun.io",
		Exe:             exe,
		IdentityFile:    filepath.Join(dir, "ssh", "id_ed25519"),
		CertificateFile: filepath.Join(dir, "ssh", testID+"-cert.pub"),
		KnownHostsFile:  filepath.Join(dir, "known_hosts"),
	}
}

func TestArgvPersis(t *testing.T) {
	p := params("/usr/local/bin/easyrun", "/home/budi/.easyrun")
	got, err := Argv(p, []string{"-L", "8080:localhost:8080", "ls", "-la; rm -rf /"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ssh",
		"-o", "User=ubuntu",
		"-o", "IdentityFile=/home/budi/.easyrun/ssh/id_ed25519",
		"-o", "CertificateFile=/home/budi/.easyrun/ssh/" + testID + "-cert.pub",
		"-o", "IdentitiesOnly=yes",
		"-o", "ProxyCommand=/usr/local/bin/easyrun tunnel " + testID,
		"-o", "HostKeyAlias=" + testID,
		"-o", "UserKnownHostsFile=/home/budi/.easyrun/known_hosts",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ServerAliveInterval=30",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"uji-box.easyrun.io",
		"-L", "8080:localhost:8080", "ls", "-la; rm -rf /",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "exec ") {
		t.Fatal("ProxyCommand tidak boleh berawalan exec (OpenSSH sudah menambahkannya)")
	}
}

func TestArgvMenolakNilaiServerAneh(t *testing.T) {
	p := params("/usr/local/bin/easyrun", "/x")
	bad := []func(*Params){
		func(p *Params) { p.ID = "x; touch /tmp/pwned" },
		func(p *Params) { p.ID = "-oProxyCommand=x" },
		func(p *Params) { p.Hostname = "evil host" },
		func(p *Params) { p.Hostname = "a;b" },
		func(p *Params) { p.Exe = "/opt/it's/easyrun" },
		func(p *Params) { p.IdentityFile = "/x/\"q" },
		func(p *Params) { p.Alias = "a b" },
	}
	for i, mut := range bad {
		q := p
		mut(&q)
		if _, err := Argv(q, nil); err == nil {
			t.Errorf("kasus %d harus ditolak", i)
		}
		if _, err := ConfigBlock(q); err == nil {
			t.Errorf("kasus %d (config) harus ditolak", i)
		}
	}
}

// fakeExe — skrip pengganti easyrun di folder berspasi + karakter shell yang
// mencatat argumen yang diterimanya.
func fakeExe(t *testing.T, dirName string) (exe, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skrip sh")
	}
	dir := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile = filepath.Join(dir, "args.txt")
	exe = filepath.Join(dir, "easy run")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$(dirname \"$0\")/args.txt\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, argsFile
}

// ProxyCommand dijalankan ssh lewat `$SHELL -c "exec <perintah>"`. Path yang
// penuh karakter shell tetap sampai sebagai satu argumen, dan tidak ada
// perintah sisipan yang jalan.
func TestProxyCommandAmanDiShell(t *testing.T) {
	exe, argsFile := fakeExe(t, "dir $(touch PWNED); x & y|z")
	cmd, err := ProxyCommand(Invocation{Exe: exe, GOOS: runtime.GOOS}, testID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range []string{"/bin/sh", "bash", "zsh"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		_ = os.Remove(argsFile)
		c := exec.Command(sh, "-c", "exec "+cmd)
		c.Dir = t.TempDir()
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", sh, err, out)
		}
		got, _ := os.ReadFile(argsFile)
		if string(got) != "tunnel\n"+testID+"\n" {
			t.Fatalf("%s: argumen %q", sh, got)
		}
		if _, err := os.Stat(filepath.Join(c.Dir, "PWNED")); err == nil {
			t.Fatalf("%s: perintah sisipan jalan", sh)
		}
	}
}

func sshG(t *testing.T, args ...string) map[string][]string {
	t.Helper()
	c := exec.Command("ssh", append([]string{"-G"}, args...)...)
	c.Stdin = strings.NewReader("")
	out, err := c.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("ssh -G %q: %v\n%s", args, err, stderr)
	}
	got := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if ok {
			got[k] = append(got[k], v)
		}
	}
	return got
}

func requireSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh tidak terpasang")
	}
}

// Argumen -o yang kita susun dibaca OpenSSH sungguhan apa adanya, termasuk
// path berspasi, dan opsi pengguna setelah tujuan tidak menimpa opsi kita.
func TestArgvDibacaOpenSSH(t *testing.T) {
	requireSSH(t)
	exe, _ := fakeExe(t, "bin dir")
	home := filepath.Join(t.TempDir(), "Budi Santoso", ".easyrun")
	argv, err := Argv(params(exe, home), []string{"-o", "ServerAliveInterval=5", "-o", "ForwardAgent=yes", "-p", "2222", "uptime"})
	if err != nil {
		t.Fatal(err)
	}
	got := sshG(t, append([]string{"-F", "none"}, argv[1:]...)...)
	want := map[string]string{
		"user":                  "ubuntu",
		"hostname":              "uji-box.easyrun.io",
		"hostkeyalias":          testID,
		"identitiesonly":        "yes",
		"stricthostkeychecking": "accept-new",
		"serveraliveinterval":   "30",
		"forwardagent":          "no",
		"forwardx11":            "no",
		"port":                  "2222",
		"identityfile":          filepath.Join(home, "ssh", "id_ed25519"),
		"certificatefile":       filepath.Join(home, "ssh", testID+"-cert.pub"),
		"userknownhostsfile":    filepath.Join(home, "known_hosts"),
		"proxycommand":          "'" + exe + "' tunnel " + testID,
	}
	for k, v := range want {
		if len(got[k]) == 0 || got[k][0] != v {
			t.Errorf("%s = %q, ingin %q", k, got[k], v)
		}
	}
}

// Blok ssh-config dibaca OpenSSH: Match exec benar-benar menjalankan
// `<easyrun> ssh-prepare <id>` dan semua opsi kontrak terpasang.
func TestConfigBlockDibacaOpenSSH(t *testing.T) {
	requireSSH(t)
	exe, argsFile := fakeExe(t, "bin dir $x")
	home := filepath.Join(t.TempDir(), "Budi Santoso", ".easyrun")
	block, err := ConfigBlock(params(exe, home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, `Match host uji-box exec "'`) {
		t.Fatalf("blok Match:\n%s", block)
	}
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sshG(t, "-F", cfg, "uji-box")
	if args, _ := os.ReadFile(argsFile); string(args) != "ssh-prepare\n"+testID+"\n" {
		t.Fatalf("Match exec tidak menjalankan ssh-prepare: %q", args)
	}
	want := map[string]string{
		"hostname":              "uji-box.easyrun.io",
		"user":                  "ubuntu",
		"hostkeyalias":          testID,
		"identitiesonly":        "yes",
		"stricthostkeychecking": "accept-new",
		"serveraliveinterval":   "30",
		"forwardagent":          "no",
		"forwardx11":            "no",
		"identityfile":          filepath.Join(home, "ssh", "id_ed25519"),
		"certificatefile":       filepath.Join(home, "ssh", testID+"-cert.pub"),
		"userknownhostsfile":    filepath.Join(home, "known_hosts"),
		"proxycommand":          "'" + exe + "' tunnel " + testID,
	}
	for k, v := range want {
		if len(got[k]) == 0 || got[k][0] != v {
			t.Errorf("%s = %q, ingin %q", k, got[k], v)
		}
	}
	// Host lain tidak tersentuh blok ini.
	_ = os.Remove(argsFile)
	other := sshG(t, "-F", cfg, "host-lain.example")
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatal("Match exec jalan untuk host lain")
	}
	if len(other["proxycommand"]) > 0 && other["proxycommand"][0] != "none" {
		t.Fatalf("host lain mendapat ProxyCommand: %q", other["proxycommand"])
	}
}

func TestConfigBlockWindowsPath(t *testing.T) {
	p := Params{
		Alias: "coba", ID: testID, Hostname: "coba.easyrun.io", GOOS: "windows",
		Exe:             `C:\Program Files\EasyRun\easyrun.exe`,
		IdentityFile:    `C:\Users\Budi Santoso\.easyrun\ssh\id_ed25519`,
		CertificateFile: `C:\Users\Budi Santoso\.easyrun\ssh\` + testID + `-cert.pub`,
		KnownHostsFile:  `C:\Users\Budi Santoso\.easyrun\known_hosts`,
	}
	block, err := ConfigBlock(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Match host coba exec "\"C:\\Program Files\\EasyRun\\easyrun.exe\" ssh-prepare ` + testID + `"`,
		`IdentityFile "C:/Users/Budi Santoso/.easyrun/ssh/id_ed25519"`,
		`ProxyCommand "C:\Program Files\EasyRun\easyrun.exe" tunnel ` + testID,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("blok tak memuat %q:\n%s", want, block)
		}
	}
}

// Temuan review 1: ProxyCommand/Match exec membawa folder konfigurasi dan
// alamat API, dikutip dengan aturan yang sama dengan path program.
func TestInvocationMembawaFolderDanAlamatAPI(t *testing.T) {
	cases := []struct {
		v    Invocation
		sub  string
		want string
	}{
		{Invocation{Exe: "/usr/local/bin/easyrun", ConfigDir: "/Users/budi lain/.easyrun", APIURL: "https://api.contoh.test", GOOS: "darwin"},
			"tunnel", "/usr/local/bin/easyrun --config-dir '/Users/budi lain/.easyrun' --api-url https://api.contoh.test tunnel " + testID},
		{Invocation{Exe: "/usr/local/bin/easyrun", ConfigDir: "/data/100%/.easyrun", GOOS: "linux"},
			"ssh-prepare", "/usr/local/bin/easyrun --config-dir '/data/100%%/.easyrun' ssh-prepare " + testID},
		{Invocation{Exe: `C:\Program Files\EasyRun\easyrun.exe`, ConfigDir: `C:\Users\budi\.easyrun`, GOOS: "windows"},
			"tunnel", `"C:\Program Files\EasyRun\easyrun.exe" --config-dir C:\Users\budi\.easyrun tunnel ` + testID},
		{Invocation{Exe: "/usr/local/bin/easyrun", GOOS: "linux"},
			"tunnel", "/usr/local/bin/easyrun tunnel " + testID},
	}
	for _, c := range cases {
		f := ProxyCommand
		if c.sub == "ssh-prepare" {
			f = PrepareCommand
		}
		got, err := f(c.v, testID)
		if err != nil || got != c.want {
			t.Errorf("%+v:\n got %q %v\nwant %q", c.v, got, err, c.want)
		}
	}
	if _, err := ProxyCommand(Invocation{Exe: "/usr/local/bin/easyrun", ConfigDir: "/tmp/a'b", GOOS: "linux"}, testID); err == nil {
		t.Fatal("folder berisi ' harus ditolak")
	}
}

// Folder berisi spasi dan karakter shell sampai utuh sebagai satu argumen.
func TestInvocationAmanDiShell(t *testing.T) {
	exe, argsFile := fakeExe(t, "bin")
	dir := filepath.Join(t.TempDir(), "folder $(touch PWNED) & x")
	cmd, err := PrepareCommand(Invocation{Exe: exe, ConfigDir: dir, APIURL: "https://api.contoh.test", GOOS: runtime.GOOS}, testID)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-c", "exec "+cmd)
	c.Dir = t.TempDir()
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	want := "--config-dir\n" + dir + "\n--api-url\nhttps://api.contoh.test\nssh-prepare\n" + testID + "\n"
	if got, _ := os.ReadFile(argsFile); string(got) != want {
		t.Fatalf("argumen %q, ingin %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "PWNED")); err == nil {
		t.Fatal("perintah sisipan jalan")
	}
}
