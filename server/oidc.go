package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type oidcProvider struct {
	AuthURL  string `json:"authorization_endpoint"`
	TokenURL string `json:"token_endpoint"`
	Issuer   string `json:"issuer"`
}

type oidcClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	Issuer        string `json:"iss"`
	Audience      any    `json:"aud"`
	Expiry        int64  `json:"exp"`
}

type oidcState struct {
	mu       sync.Mutex
	provider *oidcProvider
}

func (a *App) ensureOIDC() (*oidcProvider, error) {
	a.oidc.mu.Lock()
	defer a.oidc.mu.Unlock()
	if a.oidc.provider != nil {
		return a.oidc.provider, nil
	}

	res, err := a.http.Get(a.cfg.OIDCIssuer + "/.well-known/openid-configuration")
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("oidc discovery: HTTP %d", res.StatusCode)
	}
	var p oidcProvider
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("oidc discovery decode: %w", err)
	}
	if p.AuthURL == "" || p.TokenURL == "" {
		return nil, fmt.Errorf("oidc discovery: missing endpoints")
	}
	a.oidc.provider = &p
	return a.oidc.provider, nil
}

func (a *App) oidcAuthCodeURL(state string) (string, error) {
	p, err := a.ensureOIDC()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(p.AuthURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", a.cfg.OIDCClientID)
	q.Set("redirect_uri", a.cfg.callbackURL())
	q.Set("response_type", "code")
	q.Set("scope", a.cfg.OIDCScopes)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (a *App) oidcExchange(code string) (*oidcClaims, error) {
	p, err := a.ensureOIDC()
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", a.cfg.callbackURL())
	form.Set("client_id", a.cfg.OIDCClientID)

	req, err := http.NewRequest("POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if a.cfg.OIDCClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(a.cfg.OIDCClientID), url.QueryEscape(a.cfg.OIDCClientSecret))
	}

	res, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("oidc token exchange: HTTP %d", res.StatusCode)
	}

	var payload struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("oidc token response: %w", err)
	}
	if payload.IDToken == "" {
		return nil, fmt.Errorf("oidc token response carried no id_token")
	}
	return parseIDToken(payload.IDToken, a.cfg.OIDCIssuer, a.cfg.OIDCClientID)
}

// No signature check: the token came straight from the issuer's token endpoint
// over TLS (OIDC Core 3.1.3.7). Issuer, audience and expiry are still checked,
// so a token minted for someone else cannot pass.
func parseIDToken(token, issuer, audience string) (*oidcClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("id_token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("id_token payload: %w", err)
	}
	var claims oidcClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("id_token payload: %w", err)
	}
	if strings.TrimRight(claims.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("id_token issuer %q unexpected", claims.Issuer)
	}
	if !audienceContains(claims.Audience, audience) {
		return nil, fmt.Errorf("id_token audience does not contain %q", audience)
	}
	if claims.Expiry > 0 && time.Now().After(time.Unix(claims.Expiry, 0)) {
		return nil, fmt.Errorf("id_token expired")
	}
	if claims.Email == "" {
		return nil, fmt.Errorf("id_token carried no email claim")
	}
	if verified, ok := claims.EmailVerified.(bool); ok && !verified {
		return nil, fmt.Errorf("email address is not verified")
	}
	return &claims, nil
}

func audienceContains(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, entry := range v {
			if s, _ := entry.(string); s == want {
				return true
			}
		}
	}
	return false
}
