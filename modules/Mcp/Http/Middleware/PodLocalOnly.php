<?php

namespace Modules\Mcp\Http\Middleware;

use Closure;
use Modules\Mcp\Env;

/**
 * Two locks, because either one alone is too weak:
 *
 *  - The request must come from the pod itself. Traffic through the ingress
 *    carries the Traefik pod address, never 127.0.0.1, so this keeps the routes
 *    off the internet even if someone guesses the path.
 *  - It must carry the shared token. That is what stops any other process that
 *    happens to run in this pod from replying to customers.
 */
class PodLocalOnly
{
    public function handle($request, Closure $next)
    {
        // A call from the sidecar goes straight to nginx over loopback and
        // carries no forwarding headers. Anything that reached us through a
        // proxy has at least one — and the image's nginx rewrites REMOTE_ADDR
        // from X-Forwarded-For for its own network, so the address alone can
        // be made to say 127.0.0.1. Refusing the headers closes that door
        // whatever the web server in front believes.
        foreach (['X-Forwarded-For', 'X-Real-IP', 'X-Forwarded-Host', 'X-Forwarded-Proto', 'Forwarded'] as $header) {
            if ($request->headers->has($header)) {
                return response()->json(['error' => 'not local'], 403);
            }
        }

        // server('REMOTE_ADDR'), not ip(): ip() trusts forwarding headers for
        // trusted proxies, and a header is what an attacker controls.
        $remote = (string) $request->server('REMOTE_ADDR');
        if (!in_array($remote, ['127.0.0.1', '::1'], true)) {
            return response()->json(['error' => 'not local'], 403);
        }

        $expected = (string) Env::get('MCP_BRIDGE_TOKEN', '');
        $given = (string) $request->header('X-Mcp-Bridge-Token', '');

        if ($expected === '' || strlen($given) !== strlen($expected) || !hash_equals($expected, $given)) {
            return response()->json(['error' => 'bad token'], 403);
        }

        return $next($request);
    }
}
