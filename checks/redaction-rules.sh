#!/usr/bin/env bash
# check: redaction-rules
# born: 2026-09-29
# failure: the CLI and the server redacted different secrets, so the server's second pass did not cover what a stale CLI missed
# rule: the CLI (redact.go) and the server (Redaction.cs) list the same redaction rule names in the same order
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
go_rules="$(grep -oE '^\s*\{"[a-z_]+", regexp' cli/internal/capture/redact.go | grep -oE '"[a-z_]+"' | tr -d '"')"
cs_rules="$(grep -oE '^\s*\("[a-z_]+", [A-Za-z]+\(\)\)' server/Casebox.Server/Features/Capture/Redaction.cs | grep -oE '"[a-z_]+"' | tr -d '"')"
if [[ -z "$go_rules" || "$go_rules" != "$cs_rules" ]]; then
  echo "rule: redaction rules differ between cli/internal/capture/redact.go and server/Casebox.Server/Features/Capture/Redaction.cs" >&2
  diff <(echo "$go_rules") <(echo "$cs_rules") >&2 || true
  exit 1
fi
