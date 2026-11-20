package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

// TestE2EClaudeLewatControlMaster — `easyrun claude uji-box w1 attach` dengan
// ssh SUNGGUHAN: master ControlMaster → ProxyCommand `easyrun tunnel` (biner
// uji ini) → WebSocket panel palsu → `sshd -i` sungguhan. Perintah easybox di
// "box" berupa skrip palsu (lewat App.boxBin), jembatan palsu di soket unix.
// Yang dibuktikan: version, ensure, ls, tangan, dan UI berjalan lewat SATU
// sambungan (satu tiket, satu WebSocket, satu autentikasi sshd) — inilah yang
// membuat attach dihitung satu sesi SSH box — dan ControlPath berspasi
// dikutip dengan benar untuk ssh.
//
// Hanya Linux: di macOS sshd non-root mati setelah autentikasi (BSM audit),
// sehingga master tidak pernah siap.
func TestE2EClaudeLewatControlMaster(t *testing.T) {
	if runtime.GOOS != "linux" || testing.Short() {
		t.Skip("butuh OpenSSH di Linux")
	}
	sshPath, err := exec.LookPath("ssh")
	sshd := findSSHD()
	if err != nil || sshd == "" {
		t.Skip("ssh/sshd tidak terpasang")
	}
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	work := t.TempDir()
	p := newPanel(t)
	hostKey := filepath.Join(work, "host_ed25519")
	writePrivateKey(t, hostKey)
	caPub := filepath.Join(work, "ca.pub")
	principals := filepath.Join(work, "principals")
	_ = os.WriteFile(caPub, p.ca.PublicLine(), 0o644)
	_ = os.WriteFile(principals, []byte(boxID+"\n"), 0o644)
	sshdCfg := filepath.Join(work, "sshd_config")
	_ = os.WriteFile(sshdCfg, []byte(strings.Join([]string{
		"HostKey " + hostKey, "PidFile none", "UsePAM no", "StrictModes no",
		"AuthorizedKeysFile none", "PasswordAuthentication no", "KbdInteractiveAuthentication no",
		"PubkeyAuthentication yes", "TrustedUserCAKeys " + caPub,
		"AuthorizedPrincipalsFile " + principals, "AllowTcpForwarding local",
		"AllowAgentForwarding no", "X11Forwarding no", "PermitTunnel no",
		"MaxSessions 4", "LogLevel VERBOSE", "",
	}, "\n")), 0o600)
	{ // pra-uji seperti TestE2ESSHLewatTunnelKeSshd
		key := filepath.Join(work, "pre_key")
		pub := writePrivateKey(t, key)
		now := time.Now()
		_ = os.WriteFile(key+"-cert.pub", []byte(p.ca.SignLine(t, pub, boxID, now.Add(-time.Minute), now.Add(time.Hour))), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, _ := exec.CommandContext(ctx, sshPath, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-i", key, "-o", "CertificateFile="+key+"-cert.pub", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "ProxyCommand="+sshd+" -i -e -f "+sshdCfg,
			"-l", me.Username, "pra-uji", "echo", "PRA-OK").CombinedOutput()
		cancel()
		if !strings.Contains(string(out), "PRA-OK") {
			t.Skipf("sshd -i tidak bisa menjalankan perintah di lingkungan ini:\n%s", out)
		}
	}
	var sshdLog syncBuffer
	p.ws = bridgeSSHD(sshd, sshdCfg, &sshdLog)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// "Box": skrip easybox palsu + easybox-hand-connect (biner uji) lewat
	// pembungkus yang membawa env-nya (sshd tidak meneruskan env laptop).
	fake := filepath.Join(work, "fake")
	boxBin := filepath.Join(work, "boxbin")
	hcDir := filepath.Join(work, "hc")
	eb, err := os.MkdirTemp("", "eb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(eb) })
	for _, d := range []string{fake, boxBin, hcDir, filepath.Join(eb, "hands")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(hcDir, "easybox-hand-connect")); err != nil {
		t.Fatal(err)
	}
	uiExit := filepath.Join(fake, "ui-exit")
	writeExec(t, filepath.Join(boxBin, "easybox"), fmt.Sprintf(`#!/bin/sh
echo "easybox $*" >> %[1]q
case "$1" in
version) echo '{"easybox_protocol":1,"easybox_version":"0.2.0","claude_path":"/x/claude","claude_version":"2.1.329","claude_logged_in":true,"herdr_version":"0.9.0"}' ;;
ensure) echo "{\"session\":\"$2\",\"pane\":\"$3\",\"state\":\"started\"}" ;;
ls) echo '{"panes":[{"pane":"w1","session":"main","bridge_running":true,"started_at":null,"laptop":null}]}' ;;
attach-ui) while [ ! -f %[2]q ]; do sleep 0.1; done; exit "$(cat %[2]q)" ;;
*) exit 2 ;;
esac
`, filepath.Join(fake, "box.log"), uiExit))
	writeExec(t, filepath.Join(boxBin, "easybox-hand-connect"), fmt.Sprintf("#!/bin/sh\nexec env %s=1 FAKE_EB=%q %q \"$@\"\n", envFake, eb, filepath.Join(hcDir, "easybox-hand-connect")))

	home := filepath.Join(work, "home")
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(proj, "a.txt"), []byte("isi-dalam\n"), 0o600)
	claudeBin := filepath.Join(work, "lbin")
	_ = os.MkdirAll(claudeBin, 0o700)
	if err := os.Symlink(self, filepath.Join(claudeBin, "claude")); err != nil {
		t.Fatal(err)
	}
	// Folder config berspasi tetapi pendek: ControlPath tetap di <config>/cm
	// dan harus dikutip untuk -o.
	cfgDir := filepath.Join(work, "c d", ".easyrun")
	d := config.Dir{Path: cfgDir}
	if err := d.SaveCredentials(config.Credentials{APIURL: p.srv.URL, AccessToken: testToken, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{config.EnvConfigDir: cfgDir, config.EnvAPIURL: p.srv.URL}
	childEnv := []string{envMain + "=1", envFake + "=1", "FAKE_DIR=" + fake}
	for k, v := range env {
		childEnv = append(childEnv, k+"="+v)
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") && !strings.HasPrefix(kv, "EASYRUN_") {
			childEnv = append(childEnv, kv)
		}
	}

	var bmu sync.Mutex
	var readOK, denied bool
	var names []string
	ch := &claudeHarness{t: t, eb: eb, fake: fake}
	ch.serveBridge("w1", func(b *bridgeConn) {
		n := b.handshake()
		e1, t1 := b.call("Read", map[string]any{"file_path": proj + "/a.txt"})
		e2, _ := b.call("Read", map[string]any{"file_path": "/etc/passwd"})
		bmu.Lock()
		names, readOK, denied = n, !e1 && strings.Contains(t1, "isi-dalam"), e2
		bmu.Unlock()
		_ = os.WriteFile(uiExit, []byte("0"), 0o600)
		drain(b)
	})

	var stdout, stderr syncBuffer
	a := New(strings.NewReader(""), &stdout, &stderr)
	a.Getenv = func(k string) string { return env[k] }
	a.Environ = func() []string { return childEnv }
	a.HTTP = p.srv.Client()
	a.Executable = func() (string, error) { return self, nil }
	a.HomeDir = func() (string, error) { return home, nil }
	a.LookPath = func(name string) (string, error) {
		switch name {
		case "ssh":
			return sshPath, nil
		case "claude":
			return filepath.Join(claudeBin, "claude"), nil
		}
		return "", errors.New("tidak ada")
	}
	a.Getwd = func() (string, error) { return proj, nil }
	a.StdinIsTerminal = func() bool { return true }
	a.sshUser = me.Username
	a.boxBin = boxBin

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	code := a.Run(ctx, []string{"claude", "uji-box", "w1", "attach"})
	boxLog, _ := os.ReadFile(filepath.Join(fake, "box.log"))
	if code != 0 {
		t.Fatalf("kode %d\nstderr:\n%s\nbox:\n%s\nsshd:\n%s", code, stderr.String(), boxLog, sshdLog.String())
	}
	bmu.Lock()
	if strings.Join(names, ",") != "Bash,Read,Edit,Write" || !readOK || !denied {
		t.Errorf("tangan lewat ssh sungguhan: tools %v, Read %v, tolak %v", names, readOK, denied)
	}
	bmu.Unlock()
	for _, want := range []string{"easybox version", "easybox ensure main w1", "easybox ls", "easybox attach-ui main w1"} {
		if !strings.Contains(string(boxLog), want) {
			t.Errorf("box tidak menjalankan %q:\n%s", want, boxLog)
		}
	}
	if n := p.n("POST /instances/ticket"); n != 1 {
		t.Errorf("tiket SSH diminta %d kali, ingin 1 (satu sambungan untuk semua saluran)", n)
	}
	if n := p.n("GET /api/v1/console/ws"); n != 1 {
		t.Errorf("WebSocket dibuka %d kali, ingin 1", n)
	}
	if n := strings.Count(sshdLog.String(), "Accepted publickey"); n != 1 {
		t.Errorf("autentikasi sshd %d kali, ingin 1\n%s", n, sshdLog.String())
	}
	if !strings.Contains(stderr.String(), "2 panggilan tool, 1 ditolak penjaga") {
		t.Errorf("ringkasan:\n%s", stderr.String())
	}
	if ents, _ := os.ReadDir(filepath.Join(cfgDir, controlDirName)); len(ents) != 0 {
		t.Errorf("soket master tertinggal: %v", ents)
	}
	t.Logf("stderr easyrun:\n%s", stderr.String())

	// ls tanpa master: satu sambungan sendiri.
	stdout.b.Reset()
	if code := a.Run(ctx, []string{"claude", "uji-box", "ls"}); code != 0 || !strings.Contains(stdout.String(), "w1") {
		t.Errorf("ls lewat ssh sungguhan: %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
