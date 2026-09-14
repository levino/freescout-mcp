package main

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const agent = "post@levinkeller.de"

func setup(t *testing.T) (*App, *httptest.Server, *fakeBridge, string, string) {
	t.Helper()
	bridge := newFakeBridge(t, agent)
	_, clientID := newFakeClientDoc(t, "/callback")
	oidc := newFakeOIDC(t, agent, "freescout-mcp")
	app, server := newTestApp(t, bridge, oidc, "freescout-mcp")
	return app, server, bridge, clientID, "http://localhost/callback"
}

func TestFullFlowIssuesUsableToken(t *testing.T) {
	_, server, bridge, clientID, redirect := setup(t)

	verifier := "verifier-with-enough-entropy-0123456789"
	code, _ := authorize(t, server, clientID, redirect, verifier)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)
	form.Set("resource", server.URL+"/mcp")

	res, body := exchange(t, server, form)
	if res.StatusCode != 200 {
		t.Fatalf("token exchange failed: %d %v", res.StatusCode, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("no tokens: %v", body)
	}

	// The token works on the MCP endpoint and reaches the bridge as the user
	// who logged in.
	_, rpcBody := rpc(t, server, access, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_mailboxes", "arguments": map[string]any{}},
	})
	result, _ := rpcBody["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("tool call failed: %v", rpcBody)
	}
	last := bridge.calls[len(bridge.calls)-1]
	if last.ActingUser != agent {
		t.Fatalf("bridge called as %q, want %q", last.ActingUser, agent)
	}
	if last.Token != "bridge-secret" {
		t.Fatalf("bridge token not sent")
	}

	// Refreshing keeps working.
	refreshForm := url.Values{}
	refreshForm.Set("grant_type", "refresh_token")
	refreshForm.Set("client_id", clientID)
	refreshForm.Set("refresh_token", refresh)
	res, body = exchange(t, server, refreshForm)
	if res.StatusCode != 200 || body["access_token"] == "" {
		t.Fatalf("refresh failed: %d %v", res.StatusCode, body)
	}
}

func TestCodeCannotBeReplayed(t *testing.T) {
	_, server, _, clientID, redirect := setup(t)
	verifier := "verifier-with-enough-entropy-0123456789"
	code, _ := authorize(t, server, clientID, redirect, verifier)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", verifier)

	if res, body := exchange(t, server, form); res.StatusCode != 200 {
		t.Fatalf("first exchange failed: %d %v", res.StatusCode, body)
	}
	res, body := exchange(t, server, form)
	if res.StatusCode == 200 {
		t.Fatalf("second exchange succeeded, code was replayable: %v", body)
	}
	if body["error"] != "invalid_grant" {
		t.Fatalf("want invalid_grant, got %v", body)
	}
}

func TestWrongPKCEVerifierIsRejected(t *testing.T) {
	_, server, _, clientID, redirect := setup(t)
	code, _ := authorize(t, server, clientID, redirect, "the-real-verifier-0123456789")

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("code_verifier", "a-different-verifier-9876543210")

	res, body := exchange(t, server, form)
	if res.StatusCode == 200 {
		t.Fatalf("PKCE was not enforced: %v", body)
	}
}

func TestResourceMustMatchThisServer(t *testing.T) {
	_, server, _, clientID, redirect := setup(t)

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("code_challenge", challengeFor("verifier-0123456789"))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", "https://someone-elses-server.example/mcp")

	res, err := http.Get(server.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("foreign resource accepted: %d", res.StatusCode)
	}
}

func TestLoginWithoutFreeScoutAccountIsRefused(t *testing.T) {
	bridge := newFakeBridge(t, "someone-else@example.org")
	_, clientID := newFakeClientDoc(t, "/callback")
	oidc := newFakeOIDC(t, "stranger@example.org", "freescout-mcp")
	_, server := newTestApp(t, bridge, oidc, "freescout-mcp")

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), "http://localhost/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}}

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", "http://localhost/callback")
	q.Set("response_type", "code")
	q.Set("code_challenge", challengeFor("verifier-0123456789"))
	q.Set("code_challenge_method", "S256")

	res, err := browser.Get(server.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a stranger got through: %d", res.StatusCode)
	}
}

func TestMcpWithoutTokenAdvertisesTheAuthorizationServer(t *testing.T) {
	_, server, _, _, _ := setup(t)

	res, _ := rpc(t, server, "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if res.StatusCode != 401 {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
	challenge := res.Header.Get("WWW-Authenticate")
	for _, want := range []string{`error="invalid_token"`, "oauth-protected-resource/mcp", `scope="mcp"`} {
		if !strings.Contains(challenge, want) {
			t.Fatalf("challenge %q lacks %q", challenge, want)
		}
	}
}

func TestTamperedTokenIsRejected(t *testing.T) {
	app, server, _, _, _ := setup(t)

	tokens := app.mintTokens("https://client.example/meta.json", agent, "mcp")
	parts := strings.Split(tokens.AccessToken, ".")
	forged := parts[0] + "." + parts[1] + ".AAAA"

	if res, _ := rpc(t, server, forged, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); res.StatusCode != 401 {
		t.Fatalf("forged signature accepted: %d", res.StatusCode)
	}
	// A refresh token must not work as an access token.
	if res, _ := rpc(t, server, tokens.RefreshToken, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); res.StatusCode != 401 {
		t.Fatalf("refresh token accepted at the MCP endpoint: %d", res.StatusCode)
	}
}

func TestDiscoveryDocumentsCarryTheClaudeFlags(t *testing.T) {
	_, server, _, _, _ := setup(t)

	res, err := http.Get(server.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(res.Body).Decode(&doc)

	if doc["client_id_metadata_document_supported"] != true {
		t.Fatalf("CIMD flag missing: %v", doc)
	}
	methods, _ := doc["token_endpoint_auth_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "none" {
		t.Fatalf("public-client auth method missing: %v", doc)
	}

	res2, err := http.Get(server.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var prm map[string]any
	_ = json.NewDecoder(res2.Body).Decode(&prm)
	if prm["resource"] != server.URL+"/mcp" {
		t.Fatalf("resource wrong: %v", prm)
	}
}
