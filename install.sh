#!/usr/bin/env bash
# Pemasang easyrun — mendeteksi sistem, mengunduh program yang sesuai dari
# rilis GitHub terbaru, memeriksa checksum-nya, lalu memasangnya.
#
# Pemakaian:
#   curl -fsSL https://raw.githubusercontent.com/EasyRunTech/easyrun-cli/main/install.sh | bash
#
# Setelan opsional:
#   EASYRUN_VERSION=v0.1.0        pasang versi tertentu (bawaan: terbaru)
#   EASYRUN_INSTALL_DIR=~/bin     folder tujuan (bawaan: /usr/local/bin)
#   EASYRUN_DOWNLOAD_BASE=<url>   unduh dari mirror/server uji, bukan GitHub
#                                 (berisi <aset> dan <aset>.sha256)
#
# `easyrun --self-update` menjalankan skrip ini dengan EASYRUN_INSTALL_DIR =
# folder tempat program yang sedang berjalan berada.
set -euo pipefail

REPO="EasyRunTech/easyrun-cli"
BIN="easyrun"
VERSION="${EASYRUN_VERSION:-latest}"
INSTALL_DIR="${EASYRUN_INSTALL_DIR:-/usr/local/bin}"
DOWNLOAD_BASE="${EASYRUN_DOWNLOAD_BASE:-}"

info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33mperingatan:\033[0m %s\n' "$*" >&2; }
fail()  { printf '\033[1;31mgalat:\033[0m %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl dibutuhkan tetapi belum terpasang"

# --- Deteksi sistem ----------------------------------------------------------
# Nama aset adalah KONTRAK dengan .github/workflows/release.yml: ganti di dua
# tempat sekaligus.
os="$(uname -s)"
arch="$(uname -m)"

case "$arch" in
  x86_64|amd64)   arch_name="x86_64" ;;
  arm64|aarch64)  arch_name="arm64" ;;
  *)              arch_name="" ;;
esac

case "$os" in
  Darwin) os_name="macos" ;;
  Linux)  os_name="linux" ;;
  MINGW*|MSYS*|CYGWIN*)
    cat >&2 <<EOF
galat: skrip ini untuk macOS dan Linux.
Di Windows, unduh $BIN-windows-x86_64.exe dari
  https://github.com/$REPO/releases/latest
ganti namanya menjadi $BIN.exe, lalu taruh di folder yang ada di PATH.
EOF
    exit 1
    ;;
  *) fail "sistem operasi tidak didukung: $os" ;;
esac

[ -n "$arch_name" ] || fail "arsitektur tidak didukung: $os/$arch"
asset="$BIN-$os_name-$arch_name"

info "sistem: $os $arch → aset $asset"

# --- Unduh -------------------------------------------------------------------
if [ -n "$DOWNLOAD_BASE" ]; then
  url="${DOWNLOAD_BASE%/}/$asset"
elif [ "$VERSION" = "latest" ]; then
  url="https://github.com/$REPO/releases/latest/download/$asset"
else
  url="https://github.com/$REPO/releases/download/$VERSION/$asset"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

info "mengunduh $url"
curl -fsSL -o "$tmp/$BIN" "$url" || fail "unduhan gagal (aset $asset, versi $VERSION)"

# --- Periksa keutuhan ----------------------------------------------------------
# Setiap rilis menyertakan <aset>.sha256 di sebelah programnya; checksum yang
# hilang atau tidak cocok berarti ada yang salah — keduanya membatalkan.
curl -fsSL -o "$tmp/$BIN.sha256" "$url.sha256" || fail "berkas checksum untuk $asset ($VERSION) tidak ada, dibatalkan"
expected="$(awk '{print $1}' "$tmp/$BIN.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/$BIN" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/$BIN" | awk '{print $1}')"
else
  fail "butuh sha256sum atau shasum untuk memeriksa unduhan"
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || fail "checksum tidak cocok — unduhan rusak atau diubah orang, dibatalkan"
info "checksum cocok (sha256)"

chmod +x "$tmp/$BIN"

# --- Pasang ------------------------------------------------------------------
# sudo hanya dipakai bila folder tujuan tidak bisa ditulis pengguna ini. Folder
# yang ada tetapi hanya-baca (mis. /usr/local/bin di Linux) juga butuh sudo,
# jadi yang diperiksa adalah bisa-ditulis setelah mencoba membuatnya.
sudo_cmd=""
mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [ ! -w "$INSTALL_DIR" ]; then
  command -v sudo >/dev/null 2>&1 || fail "tidak bisa menulis ke $INSTALL_DIR dan sudo tidak tersedia — isi EASYRUN_INSTALL_DIR dengan folder yang bisa ditulis"
  sudo_cmd="sudo"
fi
$sudo_cmd mkdir -p "$INSTALL_DIR"
$sudo_cmd mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
$sudo_cmd chmod +x "$INSTALL_DIR/$BIN"

# macOS: hapus tanda karantina bila ada (curl biasanya tidak memasangnya,
# tetapi Gatekeeper menolak menjalankan program yang bertanda itu).
if [ "$os" = "Darwin" ]; then
  $sudo_cmd xattr -d com.apple.quarantine "$INSTALL_DIR/$BIN" 2>/dev/null || true
fi

info "terpasang di $INSTALL_DIR/$BIN"

# --- Periksa hasil -----------------------------------------------------------
if "$INSTALL_DIR/$BIN" --version >/dev/null 2>&1; then
  info "versi: $("$INSTALL_DIR/$BIN" --version)"
else
  warn "terpasang, tetapi '$INSTALL_DIR/$BIN --version' gagal — periksa apakah programnya cocok dengan sistem ini"
fi

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) warn "$INSTALL_DIR belum ada di PATH — tambahkan, atau jalankan sebagai $INSTALL_DIR/$BIN" ;;
esac

printf '\nJalankan \033[1m%s --help\033[0m untuk mulai. Perbarui kapan saja dengan \033[1m%s --self-update\033[0m.\n' "$BIN" "$BIN"
