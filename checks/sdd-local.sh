#!/usr/bin/env bash
# check: sdd-local
# born: 2026-09-24
# failure: the design doc is private, and a public repo must never receive it
# rule: docs/SDD.md is ignored by git and not tracked
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
fail=0
if git ls-files --error-unmatch docs/SDD.md >/dev/null 2>&1; then
  echo "rule: docs/SDD.md is tracked; run git rm --cached docs/SDD.md" >&2
  fail=1
fi
if ! git check-ignore -q docs/SDD.md; then
  echo "rule: docs/SDD.md is not ignored; add it to .gitignore" >&2
  fail=1
fi
exit "$fail"
