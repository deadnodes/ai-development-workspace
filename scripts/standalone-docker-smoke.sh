#!/usr/bin/env bash
set -euo pipefail

image=${1:?Pass the image to test}
container="rcp-sqlite-smoke-$$"
volume="$container"
port=${RCP_SMOKE_HOST_PORT:-8090}
url="http://127.0.0.1:$port"
export RC_TOKEN="$(openssl rand -hex 32)"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

start() {
  docker run -d --name "$container" \
    --env RC_TOKEN \
    -p "127.0.0.1:$port:8080" \
    -v "$volume:/data" \
    "$image"
  for attempt in {1..30}; do
    if curl --fail --silent --max-time 2 "$url/healthz" >/dev/null &&
       [[ "$(docker inspect -f '{{.State.Running}}' "$container")" == true ]]; then
      return
    fi
    sleep 1
  done
  docker logs "$container"
  return 1
}

start
[[ "$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$url/api/state")" == 401 ]]
curl --fail --silent --show-error -H "Authorization: Bearer $RC_TOKEN" "$url/api/state" >/dev/null
curl --fail --silent --show-error -H "Authorization: Bearer $RC_TOKEN" -H 'Content-Type: application/json' \
  --data '{"action":"create_product","id":"sqlite-smoke-product","data":{"name":"SQLite smoke"}}' \
  "$url/api/commands" >/dev/null
docker exec "$container" test -s /data/rcp.sqlite
docker stop "$container" >/dev/null
docker rm "$container" >/dev/null
start
state=$(curl --fail --silent --show-error -H "Authorization: Bearer $RC_TOKEN" "$url/api/state")
[[ "$state" == *sqlite-smoke-product* ]]
