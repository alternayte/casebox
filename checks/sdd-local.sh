#!/usr/bin/env bash
# check: sdd-local
# born: 2026-09-24
# failure: the design doc, the build log and the build specs are private, and a public repo must never receive them
# rule: docs/SDD.md, PROGRESS.md and docs/specs/ are ignored by git and not tracked
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
fail=0
for path in docs/SDD.md PROGRESS.md docs/specs/; do
  if [ -n "$(git ls-files -- "$path")" ]; then
    echo "rule: $path is tracked; run git rm -r --cached $path" >&2
    fail=1
  fi
  if ! git check-ignore -q "$path"; then
    echo "rule: $path is not ignored; add it to .gitignore" >&2
    fail=1
  fi
done
exit "$fail"
