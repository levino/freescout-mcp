<?php

namespace Modules\Mcp\Providers;

use Illuminate\Support\ServiceProvider;
use Modules\Mcp\Http\Middleware\PodLocalOnly;

class McpServiceProvider extends ServiceProvider
{
    protected $defer = false;

    public function boot()
    {
        $this->app['router']->aliasMiddleware('mcp.pod_local', PodLocalOnly::class);

        // Loaded without a route group on purpose: no session, no CSRF token,
        // no auth middleware. The only caller is the MCP server in this pod and
        // it authenticates with the shared token instead.
        $this->loadRoutesFrom(__DIR__.'/../Routes/bridge.php');
    }

    public function register()
    {
    }

    public function provides()
    {
        return [];
    }
}
