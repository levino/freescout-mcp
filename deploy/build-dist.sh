#!/bin/sh
set -eu

target=${1:?usage: build-dist.sh <target-dir> [commit]}
commit=${2:-}
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

rm -rf "$target"
mkdir -p "$target/modules" "$target/deploy"

CGO_ENABLED=0 go build -C "$repo" -trimpath -ldflags "-s -w" -o "$target/mcp-server" ./server
cp -R "$repo/modules/Mcp" "$repo/modules/Zitadel" "$target/modules/"
cp "$repo/deploy/activate-modules.sh" "$target/deploy/activate-modules.sh"
chmod 0755 "$target/deploy/activate-modules.sh"

if [ -n "$commit" ]; then
    printf '%s\n' "$commit" > "$target/.dist-commit"
fi
