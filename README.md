# easyrun

Program baris perintah EasyRun.

## Pasang

macOS dan Linux (x86_64, arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/EasyRunTech/easyrun-cli/main/install.sh | bash
```

Setelan opsional:

| Env | Arti | Bawaan |
|---|---|---|
| `EASYRUN_VERSION` | pasang versi tertentu, mis. `v0.1.0` | terbaru |
| `EASYRUN_INSTALL_DIR` | folder tujuan | `/usr/local/bin` (memakai `sudo` bila tidak bisa ditulis) |
| `EASYRUN_DOWNLOAD_BASE` | unduh dari mirror/server uji, bukan GitHub | — |

Contoh tanpa `sudo`:

```bash
curl -fsSL https://raw.githubusercontent.com/EasyRunTech/easyrun-cli/main/install.sh | EASYRUN_INSTALL_DIR="$HOME/.local/bin" bash
```

Windows: unduh `easyrun-windows-x86_64.exe` dari [rilis terbaru](https://github.com/EasyRunTech/easyrun-cli/releases/latest), ganti namanya menjadi `easyrun.exe`, lalu taruh di folder yang ada di PATH.

Pemasang selalu memeriksa checksum SHA-256 dan membatalkan bila tidak cocok atau tidak ada.

## Perbarui

```bash
easyrun --self-update
```

## Mulai

```bash
easyrun login                        # buka halaman aktivasi, ketik kode dari terminal
easyrun boxes                        # daftar box yang bisa kamu akses
easyrun ssh contoh-box.easyrun.io    # masuk ke box
```

`easyrun login` mencetak alamat halaman aktivasi dan kode login secara terpisah. Ketik kode itu sendiri di halaman tersebut; tautannya sengaja tidak memuat kode. Jangan setujui kode yang dikirim orang lain: siapa pun yang memegang kode yang kamu setujui mendapat akses SSH atas namamu.

`<box>` boleh berupa nama, hostname lengkap (`contoh-box.easyrun.io`), label pertama hostname (`contoh-box`), atau id dari `easyrun boxes`, tanpa beda huruf besar/kecil. Bila cocok dengan lebih dari satu box, easyrun mendaftar pilihannya dan berhenti.

Supaya `ssh`, `scp`, `rsync`, `sftp`, dan VS Code Remote-SSH langsung jalan tanpa `easyrun` di depannya:

```bash
easyrun ssh-config contoh-box --write
ssh contoh-box
scp berkas.txt contoh-box:~/
```

`--write` menulis blok box ke `~/.ssh/easyrun/<id-box>.conf` (0600) dan memastikan `~/.ssh/config` memuat satu baris ini di paling atas:

```
Include ~/.ssh/easyrun/*.conf
```

Selain menyisipkan baris itu, isi `~/.ssh/config` tidak diubah atau diurutkan ulang. Isi lamanya disalin sekali ke `~/.ssh/config.easyrun.bak` sebelum perubahan pertama. Berkas yang belum ada dibuat dengan izin 0600, izin berkas yang sudah ada tidak diubah, dan symlink (dotfiles) diikuti. Menjalankan `--write` berulang kali aman: baris Include tidak digandakan.

Barisnya harus di atas karena ssh memakai nilai **pertama** yang ditemukannya untuk tiap opsi. Blok yang ditempel di bawah (`>> ~/.ssh/config`) kalah oleh `Host *` yang lebih dulu menyetel `User`, `ProxyJump`, atau `ForwardAgent`. Tanpa `--write`, `easyrun ssh-config contoh-box` hanya mencetak bloknya. Kalau menyalinnya sendiri, taruh di atas semua blok `Host *` / `Match all`.

## Perintah

| Perintah | Isi |
|---|---|
| `easyrun login [--api-url URL] [--no-browser]` | Login lewat kode perangkat. Tautan halaman aktivasi (tanpa kode) dan kode login dicetak terpisah, lalu kodenya diketik sendiri. Browser dibuka ke halaman itu bila bisa. `--api-url` disimpan ke `config.json`. Sertifikat SSH tersimpan dibuang, karena terikat token lama. |
| `easyrun logout` | Cabut token CLI di server, hapus token dan sertifikat dari laptop. |
| `easyrun boxes` | Tabel box: nama, hostname, tenant, status, SSH tersedia, id. |
| `easyrun ssh <box> [-- argumen ssh…]` | Perbarui sertifikat bila sisa < 5 menit, lalu jalankan `ssh` sistem. Argumen setelah `--` diteruskan apa adanya, mis. `-- -L 8080:localhost:8080`. |
| `easyrun ssh-config <box> [--write]` | Blok `Match host … exec "easyrun --config-dir … ssh-prepare …"` + `Host …`. Folder data (dan alamat API bila diisi lewat env/opsi) ikut ditulis di perintahnya, karena `ssh`, `scp`, dan VS Code tidak selalu mewarisi env shell. Tanpa `--write` blok dicetak. Dengan `--write` blok ditulis ke `~/.ssh/easyrun/<id>.conf`, blok lama untuk nama yang sama (box lain) dihapus, dan satu baris `Include` ditambahkan di puncak `~/.ssh/config`. `~` = home akun yang dibaca OpenSSH. |
| `easyrun ssh-prepare <box>` | Hanya memperbarui sertifikat bila perlu (dipanggil blok `Match exec`). Tanpa jaringan selama sertifikat masih segar. |
| `easyrun tunnel <box>` | ProxyCommand: saluran SSH lewat WebSocket HTTPS panel. Dipanggil `ssh`, bukan untuk diketik. |
| `easyrun --version`, `--help`, `--self-update` | Versi, bantuan, pembaruan. |
| `easyrun --config-dir DIR --api-url URL <perintah> …` | Opsi global (sebelum nama perintah), menggantikan `EASYRUN_CONFIG_DIR` / `EASYRUN_API_URL`. |

| `easyrun claude <box> …` | Claude Code di box, berkas & perintah di laptop ini. Lihat [Claude Code di box](#claude-code-di-box-easyrun-claude). |

Butuh OpenSSH (`ssh`) di PATH: sudah ada di macOS dan kebanyakan Linux; Windows: fitur "OpenSSH Client".

Env:

| Env | Arti | Bawaan |
|---|---|---|
| `EASYRUN_API_URL` | alamat API panel (https; http hanya untuk localhost) | `config.json`, lalu `https://api.easyrun.io` |
| `EASYRUN_CONFIG_DIR` | folder data easyrun | `~/.easyrun` |
| `EASYRUN_NO_BROWSER` | isi apa pun supaya `login` tidak membuka browser | — |

## Claude Code di box (`easyrun claude`)

Claude Code berjalan di EasyBox (pane herdr, login Claude di box), tetapi berkas dan perintahnya dijalankan di **folder laptop tempat perintah diketik**. Tool bawaan claude di box dimatikan; ia hanya punya tool `Bash`/`Read`/`Edit`/`Write` yang diteruskan ke laptop lewat MCP, melewati penjaga di laptop.

```
  EasyBox                                 laptop (folder proyek = root)
  +----------------------------+          +-------------------------------+
  | claude (pane herdr)        |          | easyrun claude … attach|hand  |
  |   | stdio (MCP "laptop")   |          |   PENJAGA: allowlist, jail    |
  |   v                        |  1 sesi  |   root, folder rahasia, audit |
  | easybox-bridge             |   SSH    |   |                           |
  |   ^ soket unix             |          |   v                           |
  | easybox-hand-connect <pane>+<========>+ claude mcp serve (cwd = root) |
  +----------------------------+          +-------------------------------+
```

```bash
cd ~/proyek/toko                         # root = folder ini
easyrun claude contoh-box w1 attach      # buka pane w1 (sesi "default", sama dengan Konsol mode herdr); lepas dari herdr = selesai
easyrun claude contoh-box kerja:w2 hand  # tangan saja; lihat pane lewat Konsol di browser; Ctrl-C = lepas
easyrun claude contoh-box ls             # pane, sesi, jalan?, laptop yang tersambung
easyrun claude contoh-box w1 stop        # hentikan claude di pane itu
```

| Aksi | Yang terjadi |
|---|---|
| `attach [--read-only] [--allow-home]` | Periksa laptop (root sah, `claude` ada dan `claude mcp serve` jalan, ada terminal) → satu sambungan ssh master (ControlMaster) → `easybox version` (protokol 1, claude terpasang & login) → `easybox ensure <sesi> <pane>` → tangan berjalan di latar lewat `easybox-hand-connect <pane> --wait 60`, disambung ulang otomatis (jeda 1–30 dtk) selama UI hidup → UI `ssh -t … easybox attach-ui <sesi> <pane>`. Saat UI keluar: tangan dilepas, master ditutup, ringkasan dicetak (jumlah panggilan, yang ditolak, path log audit). |
| `hand [--read-only] [--allow-home]` | Sama tanpa UI; setiap aksi dicetak di terminal. Bila master ssh putus, sertifikat diperbarui dulu (bawaan berumur 1 jam) lalu master dibuka ulang; ditolak autentikasi 3× berturut-turut → berhenti dengan catatan (`easyrun login`). |
| `ls` | `easybox ls`, satu sambungan ssh biasa. |
| `stop` | `easybox stop <pane>`. |

`<sesi>` dan `<pane>`: diawali huruf/angka, sisanya huruf/angka/`._-`, maks 40 karakter (`-h`, `.`, `..`, `.x` ditolak). Sambung ulang tangan (keduanya): setelah pernah tersambung, `easybox ls` diperiksa dulu — bila pane kini dipegang tangan lain (laptop lain, atau laptop & folder yang sama dari proses easyrun lain; dibedakan lewat host/user/root/mode hello + `connected_at` sambungan ini), tangan berhenti dan tidak merebutnya kembali walau pemberitahuan "digantikan" hilang (laptop tidur, jaringan putus). Bila tangan tidak pernah tersambung, ringkasan akhir menyebut galat terakhirnya.

Penjaga laptop (laptop tidak percaya box):

- Hanya `Bash`, `Read`, `Edit`, `Write` (`--read-only`: hanya `Read`); daftar tool dari `claude mcp serve` disaring ke daftar itu, tool lain ditolak.
- `file_path` wajib di dalam root setelah symlink diikuti (termasuk symlink yang targetnya belum ada); komponen `..` ditolak; berkas baru di folder baru boleh.
- Folder rahasia ditolak walau di dalam root: `~/.ssh ~/.aws ~/.gnupg ~/Library/Keychains ~/.config/gh ~/.kube ~/.docker ~/.netrc ~/.easyrun ~/.claude ~/.claude.json ~/.password-store ~/.config/gcloud ~/.azure ~/.npmrc ~/.pypirc ~/.git-credentials`, folder config easyrun yang dipakai, config tangan, dan `$CLAUDE_CONFIG_DIR` (tanpa beda huruf besar/kecil; symlink ke folder itu juga).
- Root ditolak bila `/`, home, folder yang memuat home (kecuali `--allow-home`), atau di dalam folder rahasia.
- `Bash` dengan `run_in_background` / `dangerouslyDisableSandbox` ditolak. **Perintah Bash sendiri tidak dibatasi ke root** (sama dengan terminal biasa); pakai `--read-only` bila ragu.
- Permintaan dari `claude mcp serve` ke klien dijawab "tidak didukung" di laptop; hanya `notifications/tools/list_changed` yang diteruskan ke box.
- `claude mcp serve` dijalankan dengan cwd = root, `CLAUDE_CONFIG_DIR` terpisah, tanpa `ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN`/`ANTHROPIC_AUTH_TOKEN`. Perintah itu tidak terdokumentasi resmi; bila tool yang diharapkan hilang atau skemanya berubah, easyrun memperingatkan (panggilan yang tidak cocok ditolak, bukan diloloskan).
- `CLAUDE.md`, `.claude/CLAUDE.md`, `CLAUDE.local.md` di root (≤ 20.000 karakter) dikirim ke box sebagai instruksi proyek, lewat pemeriksaan path yang sama.
- Teks dari box (perintah Bash, path, nama tool) yang dicetak ke terminal atau ditulis ke log audit dinetralkan: karakter kontrol C0/C1, DEL, pemisah baris Unicode, dan penanda arah teks (bidi) tampil sebagai escape (`\x1b`, `\r`, `\u202e`), jadi box tidak bisa menyamarkan aksinya dengan escape ANSI/OSC. Teks Unicode biasa tidak diubah.

Butuh: Claude Code di laptop (tidak perlu login) dan di box (sudah login: `easyrun ssh <box>` lalu `claude`), box versi terbaru (perintah `easybox`). Belum didukung di Windows (OpenSSH Windows tidak punya ControlMaster); `ls` dan `stop` tetap jalan.

## Yang disimpan, dan di mana

Semua di `~/.easyrun/` (folder 0700):

| Berkas | Isi | Izin |
|---|---|---|
| `config.json` | `api_url` (hanya bila diisi lewat `login --api-url`) | 0600 |
| `credentials.json` | token CLI (`erc_…`), id token, email, kedaluwarsa, dan alamat API penerbitnya | 0600 |
| `ssh/id_ed25519`, `ssh/id_ed25519.pub` | kunci SSH khusus easyrun, dibuat otomatis | 0600 / 0644 |
| `ssh/<id-box>-cert.pub` | sertifikat SSH berumur pendek (bawaan 1 jam) untuk satu box | 0600 |
| `known_hosts` | kunci host box (`StrictHostKeyChecking accept-new`, per id box: tetap berlaku bila alamat box diganti, dan hostname bekas box lain tidak memicu "host key changed") | dibuat `ssh` |
| `clock.json` | selisih jam laptop dengan server (dari header `Date`), supaya umur sertifikat dinilai dengan jam server | 0600 |
| `claude-audit.log` | `easyrun claude`: satu baris per aksi (waktu lokal, path atau perintah Bash, penolakan, sambung/lepas) — tanpa isi berkas atau keluaran perintah; dipindah ke `.1` setelah 10 MiB | 0600 |
| `claude-hand/` | `CLAUDE_CONFIG_DIR` untuk `claude mcp serve` milik tangan (terpisah dari config Claude-mu) | 0700 |
| `cm/` | soket ssh ControlMaster per jalan `easyrun claude` + `<soket>.pid` (pid easyrun pemilik dan pid master), dihapus saat selesai (bila path folder config terlalu panjang untuk soket unix: `$TMPDIR/easyrun-<uid>/`). Master yatim dari jalan yang mati mendadak (SIGKILL/panic) dibersihkan di jalan berikutnya: hanya bila pemiliknya sudah mati (`ssh -O exit` lewat soketnya); di Linux master juga menerima SIGTERM saat easyrun mati | 0700 |

`ssh-config --write` juga menulis di luar `~/.easyrun/`:

| Berkas | Isi | Izin |
|---|---|---|
| `~/.ssh/easyrun/<id-box>.conf` | blok satu box, ditimpa setiap `--write` | 0600 (folder 0700) |
| `~/.ssh/config` | hanya satu baris `Include ~/.ssh/easyrun/*.conf` di puncaknya | 0600 bila baru dibuat; selain itu tidak diubah |
| `~/.ssh/config.easyrun.bak` | salinan `~/.ssh/config` sebelum diubah pertama kali, dibuat sekali | 0600 |

Catatan keamanan:

- Login: tautan aktivasi tidak memuat kode, dan halaman aktivasi tidak mengisinya otomatis. Kode harus diketik dari terminalmu sendiri, jadi tautan dari penipu tidak bisa membuatmu menyetujui perangkatnya tanpa sadar. Jangan setujui kode yang tidak kamu lihat di terminalmu.
- Kunci privat tidak pernah dicetak atau dikirim; panel hanya menerima kunci publiknya untuk ditandatangani.
- `easyrun ssh` dan blok `ssh-config` selalu memasang `ForwardAgent no` dan `ForwardX11 no`, jadi agent SSH dan X11 laptop tidak terbawa ke box walau `Host *` milikmu menyalakannya. Box juga mematikan keduanya di sshd. `ClearAllForwardings` sengaja tidak dipakai, karena opsi itu juga mematikan `-L`, yang dibutuhkan VS Code Remote-SSH.
- Pengalihan HTTP (3xx) dari server API tidak pernah diikuti, supaya token tidak terkirim ke alamat lain. Pengalihan dilaporkan sebagai galat beserta tujuannya.
- Sertifikat box terikat ke token yang memintanya. `login` dan `logout` membuang semua `ssh/*-cert.pub`, sehingga pengguna berikutnya di laptop yang sama tidak bisa memakai sertifikat pengguna sebelumnya.
- Token hanya dikirim ke alamat API yang menerbitkannya. Bila `EASYRUN_API_URL` menunjuk server lain, easyrun meminta login ulang alih-alih mengirim token.
- `credentials.json` atau kunci privat yang bisa dibaca grup/pengguna lain ditolak dipakai (seperti OpenSSH), dengan petunjuk `chmod 600`. `logout` tetap jalan supaya token seperti itu bisa dicabut.
- Berkas ditulis atomik (berkas sementara 0600 lalu rename), jadi tidak pernah setengah jadi atau sempat terbuka.
- Token bisa dicabut kapan saja dari panel: Pengaturan akun → Perangkat CLI. Setelah dicabut, sambungan baru ditolak (setiap sambungan butuh tiket yang diminta dengan token, walau sertifikatnya belum kedaluwarsa), dan sesi yang sedang berjalan diputus panel pada pemeriksaan ulang berkala.
- Di Windows, izin berkas mengikuti ACL folder pengguna; pemeriksaan bit izin Unix dilewati.

## Pesan penolakan

| Situasi | Yang dicetak easyrun |
|---|---|
| Panel menolak dengan 409 (aplikasi tanpa SSH, box belum siap, alamat sedang diganti, sedang diproses) | kalimat panel apa adanya, karena panel yang tahu alasannya |
| Batas jumlah sesi SSH (429 saat membuka sambungan) | "batas sesi SSH tercapai (untuk box ini, tenant, atau platform); tutup sesi lain atau coba lagi …" |
| Terlalu sering meminta sertifikat/tiket (429) | kalimat batas laju dari panel + lama tunggu dari `Retry-After` |
| Saklar SSH platform mati (503 "SSH belum tersedia") | "SSH sedang dimatikan di platform" |
| Token CLI ditolak saat meminta daftar box, sertifikat, atau tiket (401) | "sesi CLI tidak berlaku (kedaluwarsa atau dicabut) — jalankan: easyrun login" |
| Panel menolak saat membuka sambungan (401; biasanya tiket sekali pakai 30 detik sudah kedaluwarsa) | kalimat panel, lalu "ulangi perintahnya; bila tetap gagal, jalankan: easyrun login" |
| Box mengirim galat lewat saluran (mis. image belum mendukung SSH) | kalimatnya apa adanya, tercetak sebelum pesan "Connection closed" milik ssh, dan keluar dengan kode ≠ 0 |

## Pengembangan

```bash
go test ./...
bash scripts/test-install.sh    # uji pemasang terhadap server lokal, tanpa GitHub
```

Semua uji memakai panel palsu (`httptest`), tidak pernah server EasyRun. Uji `TestE2ESSHLewatTunnelKeSshd` menjalankan `ssh` dan `sshd -i` sungguhan bila keduanya terpasang (dilewati bila tidak): di Linux sampai perintah jalan dan 4 MiB bolak-balik; di macOS sampai autentikasi sertifikat (sshd non-root ditolak audit BSM setelah login).

`easyrun claude`: uji penjaga di `internal/claudehand`; uji alur CLI memakai `ssh`/`claude` palsu (biner uji); `TestE2EClaudeLewatControlMaster` (Linux, ssh + `sshd -i` sungguhan) membuktikan semua saluran berbagi satu tiket/WebSocket/autentikasi. Uji silang dengan jembatan python rujukan P1 dan `claude mcp serve` sungguhan (dilewati di CI):

```bash
EASYBOX_P1_DIR=/path/ke/easybox-poc go test ./internal/claudehand -run TestSilangJembatanP1 -v
```

Rilis: dorong tag `vX.Y.Z`; workflow `release` membangun program untuk tiap sistem beserta `.sha256` lalu menerbitkannya. Nama aset adalah kontrak dengan `install.sh`.
