// Package sshconf — validasi nama box dan penyusunan argumen `ssh` serta blok
// ~/.ssh/config (kontrak SSH box §6).
//
// Yang masuk ke perintah shell (ProxyCommand, Match exec) hanya: path program
// ini (dikutip), opsi --config-dir/--api-url dengan nilai yang dikutip dengan
// aturan yang sama, kata tetap "tunnel"/"ssh-prepare", dan id box yang sudah
// lolos validasi ketat. Nama/hostname dari server juga divalidasi sebelum
// dipakai sebagai nilai opsi ssh.
package sshconf

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// User — satu-satunya pengguna yang dilayani sshd di box (sshd non-root
// hanya bisa melayani dirinya sendiri).
const User = "ubuntu"

// ServerAliveInterval — detik (kontrak §6).
const ServerAliveInterval = 30

var (
	// queryRe — <box> yang diketik pengguna: huruf/angka/titik/garis/garis
	// bawah, tidak diawali "-" (tidak bisa disangka opsi), tanpa karakter
	// shell maupun pola ssh (* ? ! ,).
	queryRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	// idRe — id box dari server; ikut masuk ke perintah shell.
	idRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	labelRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	plainRe  = regexp.MustCompile(`^[A-Za-z0-9/._+:@,=-]+$`)
	winPlain = regexp.MustCompile(`^[A-Za-z0-9/\\._+:@,=-]+$`)
)

// ValidQuery — periksa <box> yang diketik pengguna.
func ValidQuery(q string) error {
	if !queryRe.MatchString(q) {
		return fmt.Errorf("nama box %q tidak valid: pakai nama, hostname, atau id dari `easyrun boxes` (huruf, angka, titik, '-', '_')", q)
	}
	return nil
}

// ValidID — id box dari server.
func ValidID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("id box dari server tidak valid: %q", id)
	}
	return nil
}

// IsUUID — <box> berbentuk UUID dianggap id langsung (tanpa daftar box).
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// ValidHostname — nama DNS biasa (dipakai untuk HostName dan HostKeyAlias).
func ValidHostname(h string) error {
	if h == "" || len(h) > 253 {
		return fmt.Errorf("hostname box dari server tidak valid: %q", h)
	}
	for _, l := range strings.Split(h, ".") {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("hostname box dari server tidak valid: %q", h)
		}
	}
	return nil
}

// Params — semua yang dibutuhkan untuk menyusun argumen dan blok config.
type Params struct {
	Alias    string // nama di `Host` (blok config) — <box> yang diketik
	ID       string // id box (principal sertifikat + HostKeyAlias)
	Hostname string // HostName (tujuan ssh)
	User     string // bawaan: User
	Exe      string // path absolut program easyrun
	// ConfigDir — folder konfigurasi easyrun (absolut) yang dibawa ke
	// ProxyCommand / Match exec sebagai --config-dir: ssh, scp, dan VS Code
	// tidak selalu mewarisi env shell (EASYRUN_CONFIG_DIR). "" = tidak dibawa.
	ConfigDir string
	// APIURL — alamat API yang dibawa sebagai --api-url; diisi hanya bila
	// alamat itu datang dari env/opsi (yang dari config.json sudah ikut lewat
	// ConfigDir). "" = tidak dibawa.
	APIURL          string
	IdentityFile    string
	CertificateFile string
	KnownHostsFile  string
	// GOOS — sistem tujuan (memengaruhi cara mengutip path); "" = unix.
	GOOS string
}

func (p Params) validate() error {
	if err := ValidID(p.ID); err != nil {
		return err
	}
	if err := ValidHostname(p.Hostname); err != nil {
		return err
	}
	if p.Alias != "" {
		if err := ValidQuery(p.Alias); err != nil {
			return err
		}
	}
	if p.Exe == "" || p.IdentityFile == "" || p.CertificateFile == "" || p.KnownHostsFile == "" {
		return errors.New("parameter ssh tidak lengkap")
	}
	return nil
}

func (p Params) user() string {
	if p.User != "" {
		return p.User
	}
	return User
}

// errUnquotable — path yang tidak bisa dikutip dengan aman untuk semua shell
// (sh, bash, zsh, fish) dan parser ssh_config sekaligus.
func errUnquotable(path string) error {
	return fmt.Errorf("path %q memuat karakter yang tidak bisa dikutip dengan aman (', \", \\, atau baris baru); pindahkan ke folder lain", path)
}

// escapePercent — ssh mengembangkan token %h, %p, … pada path dan perintah;
// "%" harfiah harus ditulis "%%".
func escapePercent(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// QuoteExe — path program (atau nilai argumen lain, mis. folder konfigurasi
// dan alamat API) untuk dipakai di ProxyCommand / Match exec.
//
// Unix: ssh menjalankan perintah itu lewat $SHELL -c "exec …". Path yang
// aman dibiarkan; selain itu dikutip tunggal — tetapi path berisi ', ", \
// atau baris baru ditolak karena fish menafsirkan \ di dalam kutip tunggal.
// Windows: OpenSSH Win32 menjalankan baris perintah langsung; dikutip ganda.
func QuoteExe(exe, goos string) (string, error) {
	if strings.ContainsAny(exe, "\"\n\r\x00") {
		return "", errUnquotable(exe)
	}
	if goos == "windows" {
		if winPlain.MatchString(exe) {
			return escapePercent(exe), nil
		}
		return `"` + escapePercent(exe) + `"`, nil
	}
	if strings.ContainsAny(exe, `'\`) {
		return "", errUnquotable(exe)
	}
	if plainRe.MatchString(exe) {
		return escapePercent(exe), nil
	}
	return "'" + escapePercent(exe) + "'", nil
}

// quoteConfigPath — nilai path di ssh_config / -o (parser ssh memecah di
// spasi dan memahami kutip ganda). Windows memakai garis miring maju supaya
// "\" tidak dibaca sebagai escape oleh parser ssh.
func quoteConfigPath(path, goos string) (string, error) {
	if goos == "windows" {
		path = strings.ReplaceAll(path, `\`, "/")
	}
	if strings.ContainsAny(path, "\"'\\\n\r\x00") {
		return "", errUnquotable(path)
	}
	path = escapePercent(path)
	if strings.ContainsAny(path, " \t#") {
		return `"` + path + `"`, nil
	}
	return path, nil
}

// Invocation — cara ssh memanggil program ini (ProxyCommand / Match exec).
type Invocation struct {
	Exe       string
	ConfigDir string // "" = tanpa --config-dir
	APIURL    string // "" = tanpa --api-url
	GOOS      string
}

func (v Invocation) command(sub, id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	q, err := QuoteExe(v.Exe, v.GOOS)
	if err != nil {
		return "", err
	}
	parts := []string{q}
	for _, o := range []struct{ flag, val string }{{"--config-dir", v.ConfigDir}, {"--api-url", v.APIURL}} {
		if o.val == "" {
			continue
		}
		qv, err := QuoteExe(o.val, v.GOOS)
		if err != nil {
			return "", err
		}
		parts = append(parts, o.flag, qv)
	}
	return strings.Join(append(parts, sub, id), " "), nil
}

// ProxyCommand — "<exe> [--config-dir D] [--api-url U] tunnel <id>". Tanpa
// awalan exec: OpenSSH sudah menambahkannya sendiri.
func ProxyCommand(v Invocation, id string) (string, error) { return v.command("tunnel", id) }

// PrepareCommand — "<exe> [--config-dir D] [--api-url U] ssh-prepare <id>"
// untuk Match exec.
func PrepareCommand(v Invocation, id string) (string, error) { return v.command("ssh-prepare", id) }

func (p Params) invocation() Invocation {
	return Invocation{Exe: p.Exe, ConfigDir: p.ConfigDir, APIURL: p.APIURL, GOOS: p.GOOS}
}

type option struct{ key, value string }

func (p Params) options() ([]option, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	proxy, err := ProxyCommand(p.invocation(), p.ID)
	if err != nil {
		return nil, err
	}
	id, err := quoteConfigPath(p.IdentityFile, p.GOOS)
	if err != nil {
		return nil, err
	}
	cert, err := quoteConfigPath(p.CertificateFile, p.GOOS)
	if err != nil {
		return nil, err
	}
	kh, err := quoteConfigPath(p.KnownHostsFile, p.GOOS)
	if err != nil {
		return nil, err
	}
	return []option{
		{"User", p.user()},
		{"IdentityFile", id},
		{"CertificateFile", cert},
		{"IdentitiesOnly", "yes"},
		{"ProxyCommand", proxy},
		// Kunci host disimpan per id box, bukan per hostname: hostname bisa
		// berpindah (ganti alamat) atau dipakai ulang box lain setelah box
		// lama dihapus — keduanya tidak boleh memicu "host key changed".
		// Sambungan memang selalu ke id ini (ProxyCommand tunnel <id>).
		{"HostKeyAlias", p.ID},
		{"UserKnownHostsFile", kh},
		{"StrictHostKeyChecking", "accept-new"},
		{"ServerAliveInterval", fmt.Sprint(ServerAliveInterval)},
		// Kontrak §7b P7: agent dan X11 laptop tidak pernah diteruskan ke
		// box, walau `Host *` di ~/.ssh/config pengguna menyalakannya (box
		// juga mematikannya di sshd). ClearAllForwardings sengaja TIDAK
		// dipakai: ia juga menghapus -L, yang dibutuhkan VS Code Remote-SSH.
		{"ForwardAgent", "no"},
		{"ForwardX11", "no"},
	}, nil
}

// Argv — argumen lengkap untuk exec ssh: opsi easyrun DULU (ssh memakai nilai
// pertama yang ditemui, jadi tidak bisa ditimpa tanpa sengaja), lalu tujuan,
// lalu argumen tambahan pengguna. OpenSSH tetap membaca opsi yang muncul
// setelah tujuan (mis. `-L 8080:localhost:80`), dan sisanya menjadi perintah
// jarak jauh. Tidak ada shell di antaranya — setiap argumen dikirim apa adanya.
func Argv(p Params, extra []string) ([]string, error) {
	opts, err := p.options()
	if err != nil {
		return nil, err
	}
	argv := []string{"ssh"}
	for _, o := range opts {
		argv = append(argv, "-o", o.key+"="+o.value)
	}
	argv = append(argv, p.Hostname)
	return append(argv, extra...), nil
}

// ConfigBlock — blok untuk ~/.ssh/config. Blok Match tidak berisi opsi; ia
// ada hanya supaya ssh menjalankan `ssh-prepare` (memperbarui sertifikat bila
// perlu) sebelum membaca CertificateFile.
func ConfigBlock(p Params) (string, error) {
	if p.Alias == "" {
		return "", errors.New("alias Host kosong")
	}
	opts, err := p.options()
	if err != nil {
		return "", err
	}
	prep, err := PrepareCommand(p.invocation(), p.ID)
	if err != nil {
		return "", err
	}
	alias := strings.ToLower(p.Alias)
	var b strings.Builder
	fmt.Fprintf(&b, "# EasyRun box %s (id %s) — dibuat oleh `easyrun ssh-config`.\n", alias, p.ID)
	fmt.Fprintf(&b, "Match host %s exec \"%s\"\n\n", alias, escapeArgvSplit(prep))
	fmt.Fprintf(&b, "Host %s\n", alias)
	fmt.Fprintf(&b, "    HostName %s\n", p.Hostname)
	for _, o := range opts {
		fmt.Fprintf(&b, "    %s %s\n", o.key, o.value)
	}
	return b.String(), nil
}

// escapeArgvSplit — isi argumen berkutip ganda untuk parser ssh_config:
// "\" dan "\"" di-escape supaya perintahnya sampai utuh.
func escapeArgvSplit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
