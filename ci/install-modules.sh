#!/bin/sh
# Test scaffolding: puts the staged modules into /data/Modules at container
# start, which is what the init container does in the cluster. Bind-mounting
# them instead would test a shape we do not deploy — and would hand FreeScout
# directories it cannot chown.
set -e

SOURCE="/ci/.stage/Modules"
TARGET="${DATA_PATH:-/data}/Modules"

[ -d "$SOURCE" ] || exit 0
mkdir -p "$TARGET"

for module in Mcp Zitadel; do
    [ -d "$SOURCE/$module" ] || continue
    rm -rf "$TARGET/$module"
    cp -R "$SOURCE/$module" "$TARGET/$module"
    echo "[install-modules] $module installed"
done

chown -R "${NGINX_USER:-nginx}":"${NGINX_GROUP:-www-data}" "$TARGET"
