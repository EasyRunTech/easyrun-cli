package sshconf

// include.go — `easyrun ssh-config <box> --write` (kontrak §7b P7): blok tiap
// box ditulis ke berkasnya sendiri, ~/.ssh/easyrun/<id>.conf, dan
// ~/.ssh/config hanya mendapat SATU baris Include di paling atas.
//
// Kenapa di atas: ssh memakai nilai PERTAMA yang ditemukan untuk tiap opsi.
// Blok yang ditempel di bawah (`>> ~/.ssh/config`) kalah oleh `Host *`
// pengguna yang lebih dulu menyetel User, ProxyJump, atau ForwardAgent.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/EasyRunTech/easyrun-cli/internal/config"
)

const (
	// IncludeDirName — folder di ~/.ssh untuk berkas blok box.
	IncludeDirName = "easyrun"
	// IncludePattern — pola yang di-Include; "~" dikembangkan ssh sendiri.
	IncludePattern = "~/.ssh/" + IncludeDirName + "/*.conf"
	// IncludeLine — satu-satunya baris yang ditambahkan ke ~/.ssh/config.
	IncludeLine = "Include " + IncludePattern
	// BackupSuffix — cadangan ~/.ssh/config sebelum diubah pertama kali.
	BackupSuffix = ".easyrun.bak"

	dirPerm  fs.FileMode = 0o700
	filePerm fs.FileMode = 0o600
)

var utf8BOM = []byte("\xef\xbb\xbf")

// IncludeResult — apa yang dilakukan EnsureInclude.
type IncludeResult struct {
	Created  bool   // ~/.ssh/config belum ada, dibuat (0600)
	Inserted bool   // baris Include ditambahkan di baris 1
	Backup   string // cadangan yang dibuat KALI INI ("" = tidak dibuat / sudah ada)
	// Stale — nomor baris (1-based, sebelum disisipi) baris Include easyrun
	// lain yang tidak berada di bagian atas; dibiarkan apa adanya.
	Stale []int
	// Shadowing — berkas yang di-Include DI ATAS baris kita dan menyetel
	// opsi yang bentrok untuk semua host (`Host *`, `Match all`, atau di luar
	// blok); karena itu baris kita disisipkan ulang di baris 1.
	Shadowing []string
}

// isOurInclude — baris `Include` (tanpa beda huruf besar/kecil, boleh
// `Include=`, boleh dikutip, boleh bersama pola lain) yang memuat
// IncludePattern.
func isOurInclude(line string) bool {
	kw, rest, ok := directive(line)
	if !ok || kw != "include" {
		return false
	}
	for _, f := range strings.Fields(rest) {
		if strings.Trim(f, `"`) == IncludePattern {
			return true
		}
	}
	return false
}

// directive — kata kunci (huruf kecil) dan sisa baris; ok=false untuk baris
// kosong atau komentar.
func directive(line string) (kw, rest string, ok bool) {
	s := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	end := strings.IndexAny(s, " \t=")
	if end < 0 {
		return strings.ToLower(s), "", true
	}
	rest = strings.TrimLeft(s[end:], " \t")
	rest = strings.TrimPrefix(rest, "=")
	return strings.ToLower(s[:end]), strings.TrimSpace(rest), true
}

// includeAtTop — baris Include kita sudah ada sebelum direktif apa pun
// selain Include lain (komentar dan baris kosong diabaikan). Include lain di
// atasnya dibiarkan: alat seperti OrbStack/Colima menyisipkan baris Include
// mereka di puncak, dan itu tidak boleh membuat baris kita digandakan —
// KECUALI berkas yang mereka muat menyetel opsi bentrok untuk semua host
// (shadowing ≠ nil): ssh memakai nilai pertama, jadi `Host *` di sana menang
// atas blok box. Mengembalikan juga nomor baris kemunculan lain yang tidak
// di atas, dan berkas yang membayangi.
func includeAtTop(lines []string, shadowing func(includeArgs string) []string) (atTop bool, stale []int, shadow []string) {
	top := true // belum bertemu direktif selain Include
	checked := false
	var above []string
	for i, l := range lines {
		kw, rest, ok := directive(l)
		if !ok {
			continue
		}
		if isOurInclude(l) {
			if top && !atTop && !checked {
				checked = true
				if shadowing != nil {
					for _, a := range above {
						shadow = append(shadow, shadowing(a)...)
					}
				}
				if len(shadow) == 0 {
					atTop = true
					continue
				}
			}
			stale = append(stale, i+1)
			continue
		}
		if kw != "include" {
			top = false
		} else if top && !atTop {
			above = append(above, rest)
		}
	}
	return atTop, stale, shadow
}

// conflictingOptions — opsi (huruf kecil) yang, bila disetel untuk semua host
// SEBELUM blok box dibaca, mengalahkan atau merusak blok itu: ssh memakai
// nilai pertama. Opsi yang menumpuk (IdentityFile, CertificateFile,
// LocalForward) atau tidak berbahaya (AddKeysToAgent, UseKeychain) tidak
// termasuk.
var conflictingOptions = map[string]bool{
	"user": true, "hostname": true, "port": true, "proxyjump": true, "proxycommand": true,
	"identitiesonly": true, "hostkeyalias": true, "userknownhostsfile": true,
	"stricthostkeychecking": true, "forwardagent": true, "forwardx11": true,
	"forwardx11trusted": true, "remotecommand": true, "requesttty": true,
}

const (
	// maxIncludeDepth — Include bersarang yang ditelusuri (OpenSSH: 16).
	maxIncludeDepth = 8
	// maxIncludedFile — berkas lebih besar dari ini tidak dibaca.
	maxIncludedFile = 1 << 20
)

// ShadowingFiles — berkas dari argumen satu baris Include (pola glob seperti
// yang dipahami ssh: "~/" = home, path relatif = relatif ke sshDir) yang
// menyetel opsi bentrok untuk semua host. Berkas yang tidak terbaca
// dilewati: ssh sendiri juga mengabaikannya.
func ShadowingFiles(includeArgs, sshDir, home string) []string {
	return shadowingFiles(includeArgs, sshDir, home, 0, map[string]bool{})
}

func shadowingFiles(includeArgs, sshDir, home string, depth int, seen map[string]bool) []string {
	if depth > maxIncludeDepth {
		return nil
	}
	var out []string
	for _, f := range strings.Fields(includeArgs) {
		f = strings.Trim(f, `"`)
		switch {
		case f == "~":
			f = home
		case strings.HasPrefix(f, "~/"):
			f = filepath.Join(home, f[2:])
		case !filepath.IsAbs(f):
			f = filepath.Join(sshDir, f)
		}
		matches, err := filepath.Glob(f)
		if err != nil || len(matches) == 0 {
			matches = []string{f}
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			fi, err := os.Stat(m)
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxIncludedFile {
				continue
			}
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			if setsGlobalConflict(data) {
				out = append(out, m)
			}
			for _, l := range strings.Split(string(data), "\n") {
				if kw, rest, ok := directive(l); ok && kw == "include" {
					out = append(out, shadowingFiles(rest, sshDir, home, depth+1, seen)...)
				}
			}
		}
	}
	return out
}

// setsGlobalConflict — isi berkas config menyetel opsi bentrok di luar blok
// (berlaku untuk semua host), di dalam `Host` dengan pola "*", atau di dalam
// `Match` yang memuat kriteria "all" atau `host *`.
func setsGlobalConflict(data []byte) bool {
	global := true // sebelum Host/Match pertama
	for _, l := range strings.Split(string(data), "\n") {
		kw, rest, ok := directive(l)
		if !ok {
			continue
		}
		switch kw {
		case "host":
			global = false
			for _, pat := range strings.Fields(rest) {
				if strings.Trim(pat, `"`) == "*" {
					global = true
				}
			}
		case "match":
			f := strings.Fields(strings.ToLower(rest))
			global = slices.Contains(f, "all")
			for i := 0; i+1 < len(f); i++ {
				if f[i] == "host" && strings.Trim(f[i+1], `"`) == "*" {
					global = true
				}
			}
		case "include":
		default:
			if global && conflictingOptions[kw] {
				return true
			}
		}
	}
	return false
}

// EnsureInclude — pastikan ~/.ssh/config (configPath) memuat IncludeLine di
// bagian atas. Tidak ada yang diurutkan ulang atau ditulis ulang: satu-
// satunya perubahan adalah menyisipkan baris itu di baris 1. Berkas yang
// belum ada dibuat 0600; izin berkas yang sudah ada dipertahankan; symlink
// (dotfiles) diikuti sehingga berkas tujuannya yang diubah. Sebelum
// perubahan pertama, isi lama disalin SEKALI ke configPath+BackupSuffix.
// Idempoten. home dipakai untuk mengembangkan "~/" pada baris Include lain
// yang diperiksa (ShadowingFiles).
func EnsureInclude(configPath, home string) (IncludeResult, error) {
	var res IncludeResult
	target := configPath
	if fi, err := os.Lstat(configPath); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(configPath)
		if err != nil {
			return res, fmt.Errorf("%s adalah symlink yang tujuannya tidak bisa dibaca: %w", configPath, err)
		}
		target = resolved
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}

	data, err := os.ReadFile(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
			return res, err
		}
		if err := config.WriteFileExclusive(target, []byte(IncludeLine+"\n"), filePerm); err != nil {
			if errors.Is(err, fs.ErrExist) {
				// Dibuat proses lain di sela-sela: ulangi dengan isi barunya.
				return EnsureInclude(configPath, home)
			}
			return res, err
		}
		res.Created, res.Inserted = true, true
		return res, nil
	case err != nil:
		return res, err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return res, err
	}

	body := data
	bom := bytes.HasPrefix(body, utf8BOM)
	if bom {
		body = body[len(utf8BOM):]
	}
	lines := strings.Split(string(body), "\n")
	sshDir := filepath.Dir(configPath)
	atTop, stale, shadow := includeAtTop(lines, func(args string) []string {
		return ShadowingFiles(args, sshDir, home)
	})
	res.Stale, res.Shadowing = stale, shadow
	if atTop {
		return res, nil
	}

	backup := configPath + BackupSuffix
	switch err := config.WriteFileExclusive(backup, data, filePerm); {
	case err == nil:
		res.Backup = backup
	case errors.Is(err, fs.ErrExist):
		// Cadangan dibuat sekali saja: yang pertama = isi asli pengguna.
	default:
		return res, fmt.Errorf("membuat cadangan %s: %w", backup, err)
	}

	nl := "\n"
	if bytes.Contains(body, []byte("\r\n")) {
		nl = "\r\n"
	}
	var out bytes.Buffer
	if bom {
		out.Write(utf8BOM)
	}
	out.WriteString(IncludeLine + nl)
	out.Write(body)
	if err := config.WriteFileAtomic(target, out.Bytes(), fi.Mode().Perm()); err != nil {
		return res, err
	}
	res.Inserted = true
	return res, nil
}

// BoxConfPath — ~/.ssh/easyrun/<id>.conf.
func BoxConfPath(sshDir, id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return filepath.Join(sshDir, IncludeDirName, id+".conf"), nil
}

// WriteBoxConf — tulis blok satu box ke ~/.ssh/easyrun/<id>.conf (folder
// 0700, berkas 0600, atomik). existed = berkas lama ditimpa.
func WriteBoxConf(sshDir, id, block string) (path string, existed bool, err error) {
	path, err = BoxConfPath(sshDir, id)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return "", false, err
	}
	if _, err := os.Stat(path); err == nil {
		existed = true
	}
	return path, existed, config.WriteFileAtomic(path, []byte(block), filePerm)
}

// confMarker — penanda berkas blok yang ditulis easyrun (baris komentar
// pertama WriteBoxConf / ConfigBlock).
const confMarker = "`easyrun ssh-config"

// StaleConfs — hasil RemoveStaleConfs.
type StaleConfs struct {
	// Removed — berkas buatan easyrun untuk alias yang sama (box lain, mis.
	// box lama yang sudah dihapus) yang dihapus.
	Removed []string
	// Foreign — berkas di folder yang sama yang juga memuat `Host <alias>`
	// tetapi tidak dibuat easyrun; dibiarkan, hanya dilaporkan.
	Foreign []string
}

// RemoveStaleConfs — hapus ~/.ssh/easyrun/*.conf SELAIN milik keepID yang
// memuat `Host <alias>`: ssh memuatnya berurutan menurut nama berkas, jadi
// blok box lama dengan alias yang sama bisa menang atas blok yang baru
// ditulis. Hanya berkas berpenanda easyrun yang dihapus.
func RemoveStaleConfs(sshDir, keepID, alias string) (StaleConfs, error) {
	var res StaleConfs
	dir := filepath.Join(sshDir, IncludeDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	keep := keepID + ".conf"
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".conf") || strings.EqualFold(name, keep) {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil || !declaresHost(data, alias) {
			continue
		}
		if !bytes.Contains(data, []byte(confMarker)) {
			res.Foreign = append(res.Foreign, path)
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		res.Removed = append(res.Removed, path)
	}
	return res, errors.Join(errs...)
}

// declaresHost — ada baris `Host` yang salah satu polanya persis alias.
func declaresHost(data []byte, alias string) bool {
	for _, l := range strings.Split(string(data), "\n") {
		kw, rest, ok := directive(l)
		if !ok || kw != "host" {
			continue
		}
		for _, pat := range strings.Fields(rest) {
			if strings.EqualFold(strings.Trim(pat, `"`), alias) {
				return true
			}
		}
	}
	return false
}
