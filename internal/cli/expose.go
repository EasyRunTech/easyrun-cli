package cli

// `easyrun expose` dan `easyrun tunnels` — tunnel publik (kontrak F2 tunnel,
// rute token CLI). Aplikasi lokal dibuka ke internet lewat gerbang EasyRun:
//
//	expose ─▶ POST /cli/tunnels ─▶ POST /cli/tunnels/:id/ticket (tiap sambung)
//	   │                                   │ tiket + agent_url
//	   ▼                                   ▼
//	agent.Run ── WSS (Bearer tiket) ──▶ gerbang ──▶ pengunjung https://<host>
//	   │ stream yamux ⇄ TCP <host>:<port> aplikasi lokal
//	   │ agent_url tiket berikutnya berubah → agent.Run dimulai lagi ke sana
//	   ▼
//	Ctrl-C ─▶ tunnel sementara dihapus (DELETE /cli/tunnels/:id, upaya terbaik)
//
// Kebijakan sambung ada di exposeSession: sebelum tunnel pertama kali siap
// gagal sesaat dicoba ulang selama anggaran waktu exposeStartBudget,
// sesudahnya sambung ulang tanpa batas.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	// exposeCreateTimeout — batas menunggu jawaban POST /cli/tunnels TANPA
	// --name SETELAH Ctrl-C/SIGTERM. Permintaan itu sengaja tidak ikut
	// dibatalkan sinyal (supaya id tunnel sementara yang sudah disimpan panel
	// tetap diketahui lalu dihapus); sebelum ada sinyal yang berlaku hanya
	// batas klien biasa (api.RequestTimeout), karena Create panel bisa sah
	// lebih lama dari ini (tenant terkena batas: cek keanggotaan + daftar
	// sesi gerbang + tulis activity). Dengan --name juga batas klien biasa,
	// dan Ctrl-C membatalkannya.
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
	// exposeStartBudget — anggaran WAKTU menyambung sebelum tunnel pertama
	// kali siap, dihitung sejak permintaan tiket pertama. Selama itu gagal
	// sesaat (tiket 429/5xx/jaringan, gerbang tak terjangkau atau sedang
	// restart/deploy) dicoba ulang dengan jeda backoff/Retry-After yang
	// dipangkas ke sisa anggaran; habis = berhenti dengan alamat dan
	// sebabnya. Cukup untuk gerbang satu replika yang sedang restart.
	// Penolakan tiket 401 bertanda gerbang tetap berhenti setelah
	// agent.DefaultMaxTicketRejects+1 kali berturut-turut ("ulangi
	// perintahnya"), sama dengan agen; bila anggaran habis lebih dulu, pesan
	// akhirnya memakai penjelasan yang sama.
	exposeStartBudget = 60 * time.Second
	// agentTicketRejectedPrefix — awal Error() penolakan tiket 401 bertanda
	// gerbang di modul agen v0.1.0 ("gerbang menolak tiket (<status>):
	// <pesan>"). Tipenya (ticketRejectedError) tidak diekspor, jadi tidak bisa
	// diperiksa dengan errors.As; TestExposeGerbangTiketTidakValid dan
	// TestExposeAnggaranHabisSaatGerbangTolakTiket menjaga kecocokannya
	// dengan modul sungguhan.
	agentTicketRejectedPrefix = "gerbang menolak tiket ("
)

// exposeMore — var, bukan const: anggaran waktu diambil dari
// exposeStartBudget.
var exposeMore = `  --name NAMA       tunnel bernama (tetap ada setelah berhenti, dipakai ulang lain kali);
                    tanpa --name: nama acak, tunnel dihapus saat berhenti
  --host HOST       alamat aplikasi lokal (bawaan ` + defaultExposeHost + `: ::1 dan 127.0.0.1 dicoba,
                    jadi dev server yang hanya listen di ::1 ikut terjangkau)
  --rewrite-host    ganti header Host menjadi HOST:PORT sebelum sampai ke aplikasi
                    (dev server seperti Vite menolak host asing)
  Alamat publik dicetak begitu tunnel siap. Sebelum siap, gagal menyambung dicoba
  ulang selama maks. ` + strconv.Itoa(int(exposeStartBudget/time.Second)) + ` detik; setelah pernah siap, sambungan yang putus
  disambung ulang otomatis tanpa batas. Ctrl-C (atau SIGTERM) untuk berhenti;
  Ctrl-C kedua keluar paksa tanpa menunggu pembersihan.
  Berjalan di latar (belum ada --detach): pakai tmux/screen, mis.
    tmux new -d -s tunnel 'easyrun expose 3000 --name demo'
  atau nohup (tetap jalan setelah terminal ditutup), mis.
    nohup easyrun expose 3000 --name demo > expose.log 2>&1 &
  lalu hentikan dengan kill <pid> (SIGTERM) — tunnel sementara tetap dihapus.
`

const tunnelsMore = `  tanpa argumen   tabel tunnel tenant token CLI ini (tenant yang aktif saat login disetujui)
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
	// Pesan paket flag (usage bahasa Inggris) tidak dicetak: -h/--help di
	// mana pun = bantuan yang sama dengan `easyrun expose --help`, opsi
	// salah = galat pemakaian biasa.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	fs.StringVar(&x.name, "name", "", "nama tunnel")
	fs.StringVar(&x.host, "host", defaultExposeHost, "alamat aplikasi lokal")
	fs.BoolVar(&x.rewriteHost, "rewrite-host", false, "ganti header Host menjadi HOST:PORT")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return x, a.commandHelpFor("expose"), false
			}
			return x, a.usageError("expose", api.Clean(err.Error())), false
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
	// --name yang diberikan tetapi kosong (mis. variabel shell kosong) bukan
	// "tanpa --name": itu akan diam-diam membuat tunnel acak yang dihapus saat
	// berhenti.
	named := false
	fs.Visit(func(f *flag.Flag) { named = named || f.Name == "name" })
	x.name = strings.TrimSpace(x.name)
	if named && x.name == "" {
		return x, a.usageError("expose", "--name tidak boleh kosong (tanpa --name: nama acak, tunnel sementara)"), false
	}
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

	tun, created, code, ok := a.createExposeTunnel(ctx, c, x)
	if !ok {
		return code
	}
	// Hanya tunnel sementara yang DIBUAT perintah ini (201 + ephemeral) yang
	// dihapus saat berhenti (Ctrl-C, SIGTERM, ditolak, galat), supaya tidak
	// memakan jatah tunnel tenant. 200 = tunnel yang sudah ada, bukan milik
	// perintah ini: tidak pernah dihapus apa pun isi field lainnya (panel
	// sendiri tidak pernah menjawab 200 untuk tunnel sementara — namanya
	// 409 tunnel_name_taken). Syarat ephemeral menjaga tunnel bernama yang
	// baru dibuat (201) tetap ada.
	owned := created && tun.Ephemeral
	if owned {
		defer a.dropEphemeral(c, tun)
	}
	if !created {
		fmt.Fprintf(a.Stderr, "easyrun: memakai tunnel %s yang sudah ada (dibuat %s).\n", dash(tun.Name), createdAt(tun.CreatedAt))
		a.warnTunnelInUse(tun)
	}
	if ctx.Err() != nil {
		return a.exposeStopped()
	}
	s := &exposeSession{a: a, c: c, tun: tun, x: x}
	if code, ok := s.firstTicket(ctx); !ok {
		return code
	}
	a.warnLocalDown(ctx, x)
	return s.run(ctx, owned)
}

// createExposeTunnel — POST /cli/tunnels. ok=false → keluar dengan code.
//
// Dengan --name: batas klien biasa (api.RequestTimeout), dan Ctrl-C
// membatalkannya — tunnel bernama tetap ada setelah berhenti, jadi tidak ada
// yang perlu ditunggu.
//
// Tanpa --name permintaan TIDAK ikut dibatalkan Ctrl-C/SIGTERM: bila panel
// sudah menyimpan tunnel sementara, id-nya harus diketahui supaya bisa
// dihapus. Selama pengguna tidak berhenti, yang berlaku hanya batas klien
// biasa (Create panel bisa sah lama). Setelah sinyal berhenti, jawaban
// ditunggu paling lama exposeCreateTimeout lagi (penggunanya diberi tahu).
// Batas mana pun yang habis = tunnel sementara mungkin sudah terbuat tanpa
// id yang diketahui, jadi pengguna diberi tahu cara memeriksa dan
// menghapusnya (tidak ada pembersihan otomatis); exit 1.
func (a *App) createExposeTunnel(ctx context.Context, c *api.Client, x exposeArgs) (tun *api.Tunnel, created bool, code int, ok bool) {
	failed := func(err error) int {
		if ctx.Err() != nil {
			return a.exposeStopped()
		}
		code := a.failTunnel("membuat tunnel", opTunnelCreate, err)
		a.hintDisabledTunnels(ctx, c, err)
		return code
	}
	if x.name != "" {
		tun, created, err := c.CreateTunnel(ctx, x.name)
		if err != nil {
			return nil, false, failed(err), false
		}
		return tun, created, 0, true
	}
	limit := a.exposeCreateTimeout()
	cctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	var giveUp *time.Timer
	waitNote := afterStop(ctx, func() {
		a.stopSignals() // sebelum pesannya: Ctrl-C kedua sudah keluar paksa
		fmt.Fprintf(a.Stderr, "\neasyrun: berhenti — menunggu jawaban panel (maks %d detik) supaya tunnel sementara yang terlanjur dibuat bisa dihapus; Ctrl-C sekali lagi untuk keluar paksa.\n", ceilSeconds(limit))
		giveUp = time.AfterFunc(limit, func() { cancel(errCreateStopWait) })
	})
	tun, created, err := c.CreateTunnel(cctx, "")
	waitNote() // sesudahnya giveUp tidak ditulis lagi
	if giveUp != nil {
		giveUp.Stop()
	}
	if err != nil {
		switch {
		case errors.Is(context.Cause(cctx), errCreateStopWait):
			return nil, false, a.createNoAnswer(limit), false
		case errors.Is(err, api.ErrTimeout):
			return nil, false, a.createNoAnswer(a.httpTimeout()), false
		}
		return nil, false, failed(err), false
	}
	return tun, created, 0, true
}

// errCreateStopWait — sebab dibatalkannya POST /cli/tunnels tanpa --name:
// batas tunggu exposeCreateTimeout setelah Ctrl-C/SIGTERM habis.
var errCreateStopWait = errors.New("panel tidak menjawab setelah berhenti")

// createNoAnswer — POST /cli/tunnels tanpa --name tidak dijawab dalam
// batasnya: tunnel sementara mungkin sudah terbuat tanpa id yang diketahui.
func (a *App) createNoAnswer(limit time.Duration) int {
	fmt.Fprintf(a.Stderr, "easyrun: membuat tunnel: panel tidak menjawab dalam %d detik; tunnel sementara mungkin sudah terbuat — cek dengan: easyrun tunnels, lalu hapus bila ada: easyrun tunnels rm <nama>\n", ceilSeconds(limit))
	return 1
}

// httpTimeout — batas klien HTTP yang dipakai (api.RequestTimeout bawaan).
func (a *App) httpTimeout() time.Duration {
	if a.HTTP != nil && a.HTTP.Timeout > 0 {
		return a.HTTP.Timeout
	}
	return api.RequestTimeout
}

func ceilSeconds(d time.Duration) int { return int(math.Ceil(d.Seconds())) }

// warnTunnelInUse — panel menandai tunnel bernama yang dipakai ulang masih
// aktif (field aditif `active`: sesi terakhirnya belum dicap putus dan tanda
// hidupnya baru). Gerbang mengganti sesi lama dengan yang baru, jadi dua
// `expose --name` yang sama di dua tempat saling memutus tanpa henti. Hanya
// peringatan: tetap menyambung. Panel lama tanpa field ini = tanpa
// peringatan.
func (a *App) warnTunnelInUse(tun *api.Tunnel) {
	if tun.Active == nil || !*tun.Active {
		return
	}
	when := ""
	if t, ok := localTime(tun.LastSeenAt); ok {
		when = " (terakhir aktif " + t + ")"
	}
	fmt.Fprintf(a.Stderr, "easyrun: peringatan: tunnel %s tampaknya sedang dipakai sesi lain%s. Kalau sesi itu masih jalan, keduanya akan saling memutus — hentikan salah satunya.\n",
		dash(tun.Name), when)
}

// exposeCreateTimeout — App.ExposeCreateTimeout bila diisi (uji).
func (a *App) exposeCreateTimeout() time.Duration {
	if a.ExposeCreateTimeout > 0 {
		return a.ExposeCreateTimeout
	}
	return exposeCreateTimeout
}

// exposeBackoff — jeda awal dan maksimum percobaan sebelum tunnel siap
// (startRetry): sama dengan bawaan agen (Config.fill), supaya jeda sebelum
// dan sesudah siap serupa.
func (a *App) exposeBackoff() (lo, hi time.Duration) {
	lo, hi = a.ExposeBackoffMin, a.ExposeBackoffMax
	if lo <= 0 {
		lo = agent.DefaultBackoffMin
	}
	if hi < lo {
		hi = max(agent.DefaultBackoffMax, lo)
	}
	return lo, hi
}

// errGatewayMoved — sebab berhentinya agent.Run: agent_url tiket berikutnya
// menunjuk gerbang lain (exposeSession.nextTicket).
var errGatewayMoved = errors.New("panel memindahkan tunnel ke gerbang lain")

// startRetryError — sebab berhentinya agent.Run sebelum tunnel pernah siap:
// percobaan gagal; jeda dan batasnya diatur exposeSession.startRetry, bukan
// agen. err = galat percobaan itu.
type startRetryError struct{ err error }

func (e *startRetryError) Error() string { return e.err.Error() }
func (e *startRetryError) Unwrap() error { return e.err }

// exposeSession — menyambungkan tunnel yang sudah dibuat/diambil ke gerbang.
//
//	firstTicket ─▶ agent.Run(gerbang) ─┬─ OnReady: siap (sekali = selamanya)
//	  ▲  ▲ belum siap: startRetry       ├─ OnDisconnect: belum siap → Run
//	  │  │ (jeda ≤ sisa anggaran)       │  dihentikan, startRetry, Run lagi
//	  └──┴─ exposeStartBudget           └─ Ticket: agent_url berubah → Run
//	                                       dihentikan, dimulai lagi ke
//	                                       gerbang baru dengan tiket itu
//
// Sebelum tunnel pernah siap, gagal sesaat (tiket 429/5xx/jaringan, gerbang
// tak terjangkau atau sedang restart, DNS salah) dicoba ulang oleh CLI
// sendiri selama anggaran WAKTU exposeStartBudget sejak permintaan tiket
// pertama. Jedanya backoff (bawaan agen) atau Retry-After, dipangkas ke sisa
// anggaran, lewat App.Sleep; agent.Run dihentikan di setiap kegagalan lalu
// dimulai lagi, karena jeda internal agen tidak bisa dipangkas. Anggaran
// habis = berhenti dengan alamat dan sebabnya (exit 1; tunnel sementara
// buatan sendiri dihapus oleh defer cmdExpose). Penolakan pasti panel/gerbang
// tetap langsung berhenti. Karena agent.Run dimulai lagi, hitungan
// penolakan tiket 401 milik agen ikut mulai lagi, jadi CLI menghitungnya
// sendiri dengan aturan agen (agent.DefaultMaxTicketRejects). Setelah pernah
// siap, sambung ulang tanpa batas oleh agent.Run.
//
// Modul agen tidak bisa mengganti ConnectURL saat berjalan, jadi gerbang
// baru = agent.Run baru. Pada saat itu sesi lama sudah putus (tiket hanya
// diminta untuk menyambung), jadi tidak ada sambungan yang ikut terpotong.
//
// Semua callback (Ticket, OnReady, OnDisconnect) dipanggil di goroutine
// agent.Run = goroutine run, jadi field-nya tidak perlu kunci.
type exposeSession struct {
	a   *App
	c   *api.Client
	tun *api.Tunnel
	x   exposeArgs

	gateway   *url.URL  // agent_url tervalidasi yang sedang dipakai
	pending   string    // tiket yang sudah diminta, belum dipakai
	notBefore time.Time // Retry-After 429 tiket terakhir
	// ready — tunnel pernah siap (OnReady), termasuk di gerbang sebelumnya.
	ready bool
	// startAt — permintaan tiket pertama (awal exposeStartBudget).
	startAt time.Time
	// backoff, backoffMax — jeda percobaan berikutnya sebelum siap.
	backoff, backoffMax time.Duration
	// rejects — penolakan tiket 401 bertanda gerbang berturut-turut sebelum
	// siap (tidak direset galat lain; aturan agen).
	rejects int
	// ticketFailed — percobaan terakhir gagal di permintaan tiket (panel),
	// bukan di gerbang.
	ticketFailed bool
	stopRun      context.CancelCauseFunc
}

// firstTicket — tiket pertama, beserta alamat gerbang (agent_url).
// Penolakan pasti panel dan agent_url yang tidak sah langsung berhenti; gagal
// sesaat dicoba ulang (startRetry). ok=false → keluar dengan code.
func (s *exposeSession) firstTicket(ctx context.Context) (code int, ok bool) {
	s.startAt = s.a.Now()
	s.backoff, s.backoffMax = s.a.exposeBackoff()
	for {
		t, err := s.c.TunnelTicket(ctx, s.tun.ID)
		if ctx.Err() != nil {
			return s.a.exposeStopped(), false
		}
		if err == nil {
			u, err := tunnel.ValidateWSURL("agent_url", t.AgentURL, s.c.BaseURL)
			if err != nil {
				return s.a.fail("tunnel", err), false
			}
			s.gateway, s.pending = u, t.Ticket
			return 0, true
		}
		var fe *agent.FatalError
		if errors.As(ticketError(err), &fe) {
			return s.a.failTunnel("meminta tiket tunnel", opTunnelTicket, err), false
		}
		s.ticketFailed = true
		if d := ticketRetryAfter(err); d > 0 {
			s.notBefore = s.a.Now().Add(d)
		}
		if code, ok := s.startRetry(ctx, err); !ok {
			return code, false
		}
	}
}

// run — agent.Run ke gerbang saat ini; dimulai lagi setelah percobaan yang
// gagal sebelum siap, atau bila panel memindahkan tunnel ke gerbang lain.
func (s *exposeSession) run(ctx context.Context, owned bool) int {
	onReady := s.a.onTunnelReady(s.tun, s.x, owned)
	for {
		rctx, stop := context.WithCancelCause(ctx)
		s.stopRun = stop
		err := agent.Run(rctx, s.agentConfig(func(r agent.Ready) {
			s.ready = true
			onReady(r)
		}))
		cause := context.Cause(rctx)
		stop(nil)
		if ctx.Err() != nil {
			return s.a.exposeStopped()
		}
		var retry *startRetryError
		switch {
		case errors.Is(cause, errGatewayMoved):
			fmt.Fprintf(s.a.Stderr, "easyrun: panel memindahkan tunnel ke gerbang %s; menyambung ke sana…\n", api.Clean(s.gateway.Host))
			continue
		case errors.As(cause, &retry):
			if code, ok := s.startRetry(ctx, retry.err); !ok {
				return code
			}
			continue
		}
		return s.a.failTunnel("tunnel berhenti", opTunnelRun, err)
	}
}

func (s *exposeSession) agentConfig(onReady func(agent.Ready)) agent.Config {
	cfg := agent.Config{
		ConnectURL:   s.gateway.String(),
		Ticket:       s.nextTicket,
		LocalAddr:    s.x.localAddr(),
		UserAgent:    userAgent(),
		BackoffMin:   s.a.ExposeBackoffMin,
		BackoffMax:   s.a.ExposeBackoffMax,
		OnReady:      onReady,
		OnDisconnect: s.onDisconnect,
	}
	if s.x.rewriteHost {
		cfg.HostRewrite = s.x.localAddr()
	}
	return cfg
}

// nextTicket — Config.Ticket: tiket yang sudah diminta (tiket pertama, atau
// tiket yang dibawa dari gerbang sebelumnya), lalu tiket baru setiap sambung
// ulang. Penolakan panel yang tidak sembuh dengan mencoba lagi dibungkus
// *agent.FatalError supaya agen berhenti. agent_url setiap tiket divalidasi
// seperti tiket pertama; bila menunjuk gerbang lain, agent.Run dihentikan
// (errGatewayMoved) dan run memulainya lagi ke alamat baru dengan tiket ini.
func (s *exposeSession) nextTicket(ctx context.Context) (string, error) {
	s.ticketFailed = false
	if s.pending != "" {
		t := s.pending
		s.pending = ""
		return t, nil
	}
	// Retry-After dari 429 sebelumnya (hanya setelah pernah siap; sebelum
	// siap startRetry yang menunggunya, dipangkas ke sisa anggaran): sisa
	// tunggunya dijalani di sini (jeda agen sudah berjalan lebih dulu, dan
	// pesan "… dalam …" hanya menyebut jeda itu) — durasi sebenarnya
	// diumumkan dulu supaya tunnel tidak tampak macet.
	if wait := s.notBefore.Sub(s.a.Now()); s.ready && wait > 0 {
		fmt.Fprintf(s.a.Stderr, "easyrun: panel membatasi permintaan tiket tunnel; menunggu %d detik sebelum menyambung ulang…\n",
			ceilSeconds(wait))
		if err := s.a.Sleep(ctx, wait); err != nil {
			return "", err
		}
	}
	t, err := s.c.TunnelTicket(ctx, s.tun.ID)
	if err != nil {
		s.ticketFailed = true
		if d := ticketRetryAfter(err); d > 0 {
			s.notBefore = s.a.Now().Add(d)
		}
		return "", ticketError(err)
	}
	u, err := tunnel.ValidateWSURL("agent_url", t.AgentURL, s.c.BaseURL)
	if err != nil {
		// Sama dengan tiket pertama: alamat gerbang yang tidak sah dari panel
		// tidak sembuh dengan mencoba lagi.
		return "", &panelFatalError{err: err, fatal: &agent.FatalError{Message: err.Error()}}
	}
	if u.String() != s.gateway.String() {
		s.gateway, s.pending = u, t.Ticket
		s.stopRun(errGatewayMoved)
		return "", errGatewayMoved
	}
	return t.Ticket, nil
}

// onDisconnect — Config.OnDisconnect. Setelah pernah siap: "terputus,
// menyambung ulang" (jeda agen). Sebelum siap: agent.Run dihentikan supaya
// run/startRetry yang menunggu dan menilai anggaran waktu.
func (s *exposeSession) onDisconnect(err error, retryIn time.Duration) {
	if s.ready {
		s.a.onTunnelDisconnect(err, retryIn)
		return
	}
	s.stopRun(&startRetryError{err: err})
}

// startRetry — satu percobaan gagal sebelum tunnel pernah siap: tunggu lalu
// coba lagi selama anggaran waktu masih ada. ok=false → berhenti dengan
// code (penolakan tiket gerbang berulang, anggaran habis, atau dihentikan
// pengguna).
func (s *exposeSession) startRetry(ctx context.Context, err error) (code int, ok bool) {
	if fe, isReject := gatewayTicketRejection(err); isReject {
		// Aturan agen: penolakan ke-(DefaultMaxTicketRejects+1) berturut-turut
		// fatal, dengan penjelasan yang sama seperti FatalError agen.
		if s.rejects++; s.rejects > agent.DefaultMaxTicketRejects {
			return s.a.failTunnel("tunnel berhenti", opTunnelRun, fe), false
		}
	}
	now := s.a.Now()
	left := s.startAt.Add(exposeStartBudget).Sub(now)
	if left <= 0 {
		return s.startFailed(err), false
	}
	d := min(max(s.backoff, s.notBefore.Sub(now)), left)
	fmt.Fprintf(s.a.Stderr, "easyrun: gagal menyambung tunnel (%s); mencoba lagi dalam %s… (sisa waktu menyambung %d detik)\n",
		s.startCause(err), d.Round(100*time.Millisecond), ceilSeconds(left))
	if s.a.Sleep(ctx, d) != nil {
		return s.a.exposeStopped(), false
	}
	s.backoff = min(s.backoff*2, s.backoffMax)
	return 0, true
}

// startCause — penjelasan percobaan yang gagal sebelum siap.
func (s *exposeSession) startCause(err error) string {
	if s.ticketFailed {
		return explainTunnel(opTunnelTicket, err)
	}
	return api.Clean(explain(err))
}

// startFailed — anggaran waktu sebelum siap habis: sebut alamat yang gagal
// (panel bila tiketnya yang gagal, gerbang bila jabat tangannya) dan sebab
// percobaan terakhir. Penolakan tiket bertanda gerbang dijelaskan sama
// dengan FatalError agen (gatewayRejectText: "… ulangi perintahnya").
// Tunnel sementara buatan sendiri dihapus oleh defer cmdExpose.
func (s *exposeSession) startFailed(err error) int {
	secs := ceilSeconds(exposeStartBudget)
	if s.ticketFailed {
		fmt.Fprintf(s.a.Stderr, "easyrun: gagal menyambung tunnel: panel %s tidak memberi tiket dalam %d detik: %s\n",
			hostOfURL(s.c.BaseURL), secs, explainTunnel(opTunnelTicket, err))
		return 1
	}
	why := explainTunnel(opTunnelRun, err)
	if fe, ok := gatewayTicketRejection(err); ok {
		why = gatewayRejectText(fe)
	}
	fmt.Fprintf(s.a.Stderr, "easyrun: gagal menyambung tunnel ke gerbang %s dalam %d detik: %s\n",
		api.Clean(s.gateway.Host), secs, why)
	return 1
}

// gatewayTicketRejection — galat OnDisconnect berupa penolakan tiket 401
// bertanda gerbang (lihat agentTicketRejectedPrefix), sebagai FatalError
// yang setara dengan yang dikembalikan agent.Run setelah batas
// penolakannya.
func gatewayTicketRejection(err error) (*agent.FatalError, bool) {
	rest, ok := strings.CutPrefix(err.Error(), agentTicketRejectedPrefix)
	if !ok {
		return nil, false
	}
	code, msg, ok := strings.Cut(rest, "): ")
	if !ok {
		return nil, false
	}
	status, cerr := strconv.Atoi(code)
	if cerr != nil {
		return nil, false
	}
	return &agent.FatalError{Status: status, Message: msg}, true
}

// ticketRetryAfter — Retry-After 429 permintaan tiket, dipangkas ke
// maxTicketRetryAfter; 0 bila tidak ada.
func ticketRetryAfter(err error) time.Duration {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests && ae.RetryAfter > 0 {
		return min(ae.RetryAfter, maxTicketRetryAfter)
	}
	return 0
}

// hostOfURL — host[:port] alamat (untuk pesan), dibersihkan.
func hostOfURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return api.Clean(u.Host)
	}
	return api.Clean(raw)
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

// panelFatalError — penolakan panel atas permintaan tiket yang tidak akan
// sembuh dengan mencoba lagi. Membungkus *agent.FatalError (agen berhenti)
// dan galat aslinya (untuk penjelasan ke pengguna).
type panelFatalError struct {
	err   error
	fatal *agent.FatalError
}

func (e *panelFatalError) Error() string   { return e.err.Error() }
func (e *panelFatalError) Unwrap() []error { return []error{e.err, e.fatal} }

// ticketError — penolakan panel atas permintaan tiket. Fatal (sebelum
// maupun sesudah tunnel siap): 401 (token; termasuk user_banned = akun
// diblokir), 403 (tunnel_forbidden, tunnel_not_allowed, tunnel_revoked,
// membership_lost), 404 (tunnel_not_found), dan 503 ber-reason
// tunnel_disabled (saklar tunnel platform mati = berhenti, bukan menunggu
// saklar dinyalakan lagi). Dicoba ulang: 429, 5xx lain (termasuk 503 tanpa
// reason: keanggotaan belum bisa dipastikan, panel bermasalah), dan
// jaringan. 4xx lain (selain 408) juga fatal walau panel tidak mengirimnya
// untuk tiket (409 hanya ada di rancangan awal): permintaan yang sama tidak
// akan diterima dengan mencoba lagi. Pengalihan (3xx) fatal karena token
// tidak pernah dikirim ke alamat lain.
func ticketError(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) {
		clientErr := ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests && ae.Status != http.StatusRequestTimeout
		switchedOff := ae.Status == http.StatusServiceUnavailable && ae.Reason == api.ReasonTunnelDisabled
		if clientErr || switchedOff {
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
		// Tunnel sementara yang tidak dibuat perintah ini tidak mungkin
		// sampai di sini: panel menjawab nama tunnel sementara dengan 409
		// tunnel_name_taken, dan tanpa --name selalu 201.
		if dropOnStop {
			fmt.Fprintln(a.Stderr, "easyrun: tunnel sementara — dihapus saat berhenti (pakai --name untuk alamat tetap). Ctrl-C untuk berhenti.")
			return
		}
		fmt.Fprintln(a.Stderr, "easyrun: Ctrl-C untuk berhenti; tunnel tetap ada (hapus: easyrun tunnels rm "+dash(tun.Name)+").")
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
		if ctx.Err() != nil {
			return // berhenti (Ctrl-C) saat memeriksa, bukan aplikasi yang belum jalan
		}
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
		// Saklar tunnel platform mati: panel juga menolak DELETE (503
		// tunnel_disabled), jadi tunnel sementara ini tertinggal sampai
		// dinyalakan lagi.
		if errors.As(err, &ae) && ae.Status == http.StatusServiceUnavailable && ae.Reason == api.ReasonTunnelDisabled {
			fmt.Fprintf(a.Stderr, "easyrun: catatan: tunnel sementara %s tidak bisa dihapus selama tunnel publik dimatikan di platform; hapus setelah dinyalakan lagi: easyrun tunnels rm %s\n", dash(tun.Name), dash(tun.Name))
			return
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

// tunnelDisabledText — saklar tunnel publik platform mati (503
// tunnel_disabled dari panel, atau pesan panelTunnelDisabledMessage yang
// diteruskan gerbang): `expose` berhenti, tidak menunggu saklar nyala.
const tunnelDisabledText = "tunnel publik sedang dimatikan di platform — jalankan perintahnya lagi nanti"

// panelTunnelDisabledMessage — pesan panel saat saklar tunnel mati (503 rute
// CLI, dan 403 penukaran tiket yang diteruskan gerbang tanpa reason).
const panelTunnelDisabledMessage = "Tunnel belum tersedia"

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
		return tunnelDisabledText, true
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
		// Gerbang hanya meneruskan pesan panel (tanpa reason): akun diblokir
		// dan saklar mati dijelaskan sama dengan penolakan panel langsung.
		switch msg {
		case "":
			return "ditolak gerbang tunnel"
		case gatewayBannedMessage, panelBannedMessage:
			return userBannedText
		case panelTunnelDisabledMessage:
			return tunnelDisabledText
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
	fmt.Fprintln(tw, "NAMA\tURL\tSEMENTARA\tSTATUS\tTERSAMBUNG\tDIBUAT\tID")
	for _, t := range ts {
		eph, status := "tidak", "aktif"
		if t.Ephemeral {
			eph = "ya"
		}
		if t.Disabled {
			status = "dinonaktifkan"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dash(t.Name), tunnelURL(t), eph, status, tunnelConnected(t), createdAt(t.CreatedAt), dash(t.ID))
	}
	return boolCode(tw.Flush() == nil)
}

// tunnelConnected — kolom TERSAMBUNG dari field aditif `active` panel
// (sesi terakhir belum dicap putus dan tanda hidupnya baru); "-" = panel
// lama yang belum mengirimnya.
func tunnelConnected(t api.Tunnel) string {
	switch {
	case t.Active == nil:
		return "-"
	case *t.Active:
		return "ya"
	}
	return "tidak"
}

func createdAt(s string) string {
	if t, ok := localTime(s); ok {
		return t
	}
	return dash(s)
}

// localTime — waktu RFC3339 dari panel dalam zona waktu laptop.
func localTime(s string) (string, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", false
	}
	return t.Local().Format(timeLayout), true
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
