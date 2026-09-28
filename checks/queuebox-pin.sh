#!/usr/bin/env bash
# check: queuebox-pin
# born: 2026-09-29
# failure: the local stack and the server tests ran different QueueBox versions, so tests passed against a schema users never got
# rule: deploy/compose.yaml and server tests pin the same QueueBox image, by exact version
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
pattern='ghcr\.io/alternayte/queuebox:[0-9A-Za-z.+-]+'
compose="$(grep -oE "$pattern" deploy/compose.yaml | sort -u)"
tests="$(grep -rhoE "$pattern" server/Casebox.Server.Tests | sort -u)"
fail=0
for v in "$compose" "$tests"; do
  if [[ -z "$v" || "$(wc -l <<< "$v")" -ne 1 ]]; then
    echo "rule: expected exactly one QueueBox image in deploy/compose.yaml and in server tests; found: ${v:-none}" >&2
    fail=1
  fi
done
if [[ "$fail" -eq 0 && "$compose" != "$tests" ]]; then
  echo "rule: deploy/compose.yaml pins $compose but server tests pin $tests" >&2
  fail=1
fi
if grep -qE 'queuebox:(latest|[0-9]+(\.[0-9]+)?)$' <<< "$compose$tests"; then
  echo "rule: pin QueueBox by exact version, not a moving tag" >&2
  fail=1
fi
exit "$fail"
