#!/bin/sh
# The Casebox verifier: restore the held-out test files as they are at the base, apply the
# held-out tests, run the recipe's test commands, and let the verdict program decide from
# their result files. The reward is 1 when every fail-to-pass and pass-to-pass test passes.
set -u
tests=${CASEBOX_TESTS_DIR:-/tests}
logs=${CASEBOX_LOGS_DIR:-/logs/verifier}
results=${CASEBOX_RESULTS_DIR:-/results}
workdir=${CASEBOX_WORKDIR:-'/workspace'}
verdict=${CASEBOX_VERDICT:-/usr/local/bin/casebox-verdict}
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
mkdir -p "$logs"
fail() {
  echo "$1" >&2
  "$verdict" -oracle "$tests/oracle.json" -out "$logs" -error "$1" || echo 0 >"$logs/reward.txt"
  exit 0
}
cd "$workdir" || fail "the working directory $workdir is missing"

# The agent's changes to the held-out files never count.
rm -rf -- 'store/put_test.go'
rm -rf -- 'store/store_test.go'
cp -R "$tests/base/." "$workdir/" || fail "the held-out files could not be restored"
if [ -s "$tests/tests.patch" ]; then
  git -c core.hooksPath=/dev/null -c core.fsmonitor=false apply --whitespace=nowarn "$tests/tests.patch" 2>"$logs/apply.log" || fail "the held-out tests do not apply: $(cat "$logs/apply.log")"
fi

set --
# go test -json ./... > /results/go-test.json
find "$results" -mindepth 1 -delete 2>/dev/null || mkdir -p "$results"
if command -v timeout >/dev/null 2>&1; then timeout 900 sh -c 'go test -json ./... > /results/go-test.json'; else sh -c 'go test -json ./... > /results/go-test.json'; fi >"$logs/command-0.log" 2>&1
echo "exit $?" >>"$logs/command-0.log"
mkdir -p "$logs/results/0"
cp -R "$results/." "$logs/results/0/" 2>/dev/null
set -- "$@" 'go-test-json'="$logs/results/0"
# go vet ./...
find "$results" -mindepth 1 -delete 2>/dev/null || mkdir -p "$results"
if command -v timeout >/dev/null 2>&1; then timeout 600 sh -c 'go vet ./...'; else sh -c 'go vet ./...'; fi >"$logs/command-1.log" 2>&1
echo "exit $?" >>"$logs/command-1.log"

"$verdict" -oracle "$tests/oracle.json" -out "$logs" "$@" || echo 0 >"$logs/reward.txt"
