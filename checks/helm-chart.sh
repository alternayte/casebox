#!/usr/bin/env bash
# check: helm-chart
# born: 2026-09-29
# failure: a chart change rendered invalid manifests that only failed on a customer's cluster
# rule: the Helm chart lints and renders with its defaults refused for missing required values and with a full set of values
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
chart=deploy/helm/casebox
# The chart needs no cluster to lint or render.
export KUBECONFIG="$(mktemp)"
chmod 600 "$KUBECONFIG"
trap 'rm -f "$KUBECONFIG"' EXIT
if ! command -v helm >/dev/null; then
  echo "rule: helm is not installed; install it (brew install helm) to check $chart" >&2
  exit 1
fi
helm lint --quiet "$chart" -f "$chart/ci/full-values.yaml" >/dev/null
helm template t "$chart" -f "$chart/ci/full-values.yaml" >/dev/null
# Without a database the chart refuses to render, rather than start a server that cannot run.
if helm template t "$chart" >/dev/null 2>&1; then
  echo "rule: $chart renders without database.host; it must require it" >&2
  exit 1
fi
