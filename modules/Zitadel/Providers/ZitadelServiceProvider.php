<?php

namespace Modules\Zitadel\Providers;

use Illuminate\Contracts\Http\Kernel;
use Illuminate\Support\ServiceProvider;
use Modules\Zitadel\Env;
use Modules\Zitadel\Http\Middleware\RedirectLoginToZitadel;

class ZitadelServiceProvider extends ServiceProvider
{
    protected $defer = false;

    public function boot()
    {
        $this->loadRoutesFrom(__DIR__.'/../Routes/web.php');

        // Pushed rather than aliased: the route for /login belongs to
        // FreeScout, and a global middleware is the only place a module can
        // step in front of it without touching the application tree.
        $this->app->make(Kernel::class)->pushMiddleware(RedirectLoginToZitadel::class);

        // The password reset form would be a way around the redirect.
        \Eventy::addFilter('auth.password_reset_available', function ($available) {
            if (!Env::get('ZITADEL_ISSUER') || !Env::bool('ZITADEL_FORCE_LOGIN', true)) {
                return $available;
            }

            return false;
        });
    }

    public function register()
    {
    }

    public function provides()
    {
        return [];
    }
}
