<?php

namespace Modules\Mcp;

class Status
{
    public static function endpointUrl()
    {
        return rtrim((string) config('app.url'), '/').'/mcp';
    }

    public static function serverUrl()
    {
        $url = trim((string) Env::get('MCP_SERVER_URL'));

        return $url !== '' ? rtrim($url, '/') : 'http://127.0.0.1:8081';
    }

    public static function bridgeTokenConfigured()
    {
        return trim((string) Env::get('MCP_BRIDGE_TOKEN')) !== '';
    }

    public static function serverVersion()
    {
        $ch = curl_init(self::serverUrl().'/healthz');
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_TIMEOUT        => 3,
            CURLOPT_CONNECTTIMEOUT => 2,
        ]);
        $response = curl_exec($ch);
        $status = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);

        if ($response === false || $status !== 200) {
            return null;
        }

        $decoded = json_decode($response, true);

        return is_array($decoded) && !empty($decoded['version']) ? (string) $decoded['version'] : 'unknown';
    }
}
