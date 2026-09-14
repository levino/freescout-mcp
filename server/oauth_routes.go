package main

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	pendingCookie = "freescout_mcp_authz"
	pendingTTLSec = 30 * 60
)

func (a *App) mountOauth(mux *http.ServeMux) {
	base := a.cfg.BaseURL

	// Served at the root and at the path-suffixed location, which is the one
	// MCP clients try first (RFC 9728).
	prm := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"resource":                 a.cfg.mcpResource(),
			"authorization_servers":    []string{base},
			"bearer_methods_supported": []string{"header"},
			"scopes_supported":         supportedScopes,
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", prm)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", prm)

	// Two fields make Claude pick CIMD instead of asking for a registration:
	// client_id_metadata_document_supported and "none" among the auth methods.
	asm := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/oauth/authorize",
			"token_endpoint":                        base + "/oauth/token",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"client_id_metadata_document_supported": true,
			"scopes_supported":                      supportedScopes,
		})
	}
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", asm)
	mux.HandleFunc("GET /.well-known/openid-configuration", asm)

	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pending, err := a.beginAuthz(beginAuthzInput{
			ClientID:            q.Get("client_id"),
			RedirectURI:         q.Get("redirect_uri"),
			ResponseType:        q.Get("response_type"),
			Scope:               q.Get("scope"),
			State:               q.Get("state"),
			CodeChallenge:       q.Get("code_challenge"),
			CodeChallengeMethod: q.Get("code_challenge_method"),
			Resource:            q.Get("resource"),
		})
		if err != nil {
			writeAuthzError(w, err)
			return
		}

		target, err := a.oidcAuthCodeURL(pending.ID)
		if err != nil {
			logJSON("error", "oidc discovery failed", map[string]any{"err": err.Error()})
			http.Error(w, "login provider unavailable", http.StatusBadGateway)
			return
		}
		a.setCookie(w, pendingCookie, pending.ID, pendingTTLSec)
		http.Redirect(w, r, target, http.StatusFound)
	})

	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			http.Error(w, "login failed: "+errCode, http.StatusForbidden)
			return
		}
		authzID := q.Get("state")
		if authzID == "" || authzID != cookieValue(r, pendingCookie) {
			http.Error(w, "unknown or mismatched login state", http.StatusBadRequest)
			return
		}
		if a.loadAuthzRequest(authzID) == nil {
			http.Error(w, "authorization expired, please start again", http.StatusBadRequest)
			return
		}

		claims, err := a.oidcExchange(q.Get("code"))
		if err != nil {
			logJSON("warn", "oidc exchange failed", map[string]any{"err": err.Error()})
			http.Error(w, "login failed", http.StatusForbidden)
			return
		}

		email := strings.ToLower(strings.TrimSpace(claims.Email))
		known, err := a.bridge.knownUser(email)
		if err != nil {
			logJSON("error", "user lookup failed", map[string]any{"err": err.Error()})
			http.Error(w, "helpdesk unavailable", http.StatusBadGateway)
			return
		}
		if !known {
			http.Error(w, "There is no active FreeScout user for "+email, http.StatusForbidden)
			return
		}

		pending := a.attachUser(authzID, email)
		if pending == nil {
			http.Error(w, "authorization expired, please start again", http.StatusBadRequest)
			return
		}
		a.renderConsent(w, pending)
	})

	mux.HandleFunc("POST /oauth/approve", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		authzID := r.PostForm.Get("authz_id")
		if authzID == "" || authzID != cookieValue(r, pendingCookie) {
			http.Error(w, "unknown authorization", http.StatusBadRequest)
			return
		}
		a.deleteCookie(w, pendingCookie)

		res, err := a.completeAuthz(authzID)
		if err != nil {
			writeAuthzError(w, err)
			return
		}
		u, err := url.Parse(res.RedirectURI)
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		q := u.Query()
		q.Set("code", res.Code)
		if res.State != "" {
			q.Set("state", res.State)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})

	mux.HandleFunc("POST /oauth/deny", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		authzID := r.PostForm.Get("authz_id")
		if authzID == "" || authzID != cookieValue(r, pendingCookie) {
			http.Error(w, "unknown authorization", http.StatusBadRequest)
			return
		}
		a.deleteCookie(w, pendingCookie)

		req := a.denyAuthz(authzID)
		if req == nil {
			http.Error(w, "authorization expired", http.StatusBadRequest)
			return
		}
		u, err := url.Parse(req.RedirectURI)
		if err != nil {
			http.Error(w, "denied", http.StatusOK)
			return
		}
		q := u.Query()
		q.Set("error", "access_denied")
		if req.State != "" {
			q.Set("state", req.State)
		}
		u.RawQuery = q.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})

	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		form, err := parseTokenForm(r)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid_request", "error_description": err.Error()})
			return
		}
		switch form["grant_type"] {
		case "authorization_code":
			tokens, err := a.exchangeAuthorizationCode(exchangeCodeInput{
				ClientID:     form["client_id"],
				Code:         form["code"],
				RedirectURI:  form["redirect_uri"],
				CodeVerifier: form["code_verifier"],
				Resource:     form["resource"],
			})
			if err != nil {
				writeOAuthError(w, err)
				return
			}
			writeJSON(w, 200, tokens)
		case "refresh_token":
			tokens, err := a.exchangeRefreshToken(exchangeRefreshInput{
				ClientID:     form["client_id"],
				RefreshToken: form["refresh_token"],
			})
			if err != nil {
				writeOAuthError(w, err)
				return
			}
			writeJSON(w, 200, tokens)
		default:
			writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type", "error_description": form["grant_type"]})
		}
	})
}

// The trust anchor shown to the user is the host of the client_id URL: the
// metadata document is self-asserted, so its client_name proves nothing.
func (a *App) renderConsent(w http.ResponseWriter, pending *PendingAuthz) {
	clientURL, _ := url.Parse(pending.ClientID)
	redirectURL, _ := url.Parse(pending.RedirectURI)

	name := pending.ClientID
	if client, _ := a.resolveClient(pending.ClientID); client != nil && client.ClientName != "" {
		name = client.ClientName
	}

	page := `<!doctype html><meta charset="utf-8"><title>Zugriff erlauben</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 body{font:16px/1.55 system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1.25rem;color:#1c1c1c;background:#fbfbfa}
 h1{font-size:1.3rem;margin:0 0 1rem}
 dl{background:#fff;border:1px solid #e4e4e0;border-radius:.5rem;padding:1rem 1.25rem;margin:1.5rem 0}
 dt{font-size:.8rem;text-transform:uppercase;letter-spacing:.04em;color:#6b6b66}
 dd{margin:0 0 .9rem;word-break:break-all}dd:last-child{margin-bottom:0}
 form{display:inline}
 button{font:inherit;padding:.55rem 1.1rem;border-radius:.4rem;border:1px solid #c9c9c3;background:#fff;cursor:pointer}
 button.primary{background:#1c1c1c;color:#fff;border-color:#1c1c1c}
 @media (prefers-color-scheme:dark){body{background:#17171a;color:#eee}dl{background:#202024;border-color:#33333a}
 button{background:#202024;color:#eee;border-color:#44444c}button.primary{background:#eee;color:#17171a}}
</style>
<h1>` + html.EscapeString(name) + ` möchte auf das Postfach zugreifen</h1>
<p>Angemeldet als <strong>` + html.EscapeString(pending.Email) + `</strong>. Der Zugriff gilt für alle Postfächer, die du in FreeScout sehen darfst — lesen, antworten, Notizen, Status und Zuweisung.</p>
<dl>
 <dt>Anwendung</dt><dd>` + html.EscapeString(clientURL.Host) + `</dd>
 <dt>Rückleitung</dt><dd>` + html.EscapeString(redirectURL.Host) + `</dd>
 <dt>Berechtigung</dt><dd>` + html.EscapeString(scopeOrDefault(pending.Scope)) + `</dd>
</dl>
<form method="post" action="/oauth/approve"><input type="hidden" name="authz_id" value="` + html.EscapeString(pending.ID) + `"><button class="primary" type="submit">Erlauben</button></form>
<form method="post" action="/oauth/deny"><input type="hidden" name="authz_id" value="` + html.EscapeString(pending.ID) + `"><button type="submit">Ablehnen</button></form>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}

func scopeOrDefault(scope string) string {
	if strings.TrimSpace(scope) == "" {
		return defaultScope
	}
	return scope
}

func parseTokenForm(r *http.Request) (map[string]string, error) {
	ct := r.Header.Get("Content-Type")
	out := map[string]string{}
	switch {
	case strings.Contains(ct, "application/x-www-form-urlencoded"):
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil, err
		}
		for k := range vals {
			out[k] = vals.Get(k)
		}
	case strings.Contains(ct, "application/json"):
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&out); err != nil {
			return nil, err
		}
	default:
		return nil, newHTTPError(400, "invalid_request", "unsupported content type: "+ct)
	}
	return out, nil
}

func writeAuthzError(w http.ResponseWriter, err error) {
	if he, ok := err.(*httpError); ok {
		http.Error(w, he.code+": "+he.message, he.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeOAuthError(w http.ResponseWriter, err error) {
	if he, ok := err.(*httpError); ok {
		writeJSON(w, he.status, map[string]string{"error": he.code, "error_description": he.message})
		return
	}
	writeJSON(w, 500, map[string]string{"error": "server_error", "error_description": err.Error()})
}
