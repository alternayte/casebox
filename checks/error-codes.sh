#!/usr/bin/env bash
# check: error-codes
# born: 2026-09-29
# failure: an error named a code with no page, so its docs URL led nowhere
# rule: every CBX code in the server, the CLI and the web UI has a page under docs/src/content/docs/reference/errors/, and every page has a code in use
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
pages="docs/src/content/docs/reference/errors"
used="$(grep -rhoE 'CBX[0-9]{3}' server/Casebox.Server/Infrastructure/Errors.cs cli/internal/cbx/cbx.go | sort -u)"
fail=0
for code in $used; do
  lower="$(echo "$code" | tr '[:upper:]' '[:lower:]')"
  if [ ! -f "$pages/$lower.md" ]; then
    echo "rule: $code has no page $pages/$lower.md" >&2
    fail=1
  fi
done
for page in "$pages"/cbx*.md; do
  code="$(basename "$page" .md | tr '[:lower:]' '[:upper:]')"
  if ! echo "$used" | grep -qx "$code"; then
    echo "rule: $page documents $code, which no code uses" >&2
    fail=1
  fi
done
exit "$fail"
