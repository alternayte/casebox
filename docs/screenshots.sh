#!/usr/bin/env bash
# Makes the docs site's screenshots from the demo data (`casebox up --demo`), so they show the
# product as it is: docs/public/screenshots/*.png, copied into docs/dist when the site is built.
# Needs Docker and agent-browser. CASEBOX_SERVER_IMAGE names a server image; without it the
# script builds one from the source.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
cd "$root"
out="docs/public/screenshots"
mkdir -p "$out"
work="$(mktemp -d)"
project="casebox-docs-shots"
port=18099
export AGENT_BROWSER_SESSION="casebox-docs-shots"

image="${CASEBOX_SERVER_IMAGE:-}"
if [[ -z "$image" ]]; then
  image="casebox-server:docs"
  docker build -q -f server/Dockerfile -t "$image" . >/dev/null
fi

cleanup() {
  agent-browser close >/dev/null 2>&1 || true
  docker compose -p "$project" -f "$work/compose.yaml" --env-file "$work/.env" down -v >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

cp deploy/compose.yaml deploy/queuebox.yml "$work/"
password="$(openssl rand -hex 16)"
cat > "$work/.env" <<ENV
CASEBOX_DB_PASSWORD=$(openssl rand -hex 16)
CASEBOX_ADMIN_PASSWORD=$password
CASEBOX_POLL_TOKEN=$(openssl rand -hex 16)
CASEBOX_QUEUEBOX_ADMIN_TOKEN=$(openssl rand -hex 16)
CASEBOX_SERVER_IMAGE=$image
CASEBOX_PORT=$port
CASEBOX_DEMO=true
ENV
docker compose -p "$project" -f "$work/compose.yaml" --env-file "$work/.env" up -d --no-build >/dev/null
url="http://127.0.0.1:$port"
for _ in $(seq 1 90); do
  curl -sf -o /dev/null "$url/api/v1/auth/methods" && break
  sleep 2
done
# The demo loads at start; its pages are ready once the proposal exists.
jar="$work/cookies"
curl -sf -c "$jar" -H 'Content-Type: application/json' -d "{\"password\":\"$password\"}" "$url/api/v1/auth/local" >/dev/null
for _ in $(seq 1 30); do
  proposal="$(curl -sf -b "$jar" "$url/api/v1/proposals/" | tr '{' '\n' | grep '"kind":"skill"' | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1 || true)"
  [[ -n "$proposal" ]] && break
  sleep 2
done
pattern="$(curl -sf -b "$jar" "$url/api/v1/patterns/" | sed -n 's/.*"patterns":\[{"id":"\([^"]*\)".*/\1/p')"

agent-browser set viewport 1280 900 >/dev/null
agent-browser open "$url/login" >/dev/null
agent-browser find label "Local admin password" fill "$password" >/dev/null
agent-browser find role button click --name "Log in" >/dev/null
agent-browser wait --load networkidle >/dev/null
shot() {
  agent-browser open "$url$1" >/dev/null
  agent-browser wait --load networkidle >/dev/null
  sleep 1
  agent-browser screenshot "$out/$2.png" >/dev/null
}
shot "/" overview
shot "/patterns/$pattern" pattern
shot "/proposals/$proposal" proposal
shot "/proposals" proposals

if [[ -d docs/dist ]]; then
  mkdir -p docs/dist/screenshots && cp "$out"/*.png docs/dist/screenshots/
fi
ls "$out"
