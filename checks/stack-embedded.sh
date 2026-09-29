#!/usr/bin/env bash
# check: stack-embedded
# born: 2026-09-29
# failure: casebox up started a stack whose compose file differed from the one the server tests and docs use
# rule: cli/internal/stack/assets holds exact copies of deploy/compose.yaml and deploy/queuebox.yml; run `just sync-stack` after changing them
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
fail=0
for f in compose.yaml queuebox.yml; do
  if ! cmp -s "deploy/$f" "cli/internal/stack/assets/$f"; then
    echo "rule: cli/internal/stack/assets/$f differs from deploy/$f; run just sync-stack" >&2
    fail=1
  fi
done
exit "$fail"
