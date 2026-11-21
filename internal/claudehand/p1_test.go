package claudehand

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// EnvP1Dir — folder rujukan P1 (berisi box/easybox-bridge,
// box/easybox-hand-connect) untuk uji silang. Tidak diisi → uji dilewati.
const EnvP1Dir = "EASYBOX_P1_DIR"

// bridgeClient — peran claude "otak" di box: klien MCP stdio bagi jembatan.
type bridgeClient struct {
	t    *testing.T
	in   io.WriteCloser
	mu   sync.Mutex
	cond *sync.Cond
	got  map[float64]map[string]any
	nots []map[string]any
	nid  float64
}

func (c *bridgeClient) rpc(method string, params any, timeout time.Duration) map[string]any {
	c.mu.Lock()
	c.nid++
	id := c.nid
	c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.got[id] == nil && time.Now().Before(deadline) {
		go func() { time.Sleep(100 * time.Millisecond); c.mu.Lock(); c.cond.Broadcast(); c.mu.Unlock() }()
		c.cond.Wait()
	}
	m := c.got[id]
	delete(c.got, id)
	return m
}

func (c *bridgeClient) call(name string, args map[string]any, timeout time.Duration) (bool, string) {
	if args == nil {
		args = map[string]any{}
	}
	return callText(c.rpc("tools/call", map[string]any{"name": name, "arguments": args}, timeout))
}

// p1Hand — penjaga Go yang tersambung lewat easybox-hand-connect P1 (perintah
// lokal pengganti ssh), dengan `claude mcp serve` nyata.
type p1Hand struct {
	cancel context.CancelFunc
	done   chan Result
}

func startP1Hand(t *testing.T, p1, ebHome, cfgDir, root string, readOnly bool, audit *Audit, claudeVer string) *p1Hand {
	t.Helper()
	home, _ := os.UserHomeDir()
	pol, err := NewPolicy(PolicyConfig{Root: root, Home: home, ReadOnly: readOnly, ExtraSecrets: []string{cfgDir}})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	line, err := BuildHello(pol, HelloInfo{Host: host, User: LocalUser(os.Getenv), OS: OSString(), ClaudeVersion: claudeVer, Guard: "easyrun-cli/uji"}).Line()
	if err != nil {
		t.Fatal(err)
	}
	link := exec.Command("python3", filepath.Join(p1, "box", "easybox-hand-connect"), "w1", "--wait", "20")
	link.Env = append(os.Environ(), "EASYBOX_HOME="+ebHome)
	stdin, _ := link.StdinPipe()
	stdout, _ := link.StdoutPipe()
	if err := link.Start(); err != nil {
		t.Fatal(err)
	}
	claude, _ := exec.LookPath("claude")
	srv := ServerCommand{Argv: []string{claude, "mcp", "serve"}, Dir: pol.Root, Env: ServerEnv(os.Environ(), cfgDir)}
	ctx, cancel := context.WithCancel(context.Background())
	h := &p1Hand{cancel: cancel, done: make(chan Result, 1)}
	go func() {
		res := Serve(ctx, stdout, stdin, Config{Policy: pol, Hello: line, Start: srv.Start, Audit: audit, Stats: &Stats{}})
		_ = stdin.Close()
		exited := make(chan struct{})
		go func() { _ = link.Wait(); close(exited) }()
		StopProcess(link.Process, exited, 5*time.Second)
		h.done <- res
	}()
	return h
}

func (h *p1Hand) stop(t *testing.T) Result {
	h.cancel()
	select {
	case r := <-h.done:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("tangan tidak berhenti")
	}
	return Result{}
}

// TestSilangJembatanP1 — penjaga Go ⇄ jembatan python P1 (`easybox-bridge` +
// `easybox-hand-connect`, dijalankan lokal dengan EASYBOX_HOME sementara) ⇄
// `claude mcp serve` nyata. Cermin test/test_protocol.py P1 (bagian yang
// menyangkut penjaga), ditambah pemeriksaan khas penjaga Go.
func TestSilangJembatanP1(t *testing.T) {
	p1 := os.Getenv(EnvP1Dir)
	if p1 == "" || os.Getenv("CI") != "" {
		t.Skipf("isi %s=<folder P1> untuk uji silang (dilewati di CI)", EnvP1Dir)
	}
	for _, f := range []string{"box/easybox-bridge", "box/easybox-hand-connect"} {
		if _, err := os.Stat(filepath.Join(p1, f)); err != nil {
			t.Skipf("%s tidak ada: %v", f, err)
		}
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 tidak ada")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude tidak ada")
	}
	verOut, _ := exec.Command("claude", "--version").Output()
	claudeVer := strings.TrimSpace(string(verOut))

	base, _ := filepath.EvalSymlinks(t.TempDir())
	ebHome := filepath.Join(base, "eb")
	cfgDir := filepath.Join(base, "cfg")
	A, LUAR, B := filepath.Join(base, "lapA", "proj"), filepath.Join(base, "lapA", "luar"), filepath.Join(base, "lapB", "proj2")
	for _, d := range []string{A + "/sub", LUAR, B, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, A+"/a.txt", "isi-dalam\n")
	write(t, A+"/CLAUDE.md", "Aturan proyek A: pakai tab.\n")
	write(t, LUAR+"/b.txt", "rahasia-luar\n")
	write(t, B+"/c.txt", "isi-laptop-B\n")
	if err := os.Symlink(LUAR, A+"/link"); err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAudit(filepath.Join(cfgDir, "claude-audit.log"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()

	bridge := exec.Command("python3", filepath.Join(p1, "box", "easybox-bridge"), "w1")
	bridge.Env = append(os.Environ(), "EASYBOX_HOME="+ebHome)
	bin, _ := bridge.StdinPipe()
	bout, _ := bridge.StdoutPipe()
	if err := bridge.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bin.Close(); _ = bridge.Wait() }()
	c := &bridgeClient{t: t, in: bin, got: map[float64]map[string]any{}}
	c.cond = sync.NewCond(&c.mu)
	go func() {
		sc := bufio.NewScanner(bout)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			c.mu.Lock()
			if id, ok := m["id"].(float64); ok {
				c.got[id] = m
			} else {
				c.nots = append(c.nots, m)
			}
			c.cond.Broadcast()
			c.mu.Unlock()
		}
	}()
	check := func(label string, ok bool, detail ...any) {
		t.Helper()
		if !ok {
			t.Errorf("GAGAL %s %v", label, detail)
		} else {
			t.Logf("LULUS %s", label)
		}
	}
	waitRoot := func(root, want string) string {
		end := time.Now().Add(40 * time.Second)
		for time.Now().Before(end) {
			e, txt := c.call("laptop_info", nil, 30*time.Second)
			if !e && strings.Contains(txt, "root: "+root) && strings.Contains(txt, want) {
				return txt
			}
			time.Sleep(300 * time.Millisecond)
		}
		return ""
	}

	r := c.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "uji", "version": "0"}}, 20*time.Second)
	check("1 initialize dijawab jembatan", r != nil && r["result"] != nil, r)
	_, _ = bin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	e, txt := c.call("Read", map[string]any{"file_path": A + "/a.txt"}, 30*time.Second)
	check("4 Read tanpa laptop → galat, tidak menggantung", e && strings.Contains(txt, "tidak tersambung"), txt)

	hA := startP1Hand(t, p1, ebHome, cfgDir, A, false, audit, claudeVer)
	info := waitRoot(A, "Aturan proyek A")
	check("5 laptop A tersambung lewat penjaga Go, laptop_info memuat root + CLAUDE.md", info != "", info)
	r = c.rpc("tools/list", map[string]any{}, 20*time.Second)
	var names []string
	if r != nil {
		for _, tl := range r["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tl.(map[string]any)["name"].(string))
		}
	}
	check("5b tools/list dari claude mcp serve nyata disaring ke allowlist", strings.Join(names, ",") == "Bash,Read,Edit,Write,laptop_info", names)
	e, txt = c.call("Read", map[string]any{"file_path": A + "/a.txt"}, 30*time.Second)
	check("6 Read di dalam root", !e && strings.Contains(txt, "isi-dalam"), txt)
	e, txt = c.call("Read", map[string]any{"file_path": LUAR + "/b.txt"}, 30*time.Second)
	check("7 Read di luar root ditolak (isi tidak bocor)", e && strings.Contains(txt, "di luar root") && !strings.Contains(txt, "rahasia-luar"), txt)
	e, txt = c.call("Read", map[string]any{"file_path": A + "/link/b.txt"}, 30*time.Second)
	check("8 symlink keluar root ditolak", e && strings.Contains(txt, "di luar root"), txt)
	e, txt = c.call("Read", map[string]any{"file_path": A + "/sub/../../luar/b.txt"}, 30*time.Second)
	check("9 path '..' ditolak", e && strings.Contains(txt, "'..'"), txt)
	e, txt = c.call("Workflow", map[string]any{"script": "x"}, 30*time.Second)
	check("10 tool di luar allowlist ditolak", e && strings.Contains(txt, "tidak diizinkan"), txt)
	e, txt = c.call("Bash", map[string]any{"command": "pwd", "description": "uji"}, 60*time.Second)
	check("11 Bash jalan di root laptop", !e && strings.Contains(txt, A), txt)
	e, txt = c.call("Bash", map[string]any{"command": "sleep 1", "run_in_background": true}, 30*time.Second)
	check("12 Bash latar ditolak", e && strings.Contains(txt, "run_in_background"), txt)
	e, txt = c.call("Edit", map[string]any{"file_path": A + "/a.txt", "old_string": "isi-dalam", "new_string": "isi-baru"}, 30*time.Second)
	got, _ := os.ReadFile(A + "/a.txt")
	check("13 Edit mengubah berkas di laptop", !e && string(got) == "isi-baru\n", txt)
	e, txt = c.call("Write", map[string]any{"file_path": A + "/sub/baru/n.txt", "content": "ISI-TULISAN\n"}, 30*time.Second)
	_, statErr := os.Stat(A + "/sub/baru/n.txt")
	check("14 Write berkas baru (folder baru) di dalam root", !e && statErr == nil, txt)
	home, _ := os.UserHomeDir()
	e, txt = c.call("Write", map[string]any{"file_path": home + "/.ssh/uji-easybox-go", "content": "x"}, 30*time.Second)
	_, statErr = os.Stat(home + "/.ssh/uji-easybox-go")
	check("15 Write ke ~/.ssh ditolak", e && statErr != nil, txt)

	var wg sync.WaitGroup
	var e16 bool
	var t16 string
	wg.Add(1)
	go func() {
		defer wg.Done()
		e16, t16 = c.call("Bash", map[string]any{"command": "sleep 8; echo selesai", "description": "uji"}, 30*time.Second)
	}()
	time.Sleep(3 * time.Second)
	resA := hA.stop(t)
	wg.Wait()
	check("16 laptop lepas saat perintah jalan → 'putus', tidak menggantung", e16 && strings.Contains(t16, "putus"), t16)
	check("16b hasil sambungan A: jabat tangan selesai, tidak digantikan", resA.Ready && !resA.Replaced, resA)
	e, txt = c.call("laptop_info", nil, 30*time.Second)
	check("17 setelah lepas: tidak tersambung", e, txt)

	hB := startP1Hand(t, p1, ebHome, cfgDir, B, false, audit, claudeVer)
	check("18 pindah ke laptop/folder B: root baru", waitRoot(B, "") != "")
	e, txt = c.call("Read", map[string]any{"file_path": B + "/c.txt"}, 30*time.Second)
	check("19 Read di laptop B", !e && strings.Contains(txt, "isi-laptop-B"), txt)
	e, txt = c.call("Read", map[string]any{"file_path": A + "/a.txt"}, 30*time.Second)
	check("20 folder A kini di luar root", e && strings.Contains(txt, "di luar root"), txt)

	hA2 := startP1Hand(t, p1, ebHome, cfgDir, A, false, audit, claudeVer)
	check("21 laptop terbaru menang (A2 menggantikan B)", waitRoot(A, "") != "")
	select {
	case resB := <-hB.done:
		check("22 tangan lama (B) ditutup dan tahu ia digantikan", resB.Replaced, resB)
	case <-time.After(15 * time.Second):
		check("22 tangan lama (B) ditutup", false, "masih hidup")
		hB.stop(t)
	}
	hA2.stop(t)

	hR := startP1Hand(t, p1, ebHome, cfgDir, A, true, audit, claudeVer)
	check("23a read-only tersambung", waitRoot(A, "read-only") != "")
	e, txt = c.call("Bash", map[string]any{"command": "id"}, 30*time.Second)
	check("23 read-only: Bash ditolak", e && strings.Contains(txt, "tidak diizinkan"), txt)
	e, txt = c.call("Edit", map[string]any{"file_path": A + "/a.txt", "old_string": "isi-baru", "new_string": "x"}, 30*time.Second)
	got, _ = os.ReadFile(A + "/a.txt")
	check("24 read-only: Edit ditolak", e && string(got) == "isi-baru\n", txt)
	e, txt = c.call("Read", map[string]any{"file_path": A + "/a.txt"}, 30*time.Second)
	check("25 read-only: Read boleh", !e && strings.Contains(txt, "isi-baru"), txt)
	time.Sleep(time.Second)
	r = c.rpc("tools/list", map[string]any{}, 20*time.Second)
	names = nil
	if r != nil {
		for _, tl := range r["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tl.(map[string]any)["name"].(string))
		}
	}
	c.mu.Lock()
	changed := false
	for _, n := range c.nots {
		changed = changed || n["method"] == "notifications/tools/list_changed"
	}
	c.mu.Unlock()
	check("26 ganti mode → list_changed + daftar tool menyusut", changed && strings.Join(names, ",") == "Read,laptop_info", changed, names)
	hR.stop(t)

	_ = audit.Close()
	alog, _ := os.ReadFile(audit.Path)
	blog, _ := os.ReadFile(filepath.Join(ebHome, "logs", "bridge-w1.log"))
	for name, l := range map[string]string{"audit laptop": string(alog), "log jembatan": string(blog)} {
		check("31 "+name+" tanpa isi berkas", !strings.Contains(l, "ISI-TULISAN") && !strings.Contains(l, "rahasia-luar") && !strings.Contains(l, "isi-dalam"), l)
	}
	check("32 audit laptop mencatat aksi + path/perintah", strings.Contains(string(alog), "Read "+A+"/a.txt") && strings.Contains(string(alog), `Bash "pwd"`) && strings.Contains(string(alog), "tolak tool Workflow"), string(alog))
}
