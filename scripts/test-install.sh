#!/usr/bin/env bash
# ok/bad selalu keluar 0, jadi pola `uji && ok || bad` aman di sini.
# shellcheck disable=SC2015
# Uji install.sh tanpa GitHub: bangun program untuk sistem ini, sajikan lewat
# server HTTP lokal sementara, lalu jalankan pemasang seperti `curl … | bash`.
# Juga memastikan checksum yang salah atau hilang MEMBATALKAN pemasangan.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); printf '  ok     %s\n' "$*"; }
bad() { fail=$((fail + 1)); printf '  GAGAL  %s\n' "$*"; }

case "$(uname -s)" in Darwin) os_name=macos ;; Linux) os_name=linux ;; *) echo "sistem tidak didukung"; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch_name=x86_64 ;; arm64|aarch64) arch_name=arm64 ;; *) echo "arsitektur tidak didukung"; exit 1 ;; esac
asset="easyrun-$os_name-$arch_name"
want_version="v0.0.0-uji"

sum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

mkdir -p "$work/good" "$work/badsum" "$work/nosum"
(cd "$root" && CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X github.com/EasyRunTech/easyrun-cli/internal/version.Version=$want_version" \
  -o "$work/good/$asset" ./cmd/easyrun)
(cd "$work/good" && sum "$asset" > "$asset.sha256")
cp "$work/good/$asset" "$work/badsum/$asset"
echo "0000000000000000000000000000000000000000000000000000000000000000  $asset" > "$work/badsum/$asset.sha256"
cp "$work/good/$asset" "$work/nosum/$asset"

# Port acak bebas, dipilih oleh OS.
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
(cd "$work" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
server_pid=$!
for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 0.1; done

install_from() { # $1 = subfolder, $2 = folder tujuan
  EASYRUN_DOWNLOAD_BASE="http://127.0.0.1:$port/$1" EASYRUN_INSTALL_DIR="$2" bash < "$root/install.sh"
}

echo "== pemasangan normal (seperti curl | bash)"
dest="$work/bin"
if out="$(install_from good "$dest" 2>&1)"; then ok "pemasang selesai"; else bad "pemasang gagal: $out"; fi
[ -x "$dest/easyrun" ] && ok "program terpasang dan bisa dijalankan" || bad "program tidak ada di $dest"
got="$("$dest/easyrun" --version 2>&1 || true)"
[ "$got" = "easyrun $want_version" ] && ok "versi: $got" || bad "versi salah: $got"
grep -q "checksum cocok" <<<"$out" && ok "checksum diperiksa" || bad "pesan checksum tidak muncul"
grep -q "belum ada di PATH" <<<"$out" && ok "memperingatkan folder di luar PATH" || bad "peringatan PATH tidak muncul"

echo "== pasang ulang menimpa versi lama"
echo "lama" > "$dest/easyrun"
if install_from good "$dest" >/dev/null 2>&1 && [ "$("$dest/easyrun" --version)" = "easyrun $want_version" ]; then
  ok "versi lama tertimpa"
else
  bad "pasang ulang gagal"
fi

echo "== checksum salah membatalkan"
dest2="$work/bin2"
if out="$(install_from badsum "$dest2" 2>&1)"; then bad "pemasang LOLOS padahal checksum salah"; else ok "dibatalkan"; fi
grep -q "checksum tidak cocok" <<<"$out" && ok "pesan jelas" || bad "pesan tidak jelas: $out"
[ ! -e "$dest2/easyrun" ] && ok "tidak ada yang terpasang" || bad "program tetap terpasang"

echo "== checksum hilang membatalkan"
if out="$(install_from nosum "$dest2" 2>&1)"; then bad "pemasang LOLOS tanpa checksum"; else ok "dibatalkan"; fi
grep -q "berkas checksum" <<<"$out" && ok "pesan jelas" || bad "pesan tidak jelas: $out"

echo "== aset tidak ada"
if out="$(install_from kosong "$dest2" 2>&1)"; then bad "pemasang LOLOS tanpa aset"; else ok "dibatalkan: unduhan gagal"; fi

echo
echo "lulus: $pass, gagal: $fail"
[ "$fail" -eq 0 ]
