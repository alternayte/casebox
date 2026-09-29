#!/usr/bin/env bash
# Lints the docs and the README with Vale against the Casebox style (docs/styles/Casebox): second
# person, present tense, short sentences, plain words, and each page opens with what the reader
# achieves. Downloads a pinned Vale into artifacts/tools on first use.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
version="3.22.0"
tools="artifacts/tools/vale-$version"
if [[ ! -x "$tools/vale" ]]; then
  case "$(uname -s)-$(uname -m)" in
    Darwin-arm64) asset="macOS_arm64" ;;
    Darwin-x86_64) asset="macOS_64-bit" ;;
    Linux-x86_64) asset="Linux_64-bit" ;;
    Linux-aarch64) asset="Linux_arm64" ;;
    *) echo "No Vale build for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
  esac
  mkdir -p "$tools"
  curl -fsSL "https://github.com/errata-ai/vale/releases/download/v$version/vale_${version}_$asset.tar.gz" | tar -xz -C "$tools" vale
fi
"$tools/vale" --config docs/.vale.ini docs/src/content/docs README.md "$@"
