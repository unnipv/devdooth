#!/bin/sh
# Install the latest Devdooth release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/unnipv/devdooth/main/scripts/install.sh | sh
#
# Honours DEVDOOTH_VERSION (e.g. v0.1.0) and DEVDOOTH_INSTALL_DIR.
# The downloaded archive is verified against the release's SHA-256 checksums.
set -eu

REPO="unnipv/devdooth"
VERSION="${DEVDOOTH_VERSION:-}"
REQUESTED_DIR="${DEVDOOTH_INSTALL_DIR:-}"

die() { echo "devdooth install: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin) os="darwin" ;;
  linux) os="linux" ;;
  *) die "unsupported OS: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64) arch="amd64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
fi
[ -n "$VERSION" ] || die "could not determine the latest release; set DEVDOOTH_VERSION"

base="devdooth_${VERSION}_${os}_${arch}"
release_url="https://github.com/${REPO}/releases/download/${VERSION}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading ${base}.tar.gz"
curl -fsSL "${release_url}/${base}.tar.gz" -o "$tmp/devdooth.tar.gz" \
  || die "no release asset for ${os}/${arch} at ${VERSION}"

echo "verifying checksum"
curl -fsSL "${release_url}/checksums.txt" -o "$tmp/checksums.txt" \
  || die "could not download checksums.txt"
expected=$(grep -E "(^|[[:space:]])(\./)?${base}\.tar\.gz\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || die "checksums.txt has no entry for ${base}.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/devdooth.tar.gz" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/devdooth.tar.gz" | awk '{print $1}')
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for ${base}.tar.gz"

tar -xzf "$tmp/devdooth.tar.gz" -C "$tmp"

if [ -n "$REQUESTED_DIR" ]; then
  install_dir="$REQUESTED_DIR"
  mkdir -p "$install_dir" 2>/dev/null || die "cannot create $install_dir"
else
  install_dir="/usr/local/bin"
  if ! { mkdir -p "$install_dir" 2>/dev/null && [ -w "$install_dir" ]; }; then
    install_dir="$HOME/.local/bin"
    mkdir -p "$install_dir"
  fi
fi
[ -w "$install_dir" ] || die "$install_dir is not writable"

install -m 0755 "$tmp/${base}/devdooth" "$install_dir/devdooth"
echo "installed devdooth ${VERSION} to ${install_dir}/devdooth"

case ":${PATH:-}:" in
  *":${install_dir}:"*) ;;
  *) echo "note: add ${install_dir} to your PATH to run 'devdooth'" ;;
esac

if command -v devdooth >/dev/null 2>&1; then
  resolved=$(command -v devdooth)
  [ "$resolved" = "${install_dir}/devdooth" ] || \
    echo "note: 'devdooth' on PATH resolves to ${resolved}, not the newly installed binary"
else
  echo "run: ${install_dir}/devdooth version"
fi
