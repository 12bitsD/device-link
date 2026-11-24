#!/bin/sh
set -eu

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *)
      echo "dl install: unsupported OS $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "dl install: unsupported architecture $(uname -m)" >&2
      exit 1
      ;;
  esac
}

fetch_file() {
  url="$1"
  dst="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dst"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -qO "$dst" "$url"
    return
  fi
  echo "dl install: curl or wget is required" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    out="$(sha256sum "$1")"
    printf '%s\n' "${out%% *}" | tr 'A-F' 'a-f'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    out="$(shasum -a 256 "$1")"
    printf '%s\n' "${out%% *}" | tr 'A-F' 'a-f'
    return
  fi
  echo "dl install: sha256sum or shasum is required" >&2
  exit 1
}

lookup_checksum() {
  awk -v f="$1" '
    { sub(/\r$/, "") }
    /^[[:space:]]*(#|$)/ { next }
    NF == 2 && ($2 == f || $2 == "*" f) {
      h = tolower($1)
      if (length(h) != 64 || h !~ /^[0-9a-f]+$/) { exit 2 }
      if (found != "" && found != h) { exit 3 }
      found = h
    }
    END {
      if (found == "") { exit 1 }
      print found
    }
  ' "$2"
}

resolve_base_url() {
  if [ -n "${DL_BASE_URL:-}" ]; then
    printf '%s\n' "${DL_BASE_URL%/}"
    return
  fi
  repo="${DL_REPO:-12bitsD/device-link}"
  version="${DL_VERSION:-latest}"
  if [ "$version" = "latest" ]; then
    printf 'https://github.com/%s/releases/latest/download\n' "$repo"
    return
  fi
  printf 'https://github.com/%s/releases/download/%s\n' "$repo" "$version"
}

resolve_install_dir() {
  if [ -n "${DL_INSTALL_DIR:-}" ]; then
    printf '%s\n' "$DL_INSTALL_DIR"
    return
  fi
  if [ -w "/usr/local/bin" ] || { [ ! -e "/usr/local/bin" ] && [ -w "/usr/local" ]; }; then
    printf '/usr/local/bin\n'
    return
  fi
  printf '%s/.local/bin\n' "$HOME"
}

os="$(detect_os)"
arch="$(detect_arch)"
archive="dl_${os}_${arch}.tar.gz"
base_url="$(resolve_base_url)"
install_dir="$(resolve_install_dir)"

tmp_dir="$(mktemp -d)"
stage_bin=""
cleanup() {
  rm -rf "$tmp_dir"
  if [ -n "$stage_bin" ]; then
    rm -f "$stage_bin"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

fetch_file "${base_url}/${archive}" "${tmp_dir}/${archive}"
fetch_file "${base_url}/checksums.txt" "${tmp_dir}/checksums.txt"

if ! expected="$(lookup_checksum "$archive" "${tmp_dir}/checksums.txt")"; then
  echo "dl install: valid SHA-256 entry for ${archive} missing in checksums.txt" >&2
  exit 1
fi

actual="$(sha256_file "${tmp_dir}/${archive}")"
if [ "$actual" != "$expected" ]; then
  echo "dl install: SHA-256 mismatch for ${archive} (got ${actual}, want ${expected})" >&2
  exit 1
fi

tar -xzf "${tmp_dir}/${archive}" -C "$tmp_dir"
if [ -L "${tmp_dir}/dl" ] || [ ! -f "${tmp_dir}/dl" ]; then
  echo "dl install: regular binary missing in ${archive}" >&2
  exit 1
fi

mkdir -p "$install_dir"
stage_bin="$(mktemp "${install_dir}/.dl.tmp.XXXXXX")"
cp "${tmp_dir}/dl" "$stage_bin"
chmod 0755 "$stage_bin"
mv -f "$stage_bin" "${install_dir}/dl"
stage_bin=""

cleanup
trap - EXIT INT TERM HUP

if [ "${1:-}" = "--" ]; then
  shift
fi
if [ "$#" -gt 0 ]; then
  exec "${install_dir}/dl" "$@"
fi
