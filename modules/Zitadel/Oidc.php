<?php

namespace Modules\Zitadel;

/**
 * The OpenID Connect bits that are worth testing on their own: reading the
 * provider's discovery document and deciding whether an id_token may be
 * believed. No Laravel, no FreeScout, no network in the checking path.
 */
class Oidc
{
    /**
     * Pick the endpoints out of a discovery document.
     *
     * @throws \RuntimeException
     */
    public static function endpointsFromDiscovery($json)
    {
        $doc = json_decode((string) $json, true);
        if (!is_array($doc)) {
            throw new \RuntimeException('discovery document is not JSON');
        }
        foreach (['issuer', 'authorization_endpoint', 'token_endpoint'] as $key) {
            if (empty($doc[$key]) || !is_string($doc[$key])) {
                throw new \RuntimeException('discovery document has no '.$key);
            }
        }

        return [
            'issuer'    => $doc['issuer'],
            'authorize' => $doc['authorization_endpoint'],
            'token'     => $doc['token_endpoint'],
            'end_session' => isset($doc['end_session_endpoint']) && is_string($doc['end_session_endpoint'])
                ? $doc['end_session_endpoint']
                : '',
        ];
    }

    /**
     * Read the claims of an id_token that was just fetched over TLS from the
     * provider's own token endpoint. On that path the signature adds nothing
     * (OIDC Core 3.1.3.7), but issuer, audience and expiry still have to match,
     * otherwise a token meant for someone else would let its bearer in.
     *
     * @throws \RuntimeException
     */
    public static function claimsFromIdToken($idToken, $issuer, $audience, $now = null)
    {
        $now = $now ?: time();
        $parts = explode('.', (string) $idToken);
        if (count($parts) !== 3) {
            throw new \RuntimeException('id_token is not a JWT');
        }
        $payload = json_decode(self::base64UrlDecode($parts[1]), true);
        if (!is_array($payload)) {
            throw new \RuntimeException('id_token payload is not JSON');
        }

        $tokenIssuer = rtrim((string) ($payload['iss'] ?? ''), '/');
        if ($tokenIssuer === '' || $tokenIssuer !== rtrim((string) $issuer, '/')) {
            throw new \RuntimeException('id_token issuer does not match');
        }
        if (!self::audienceContains($payload['aud'] ?? null, $audience)) {
            throw new \RuntimeException('id_token audience does not match');
        }
        if (isset($payload['exp']) && (int) $payload['exp'] < $now) {
            throw new \RuntimeException('id_token expired');
        }
        if (isset($payload['nbf']) && (int) $payload['nbf'] > $now + 60) {
            throw new \RuntimeException('id_token not valid yet');
        }

        $email = strtolower(trim((string) ($payload['email'] ?? '')));
        if ($email === '') {
            throw new \RuntimeException('id_token carries no email');
        }
        if (array_key_exists('email_verified', $payload) && $payload['email_verified'] === false) {
            throw new \RuntimeException('email address is not verified');
        }

        return [
            'sub'   => (string) ($payload['sub'] ?? ''),
            'email' => $email,
            'name'  => trim((string) ($payload['name'] ?? '')),
        ];
    }

    public static function audienceContains($aud, $expected)
    {
        if (is_string($aud)) {
            return $aud === $expected;
        }
        if (is_array($aud)) {
            return in_array($expected, $aud, true);
        }

        return false;
    }

    /**
     * PKCE S256: the verifier is kept in the session, only its hash travels.
     */
    public static function codeChallenge($verifier)
    {
        return rtrim(strtr(base64_encode(hash('sha256', $verifier, true)), '+/', '-_'), '=');
    }

    public static function randomString($bytes = 32)
    {
        return rtrim(strtr(base64_encode(random_bytes($bytes)), '+/', '-_'), '=');
    }

    public static function base64UrlDecode($value)
    {
        $padded = strtr((string) $value, '-_', '+/');
        $remainder = strlen($padded) % 4;
        if ($remainder) {
            $padded .= str_repeat('=', 4 - $remainder);
        }

        return (string) base64_decode($padded, true);
    }
}
