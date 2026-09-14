<?php

namespace Modules\Mcp\Http\Middleware;

use Closure;
use Modules\Mcp\Env;

class PodLocalOnly
{
    public function handle($request, Closure $next)
    {
        // The image's nginx rewrites REMOTE_ADDR from X-Forwarded-For for its
        // own network, so the address alone can be made to say 127.0.0.1. A
        // call from the sidecar carries no forwarding header at all.
        foreach (['X-Forwarded-For', 'X-Real-IP', 'X-Forwarded-Host', 'X-Forwarded-Proto', 'Forwarded'] as $header) {
            if ($request->headers->has($header)) {
                return response()->json(['error' => 'not local'], 403);
            }
        }

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
