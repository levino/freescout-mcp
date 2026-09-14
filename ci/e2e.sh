#!/usr/bin/env bash
# Brings up the end-to-end stack, seeds it, starts the MCP server and the fake
# login provider inside the FreeScout container, and runs the test against it.
#
#   ci/e2e.sh          full run, tears the stack down afterwards
#   ci/e2e.sh up       bring the stack up and leave it running
#   ci/e2e.sh test     run the test against a stack that is already up
#   ci/e2e.sh logs     show the container logs
#   ci/e2e.sh down     tear it down
set -euo pipefail

cd "$(dirname "$0")"
STAGE="$PWD/.stage"
COMPOSE=(docker compose -f "$PWD/compose.yml")

BRIDGE_TOKEN="bridge-token-for-tests"
SIGNING_KEY="e2e-signing-key-that-is-long-enough-000"
FREESCOUT_URL="http://127.0.0.1:8080"
MCP_URL="http://127.0.0.1:8081"
OIDC_URL="http://127.0.0.1:8082"

stage() {
  # Empty the staging directories without removing them: a directory that is
  # deleted and recreated can end up bind-mounted as its old, gone inode, and
  # the container then sees nothing in it.
  mkdir -p "$STAGE/Modules" "$STAGE/bin" "$STAGE/custom-scripts"
  rm -rf "$STAGE/Modules"/* "$STAGE/bin"/* "$STAGE/custom-scripts"/*
  # Copies, not the repository: the container chowns what it mounts.
  cp -R ../modules/Mcp ../modules/Zitadel "$STAGE/Modules/"
  chmod -R a+rX "$STAGE/Modules"
  # Start hooks, run in this order by the image: install the modules the way the
  # cluster's init container does, then activate them with the very script that
  # ships to production.
  cp install-modules.sh "$STAGE/custom-scripts/00-install-modules.sh"
  cp ../deploy/activate-modules.sh "$STAGE/custom-scripts/10-activate-modules.sh"
  chmod -R a+rx "$STAGE/custom-scripts"
  # Static binaries for the container's Alpine userland, host architecture.
  CGO_ENABLED=0 go build -C .. -o "$STAGE/bin/mcp-server" ./server
  CGO_ENABLED=0 go build -C .. -o "$STAGE/bin/fake-oidc" ./ci/fake-oidc
}

wait_for() {
  local url="$1" what="$2" deadline=$((SECONDS + ${3:-420}))
  echo "waiting for $what at $url"
  until curl -fsS -o /dev/null "$url"; do
    if (( SECONDS > deadline )); then
      echo "$what did not come up in time" >&2
      "${COMPOSE[@]}" logs --tail 80 >&2 || true
      exit 1
    fi
    sleep 3
  done
  echo "$what is up"
}

up() {
  stage
  "${COMPOSE[@]}" up -d
  # FreeScout's first boot creates the schema and warms the assets.
  wait_for "$FREESCOUT_URL/login" "freescout" 600

  echo "seeding"
  "${COMPOSE[@]}" exec -T freescout php /ci/seed.php > "$STAGE/seed.json"
  cat "$STAGE/seed.json"

  echo "starting the fake login provider and the MCP server inside the container"
  "${COMPOSE[@]}" exec -d \
    -e ISSUER="$OIDC_URL" -e LISTEN=":8082" -e EMAIL="post@levinkeller.de" \
    freescout sh -c '/ci/.stage/bin/fake-oidc > /tmp/fake-oidc.log 2>&1' 
  "${COMPOSE[@]}" exec -d \
    -e BASE_URL="$MCP_URL" -e LISTEN=":8081" \
    -e BRIDGE_URL="http://127.0.0.1" -e BRIDGE_TOKEN="$BRIDGE_TOKEN" \
    -e OIDC_ISSUER="$OIDC_URL" -e OIDC_CLIENT_ID="freescout-mcp" \
    -e TOKEN_SIGNING_KEY="$SIGNING_KEY" \
    -e INSECURE_ALLOW_LOCAL_CLIENTS="true" \
    freescout sh -c '/ci/.stage/bin/mcp-server > /tmp/mcp-server.log 2>&1' 

  wait_for "$OIDC_URL/.well-known/openid-configuration" "fake oidc" 60
  wait_for "$MCP_URL/healthz" "mcp server" 60
}

run_test() {
  go test -C .. -tags e2e -count 1 -timeout 10m -v ./ci/e2e/...
}

case "${1:-all}" in
  up)   up ;;
  test) run_test ;;
  logs)
    "${COMPOSE[@]}" logs --tail 200
    echo "=== mcp-server ==="
    "${COMPOSE[@]}" exec -T freescout sh -c 'tail -100 /tmp/mcp-server.log' || true
    echo "=== freescout laravel.log ==="
    "${COMPOSE[@]}" exec -T freescout sh -c 'tail -60 /data/storage/logs/laravel.log' || true
    ;;
  down) "${COMPOSE[@]}" down -v --remove-orphans ;;
  all)
    up
    run_test
    "${COMPOSE[@]}" down -v --remove-orphans
    ;;
  *) echo "usage: $0 [all|up|test|logs|down]" >&2; exit 2 ;;
esac
