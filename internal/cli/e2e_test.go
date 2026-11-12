package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
	"github.com/EasyRunTech/easyrun-cli/internal/sshconf"
)

// syncBuffer — log sshd dari banyak sesi.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func findSSHD() string {
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func writePrivateKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := ssh.NewSignerFromKey(priv)
	return s.PublicKey()
}

// bridgeSSHD — peran orchestrator: exec `sshd -i` tanpa TTY, frame biner =
// stdin/stdout sshd, stderr ke log (tidak diteruskan), lalu exit {code}.
func bridgeSSHD(sshd, cfg string, log *syncBuffer) func(*websocket.Conn, *http.Request) {
	return func(c *websocket.Conn, r *http.Request) {
		var wmu sync.Mutex
		send := func(mt int, b []byte) {
			wmu.Lock()
			defer wmu.Unlock()
			_ = c.WriteMessage(mt, b)
		}
		cmd := exec.Command(sshd, "-i", "-e", "-f", cfg)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			send(websocket.TextMessage, []byte(`{"type":"error","message":"exec gagal"}`))
			return
		}
		send(websocket.TextMessage, []byte(`{"type":"ready"}`))
		go func() {
			defer stdin.Close()
			for {
				mt, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				switch mt {
				case websocket.BinaryMessage:
					if _, err := stdin.Write(data); err != nil {
						return
					}
				case websocket.TextMessage:
					send(websocket.TextMessage, []byte(`{"type":"pong"}`))
				}
			}
		}()
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				send(websocket.BinaryMessage, append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				break
			}
		}
		code := 0
		var ee *exec.ExitError
		if err := cmd.Wait(); errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		send(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"exit","code":%d}`, code)))
		wmu.Lock()
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		wmu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
}

// TestE2ESSHLewatTunnelKeSshd — ssh sungguhan → ProxyCommand (biner uji ini
// sebagai `easyrun tunnel`) → WebSocket panel palsu → `sshd -i` sungguhan yang
// hanya menerima sertifikat dari CA uji dengan principal = id box.
//
// macOS: sshd non-root mati setelah autentikasi ("Could not create new audit
// session", BSM), jadi di macOS yang dibuktikan adalah jabat tangan SSH +
// autentikasi sertifikat lewat tunnel; di Linux sampai perintah jalan.
func TestE2ESSHLewatTunnelKeSshd(t *testing.T) {
	if runtime.GOOS == "windows" || testing.Short() {
		t.Skip("butuh OpenSSH unix")
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
	p := newPanel(t) // jam sungguhan: proses anak memakai time.Now
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
		"Subsystem sftp internal-sftp", "LogLevel VERBOSE", "",
	}, "\n")), 0o600)

	// Pra-uji: sshd -i langsung lewat ProxyCommand (tanpa easyrun). Bila
	// lingkungan ini tidak bisa menjalankannya, lewati — bukan salah tunnel.
	{
		key := filepath.Join(work, "pre_key")
		pub := writePrivateKey(t, key)
		now := time.Now()
		_ = os.WriteFile(key+"-cert.pub", []byte(p.ca.SignLine(t, pub, boxID, now.Add(-time.Minute), now.Add(time.Hour))), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, _ := exec.CommandContext(ctx, sshPath, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-i", key, "-o", "CertificateFile="+key+"-cert.pub", "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "ProxyCommand="+sshd+" -i -e -f "+sshdCfg,
			"-l", me.Username, "pra-uji", "true").CombinedOutput()
		cancel()
		if !strings.Contains(string(out), "Accepted publickey") {
			t.Skipf("sshd -i tidak bisa dijalankan di lingkungan ini:\n%s", out)
		}
	}

	var sshdLog syncBuffer
	p.ws = bridgeSSHD(sshd, sshdCfg, &sshdLog)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(work, "home dengan spasi", ".easyrun")
	d := config.Dir{Path: cfgDir}
	if err := d.SaveCredentials(config.Credentials{APIURL: p.srv.URL, AccessToken: testToken, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{config.EnvConfigDir: cfgDir, config.EnvAPIURL: p.srv.URL, EnvNoBrowser: "1"}
	childEnv := []string{envMain + "=1"}
	for k, v := range env {
		childEnv = append(childEnv, k+"="+v)
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			childEnv = append(childEnv, kv)
		}
	}

	var sshOut, sshErr bytes.Buffer
	var stdout, stderr bytes.Buffer
	a := New(strings.NewReader(""), &stdout, &stderr)
	a.Getenv = func(k string) string { return env[k] }
	a.Environ = func() []string { return childEnv }
	a.HTTP = p.srv.Client()
	a.Executable = func() (string, error) { return self, nil }
	a.sshUser = me.Username
	a.ExecSSH = func(path string, argv, env []string) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, argv[1:]...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &sshOut, &sshErr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, err
	}

	// 1. `easyrun ssh contoh-box.easyrun.io -- …` (alamat lengkap, seperti
	//    petunjuk di panel).
	code := a.Run(context.Background(), []string{"ssh", "contoh-box.easyrun.io", "--", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ConnectTimeout=30", "echo", "E2E-OK"})
	log := sshdLog.String()
	if !strings.Contains(log, "Accepted publickey for "+me.Username) || !strings.Contains(log, "ED25519-CERT") || !strings.Contains(log, "principal:"+boxID) {
		t.Fatalf("sshd tidak menerima sertifikat lewat tunnel (kode %d)\nssh stderr:\n%s\nstderr easyrun:\n%s\nlog sshd:\n%s", code, sshErr.String(), stderr.String(), log)
	}
	fullSession := false
	if strings.Contains(sshOut.String(), "E2E-OK") {
		if code != 0 {
			t.Fatalf("perintah jalan tapi kode %d", code)
		}
		fullSession = true
		t.Logf("sesi penuh: %q", strings.TrimSpace(sshOut.String()))
	} else if runtime.GOOS == "darwin" && strings.Contains(log, "audit session") {
		t.Logf("macOS: autentikasi lewat tunnel terbukti; sesi ditolak BSM audit (sshd non-root), kode ssh %d", code)
	} else {
		t.Fatalf("perintah tidak jalan (kode %d)\nssh stderr:\n%s\nlog sshd:\n%s", code, sshErr.String(), log)
	}
	known, _ := os.ReadFile(d.KnownHostsPath())
	if !strings.HasPrefix(string(known), boxID+" ") {
		t.Fatalf("known_hosts harus memakai HostKeyAlias: %q", known)
	}

	// 2. Jalur ~/.ssh/config: sertifikat dihapus, jadi `Match exec
	//    ssh-prepare` (proses anak) yang harus memintanya sendiri.
	if err := os.Remove(d.CertPath(boxID)); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if c := a.Run(context.Background(), []string{"ssh-config", "contoh-box"}); c != 0 {
		t.Fatalf("ssh-config: %d %s", c, stderr.String())
	}
	sshCfg := filepath.Join(work, "ssh_config")
	_ = os.WriteFile(sshCfg, stdout.Bytes(), 0o600)
	certsBefore := p.n("POST /instances/certificate")
	accepted := strings.Count(sshdLog.String(), "Accepted publickey")
	sshOut.Reset()
	sshErr.Reset()
	code, err = a.ExecSSH(sshPath, []string{"ssh", "-F", sshCfg, "-l", me.Username, "-o", "BatchMode=yes", "contoh-box", "echo", "E2E-CONFIG"}, childEnv)
	if err != nil {
		t.Fatal(err)
	}
	if p.n("POST /instances/certificate") != certsBefore+1 {
		t.Fatalf("Match exec ssh-prepare tidak memperbarui sertifikat\n%s", sshErr.String())
	}
	if strings.Count(sshdLog.String(), "Accepted publickey") != accepted+1 {
		t.Fatalf("jalur ssh-config tidak terautentikasi (kode %d)\n%s\n%s", code, sshErr.String(), sshdLog.String())
	}
	if fullSession && !strings.Contains(sshOut.String(), "E2E-CONFIG") {
		t.Fatalf("jalur ssh-config: perintah tidak jalan (kode %d)\n%s", code, sshErr.String())
	}

	// 3. Bila sesi penuh bisa (Linux): 4 MiB byte acak bolak-balik lewat ssh
	//    sungguhan (`cat`) harus kembali persis.
	if !fullSession {
		return
	}
	payload := make([]byte, 4<<20)
	_, _ = rand.Read(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath, "-F", sshCfg, "-l", me.Username, "-o", "BatchMode=yes", "contoh-box", "cat")
	var back, errb bytes.Buffer
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = childEnv, bytes.NewReader(payload), &back, &errb
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("cat lewat ssh: %v\n%s", err, errb.String())
	}
	if !bytes.Equal(back.Bytes(), payload) {
		t.Fatalf("4 MiB lewat ssh+tunnel tidak kembali persis: %d byte", back.Len())
	}
	t.Logf("4 MiB bolak-balik lewat ssh+tunnel persis, %s", time.Since(start).Round(time.Millisecond))
}

// TestGalatTunnelTampilSebelumPesanSSH — ssh sungguhan dengan ProxyCommand
// `easyrun tunnel` (biner uji ini); WebSocket mengirim frame error seperti
// orchestrator bila image belum mendukung SSH (kontrak §7b P11). Di stderr
// ssh, kalimat easyrun harus muncul SEBELUM "Connection closed …" milik ssh,
// dan ssh keluar ≠ 0.
func TestGalatTunnelTampilSebelumPesanSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("butuh OpenSSH unix")
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh tidak terpasang")
	}
	const msg = "image aplikasi ini belum mendukung SSH; perbarui image di katalog"
	p := newPanel(t)
	p.ws = func(c *websocket.Conn, r *http.Request) {
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"ready"}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","message":"`+msg+`"}`))
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"exit","code":0}`))
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}
	cfgDir := filepath.Join(t.TempDir(), ".easyrun")
	d := config.Dir{Path: cfgDir}
	if err := d.SaveCredentials(config.Credentials{APIURL: p.srv.URL, AccessToken: testToken, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := sshconf.ProxyCommand(sshconf.Invocation{Exe: self, GOOS: runtime.GOOS}, boxID)
	if err != nil {
		t.Fatal(err)
	}
	env := []string{envMain + "=1", config.EnvConfigDir + "=" + cfgDir, config.EnvAPIURL + "=" + p.srv.URL}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") && !strings.HasPrefix(kv, "EASYRUN_") {
			env = append(env, kv)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ProxyCommand="+proxy, "box-uji")
	var stderr bytes.Buffer
	cmd.Env, cmd.Stdin, cmd.Stderr = env, strings.NewReader(""), &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("ssh harus gagal (≠ 0), dapat %v\n%s", err, stderr.String())
	}
	out := stderr.String()
	ours := strings.Index(out, "easyrun: "+msg+"\n")
	if ours < 0 {
		t.Fatalf("pesan easyrun tidak ada di stderr ssh:\n%s", out)
	}
	theirs := -1
	for _, m := range []string{"Connection closed", "kex_exchange_identification", "ssh_exchange_identification", "Connection reset"} {
		if i := strings.Index(out, m); i >= 0 && (theirs < 0 || i < theirs) {
			theirs = i
		}
	}
	if theirs >= 0 && theirs < ours {
		t.Fatalf("pesan ssh mendahului pesan easyrun:\n%s", out)
	}
	if strings.Contains(out, "sambungan ditutup server") || strings.Contains(out, "sambungan terputus") {
		t.Fatalf("pesan tambahan yang menyesatkan:\n%s", out)
	}
	t.Logf("stderr ssh:\n%s", out)
}
