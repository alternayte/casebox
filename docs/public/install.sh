#!/bin/sh
# Installs the Casebox CLI on macOS or Linux from its GitHub release:
#   curl -fsSL https://casebox-docs.pages.dev/install.sh | sh
# CASEBOX_VERSION picks a release (default: the newest, pre-releases included).
# CASEBOX_INSTALL_DIR picks the directory (default: ~/.local/bin; no sudo needed).
# CASEBOX_DOWNLOAD_URL replaces https://github.com/alternayte/casebox/releases/download, for a
# mirror when github.com is blocked; CASEBOX_VERSION is then required.
set -eu

repo="alternayte/casebox"
dir="${CASEBOX_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "casebox install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }
need curl
need tar

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "no build for $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "no build for $(uname -m)" ;;
esac

tag="${CASEBOX_VERSION:-}"
if [ -z "$tag" ]; then
  # The releases list includes pre-releases, which /releases/latest leaves out.
  tag="$(curl -fsSL "https://api.github.com/repos/$repo/releases?per_page=1" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
  [ -n "$tag" ] || fail "no release found at github.com/$repo/releases"
fi
case "$tag" in v*) ;; *) tag="v$tag" ;; esac
version="${tag#v}"
name="casebox_${version}_${os}_${arch}"
base="${CASEBOX_DOWNLOAD_URL:-https://github.com/$repo/releases/download}/$tag"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "Downloading Casebox $version for $os/$arch"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || fail "download $base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download $base/checksums.txt"

want="$(grep " $name.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)"
fi
[ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
mkdir -p "$dir"
install -m 0755 "$tmp/$name/casebox" "$dir/casebox" 2>/dev/null || {
  cp "$tmp/$name/casebox" "$dir/casebox" && chmod 0755 "$dir/casebox"
}
echo "Installed $dir/casebox ($("$dir/casebox" --version 2>/dev/null || echo "$version"))"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
esac
