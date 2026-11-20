package cli

// Program palsu untuk uji `easyrun claude`: biner uji ini berperan sebagai
// `ssh` (ControlMaster tiruan + "box" yang menjalankan easybox) dan `claude`
// (`claude mcp serve` tiruan) bila dipanggil lewat symlink bernama itu dengan
// env envFake=1. Semua keadaan lewat env dan folder FAKE_DIR.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const envFake = "EASYRUN_CLI_TEST_FAKE"

// fakeMain — dipanggil TestMain; false = bukan program palsu.
func fakeMain() (int, bool) {
	if os.Getenv(envFake) != "1" {
		return 0, false
	}
	switch filepath.Base(os.Args[0]) {
	case "ssh":
		return fakeSSH(os.Args[1:]), true
	case "claude":
		return fakeClaude(os.Args[1:]), true
	case "easybox-hand-connect":
		return fakeHandConnect(os.Args[1:]), true
	}
	return 0, false
}

type sshCall struct {
	Opts   map[string]string `json:"opts"`
	Flags  []string          `json:"flags"`
	Op     string            `json:"op"`
	Host   string            `json:"host"`
	Remote []string          `json:"remote"`
}

func parseSSHArgs(args []string) sshCall {
	c := sshCall{Opts: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" && i+1 < len(args):
			k, v, _ := strings.Cut(args[i+1], "=")
			if _, ok := c.Opts[k]; !ok { // ssh: nilai pertama menang
				c.Opts[k] = strings.ReplaceAll(strings.Trim(v, `"`), "%%", "%")
			}
			i++
		case a == "-O" && i+1 < len(args):
			c.Op = args[i+1]
			i++
		case strings.HasPrefix(a, "-") && c.Host == "":
			c.Flags = append(c.Flags, a)
		case c.Host == "":
			c.Host = a
		default:
			c.Remote = args[i:]
			i = len(args)
		}
	}
	return c
}

func fakeLog(name string, v any) {
	b, _ := json.Marshal(v)
	f, err := os.OpenFile(filepath.Join(os.Getenv("FAKE_DIR"), name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func muxDial(path, msg string) bool {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg + "\n")); err != nil {
		return false
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line) == "ok"
}

func fakeSSH(args []string) int {
	c := parseSSHArgs(args)
	fakeLog("ssh.log", c)
	cp := c.Opts["ControlPath"]
	switch {
	case c.Op == "check":
		if muxDial(cp, "check") {
			return 0
		}
		return 255
	case c.Op == "exit":
		if muxDial(cp, "exit") {
			return 0
		}
		return 255
	case c.Opts["ControlMaster"] == "yes":
		return fakeMaster(cp)
	}
	if c.Opts["ControlMaster"] == "no" && cp != "" && !muxDial(cp, "check") {
		// ssh sungguhan akan menyambung langsung (sesi SSH kedua); uji ini
		// menganggapnya galat supaya kebocoran sesi ketahuan.
		fmt.Fprintln(os.Stderr, "palsu: master tidak ada di", cp)
		return 255
	}
	return fakeBox(c.Remote)
}

func fakeMaster(path string) int {
	if msg := os.Getenv("FAKE_MASTER_FAIL"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 255
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "palsu: listen:", err)
		return 255
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	done := make(chan int, 1)
	go func() {
		<-sig
		done <- 143
	}()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			_, _ = conn.Write([]byte("ok\n"))
			_ = conn.Close()
			if strings.TrimSpace(line) == "exit" {
				done <- 0
				return
			}
		}
	}()
	code := <-done
	_ = l.Close()
	_ = os.Remove(path)
	return code
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func fakeBox(remote []string) int {
	if len(remote) == 0 {
		return 0
	}
	switch remote[0] {
	case "easybox":
		if len(remote) < 2 {
			return 2
		}
		fmt.Fprintln(os.Stderr, "palsu: easybox", remote[1])
		switch remote[1] {
		case "version":
			if d, err := time.ParseDuration(os.Getenv("FAKE_VERSION_SLEEP")); err == nil {
				time.Sleep(d)
			}
			v := os.Getenv("FAKE_VERSION_JSON")
			if v == "" {
				v = `{"easybox_protocol":1,"easybox_version":"0.2.0","claude_path":"/home/ubuntu/.local/bin/claude","claude_version":"2.1.329","claude_logged_in":true,"herdr_version":"0.9.0"}`
			}
			fmt.Println(v)
			return envInt("FAKE_VERSION_CODE", 0)
		case "ensure":
			if code := envInt("FAKE_ENSURE_CODE", 0); code != 0 {
				return code
			}
			fmt.Printf(`{"session":%q,"pane":%q,"state":"started"}`+"\n", remote[2], remote[3])
			return 0
		case "ls":
			v := os.Getenv("FAKE_LS_JSON")
			if v == "" {
				v = `{"panes":[]}`
			}
			fmt.Println(v)
			return 0
		case "stop":
			fmt.Printf(`{"pane":%q,"stopped":%s}`+"\n", remote[2], os.Getenv("FAKE_STOPPED"))
			return 0
		case "attach-ui":
			p := filepath.Join(os.Getenv("FAKE_DIR"), "ui-exit")
			end := time.Now().Add(60 * time.Second)
			for time.Now().Before(end) {
				if b, err := os.ReadFile(p); err == nil {
					return envIntStr(strings.TrimSpace(string(b)))
				}
				time.Sleep(50 * time.Millisecond)
			}
			return 99
		}
		return 2
	case "easybox-hand-connect":
		return fakeHandConnect(remote[1:])
	}
	fmt.Fprintf(os.Stderr, "bash: %s: command not found\n", remote[0])
	return 127
}

func envIntStr(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// fakeHandConnect — seperti box/easybox-hand-connect P1: pipa stdin/stdout ⇄
// $FAKE_EB/hands/<pane>.sock, tunggu soket ≤ --wait detik, kode 3 bila tidak ada.
func fakeHandConnect(args []string) int {
	if len(args) != 3 || args[1] != "--wait" {
		return 2
	}
	wait, _ := strconv.Atoi(args[2])
	sock := filepath.Join(os.Getenv("FAKE_EB"), "hands", args[0]+".sock")
	end := time.Now().Add(time.Duration(wait) * time.Second)
	var conn net.Conn
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(end) {
			fmt.Fprintf(os.Stderr, "easybox: tidak ada sesi claude bernama '%s'\n", args[0])
			return 3
		}
		time.Sleep(100 * time.Millisecond)
	}
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, _ = io.Copy(os.Stdout, conn)
	return 0
}

// fakeClaude — `claude --version`, `claude mcp serve --help`, `claude mcp serve`.
func fakeClaude(args []string) int {
	switch strings.Join(args, " ") {
	case "--version":
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	case "mcp serve --help":
		if os.Getenv("FAKE_CLAUDE_NO_MCP") != "" {
			fmt.Fprintln(os.Stderr, "error: unknown command 'mcp'")
			return 1
		}
		fmt.Println("Usage: claude mcp serve [options]")
		return 0
	case "mcp serve":
	default:
		return 1
	}
	cwd, _ := os.Getwd()
	fakeLog("claude.log", map[string]any{
		"cwd": cwd, "config": os.Getenv("CLAUDE_CONFIG_DIR"),
		"api_key": os.Getenv("ANTHROPIC_API_KEY") != "", "oauth": os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "",
	})
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		id := m["id"]
		switch m["method"] {
		case "initialize":
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "claude/palsu"}}})
		case "tools/list":
			var tools []any
			for _, n := range []string{"Agent", "Bash", "Read", "Edit", "Write", "Workflow"} {
				tools = append(tools, map[string]any{"name": n, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{}, "command": map[string]any{}}}})
			}
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": tools}})
		case "tools/call":
			p, _ := m["params"].(map[string]any)
			a, _ := p["arguments"].(map[string]any)
			text := "dijalankan"
			if p["name"] == "Read" {
				b, err := os.ReadFile(fmt.Sprint(a["file_path"]))
				text = string(b)
				if err != nil {
					text = err.Error()
				}
			}
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
		}
	}
	return 0
}
