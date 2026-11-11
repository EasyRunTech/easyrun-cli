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

## Perintah

| Perintah | Status |
|---|---|
| `easyrun --version`, `--help`, `--self-update` | tersedia |
| `easyrun login`, `easyrun ssh <box>`, `easyrun ssh-config <box>` | segera hadir |

## Pengembangan

```bash
go test ./...
bash scripts/test-install.sh    # uji pemasang terhadap server lokal, tanpa GitHub
```

Rilis: dorong tag `vX.Y.Z`; workflow `release` membangun program untuk tiap sistem beserta `.sha256` lalu menerbitkannya. Nama aset adalah kontrak dengan `install.sh`.
