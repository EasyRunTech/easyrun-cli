//go:build !windows

package cli

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestExposeSIGTERMBerhentiRapi — C8: SIGTERM (kill, systemd, tmux
// kill-session) berhenti persis seperti Ctrl-C lewat SignalContext yang
// dipakai main: keluar 0 dan tunnel sementara buatan sendiri dihapus.
func TestExposeSIGTERMBerhentiRapi(t *testing.T) {
	h := newHarness(t)
	h.loggedIn(t)
	g := newGateway(t, h.p)
	x := &exposeRun{out: &syncBuffer{}, errb: &syncBuffer{}, done: make(chan int, 1)}
	h.a.Stdout, h.a.Stderr = x.out, x.errb
	h.a.ExposeBackoffMin, h.a.ExposeBackoffMax = 5*time.Millisecond, 50*time.Millisecond
	ctx, stop := SignalContext()
	defer stop()
	go func() { x.done <- h.a.Run(ctx, []string{"expose", localApp(t)}) }()
	g.nextSession(t)
	waitFor(t, "alamat publik dicetak", func() bool { return x.out.contains("https://acak-1" + tunnelHostSuffix) })

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := x.wait(t); code != 0 {
		t.Fatalf("SIGTERM harus keluar 0, dapat %d\n%s", code, x.errb)
	}
	if _, deletes, _ := h.p.tunnelState(); len(deletes) != 1 {
		t.Fatalf("tunnel sementara harus dihapus saat SIGTERM: %v\n%s", deletes, x.errb)
	}
	if !x.errb.contains("tunnel dihentikan") || !x.errb.contains("tunnel sementara acak-1 dihapus") {
		t.Fatalf("stderr:\n%s", x.errb)
	}
}

// ---------- proses anak: perilaku sinyal yang sebenarnya ----------

// signalChildEnv — penanda proses anak uji sinyal (biner uji ini sendiri
// dijalankan ulang); nilainya memilih skenario.
const signalChildEnv = "EASYRUN_TEST_SIGNAL_CHILD"

const (
	signalChildCtx    = "ctx"    // SignalContext saja
	signalChildExpose = "expose" // expose dengan POST /cli/tunnels yang macet
)

// startSignalChild — jalankan ulang biner uji ini sebagai proses anak.
// ignoreHUP = SIGHUP diabaikan sebelum exec, persis seperti nohup.
func startSignalChild(t *testing.T, scenario string, ignoreHUP bool) (*exec.Cmd, io.WriteCloser, *bufio.Reader, *syncBuffer) {
	t.Helper()
	trap := ""
	if ignoreHUP {
		trap = `trap "" HUP; `
	}
	cmd := exec.Command("/bin/sh", "-c", trap+`exec "$0" "$@"`, os.Args[0], "-test.run=^TestSignalChildHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), signalChildEnv+"="+scenario)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errb := &syncBuffer{}
	cmd.Stderr = errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, stdin, bufio.NewReader(stdout), errb
}

// readChildLine — baris berikutnya dari stdout anak (batas waktu uji).
func readChildLine(t *testing.T, r *bufio.Reader, errb *syncBuffer) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() {
		s, _ := r.ReadString('\n')
		ch <- strings.TrimSpace(s)
	}()
	select {
	case s := <-ch:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("proses anak tidak menjawab\nstderr:\n%s", errb)
		return ""
	}
}

// TestSignalContextHormatiSIGHUPDiabaikan — K1: `nohup easyrun expose … &`
// harus bertahan saat terminal ditutup (SIGHUP). nohup mengabaikan SIGHUP;
// SignalContext tidak boleh memasang handler yang membatalkannya lagi.
// SIGHUP yang tidak diabaikan (ssh → ProxyCommand) tetap menghentikan, dan
// SIGTERM tetap menghentikan walau SIGHUP diabaikan.
func TestSignalContextHormatiSIGHUPDiabaikan(t *testing.T) {
	cases := []struct {
		name      string
		ignoreHUP bool
		sig       syscall.Signal
		want      string
	}{
		{"nohup+SIGHUP", true, syscall.SIGHUP, "hidup"},
		{"SIGHUP", false, syscall.SIGHUP, "dibatalkan"},
		{"nohup+SIGTERM", true, syscall.SIGTERM, "dibatalkan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.ignoreHUP && signal.Ignored(syscall.SIGHUP) {
				t.Skip("SIGHUP sudah diabaikan di proses ini (nohup/daemon), jadi anak mewarisinya")
			}
			cmd, stdin, out, errb := startSignalChild(t, signalChildCtx, tc.ignoreHUP)
			if got := readChildLine(t, out, errb); got != "siap" {
				t.Fatalf("anak: %q\nstderr:\n%s", got, errb)
			}
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(stdin, "lanjut\n"); err != nil {
				t.Fatal(err)
			}
			if got := readChildLine(t, out, errb); got != tc.want {
				t.Fatalf("setelah %v: %q, mau %q\nstderr:\n%s", tc.sig, got, tc.want, errb)
			}
		})
	}
}

func TestExitSignals(t *testing.T) {
	has := func(sigs []os.Signal, s os.Signal) bool {
		for _, x := range sigs {
			if x == s {
				return true
			}
		}
		return false
	}
	sigs := exitSignals(func(os.Signal) bool { return false })
	for _, s := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !has(sigs, s) {
			t.Fatalf("tanpa %v: %v", s, sigs)
		}
	}
	sigs = exitSignals(func(s os.Signal) bool { return s == syscall.SIGHUP })
	if has(sigs, syscall.SIGHUP) || !has(sigs, os.Interrupt) || !has(sigs, syscall.SIGTERM) {
		t.Fatalf("SIGHUP diabaikan: %v", sigs)
	}
}

// TestExposeCtrlCKeduaKeluarPaksa — K2: Ctrl-C saat POST /cli/tunnels masih
// berjalan mencetak pesan tunggu, dan Ctrl-C kedua langsung menghentikan
// proses (perilaku sinyal bawaan), tidak tertelan sampai batas waktu.
func TestExposeCtrlCKeduaKeluarPaksa(t *testing.T) {
	if signal.Ignored(os.Interrupt) {
		t.Skip("SIGINT diabaikan di proses ini (dijalankan di latar); perilaku bawaan tidak bisa diuji")
	}
	cmd, _, out, errb := startSignalChild(t, signalChildExpose, false)
	if got := readChildLine(t, out, errb); got != "masuk" {
		t.Fatalf("anak: %q\nstderr:\n%s", got, errb)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pesan tunggu dicetak", func() bool { return errb.contains("Ctrl-C sekali lagi untuk keluar paksa") })
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
			t.Fatalf("Ctrl-C kedua harus menghentikan proses lewat SIGINT: %v (%v)\nstderr:\n%s", err, ws, errb)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Ctrl-C kedua tertelan: proses masih menunggu\nstderr:\n%s", errb)
	}
}

// TestExposeSIGHUPKeduaTidakMembunuh — zsh mengirim dua SIGHUP saat jendela
// terminal ditutup. Setelah SIGHUP pertama, hanya Ctrl-C yang kembali ke
// perilaku bawaan; SIGHUP kedua tetap tertangkap supaya tunnel sementara
// yang terlanjur dibuat sempat dihapus.
func TestExposeSIGHUPKeduaTidakMembunuh(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP diabaikan di proses ini (nohup/daemon); perilakunya tidak bisa diuji")
	}
	cmd, _, out, errb := startSignalChild(t, signalChildExpose, false)
	if got := readChildLine(t, out, errb); got != "masuk" {
		t.Fatalf("anak: %q\nstderr:\n%s", got, errb)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pesan tunggu dicetak", func() bool { return errb.contains("menunggu jawaban panel") })
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("SIGHUP kedua tidak boleh menghentikan proses selama pembersihan: %v\nstderr:\n%s", err, errb)
	case <-time.After(time.Second):
	}
}

// TestSignalChildHelper — badan proses anak; dilewati di luar proses anak.
func TestSignalChildHelper(t *testing.T) {
	switch os.Getenv(signalChildEnv) {
	case signalChildCtx:
		ctx, stop := SignalContext()
		defer stop()
		fmt.Println("siap")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		select {
		case <-ctx.Done():
			fmt.Println("dibatalkan")
		case <-time.After(500 * time.Millisecond):
			fmt.Println("hidup")
		}
	case signalChildExpose:
		h := newHarness(t)
		h.loggedIn(t)
		release := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { close(release) })
		h.p.tun.createFail = func(http.ResponseWriter) bool {
			once.Do(func() { fmt.Println("masuk") })
			<-release
			return false
		}
		ctx, stop := SignalContext()
		defer stop()
		h.a.Stderr = os.Stderr
		h.a.StopSignals = RestoreInterrupt
		h.a.Run(ctx, []string{"expose", "3000"})
	default:
		t.Skip("hanya untuk proses anak uji sinyal")
	}
}
