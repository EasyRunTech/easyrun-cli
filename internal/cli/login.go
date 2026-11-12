package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/EasyRunTech/easyrun-cli/internal/api"
	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

const (
	defaultPollInterval = 5 * time.Second
	minPollInterval     = time.Second
	slowDownStep        = 5 * time.Second // RFC 8628 §3.5
	defaultDeviceTTL    = 10 * time.Minute
	maxClientNameLen    = 64
	timeLayout          = "2006-01-02 15:04 MST"
)

var (
	errDeviceExpired = errors.New("kode login kedaluwarsa sebelum disetujui — jalankan easyrun login lagi")
	errDeviceDenied  = errors.New("login ditolak di panel")
)

func (a *App) cmdLogin(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	apiFlag := fs.String("api-url", "", "alamat API panel (bawaan "+config.DefaultAPIURL+")")
	noBrowser := fs.Bool("no-browser", false, "jangan buka browser, cukup cetak tautannya")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return a.usageError("login", "argumen tak terduga: "+strings.Join(fs.Args(), " "))
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	cfg, err := d.LoadConfig()
	if err != nil {
		return a.fail("", err)
	}
	base, err := config.ResolveAPIURL(*apiFlag, a.Getenv, cfg)
	if err != nil {
		return a.fail("", err)
	}
	client := &api.Client{BaseURL: base, UserAgent: userAgent(), HTTP: a.HTTP}

	dc, err := client.StartDevice(ctx, api.DeviceRequest{ClientName: a.clientName(), ClientOS: clientOS(a.GOOS)})
	if err != nil {
		return a.fail("memulai login", err)
	}
	code := api.Clean(dc.UserCode)
	if code == "" {
		return a.fail("memulai login", errors.New("server tidak mengirim kode login"))
	}
	link := activationURL(dc.VerificationURI)
	if link == "" {
		return a.fail("memulai login", fmt.Errorf("server mengirim tautan aktivasi yang tidak aman: %q", api.Clean(dc.VerificationURI)))
	}

	// Kontrak §7b P1: tautan dan kode dicetak TERPISAH, kode tidak pernah
	// ikut di tautan maupun di browser yang dibuka — pengguna mengetiknya
	// sendiri, jadi tautan aktivasi dari orang lain tidak bisa disetujui
	// tanpa sengaja.
	fmt.Fprintf(a.Stdout, "Untuk masuk, buka halaman ini di browser:\n\n    %s\n\n", link)
	fmt.Fprintf(a.Stdout, "lalu ketik kode login berikut:\n\n    %s\n\n", code)
	fmt.Fprintln(a.Stdout, "Ketik kode ini sendiri di halaman tersebut. Jangan setujui kode yang dikirim orang lain.")
	fmt.Fprintln(a.Stdout)
	if *noBrowser || a.Getenv(EnvNoBrowser) != "" || a.OpenBrowser == nil {
		fmt.Fprintln(a.Stdout, "Menunggu persetujuan di panel… (Ctrl+C untuk batal)")
	} else if err := a.OpenBrowser(link); err != nil {
		fmt.Fprintln(a.Stdout, "Browser tidak bisa dibuka otomatis — buka tautan di atas secara manual.")
		fmt.Fprintln(a.Stdout, "Menunggu persetujuan di panel… (Ctrl+C untuk batal)")
	} else {
		fmt.Fprintln(a.Stdout, "Browser dibuka. Menunggu persetujuan di panel… (Ctrl+C untuk batal)")
	}

	tok, err := a.pollToken(ctx, client, dc)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(a.Stderr, "\neasyrun: login dibatalkan.")
			return 130
		}
		return a.fail("login", err)
	}

	now := a.Now()
	old, _ := d.LoadCredentials(true)
	creds := config.Credentials{
		APIURL:      base,
		AccessToken: tok.AccessToken,
		TokenType:   tok.TokenType,
		TokenID:     api.Clean(tok.TokenID),
		ObtainedAt:  now.UTC(),
	}
	if tok.ExpiresIn > 0 {
		creds.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
	if err := d.SaveCredentials(creds); err != nil {
		return a.fail("menyimpan token", err)
	}

	// Token baru sudah tersimpan. Langkah sesudahnya saling lepas: satu yang
	// gagal tidak boleh membatalkan yang lain (config.json tetap menyimpan
	// --api-url, token lama tetap dicabut). Kegagalannya dicetak di akhir dan
	// membuat kode keluar ≠ 0.
	var problems []string

	if *apiFlag != "" {
		cfg.APIURL = base
		if err := d.SaveConfig(cfg); err != nil {
			problems = append(problems, fmt.Sprintf(
				"easyrun: token baru tersimpan, tetapi alamat API tidak bisa disimpan ke %s: %s\n"+
					"Sampai berkas itu bisa ditulis, setel %s=%s untuk perintah berikutnya, atau ulangi login dengan --api-url setelah diperbaiki.\n",
				d.ConfigPath(), explain(err), config.EnvAPIURL, base))
		}
	}

	// Kontrak §7b P10: sertifikat tersimpan diterbitkan untuk token lama
	// (KeyId "user:… token:…") — bisa milik pengguna lain di laptop yang
	// sama. Token baru = sertifikat baru; berkasnya dibuang sekarang supaya
	// `ssh`/`ssh-prepare` berikutnya memintanya dengan token ini.
	if err := d.RemoveCertificates(); err != nil {
		problems = append(problems, fmt.Sprintf(
			"easyrun: token baru tersimpan, tetapi sertifikat SSH lama tidak bisa dihapus: %v\n"+
				"Hapus manual berkas %s supaya tidak terpakai lagi.\n",
			err, filepath.Join(d.SSHDir(), "*-cert.pub")))
	}

	client.Token = tok.AccessToken
	me, meErr := client.Me(ctx)
	if meErr == nil {
		creds.UserID, creds.Email = api.Clean(me.UserID), api.Clean(me.Email)
		if t, err := time.Parse(time.RFC3339, me.ExpiresAt); err == nil {
			creds.ExpiresAt = t.UTC()
		}
		// Berkas ditulis atomik: bila gagal, credentials.json tetap berisi
		// token baru dari penyimpanan pertama, hanya tanpa identitas.
		if err := d.SaveCredentials(creds); err != nil {
			problems = append(problems, fmt.Sprintf(
				"easyrun: token baru tersimpan, tetapi identitas akun tidak bisa ditambahkan ke %s: %s\n",
				d.CredentialsPath(), explain(err)))
		}
	}

	// Token lama di laptop ini sudah tidak dipakai: cabut supaya tidak
	// menumpuk di daftar "Perangkat CLI". Gagal pun tidak masalah.
	if old != nil && old.AccessToken != "" && old.AccessToken != tok.AccessToken {
		if oldBase, err := config.NormalizeAPIURL(old.APIURL); err == nil && oldBase == base {
			oc := &api.Client{BaseURL: base, Token: old.AccessToken, UserAgent: userAgent(), HTTP: a.HTTP}
			_ = oc.Logout(ctx)
		}
	}

	who := creds.Email
	if who == "" {
		who = creds.UserID
	}
	switch {
	case meErr != nil:
		fmt.Fprintf(a.Stdout, "Berhasil masuk. (Identitas belum bisa dibaca: %s)\n", explain(meErr))
	case who == "":
		fmt.Fprintln(a.Stdout, "Berhasil masuk.")
	case creds.ExpiresAt.IsZero():
		fmt.Fprintf(a.Stdout, "Berhasil masuk sebagai %s.\n", who)
	default:
		fmt.Fprintf(a.Stdout, "Berhasil masuk sebagai %s. Token CLI berlaku sampai %s.\n", who, creds.ExpiresAt.Local().Format(timeLayout))
	}
	for _, p := range problems {
		fmt.Fprint(a.Stderr, p)
	}
	return boolCode(len(problems) == 0)
}

// pollToken — tanya /cli/token setiap `interval` sampai disetujui, ditolak,
// kedaluwarsa, atau dibatalkan (Ctrl+C).
func (a *App) pollToken(ctx context.Context, c *api.Client, dc *api.DeviceCode) (*api.Token, error) {
	interval := time.Duration(dc.Interval) * time.Second
	if interval < minPollInterval {
		interval = defaultPollInterval
	}
	ttl := time.Duration(dc.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	deadline := a.Now().Add(ttl)
	warned := false
	for {
		if err := a.Sleep(ctx, interval); err != nil {
			return nil, err
		}
		if !a.Now().Before(deadline) {
			return nil, errDeviceExpired
		}
		tok, err := c.PollToken(ctx, dc.DeviceCode)
		if err == nil {
			return tok, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		var ae *api.Error
		var ne *api.NetError
		switch {
		case errors.As(err, &ae):
			switch ae.Code {
			case api.CodeAuthorizationPending:
				continue
			case api.CodeSlowDown:
				interval += slowDownStep
				continue
			case api.CodeExpiredToken:
				return nil, errDeviceExpired
			case api.CodeAccessDenied:
				return nil, errDeviceDenied
			}
			if ae.Status == 429 {
				if ae.RetryAfter > interval {
					interval = ae.RetryAfter
				}
				continue
			}
			if ae.Status >= 500 {
				if !warned {
					fmt.Fprintf(a.Stderr, "easyrun: %s; mencoba lagi…\n", explain(err))
					warned = true
				}
				continue
			}
			return nil, err
		case errors.As(err, &ne):
			if !warned {
				fmt.Fprintf(a.Stderr, "easyrun: %s; mencoba lagi…\n", ne.Error())
				warned = true
			}
			continue
		}
		return nil, err
	}
}

func (a *App) clientName() string {
	name := ""
	if a.Hostname != nil {
		if h, err := a.Hostname(); err == nil {
			name = strings.TrimSuffix(api.Clean(h), ".local")
		}
	}
	if name == "" {
		name = "easyrun"
	}
	if r := []rune(name); len(r) > maxClientNameLen {
		name = string(r[:maxClientNameLen])
	}
	return name
}

func clientOS(goos string) string {
	names := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}
	n, ok := names[goos]
	if !ok {
		n = goos
	}
	return n + " " + runtime.GOARCH
}

// safeBrowserURL — hanya https (atau http ke loopback untuk uji) yang boleh
// dibuka/dicetak; "" bila tidak aman. Mencegah server membuka file:// atau
// skema lain lewat rundll32/xdg-open.
func safeBrowserURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !config.IsLoopbackHost(u.Hostname()) {
			return ""
		}
	default:
		return ""
	}
	s := u.String()
	if api.Clean(s) != s || strings.ContainsAny(s, " \"'<>`") {
		return ""
	}
	return s
}

// codeParams — parameter query yang bisa membawa kode login. Dibuang dari
// verification_uri walau server (versi lama/salah setel) menyertakannya:
// kode tidak boleh terisi otomatis dari tautan.
var codeParams = []string{"kode", "code", "user_code"}

// activationURL — verification_uri yang aman dibuka, tanpa kode login di
// query; "" bila tidak aman.
func activationURL(raw string) string {
	s := safeBrowserURL(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	q := u.Query()
	changed := false
	for k := range q {
		for _, p := range codeParams {
			if strings.EqualFold(k, p) {
				q.Del(k)
				changed = true
			}
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// openBrowser — tidak pernah menunggu dan tidak pernah membuat login gagal.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		if getenvAny("DISPLAY", "WAYLAND_DISPLAY") == "" {
			return errors.New("tidak ada tampilan grafis")
		}
		cmd = exec.Command("xdg-open", u)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (a *App) cmdLogout(ctx context.Context, args []string) int {
	if len(args) > 0 {
		return a.usageError("logout", "argumen tak terduga: "+strings.Join(args, " "))
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	// Izin terbuka tetap dibaca: justru token seperti itu yang harus dicabut.
	creds, err := d.LoadCredentials(true)
	if errors.Is(err, config.ErrNotLoggedIn) {
		fmt.Fprintln(a.Stdout, "Belum masuk; tidak ada yang perlu dikeluarkan.")
		return 0
	}
	if err != nil {
		return a.fail("", err)
	}
	var revokeErr error
	base, err := config.NormalizeAPIURL(creds.APIURL)
	if err != nil {
		revokeErr = err
	} else {
		c := &api.Client{BaseURL: base, Token: creds.AccessToken, UserAgent: userAgent(), HTTP: a.HTTP}
		revokeErr = c.Logout(ctx)
	}
	if err := d.RemoveCredentials(); err != nil {
		return a.fail("menghapus token lokal", err)
	}
	_ = d.RemoveCertificates()

	var ae *api.Error
	switch {
	case revokeErr == nil:
		fmt.Fprintln(a.Stdout, "Keluar. Token CLI sudah dicabut di server dan dihapus dari laptop ini.")
		return 0
	case errors.As(revokeErr, &ae) && ae.Status == 401:
		fmt.Fprintln(a.Stdout, "Keluar. Token lokal dihapus (di server token itu sudah tidak berlaku).")
		return 0
	default:
		fmt.Fprintf(a.Stderr, "easyrun: token lokal dihapus, tetapi pencabutan di server gagal: %s\n", explain(revokeErr))
		fmt.Fprintln(a.Stderr, "Cabut manual di panel: Pengaturan akun → Perangkat CLI.")
		return 1
	}
}

func (a *App) cmdBoxes(ctx context.Context, args []string) int {
	if len(args) > 0 {
		return a.usageError("boxes", "argumen tak terduga: "+strings.Join(args, " "))
	}
	d, err := a.dir()
	if err != nil {
		return a.fail("", err)
	}
	c, err := a.session(d)
	if err != nil {
		return a.fail("", err)
	}
	boxes, err := c.Boxes(ctx)
	if err != nil {
		return a.fail("mengambil daftar box", err)
	}
	if len(boxes) == 0 {
		fmt.Fprintln(a.Stdout, "Belum ada box yang bisa kamu akses.")
		return 0
	}
	sortBoxes(boxes)
	tw := tabwriter.NewWriter(a.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMA\tHOSTNAME\tTENANT\tSTATUS\tSSH\tID")
	for _, b := range boxes {
		ssh := "tidak"
		if b.SSHAvailable {
			ssh = "ya"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(b.Name), dash(b.Hostname), dash(b.TenantName), dash(b.Status), ssh, dash(b.ID))
	}
	return boolCode(tw.Flush() == nil)
}

func sortBoxes(b []api.Box) {
	sort.SliceStable(b, func(i, j int) bool {
		ti, tj := strings.ToLower(b[i].TenantName), strings.ToLower(b[j].TenantName)
		if ti != tj {
			return ti < tj
		}
		return strings.ToLower(b[i].Name) < strings.ToLower(b[j].Name)
	})
}

func dash(s string) string {
	s = api.Clean(s)
	if s == "" {
		return "-"
	}
	return s
}

func boolCode(ok bool) int {
	if ok {
		return 0
	}
	return 1
}
