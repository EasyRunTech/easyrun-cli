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
| `easyrun ssh-config <box> [--write]` | Blok `Match host … exec "easyrun ssh-prepare …"` + `Host …`. Tanpa `--write` blok dicetak. Dengan `--write` blok ditulis ke `~/.ssh/easyrun/<id>.conf`, ditambah satu baris `Include` di puncak `~/.ssh/config`. |
| `easyrun ssh-prepare <box>` | Hanya memperbarui sertifikat bila perlu (dipanggil blok `Match exec`). Tanpa jaringan selama sertifikat masih segar. |
| `easyrun tunnel <box>` | ProxyCommand: saluran SSH lewat WebSocket HTTPS panel. Dipanggil `ssh`, bukan untuk diketik. |
| `easyrun --version`, `--help`, `--self-update` | Versi, bantuan, pembaruan. |

Butuh OpenSSH (`ssh`) di PATH: sudah ada di macOS dan kebanyakan Linux; Windows: fitur "OpenSSH Client".

Env:

| Env | Arti | Bawaan |
|---|---|---|
| `EASYRUN_API_URL` | alamat API panel (https; http hanya untuk localhost) | `config.json`, lalu `https://api.easyrun.io` |
| `EASYRUN_CONFIG_DIR` | folder data easyrun | `~/.easyrun` |
| `EASYRUN_NO_BROWSER` | isi apa pun supaya `login` tidak membuka browser | — |

## Yang disimpan, dan di mana

Semua di `~/.easyrun/` (folder 0700):

| Berkas | Isi | Izin |
|---|---|---|
| `config.json` | `api_url` (hanya bila diisi lewat `login --api-url`) | 0600 |
| `credentials.json` | token CLI (`erc_…`), id token, email, kedaluwarsa, dan alamat API penerbitnya | 0600 |
| `ssh/id_ed25519`, `ssh/id_ed25519.pub` | kunci SSH khusus easyrun, dibuat otomatis | 0600 / 0644 |
| `ssh/<id-box>-cert.pub` | sertifikat SSH berumur pendek (bawaan 1 jam) untuk satu box | 0600 |
| `known_hosts` | kunci host box (`StrictHostKeyChecking accept-new`, per hostname box) | dibuat `ssh` |

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

Rilis: dorong tag `vX.Y.Z`; workflow `release` membangun program untuk tiap sistem beserta `.sha256` lalu menerbitkannya. Nama aset adalah kontrak dengan `install.sh`.
