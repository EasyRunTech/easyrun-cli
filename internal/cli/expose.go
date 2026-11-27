package cli

// `easyrun expose` dan `easyrun tunnels` — tunnel publik (kontrak F2 tunnel,
// rute token CLI). Aplikasi lokal dibuka ke internet lewat gerbang EasyRun:
//
//	expose ─▶ POST /cli/tunnels ─▶ POST /cli/tunnels/:id/ticket (tiap sambung)
//	   │                                   │ tiket + agent_url
//	   ▼                                   ▼
//	agent.Run ── WSS (Bearer tiket) ──▶ gerbang ──▶ pengunjung https://<host>
//	   │ stream yamux ⇄ TCP <host>:<port> aplikasi lokal
//	   ▼
//	Ctrl-C ─▶ tunnel sementara dihapus (DELETE /cli/tunnels/:id, upaya terbaik)

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	agent "github.com/EasyRunTech/easyrun-tunnel-agent"
	"github.com/EasyRunTech/easyrun-tunnel-agent/protocol"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/tunnel"
)

const (
	// defaultExposeHost — alamat aplikasi lokal bila --host tidak diisi.
	// Sengaja nama "localhost", bukan "127.0.0.1": dial Go me-resolve-nya
	// ke ::1 dan 127.0.0.1 dan langsung mencoba alamat berikutnya bila yang
	// pertama ditolak. Dev server yang hanya listen di ::1 (Vite/Node >= 17
	// di macOS: "localhost" = ::1) tetap terjangkau, begitu juga aplikasi
	// yang hanya listen di 127.0.0.1. Nilai yang sama dipakai dial agen,
	// probe warnLocalDown, dan --rewrite-host (Host = localhost:PORT, yang
	// diterima dev server).
	defaultExposeHost = "localhost"
	// exposeCleanupTimeout — batas menghapus tunnel sementara setelah Ctrl-C;
	// upaya terbaik, pengguna tidak boleh menunggu lama.
	exposeCleanupTimeout = 5 * time.Second
	// exposeCreateTimeout — batas POST /cli/tunnels. Tanpa --name permintaan
	// ini sengaja tidak ikut dibatalkan Ctrl-C/SIGTERM (supaya id tunnel
	// sementara yang sudah disimpan panel tetap diketahui lalu dihapus), jadi
	// batasnya pendek.
	exposeCreateTimeout = 10 * time.Second
	// exposeHintTimeout — batas mengambil daftar tunnel untuk petunjuk
	// tambahan setelah penolakan (upaya terbaik).
	exposeHintTimeout = 5 * time.Second
	// localProbeTimeout — batas memeriksa apakah aplikasi lokal sudah jalan
	// (hanya untuk peringatan; tunnel tetap dibuka).
	localProbeTimeout = time.Second
	// maxTicketRetryAfter — Retry-After dari panel di atas ini dipangkas,
	// supaya nilai rusak tidak membuat agen diam berjam-jam.
	maxTicketRetryAfter = time.Minute
	// fallbackPublicScheme — skema alamat publik bila gerbang maupun panel
	// tidak menyebutnya.
	fallbackPublicScheme = "https"
)

const exposeMore = `  --name NAMA       tunnel bernama (tetap ada setelah berhenti, dipakai ulang lain kali);
                    tanpa --name: nama acak, tunnel dihapus saat berhenti
  --host HOST       alamat aplikasi lokal (bawaan ` + defaultExposeHost + `: ::1 dan 127.0.0.1 dicoba,
                    jadi dev server yang hanya listen di ::1 ikut terjangkau)
  --rewrite-host    ganti header Host menjadi HOST:PORT sebelum sampai ke aplikasi
                    (dev server seperti Vite menolak host asing)
  Alamat publik dicetak begitu tunnel siap. Sambungan yang putus disambung ulang
  otomatis. Ctrl-C (atau SIGTERM) untuk berhenti; Ctrl-C kedua keluar paksa
  tanpa menunggu pembersihan.
  Berjalan di latar (belum ada --detach): pakai tmux/screen, mis.
    tmux new -d -s tunnel 'easyrun expose 3000 --name demo'
  atau nohup (tetap jalan setelah terminal ditutup), mis.
    nohup easyrun expose 3000 --name demo > expose.log 2>&1 &
  lalu hentikan dengan kill <pid> (SIGTERM) — tunnel sementara tetap dihapus.
`

const tunnelsMore = `  tanpa argumen   tabel tunnel di tenant-tenant kamu
  rm <nama>       hapus tunnel (nama atau id dari tabel); alamatnya langsung tidak berlaku
`

// exposeArgs — argumen `easyrun expose` yang sudah divalidasi.
type exposeArgs struct {
	port        string
	name        string
	host        string
	rewriteHost bool
}

func (x exposeArgs) localAddr() string { return net.JoinHostPort(x.host, x.port) }

// parseExposeArgs — opsi boleh sebelum atau sesudah <port>.
func (a *App) parseExposeArgs(args []string) (exposeArgs, int, bool) {
	x := exposeArgs{host: defaultExposeHost}
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	fs.StringVar(&x.name, "name", "", "nama tunnel")
	fs.StringVar(&x.host, "host", defaultExposeHost, "alamat aplikasi lokal")
	fs.BoolVar(&x.rewriteHost, "rewrite-host", false, "ganti header Host menjadi HOST:PORT")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return x, 2, false
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 1 {
		return x, a.usageError("expose", "butuh tepat satu nomor port aplikasi lokal"), false
	}
	n, err := strconv.Atoi(pos[0])
	if err != nil || n < 1 || n > 65535 {
		return x, a.usageError("expose", fmt.Sprintf("port %q tidak sah (1–65535)", api.Clean(pos[0]))), false
	}
	x.port = strconv.Itoa(n)
	x.host = strings.TrimSpace(x.host)
	if x.host == "" || strings.ContainsAny(x.host, "/ \t") {
		return x, a.usageError("expose", fmt.Sprintf("--host %q tidak sah", api.Clean(x.host))), false
	}
	if _, _, err := net.SplitHostPort(x.host); err == nil {
		return x, a.usageError("expose", "--host diisi tanpa port; port ditulis sebagai argumen"), false
	}
	x.host = strings.TrimSuffix(strings.TrimPrefix(x.host, "["), "]")
	// Tanpa --rewrite-host HOST hanya dipakai dial lokal (nama /etc/hosts
	// bergaris bawah, IPv6 berzona boleh). Dengan --rewrite-host HOST:PORT
	// juga dikirim ke gerbang sebagai Host pengganti; nilai yang ditolak
	// gerbang (400 bertanda, fatal) ditolak di sini sebelum tunnel dibuat.
	if x.rewriteHost && !protocol.ValidHostRewrite(x.localAddr()) {
		return x, a.usageError("expose", fmt.Sprintf("--host %q tidak bisa dipakai dengan --rewrite-host: hanya huruf, angka, titik, tanda hubung, atau alamat IP tanpa zona (%%)", api.Clean(x.host))), false
	}
	x.name = strings.TrimSpace(x.name)
	return x, 0, true
}

func (a *App) cmdExpose(ctx context.Context, args []string) int {
	x, code, ok := a.parseExposeArgs(args)
	if !ok {
		return code
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	// Sinyal berhenti pertama membatalkan ctx; sesudahnya perilaku bawaan
	// Ctrl-C dikembalikan, jadi Ctrl-C kedua langsung menghentikan proses
	// walau pembersihan masih menunggu panel. SIGHUP/SIGTERM berikutnya tetap
	// tertangkap (zsh mengirim dua SIGHUP saat jendela ditutup).
	defer afterStop(ctx, a.stopSignals)()

	// Tanpa --name permintaan buat tidak ikut dibatalkan Ctrl-C/SIGTERM: bila
	// panel sudah menyimpan tunnel sementara, id-nya harus diketahui supaya
	// bisa dihapus — penggunanya diberi tahu sedang menunggu. Dengan --name
	// tunnel tetap ada setelah berhenti, jadi tidak ada yang perlu ditunggu.
	parent, waitNote := ctx, func() {}
	if x.name == "" {
		parent = context.WithoutCancel(ctx)
		waitNote = afterStop(ctx, func() {
			a.stopSignals() // sebelum pesannya: Ctrl-C kedua sudah keluar paksa
			fmt.Fprintf(a.Stderr, "\neasyrun: berhenti — menunggu jawaban panel (maks %d detik) supaya tunnel sementara yang terlanjur dibuat bisa dihapus; Ctrl-C sekali lagi untuk keluar paksa.\n",
				int(exposeCreateTimeout/time.Second))
		})
	}
	cctx, cancel := context.WithTimeout(parent, exposeCreateTimeout)
	tun, created, err := c.CreateTunnel(cctx, x.name)
	cancel()
	waitNote()
	if err != nil {
		if ctx.Err() != nil {
			return a.exposeStopped()
		}
		code := a.failTunnel("membuat tunnel", opTunnelCreate, err)
		a.hintDisabledTunnels(ctx, c, err)
		return code
	}
	// Hanya tunnel sementara yang DIBUAT perintah ini (201) yang dihapus saat
	// berhenti (Ctrl-C, SIGTERM, ditolak, galat), supaya tidak memakan jatah
	// tunnel tenant. 200 = tunnel yang sudah ada — bisa sedang dipakai sesi
	// lain, jadi dibiarkan.
	owned := created && tun.Ephemeral
	if owned {
		defer a.dropEphemeral(c, tun)
	}
	if !created {
		fmt.Fprintf(a.Stderr, "easyrun: memakai tunnel %s yang sudah ada (dibuat %s).\n", dash(tun.Name), createdAt(tun.CreatedAt))
	}
	if ctx.Err() != nil {
		return a.exposeStopped()
	}

	// Tiket pertama diminta di sini karena alamat gerbang (agent_url) ikut
	// di jawabannya. Gagal di awal = berhenti dengan penjelasan; mencoba
	// ulang hanya berlaku setelah tunnel pernah berjalan.
	first, err := c.TunnelTicket(ctx, tun.ID)
	if err != nil {
		if ctx.Err() != nil {
			return a.exposeStopped()
		}
		return a.failTunnel("meminta tiket tunnel", opTunnelTicket, err)
	}
	connectURL, err := tunnel.ValidateWSURL("agent_url", first.AgentURL, c.BaseURL)
	if err != nil {
		return a.fail("tunnel", err)
	}

	a.warnLocalDown(ctx, x)
	cfg := agent.Config{
		ConnectURL:   connectURL.String(),
		Ticket:       a.ticketSource(c, tun.ID, first.Ticket),
		LocalAddr:    x.localAddr(),
		UserAgent:    userAgent(),
		BackoffMin:   a.ExposeBackoffMin,
		BackoffMax:   a.ExposeBackoffMax,
		OnReady:      a.onTunnelReady(tun, x, owned),
		OnDisconnect: a.onTunnelDisconnect,
	}
	if x.rewriteHost {
		cfg.HostRewrite = x.localAddr()
	}
	err = agent.Run(ctx, cfg)
	if ctx.Err() != nil {
		return a.exposeStopped()
	}
	return a.failTunnel("tunnel berhenti", opTunnelRun, err)
}

// afterStop — jalankan fn (di goroutine) begitu ctx selesai. Kembaliannya
// (panggil sekali) membatalkan fn yang belum mulai, atau menunggu fn yang
// sedang berjalan sampai selesai supaya tulisannya tidak bertabrakan dengan
// pesan berikutnya.
func afterStop(ctx context.Context, fn func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		fn()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

// stopSignals — App.StopSignals bila diisi.
func (a *App) stopSignals() {
	if a.StopSignals != nil {
		a.StopSignals()
	}
}

// exposeStopped — berhenti atas permintaan pengguna (Ctrl-C, SIGTERM,
// SIGHUP): bukan galat. Tunnel sementara buatan sendiri dihapus oleh defer
// pemanggil.
func (a *App) exposeStopped() int {
	fmt.Fprintln(a.Stderr, "\neasyrun: tunnel dihentikan.")
	return 0
}

// ticketSource — Config.Ticket: tiket pertama yang sudah diminta, lalu tiket
// baru setiap sambung ulang. Penolakan panel yang tidak sembuh dengan mencoba
// lagi dibungkus *agent.FatalError supaya agen berhenti.
func (a *App) ticketSource(c *api.Client, tunnelID, firstTicket string) func(context.Context) (string, error) {
	pending := firstTicket
	var notBefore time.Time
	return func(ctx context.Context) (string, error) {
		if pending != "" {
			t := pending
			pending = ""
			return t, nil
		}
		// Retry-After dari 429 sebelumnya: sisa tunggunya dijalani di sini
		// (jeda sambung ulang agen sudah berjalan lebih dulu, dan pesan
		// "menyambung ulang dalam …" hanya menyebut jeda itu) — durasi
		// sebenarnya diumumkan dulu supaya tunnel tidak tampak macet.
		if wait := notBefore.Sub(a.Now()); wait > 0 {
			fmt.Fprintf(a.Stderr, "easyrun: panel membatasi permintaan tiket tunnel; menunggu %d detik sebelum menyambung ulang…\n",
				int(math.Ceil(wait.Seconds())))
			if err := a.Sleep(ctx, wait); err != nil {
				return "", err
			}
		}
		t, err := c.TunnelTicket(ctx, tunnelID)
		if err != nil {
			var ae *api.Error
			if errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests && ae.RetryAfter > 0 {
				notBefore = a.Now().Add(min(ae.RetryAfter, maxTicketRetryAfter))
			}
			return "", ticketError(err)
		}
		return t.Ticket, nil
	}
}

// panelFatalError — penolakan panel atas permintaan tiket yang tidak akan
// sembuh dengan mencoba lagi. Membungkus *agent.FatalError (agen berhenti)
// dan galat aslinya (untuk penjelasan ke pengguna).
type panelFatalError struct {
	err   error
	fatal *agent.FatalError
}

func (e *panelFatalError) Error() string   { return e.err.Error() }
func (e *panelFatalError) Unwrap() []error { return []error{e.err, e.fatal} }

// ticketError — kontrak F2: 401/403/404/409 fatal; 429, 5xx, dan jaringan
// dicoba ulang. 4xx lain (selain 408) juga fatal: permintaan yang sama
// tidak akan diterima dengan mencoba lagi. Pengalihan (3xx) fatal karena
// token tidak pernah dikirim ke alamat lain.
func ticketError(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		if ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusRequestTimeout {
			return &panelFatalError{err: err, fatal: &agent.FatalError{Status: ae.Status, Message: ae.Message}}
		}
		return err
	}
	var re *api.RedirectError
	if errors.As(err, &re) {
		return &panelFatalError{err: err, fatal: &agent.FatalError{Status: re.Status, Message: re.Error()}}
	}
	return err
}

// onTunnelReady — alamat publik ke stdout (sekali, atau lagi bila berubah);
// sambung ulang cukup satu baris di stderr.
// dropOnStop = tunnel sementara buatan perintah ini (dihapus saat berhenti).
func (a *App) onTunnelReady(tun *api.Tunnel, x exposeArgs, dropOnStop bool) func(agent.Ready) {
	var shown string
	return func(r agent.Ready) {
		urls := publicURLs(r, tun)
		joined := strings.Join(urls, " ")
		if joined == shown {
			fmt.Fprintln(a.Stderr, "easyrun: tunnel tersambung kembali.")
			return
		}
		reconnect := shown != ""
		shown = joined
		local := "http://" + x.localAddr()
		if reconnect {
			fmt.Fprintln(a.Stderr, "easyrun: tunnel tersambung kembali dengan alamat baru:")
		} else {
			fmt.Fprintf(a.Stdout, "Tunnel %s siap:\n\n", dash(tun.Name))
		}
		for _, u := range urls {
			fmt.Fprintf(a.Stdout, "    %s  →  %s\n", u, local)
		}
		if reconnect {
			return
		}
		fmt.Fprintln(a.Stdout)
		switch {
		case dropOnStop:
			fmt.Fprintln(a.Stderr, "easyrun: tunnel sementara — dihapus saat berhenti (pakai --name untuk alamat tetap). Ctrl-C untuk berhenti.")
		case tun.Ephemeral:
			fmt.Fprintln(a.Stderr, "easyrun: Ctrl-C untuk berhenti; tunnel sementara ini dibuat sesi lain, jadi tidak dihapus di sini.")
		default:
			fmt.Fprintln(a.Stderr, "easyrun: Ctrl-C untuk berhenti; tunnel tetap ada (hapus: easyrun tunnels rm "+dash(tun.Name)+").")
		}
	}
}

// publicURLs — dari jawaban gerbang (host + skema); bila kosong, dari panel.
// Teks server dibersihkan sebelum dicetak.
func publicURLs(r agent.Ready, tun *api.Tunnel) []string {
	scheme := strings.ToLower(api.Clean(r.Scheme))
	if scheme != "http" && scheme != "https" {
		scheme = fallbackPublicScheme
		if u, err := url.Parse(tun.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			scheme = u.Scheme
		}
	}
	var out []string
	for _, h := range r.Hosts {
		if h = api.Clean(h); h != "" && !strings.ContainsAny(h, "/ ") {
			out = append(out, scheme+"://"+h)
		}
	}
	if len(out) == 0 {
		out = append(out, tunnelURL(*tun))
	}
	return out
}

func tunnelURL(t api.Tunnel) string {
	if u := api.Clean(t.URL); u != "" {
		return u
	}
	return fallbackPublicScheme + "://" + dash(t.Host)
}

func (a *App) onTunnelDisconnect(err error, retryIn time.Duration) {
	fmt.Fprintf(a.Stderr, "easyrun: tunnel terputus (%s); menyambung ulang dalam %s…\n",
		api.Clean(explain(err)), retryIn.Round(100*time.Millisecond))
}

// warnLocalDown — aplikasi lokal belum jalan: tunnel tetap dibuka (aplikasi
// bisa dinyalakan sesudahnya), pengunjung melihat halaman galat sampai itu.
func (a *App) warnLocalDown(ctx context.Context, x exposeArgs) {
	d := net.Dialer{Timeout: localProbeTimeout}
	conn, err := d.DialContext(ctx, "tcp", x.localAddr())
	if err != nil {
		fmt.Fprintf(a.Stderr, "easyrun: peringatan: belum ada aplikasi di %s; pengunjung melihat halaman galat sampai aplikasinya jalan.\n", x.localAddr())
		return
	}
	_ = conn.Close()
}

// dropEphemeral — hapus tunnel sementara (upaya terbaik, batas waktu pendek,
// tidak ikut dibatalkan Ctrl-C yang menghentikan tunnel).
func (a *App) dropEphemeral(c *api.Client, tun *api.Tunnel) {
	ctx, cancel := context.WithTimeout(context.Background(), exposeCleanupTimeout)
	defer cancel()
	if err := c.DeleteTunnel(ctx, tun.ID); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return // sudah dihapus (admin, perangkat lain)
		}
		// Dinonaktifkan admin: tidak bisa dihapus siapa pun selama itu, dan
		// tidak melayani pengunjung — dianggap selesai, tanpa saran rm.
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden && ae.Reason == api.ReasonTunnelRevoked {
			fmt.Fprintf(a.Stderr, "easyrun: catatan: tunnel sementara %s dinonaktifkan admin EasyRun, jadi tidak dihapus dari sini; alamatnya tidak melayani pengunjung selama itu.\n", dash(tun.Name))
			return
		}
		fmt.Fprintf(a.Stderr, "easyrun: catatan: tunnel sementara %s tidak bisa dihapus (%s); hapus nanti dengan: easyrun tunnels rm %s\n",
			dash(tun.Name), explainTunnel(opTunnelDelete, err), dash(tun.Name))
		return
	}
	fmt.Fprintf(a.Stderr, "easyrun: tunnel sementara %s dihapus.\n", dash(tun.Name))
}

// hintDisabledTunnels — tunnel_limit_reached: tunnel yang dinonaktifkan
// admin ikut dihitung dalam batas dan tidak bisa dihapus, jadi saran
// `tunnels rm` tidak menolong untuk tunnel itu. Daftar diambil sekali
// (upaya terbaik; gagal = tanpa petunjuk tambahan).
func (a *App) hintDisabledTunnels(ctx context.Context, c *api.Client, err error) {
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Reason != api.ReasonTunnelLimitReached {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, exposeHintTimeout)
	defer cancel()
	ts, lerr := c.Tunnels(lctx)
	if lerr != nil {
		return
	}
	var names []string
	for _, t := range ts {
		if t.Disabled {
			names = append(names, dash(t.Name))
		}
	}
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(a.Stderr, "easyrun: catatan: %d tunnel dinonaktifkan admin EasyRun dan tetap dihitung dalam batas (%s); tunnel itu tidak bisa dihapus — minta admin EasyRun menyalakan atau melepasnya.\n",
		len(names), strings.Join(names, ", "))
}

// tunnelOp — langkah tunnel yang gagal; satu reason panel bisa berarti lain
// per langkah (mis. tunnel_forbidden saat membuat vs menghapus).
type tunnelOp int

const (
	opTunnelCreate tunnelOp = iota // POST /cli/tunnels
	opTunnelTicket                 // tiket pertama
	opTunnelRun                    // agen berhenti (tiket sambung ulang / gerbang)
	opTunnelList                   // GET /cli/tunnels
	opTunnelDelete                 // DELETE /cli/tunnels/:id
)

// failTunnel — seperti fail, dengan penjelasan khas tunnel (explainTunnel).
func (a *App) failTunnel(what string, op tunnelOp, err error) int {
	fmt.Fprintf(a.Stderr, "easyrun: %s: %s\n", what, tunnelHintError{op: op, err: err}.Error())
	return 1
}

// tunnelHintError — galat apa adanya untuk errors.As/Is (lewat Unwrap),
// ditambah penjelasan dan langkah berikutnya pada Error() lewat
// explainTunnel. Dicetak langsung, bukan lewat explain (yang akan memakai
// cabang *api.Error lebih dulu dan kehilangan petunjuknya).
type tunnelHintError struct {
	op  tunnelOp
	err error
}

func (e tunnelHintError) Error() string { return explainTunnel(e.op, e.err) }
func (e tunnelHintError) Unwrap() error { return e.err }

// explainTunnel — kalimat untuk pengguna. Teks server (pesan panel atau
// gerbang) selalu dibersihkan dari karakter kendali sebelum dicetak.
func explainTunnel(op tunnelOp, err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		if s, ok := tunnelReasonText(op, ae); ok {
			return s
		}
		if ae.Status == http.StatusBadRequest && ae.Message != "" {
			return ae.Message
		}
		if ae.Status == http.StatusServiceUnavailable && ae.Message == "" {
			return "tunnel publik belum tersedia di platform"
		}
		return explain(err)
	}
	// Penolakan panel non-API (pengalihan) yang dibungkus jadi fatal.
	var pf *panelFatalError
	if errors.As(err, &pf) {
		return api.Clean(explain(pf.err))
	}
	var fe *agent.FatalError
	if errors.As(err, &fe) {
		return gatewayRejectText(fe)
	}
	return api.Clean(explain(err))
}

// tunnelReasonText — penjelasan per `data.reason` panel (kontrak F2 tunnel).
// Kalimat CLI sendiri, karena pesan panel ditulis untuk satu langkah saja
// (mis. tunnel_forbidden hanya menyebut hapus).
func tunnelReasonText(op tunnelOp, ae *api.Error) (string, bool) {
	switch ae.Reason {
	case api.ReasonTunnelDisabled:
		return "tunnel publik sedang dimatikan di platform — coba lagi nanti", true
	case api.ReasonTunnelNotAllowed:
		return "tunnel publik belum diizinkan untuk tenant ini — minta admin EasyRun menyalakannya", true
	case api.ReasonTunnelNameInvalid:
		rule := ae.Message
		if rule == "" {
			rule = "nama tunnel tidak sah — pakai huruf kecil, angka, dan tanda hubung"
		}
		return rule + " — pilih nama lain lewat --name", true
	case api.ReasonTunnelNameTaken:
		return "nama/alamat tunnel ini sudah dipakai — pilih nama lain lewat --name", true
	case api.ReasonTunnelLimitReached:
		return "batas jumlah tunnel tenant ini tercapai (tunnel sementara yang tertinggal ikut dihitung) — lihat tunnel yang ada: easyrun tunnels; hapus yang tidak dipakai: easyrun tunnels rm <nama>", true
	case api.ReasonTunnelForbidden:
		// Lewat token CLI "owner" hanya berlaku untuk tenant pribadi; owner
		// org diakui lewat panel saja (kontrak F2).
		switch op {
		case opTunnelDelete:
			return "hanya pembuat tunnel (atau owner tenant pribadi) yang boleh menghapusnya dari CLI — owner org bisa menghapusnya lewat panel EasyRun", true
		case opTunnelCreate:
			return "tunnel bernama ini milik anggota lain di tenant ini — pilih nama lain lewat --name (owner org bisa menghapus tunnel itu lewat panel EasyRun)", true
		}
		return "tunnel ini milik anggota lain di tenant ini — hanya pembuatnya yang boleh memakainya (owner org bisa menghapusnya lewat panel EasyRun)", true
	case api.ReasonTunnelRevoked:
		switch op {
		case opTunnelDelete:
			return "tunnel ini sedang dinonaktifkan admin EasyRun, jadi tidak bisa dihapus — hubungi admin EasyRun", true
		case opTunnelRun:
			return "tunnel ini dinonaktifkan admin EasyRun saat berjalan — hubungi admin EasyRun untuk menyalakannya lagi", true
		}
		return "tunnel ini sedang dinonaktifkan admin EasyRun — hubungi admin EasyRun untuk menyalakannya lagi, atau pakai nama lain lewat --name", true
	case api.ReasonTunnelNotFound:
		if op == opTunnelRun || op == opTunnelTicket {
			return "tunnel ini sudah dihapus (lewat easyrun tunnels rm, panel, atau perangkat lain) — jalankan easyrun expose lagi", true
		}
		return "tunnel tidak ditemukan (mungkin sudah dihapus) — lihat: easyrun tunnels", true
	case api.ReasonRateLimited:
		// Rem per IP berlaku untuk semua rute /cli/tunnels*; hanya rem tiket
		// yang khusus tiket.
		s := "terlalu banyak permintaan ke panel"
		if op == opTunnelTicket || op == opTunnelRun {
			s = "terlalu sering meminta tiket tunnel"
		}
		if secs := int(math.Ceil(ae.RetryAfter.Seconds())); secs > 0 {
			return fmt.Sprintf("%s — coba lagi dalam %d detik", s, secs), true
		}
		return s + " — coba lagi sebentar lagi", true
	}
	return "", false
}

// gatewayRejectText — penolakan pasti dari gerbang saat jabat tangan. Isi
// `error` jawaban gerbang selalu kalimat Indonesia untuk pengguna (401: mis.
// "tiket tidak valid atau kedaluwarsa"; 403: kalimat panel yang diteruskan),
// jadi tetap dibersihkan sebelum dicetak.
func gatewayRejectText(fe *agent.FatalError) string {
	msg := api.Clean(fe.Message)
	switch fe.Status {
	case http.StatusUnauthorized:
		// 401 bertanda gerbang selalu soal tiket sambungan (kosong, format
		// salah, tidak dikenal, kedaluwarsa, sudah dipakai), bukan login:
		// agen sudah mencoba ulang dengan tiket baru sampai batasnya
		// (agent.DefaultMaxTicketRejects kali berturut-turut) sebelum
		// berhenti. Token CLI yang dicabut ditolak panel saat meminta tiket
		// (panelFatalError, lewat cabang *api.Error di explainTunnel), dan
		// saran login ada di sana — di sini cukup ulangi perintahnya.
		s := "gerbang tunnel berulang kali menolak tiket sambungan"
		if msg != "" {
			s += ": " + msg
		}
		return s + " — ulangi perintahnya"
	case http.StatusForbidden:
		if msg == "" {
			return "ditolak gerbang tunnel"
		}
		return "ditolak gerbang tunnel: " + msg
	}
	if msg == "" {
		return fmt.Sprintf("gerbang tunnel menolak (HTTP %d)", fe.Status)
	}
	return fmt.Sprintf("gerbang tunnel menolak (HTTP %d): %s", fe.Status, msg)
}

// ---------- easyrun tunnels ----------

func (a *App) cmdTunnels(ctx context.Context, args []string) int {
	switch {
	case len(args) == 0:
		return a.listTunnels(ctx)
	case args[0] == "rm":
		if len(args) != 2 {
			return a.usageError("tunnels", "rm butuh tepat satu nama tunnel")
		}
		return a.removeTunnel(ctx, args[1])
	}
	return a.usageError("tunnels", "argumen tak terduga: "+api.Clean(strings.Join(args, " ")))
}

func (a *App) listTunnels(ctx context.Context) int {
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	ts, err := c.Tunnels(ctx)
	if err != nil {
		return a.failTunnel("mengambil daftar tunnel", opTunnelList, err)
	}
	if len(ts) == 0 {
		fmt.Fprintln(a.Stdout, "Belum ada tunnel. Mulai: easyrun expose <port>")
		return 0
	}
	sort.SliceStable(ts, func(i, j int) bool { return strings.ToLower(ts[i].Name) < strings.ToLower(ts[j].Name) })
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMA\tURL\tSEMENTARA\tSTATUS\tDIBUAT\tID")
	for _, t := range ts {
		eph, status := "tidak", "aktif"
		if t.Ephemeral {
			eph = "ya"
		}
		if t.Disabled {
			status = "dinonaktifkan"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(t.Name), tunnelURL(t), eph, status, createdAt(t.CreatedAt), dash(t.ID))
	}
	return boolCode(tw.Flush() == nil)
}

func createdAt(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format(timeLayout)
	}
	return dash(s)
}

type tunnelNotFoundError struct{ query string }

func (e *tunnelNotFoundError) Error() string {
	return fmt.Sprintf("tunnel %q tidak ditemukan — lihat: easyrun tunnels", e.query)
}

func (a *App) removeTunnel(ctx context.Context, q string) int {
	q = api.Clean(q)
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	ts, err := c.Tunnels(ctx)
	if err != nil {
		return a.failTunnel("mengambil daftar tunnel", opTunnelList, err)
	}
	t, err := matchTunnel(ts, q)
	if err != nil {
		return a.fail("", err)
	}
	if err := c.DeleteTunnel(ctx, t.ID); err != nil {
		return a.failTunnel("menghapus tunnel "+dash(t.Name), opTunnelDelete, err)
	}
	fmt.Fprintf(a.Stdout, "Tunnel %s dihapus; %s tidak berlaku lagi.\n", dash(t.Name), tunnelURL(t))
	return 0
}

// matchTunnel — id persis lebih dulu (id otoritatif: nama boleh berbentuk
// UUID, jadi bisa sama dengan id tunnel lain), baru nama (unik di platform);
// tanpa beda huruf besar/kecil.
func matchTunnel(ts []api.Tunnel, q string) (api.Tunnel, error) {
	if q == "" {
		return api.Tunnel{}, &tunnelNotFoundError{query: q}
	}
	for _, t := range ts {
		if strings.EqualFold(t.ID, q) {
			return t, nil
		}
	}
	var found []api.Tunnel
	for _, t := range ts {
		if strings.EqualFold(t.Name, q) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return api.Tunnel{}, &tunnelNotFoundError{query: q}
	case 1:
		return found[0], nil
	}
	ids := make([]string, len(found))
	for i, t := range found {
		ids[i] = dash(t.ID)
	}
	return api.Tunnel{}, fmt.Errorf("%q cocok dengan %d tunnel — pakai id: %s", q, len(found), strings.Join(ids, ", "))
}
