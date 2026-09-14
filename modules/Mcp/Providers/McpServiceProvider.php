<?php

namespace Modules\Mcp\Providers;

use Illuminate\Support\ServiceProvider;
use Modules\Mcp\Http\Middleware\PodLocalOnly;
use Modules\Mcp\Status;

class McpServiceProvider extends ServiceProvider
{
    protected $defer = false;

    public function boot()
    {
        $this->app['router']->aliasMiddleware('mcp.pod_local', PodLocalOnly::class);

        // No route group: no session, no CSRF, no auth middleware. The caller is
        // the MCP server in this pod and it authenticates with the shared token.
        $this->loadRoutesFrom(__DIR__.'/../Routes/bridge.php');

        $this->loadViewsFrom(__DIR__.'/../Resources/views', 'mcp');

        $this->registerSettings();
    }

    protected function registerSettings()
    {
        \Eventy::addFilter('settings.sections', function ($sections) {
            $sections['mcp'] = ['title' => 'MCP', 'icon' => 'link', 'order' => 400];

            return $sections;
        });

        // An empty section is a 404 in SettingsController::view.
        \Eventy::addFilter('settings.section_settings', function ($settings, $section) {
            if ($section !== 'mcp') {
                return $settings;
            }

            return ['mcp_endpoint' => Status::endpointUrl()];
        }, 20, 2);

        \Eventy::addFilter('settings.section_params', function ($params, $section) {
            if ($section !== 'mcp') {
                return $params;
            }

            return array_merge($params, ['template_vars' => [
                'mcp_endpoint'     => Status::endpointUrl(),
                'mcp_version'      => Status::serverVersion(),
                'mcp_bridge_ready' => Status::bridgeTokenConfigured(),
            ]]);
        }, 20, 2);

        \Eventy::addFilter('settings.view', function ($view, $section) {
            return $section === 'mcp' ? 'mcp::settings' : $view;
        }, 20, 2);
    }

    public function register()
    {
    }

    public function provides()
    {
        return [];
    }
}
