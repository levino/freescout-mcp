#!/bin/sh
# Test scaffolding: does what the cluster's init container does. Bind-mounting
# the modules instead would test a shape we do not deploy.
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
