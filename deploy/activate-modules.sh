#!/bin/sh
# Activates the two modules on every container start.
#
# FreeScout only boots a module that has a row in the `modules` table, and a
# module dropped into Modules/ has none — in the interface that is the
# "Activate" click. This does the same thing the interface does, through
# FreeScout's own model, so a fresh pod comes up with the modules running
# instead of waiting for someone to click.
#
# The image runs every executable *.sh in /override/custom-scripts during init,
# after the migrations and before it registers modules. Idempotent: an already
# active module is left alone.
set -e

WEBROOT="${NGINX_WEBROOT:-/www/html}"
USER="${NGINX_USER:-nginx}"

[ -d "$WEBROOT/Modules" ] || exit 0

cat > /tmp/activate-modules.php <<'PHP'
<?php

require getenv('NGINX_WEBROOT') ? rtrim(getenv('NGINX_WEBROOT'), '/').'/vendor/autoload.php' : '/www/html/vendor/autoload.php';

$webroot = rtrim(getenv('NGINX_WEBROOT') ?: '/www/html', '/');
$app = require $webroot.'/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

foreach (['Mcp', 'Zitadel'] as $name) {
    $manifest = $webroot.'/Modules/'.$name.'/module.json';
    if (!is_file($manifest)) {
        continue;
    }
    $data = json_decode((string) file_get_contents($manifest), true);
    $alias = is_array($data) && !empty($data['alias']) ? $data['alias'] : null;
    if (!$alias) {
        echo "[activate-modules] $name has no alias in module.json\n";
        continue;
    }
    if (\App\Module::isActive($alias)) {
        echo "[activate-modules] $alias already active\n";
        continue;
    }
    \App\Module::setActive($alias, true);
    \App\Module::clearModulesCache();
    echo "[activate-modules] $alias activated\n";
}
PHP

# The first PHP CLI start in a fresh container has been seen to segfault while
# compiling the application (PHP 8.5 with JIT, aarch64). Opcache buys nothing
# for a script that runs once, and the work is idempotent, so: JIT off, and
# retry rather than leave the modules switched off.
attempt=1
while [ "$attempt" -le 3 ]; do
    if su -s /bin/sh "$USER" -c "cd $WEBROOT && NGINX_WEBROOT=$WEBROOT php -d opcache.enable_cli=0 -d opcache.jit=off -d opcache.jit_buffer_size=0 /tmp/activate-modules.php"; then
        rm -f /tmp/activate-modules.php
        exit 0
    fi
    echo "[activate-modules] attempt $attempt failed, retrying"
    attempt=$((attempt + 1))
    sleep 2
done

echo "[activate-modules] giving up after 3 attempts" >&2
rm -f /tmp/activate-modules.php
# Not fatal: the container should still come up, the modules just stay off.
exit 0
