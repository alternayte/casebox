#!/bin/sh
# Applies the merged source change: the reference solution.
set -eu
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
cd '/workspace'
git -c core.hooksPath=/dev/null -c core.fsmonitor=false apply --whitespace=nowarn /solution/solution.patch
