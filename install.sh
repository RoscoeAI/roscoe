#!/bin/sh
set -eu
REPO="RoscoeAI/roscoe"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "roscoe install: unsupported arch: $arch" >&2; exit 1 ;; esac
case "$os" in darwin|linux) ;; *) echo "roscoe install: unsupported OS: $os" >&2; exit 1 ;; esac
# ROSCOE_VERSION pins a release (e.g. v0.28.0). `roscoe deploy` sets it so every
# node in a fleet ends up on the control plane's own version; unset means latest.
if [ -n "${ROSCOE_VERSION:-}" ]; then
  url="https://github.com/$REPO/releases/download/${ROSCOE_VERSION}/roscoe_${os}_${arch}.tar.gz"
else
  url="https://github.com/$REPO/releases/latest/download/roscoe_${os}_${arch}.tar.gz"
fi
dir="${ROSCOE_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "roscoe install: downloading $url"
curl -fsSL "$url" -o "$tmp/roscoe.tar.gz"
tar -xzf "$tmp/roscoe.tar.gz" -C "$tmp"
install -m 0755 "$tmp/roscoe" "$dir/roscoe"
echo "roscoe install: installed $("$dir/roscoe" version 2>/dev/null || echo roscoe) to $dir/roscoe"
case ":$PATH:" in *":$dir:"*) : ;; *) echo "roscoe install: note: add $dir to your PATH" ;; esac
