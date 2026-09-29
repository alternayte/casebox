#!/usr/bin/env bash
# check: docs-generated
# born: 2026-09-29
# failure: the CLI reference page described flags the CLI no longer had
# rule: docs/src/content/docs/reference/cli.md is what `casebox docs` writes from the current command tree
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
page=docs/src/content/docs/reference/cli.md
fresh="$(mktemp)"
trap 'rm -f "$fresh"' EXIT
(cd cli && go run ./cmd/casebox docs --out "$fresh")
if ! cmp -s "$fresh" "$page"; then
  echo "rule: $page is stale; run (cd cli && go run ./cmd/casebox docs --out ../$page) and commit it" >&2
  exit 1
fi
