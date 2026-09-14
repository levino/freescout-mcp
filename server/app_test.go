package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeBridge struct {
	server *httptest.Server
	calls  []bridgeCall
	users  []string
}

type bridgeCall struct {
	Method     string
	Path       string
	Query      url.Values
	ActingUser string
	Token      string
	Body       map[string]any
}

func newFakeBridge(t *testing.T, users ...string) *fakeBridge {
	t.Helper()
	f := &fakeBridge{users: users}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := bridgeCall{
			Method:     r.Method,
			Path:       strings.TrimPrefix(r.URL.Path, "/mcp-bridge/"),
			Query:      r.URL.Query(),
			ActingUser: r.Header.Get("X-Mcp-Acting-User"),
			Token:      r.Header.Get("X-Mcp-Bridge-Token"),
		}
		if r.Method == "POST" {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &call.Body)
		}
		f.calls = append(f.calls, call)

		known := false
		for _, u := range f.users {
			if u == call.ActingUser {
				known = true
			}
		}
		if !known {
			writeJSON(w, 403, map[string]string{"error": "unknown acting user"})
			return
		}
		switch call.Path {
		case "users":
			list := []map[string]any{}
			for _, u := range f.users {
				list = append(list, map[string]any{"id": 1, "email": u})
			}
			writeJSON(w, 200, map[string]any{"users": list})
		case "mailboxes":
			writeJSON(w, 200, map[string]any{"mailboxes": []map[string]any{{"id": 1, "name": "Ökohaus"}}})
		case "conversations":
			writeJSON(w, 200, map[string]any{"total": 1, "conversations": []map[string]any{{"id": 7, "subject": "Dach"}}})
		case "conversations/7/reply":
			writeJSON(w, 200, map[string]any{"ok": true})
		case "conversations/9/reply":
			writeJSON(w, 404, map[string]string{"error": "not found"})
		default:
			writeJSON(w, 404, map[string]string{"error": "no such bridge route"})
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func newFakeOIDC(t *testing.T, email string, clientID string) *httptest.Server {
	t.Helper()
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", 400)
			return
		}
		q := redirect.Query()
		q.Set("code", "upstream-code")
		q.Set("state", r.URL.Query().Get("state"))
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{
			"sub": "user-1", "email": email, "email_verified": true,
			"iss": issuer, "aud": clientID, "exp": time.Now().Add(time.Hour).Unix(),
		}
		payload, _ := json.Marshal(claims)
		token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
		writeJSON(w, 200, map[string]any{"id_token": token})
	})
	server := httptest.NewServer(mux)
	issuer = server.URL
	t.Cleanup(server.Close)
	return server
}

func newFakeClientDoc(t *testing.T, redirectPath string) (*httptest.Server, string) {
	t.Helper()
	var clientID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"client_id":                  clientID,
			"client_name":                "Claude",
			"redirect_uris":              []string{"http://localhost" + redirectPath},
			"token_endpoint_auth_method": "none",
		})
	}))
	clientID = server.URL + "/client-metadata.json"
	t.Cleanup(server.Close)
	return server, clientID
}

func newTestApp(t *testing.T, bridge *fakeBridge, oidc *httptest.Server, clientID string) (*App, *httptest.Server) {
	t.Helper()
	app := NewApp(&Config{
		BaseURL:      "http://127.0.0.1",
		BridgeURL:    bridge.server.URL,
		BridgeToken:  "bridge-secret",
		OIDCIssuer:   oidc.URL,
		OIDCClientID: clientID,
		OIDCScopes:   "openid email",
		SigningKey:   []byte("0123456789abcdef0123456789abcdef"),
		AppName:      "FreeScout",
	})
	app.cimdAllowLocal = true

	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	app.cfg.BaseURL = server.URL
	return app, server
}

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorize(t *testing.T, server *httptest.Server, clientID, redirectURI, verifier string) (string, *http.Client) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{
		Jar:           jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return nil },
	}
	// The client's redirect target must not be followed: it is where the code
	// lands.
	browser.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), redirectURI) {
			return http.ErrUseLastResponse
		}
		if len(via) > 12 {
			return http.ErrUseLastResponse
		}
		return nil
	}

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "mcp")
	q.Set("state", "state-123")
	q.Set("code_challenge", challengeFor(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", server.URL+"/mcp")

	res, err := browser.Get(server.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("expected the consent page, got %d: %s", res.StatusCode, body)
	}
	authzID := betweenMarkers(string(body), `name="authz_id" value="`, `"`)
	if authzID == "" {
		t.Fatalf("no authz_id in consent page: %s", body)
	}

	form := url.Values{}
	form.Set("authz_id", authzID)
	approve, err := browser.PostForm(server.URL+"/oauth/approve", form)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer approve.Body.Close()
	if approve.StatusCode != http.StatusFound {
		raw, _ := io.ReadAll(approve.Body)
		t.Fatalf("expected a redirect with the code, got %d: %s", approve.StatusCode, raw)
	}
	location, err := url.Parse(approve.Header.Get("Location"))
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if got := location.Query().Get("state"); got != "state-123" {
		t.Fatalf("state not echoed: %q", got)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", location)
	}
	return code, browser
}

func exchange(t *testing.T, server *httptest.Server, form url.Values) (*http.Response, map[string]any) {
	t.Helper()
	res, err := http.PostForm(server.URL+"/oauth/token", form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer res.Body.Close()
	var body map[string]any
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &body)
	return res, body
}

func betweenMarkers(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func rpc(t *testing.T, server *httptest.Server, token string, payload map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	defer res.Body.Close()
	var body map[string]any
	rawBody, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(rawBody, &body)
	return res, body
}
