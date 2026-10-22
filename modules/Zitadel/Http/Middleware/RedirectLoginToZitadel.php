<?php

namespace Modules\Zitadel\Http\Middleware;

use Closure;
use Illuminate\Support\Facades\Auth;
use Modules\Zitadel\Env;

/**
 * A middleware, not a patched view: the container replaces /www/html wholesale
 * on every FreeScout upgrade, so an edit in the application tree is silently
 * gone after the next version bump. /login?local=1 keeps the built-in form.
 */
class RedirectLoginToZitadel
{
    public function handle($request, Closure $next)
    {
        if (!$request->isMethod('get')) {
            return $next($request);
        }
        if (trim($request->path(), '/') !== 'login') {
            return $next($request);
        }
        if ($request->has('local') || Auth::check()) {
            return $next($request);
        }
        if (!Env::get('ZITADEL_ISSUER') || !Env::get('ZITADEL_CLIENT_ID')) {
            return $next($request);
        }
        if (!Env::bool('ZITADEL_FORCE_LOGIN', true)) {
            return $next($request);
        }

        return redirect('/zitadel/login');
    }
}
