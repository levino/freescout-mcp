<?php

namespace Modules\Zitadel\Http\Controllers;

use App\User;
use Illuminate\Http\Request;
use Illuminate\Routing\Controller;
use Illuminate\Support\Facades\Auth;
use Modules\Zitadel\Env;
use Modules\Zitadel\Oidc;

class LoginController extends Controller
{
    const SESSION_STATE    = 'zitadel.state';
    const SESSION_VERIFIER = 'zitadel.verifier';

    public function start(Request $request)
    {
        if (Auth::check()) {
            return redirect('/');
        }
        if (!$this->configured()) {
            return $this->fail('Die ZITADEL-Anmeldung ist nicht konfiguriert.');
        }

        try {
            $endpoints = $this->endpoints();
        } catch (\Exception $e) {
            \Log::error('[zitadel] discovery failed: '.$e->getMessage());

            return $this->fail('Der Anmeldedienst ist gerade nicht erreichbar.');
        }

        $state = Oidc::randomString();
        $verifier = Oidc::randomString();
        $request->session()->put(self::SESSION_STATE, $state);
        $request->session()->put(self::SESSION_VERIFIER, $verifier);

        $query = http_build_query([
            'client_id'             => Env::get('ZITADEL_CLIENT_ID'),
            'redirect_uri'          => $this->callbackUrl(),
            'response_type'         => 'code',
            'scope'                 => Env::get('ZITADEL_SCOPES', 'openid profile email'),
            'state'                 => $state,
            'code_challenge'        => Oidc::codeChallenge($verifier),
            'code_challenge_method' => 'S256',
        ]);

        return redirect($endpoints['authorize'].'?'.$query);
    }

    public function callback(Request $request)
    {
        if ($error = $request->input('error')) {
            return $this->fail('Die Anmeldung wurde abgebrochen ('.e($error).').');
        }

        $expected = $request->session()->pull(self::SESSION_STATE);
        $verifier = $request->session()->pull(self::SESSION_VERIFIER);
        if (!$expected || !hash_equals($expected, (string) $request->input('state'))) {
            return $this->fail('Die Anmeldung konnte nicht zugeordnet werden. Bitte noch einmal versuchen.');
        }

        try {
            $endpoints = $this->endpoints();
            $idToken = $this->exchangeCode($endpoints['token'], (string) $request->input('code'), (string) $verifier);
            $claims = Oidc::claimsFromIdToken($idToken, $endpoints['issuer'], Env::get('ZITADEL_CLIENT_ID'));
        } catch (\Exception $e) {
            \Log::error('[zitadel] login failed: '.$e->getMessage());

            return $this->fail('Die Anmeldung ist fehlgeschlagen.');
        }

        $user = User::where('email', $claims['email'])->first();
        if (!$user || !$user->isActive()) {
            // No auto-provisioning on purpose: who gets into the helpdesk stays
            // a decision made in the helpdesk.
            return $this->fail('Für '.e($claims['email']).' gibt es hier kein aktives Benutzerkonto.');
        }

        Auth::login($user, true);
        $request->session()->regenerate();

        return redirect()->intended('/');
    }

    // -- helpers ---------------------------------------------------------

    protected function configured()
    {
        return Env::get('ZITADEL_ISSUER') && Env::get('ZITADEL_CLIENT_ID');
    }

    protected function callbackUrl()
    {
        return rtrim(config('app.url'), '/').'/zitadel/callback';
    }

    protected function endpoints()
    {
        $issuer = rtrim((string) Env::get('ZITADEL_ISSUER'), '/');
        $cacheKey = 'zitadel.discovery.'.md5($issuer);

        $cached = \Cache::get($cacheKey);
        if ($cached) {
            return $cached;
        }

        $body = $this->httpGet($issuer.'/.well-known/openid-configuration');
        $endpoints = Oidc::endpointsFromDiscovery($body);
        \Cache::put($cacheKey, $endpoints, 60);

        return $endpoints;
    }

    protected function exchangeCode($tokenEndpoint, $code, $verifier)
    {
        $form = http_build_query([
            'grant_type'    => 'authorization_code',
            'code'          => $code,
            'redirect_uri'  => $this->callbackUrl(),
            'client_id'     => Env::get('ZITADEL_CLIENT_ID'),
            'code_verifier' => $verifier,
        ]);

        $headers = ['Content-Type: application/x-www-form-urlencoded', 'Accept: application/json'];
        if ($secret = Env::get('ZITADEL_CLIENT_SECRET')) {
            $headers[] = 'Authorization: Basic '.base64_encode(
                rawurlencode(Env::get('ZITADEL_CLIENT_ID')).':'.rawurlencode($secret)
            );
        }

        $response = $this->httpPost($tokenEndpoint, $form, $headers);
        $decoded = json_decode($response, true);
        if (!is_array($decoded) || empty($decoded['id_token'])) {
            throw new \RuntimeException('token endpoint returned no id_token');
        }

        return $decoded['id_token'];
    }

    protected function httpGet($url)
    {
        return $this->request($url, null, ['Accept: application/json']);
    }

    protected function httpPost($url, $body, array $headers)
    {
        return $this->request($url, $body, $headers);
    }

    protected function request($url, $body, array $headers)
    {
        $ch = curl_init($url);
        curl_setopt_array($ch, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_TIMEOUT        => 15,
            CURLOPT_HTTPHEADER     => $headers,
            CURLOPT_FOLLOWLOCATION => false,
        ]);
        if ($body !== null) {
            curl_setopt($ch, CURLOPT_POST, true);
            curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
        }
        $response = curl_exec($ch);
        $status = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
        $error = curl_error($ch);
        // No curl_close(): a no-op since PHP 8.0 and deprecated in 8.5, and
        // FreeScout turns deprecations into exceptions.

        if ($response === false) {
            throw new \RuntimeException('request to '.$url.' failed: '.$error);
        }
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException('request to '.$url.' returned HTTP '.$status);
        }

        return $response;
    }

    protected function fail($message)
    {
        return redirect('/login?local=1')->with('flash_error_floating', $message);
    }
}
