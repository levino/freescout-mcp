<?php

use Modules\Zitadel\Oidc;

// A token this server would accept: right issuer, right audience, far future.
$valid = 'e30.eyJpc3MiOiAiaHR0cHM6Ly9pZC5leGFtcGxlIiwgImF1ZCI6ICJmcyIsICJlbWFpbCI6ICJhQGIuZGUiLCAiZXhwIjogNDEwMjQ0NDgwMCwgInN1YiI6ICJ1MSJ9.sig';

function token(array $claims)
{
    $encode = function ($data) {
        return rtrim(strtr(base64_encode(json_encode($data)), '+/', '-_'), '=');
    };

    return 'e30.'.$encode($claims).'.sig';
}

$base = ['iss' => 'https://id.example', 'aud' => 'fs', 'email' => 'a@b.de', 'exp' => 4102444800, 'sub' => 'u1'];

// --- the happy path --------------------------------------------------

$claims = Oidc::claimsFromIdToken($valid, 'https://id.example', 'fs');
check('email is returned', $claims['email'], 'a@b.de');
check('subject is returned', $claims['sub'], 'u1');

check(
    'a trailing slash on the issuer does not matter',
    Oidc::claimsFromIdToken($valid, 'https://id.example/', 'fs')['email'],
    'a@b.de'
);
check(
    'email is lowercased',
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => 'fs', 'email' => 'Post@Levinkeller.DE', 'exp' => 4102444800]), 'https://id.example', 'fs')['email'],
    'post@levinkeller.de'
);
check(
    'an audience list containing us is fine',
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => ['other', 'fs'], 'email' => 'a@b.de', 'exp' => 4102444800]), 'https://id.example', 'fs')['email'],
    'a@b.de'
);

// --- tokens that must not be believed --------------------------------

check_throws('a token from another issuer is refused', function () use ($valid) {
    Oidc::claimsFromIdToken($valid, 'https://id.levinkeller.de', 'fs');
}, 'issuer');

check_throws('a token for another audience is refused', function () use ($valid) {
    Oidc::claimsFromIdToken($valid, 'https://id.example', 'some-other-client');
}, 'audience');

check_throws('an audience list without us is refused', function () use ($base) {
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => ['a', 'b'], 'email' => 'a@b.de', 'exp' => 4102444800]), 'https://id.example', 'fs');
}, 'audience');

check_throws('an expired token is refused', function () {
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => 'fs', 'email' => 'a@b.de', 'exp' => 1000]), 'https://id.example', 'fs');
}, 'expired');

check_throws('a token that is not valid yet is refused', function () {
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => 'fs', 'email' => 'a@b.de', 'exp' => 4102444800, 'nbf' => 4102444800]), 'https://id.example', 'fs');
}, 'not valid yet');

check_throws('a token without an email is refused', function () {
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => 'fs', 'exp' => 4102444800]), 'https://id.example', 'fs');
}, 'email');

check_throws('an unverified email is refused', function () {
    Oidc::claimsFromIdToken(token(['iss' => 'https://id.example', 'aud' => 'fs', 'email' => 'a@b.de', 'email_verified' => false, 'exp' => 4102444800]), 'https://id.example', 'fs');
}, 'not verified');

check_throws('a token without an issuer is refused', function () {
    Oidc::claimsFromIdToken(token(['aud' => 'fs', 'email' => 'a@b.de', 'exp' => 4102444800]), 'https://id.example', 'fs');
}, 'issuer');

check_throws('something that is not a JWT is refused', function () {
    Oidc::claimsFromIdToken('not-a-token', 'https://id.example', 'fs');
}, 'JWT');

check_throws('a JWT with a broken payload is refused', function () {
    Oidc::claimsFromIdToken('e30.###.sig', 'https://id.example', 'fs');
}, 'payload');

// --- discovery -------------------------------------------------------

$endpoints = Oidc::endpointsFromDiscovery(json_encode([
    'issuer'                 => 'https://id.example',
    'authorization_endpoint' => 'https://id.example/oauth/v2/authorize',
    'token_endpoint'         => 'https://id.example/oauth/v2/token',
    'end_session_endpoint'   => 'https://id.example/oidc/v1/end_session',
]));
check('authorize endpoint is read', $endpoints['authorize'], 'https://id.example/oauth/v2/authorize');
check('token endpoint is read', $endpoints['token'], 'https://id.example/oauth/v2/token');
check('end session endpoint is optional but read', $endpoints['end_session'], 'https://id.example/oidc/v1/end_session');
check(
    'a discovery document without end_session still works',
    Oidc::endpointsFromDiscovery(json_encode(['issuer' => 'i', 'authorization_endpoint' => 'a', 'token_endpoint' => 't']))['end_session'],
    ''
);

check_throws('a discovery document without a token endpoint is refused', function () {
    Oidc::endpointsFromDiscovery(json_encode(['issuer' => 'i', 'authorization_endpoint' => 'a']));
}, 'token_endpoint');

check_throws('a discovery response that is not JSON is refused', function () {
    Oidc::endpointsFromDiscovery('<html>gateway timeout</html>');
}, 'not JSON');

// --- PKCE and encoding ------------------------------------------------

check(
    'the code challenge is the base64url sha256 of the verifier',
    Oidc::codeChallenge('test-verifier-0123456789'),
    'W0ydOrGecYPYHAWsrBbwS0ulL-PQuoph-XMZLgSM49Q'
);
check('the challenge carries no padding', strpos(Oidc::codeChallenge('x'), '='), false);
check('random strings differ', Oidc::randomString() === Oidc::randomString(), false);
check('random strings are url safe', (bool) preg_match('/^[A-Za-z0-9_-]+$/', Oidc::randomString()), true);
check('base64url decodes without padding', Oidc::base64UrlDecode('YQ'), 'a');
check('base64url decodes the url alphabet', Oidc::base64UrlDecode('Pz8_'), '???');
