package main

// OAuth 2.1 authorization server for MCP clients.
//
// The shape of this — Client ID Metadata Documents instead of dynamic
// registration, PKCE S256, loopback redirects matched without the port, the
// RFC 8707 resource check — follows levino/surveys (MIT, © Levin Keller),
// which is the implementation already proven against Claude. See NOTICE.
//
// One deliberate difference: nothing is stored in a database. Pending
// authorizations and one-time codes live in memory for minutes, and the tokens
// themselves are HMAC-signed, so a restart neither loses a connection nor
// needs a volume.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
)

const (
	codeTTLMs     = 5 * 60 * 1000
	accessTTLMs   = 60 * 60 * 1000
	refreshTTLMs  = 30 * 24 * 60 * 60 * 1000
	authzReqTTLMs = 30 * 60 * 1000

	tokenPrefix  = "fs1"
	defaultScope = "mcp"
)

var supportedScopes = []string{"mcp"}

type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) Error() string { return e.code + ": " + e.message }

func newHTTPError(status int, code, message string) *httpError {
	return &httpError{status: status, code: code, message: message}
}

type OAuthClient struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
	fetchedAt    int64
}

type PendingAuthz struct {
	ID                  string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	Email               string // filled in after the upstream login
	ExpiresAt           int64
}

type authCode struct {
	ClientID      string
	Email         string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	ExpiresAt     int64
}

type memory struct {
	mu      sync.Mutex
	pending map[string]*PendingAuthz
	codes   map[string]*authCode
	clients map[string]*OAuthClient
}

func newMemory() *memory {
	return &memory{
		pending: map[string]*PendingAuthz{},
		codes:   map[string]*authCode{},
		clients: map[string]*OAuthClient{},
	}
}

type beginAuthzInput struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}

func (a *App) beginAuthz(in beginAuthzInput) (*PendingAuthz, error) {
	client, err := a.resolveClient(in.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, newHTTPError(400, "invalid_client", "client_id must be an https URL serving a client metadata document")
	}
	if !redirectURIAllowed(client.RedirectURIs, in.RedirectURI) {
		return nil, newHTTPError(400, "invalid_redirect_uri", "redirect_uri not registered")
	}
	if err := a.checkResource(in.Resource); err != nil {
		return nil, err
	}
	if in.ResponseType != "code" {
		return nil, newHTTPError(400, "unsupported_response_type", `only "code" supported`)
	}
	if in.CodeChallenge == "" {
		return nil, newHTTPError(400, "invalid_request", "PKCE code_challenge required")
	}
	if in.CodeChallengeMethod != "S256" {
		return nil, newHTTPError(400, "invalid_request", "only S256 PKCE supported")
	}
	if in.Scope != "" && !scopeIsSupported(in.Scope) {
		return nil, newHTTPError(400, "invalid_scope", "scope not supported")
	}

	p := &PendingAuthz{
		ID:                  genID("auz"),
		ClientID:            in.ClientID,
		RedirectURI:         in.RedirectURI,
		Scope:               in.Scope,
		State:               in.State,
		CodeChallenge:       in.CodeChallenge,
		CodeChallengeMethod: in.CodeChallengeMethod,
		Resource:            in.Resource,
		ExpiresAt:           nowMs() + authzReqTTLMs,
	}

	a.mem.mu.Lock()
	defer a.mem.mu.Unlock()
	a.mem.sweepLocked()
	a.mem.pending[p.ID] = p
	return p, nil
}

func (a *App) loadAuthzRequest(id string) *PendingAuthz {
	a.mem.mu.Lock()
	defer a.mem.mu.Unlock()
	p := a.mem.pending[id]
	if p == nil || p.ExpiresAt < nowMs() {
		delete(a.mem.pending, id)
		return nil
	}
	return p
}

func (a *App) attachUser(id, email string) *PendingAuthz {
	a.mem.mu.Lock()
	defer a.mem.mu.Unlock()
	p := a.mem.pending[id]
	if p == nil || p.ExpiresAt < nowMs() {
		delete(a.mem.pending, id)
		return nil
	}
	p.Email = email
	return p
}

type completedAuthz struct {
	RedirectURI string
	Code        string
	State       string
}

func (a *App) completeAuthz(authzID string) (*completedAuthz, error) {
	a.mem.mu.Lock()
	defer a.mem.mu.Unlock()

	req := a.mem.pending[authzID]
	if req == nil || req.ExpiresAt < nowMs() {
		delete(a.mem.pending, authzID)
		return nil, newHTTPError(400, "invalid_request", "authorization request expired")
	}
	if req.Email == "" {
		return nil, newHTTPError(401, "access_denied", "not authenticated")
	}
	delete(a.mem.pending, authzID)

	code := genID("code")
	a.mem.codes[code] = &authCode{
		ClientID:      req.ClientID,
		Email:         req.Email,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Scope:         req.Scope,
		ExpiresAt:     nowMs() + codeTTLMs,
	}
	return &completedAuthz{RedirectURI: req.RedirectURI, Code: code, State: req.State}, nil
}

func (a *App) denyAuthz(authzID string) *PendingAuthz {
	a.mem.mu.Lock()
	defer a.mem.mu.Unlock()
	p := a.mem.pending[authzID]
	delete(a.mem.pending, authzID)
	return p
}

type IssuedTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type exchangeCodeInput struct {
	ClientID     string
	Code         string
	RedirectURI  string
	CodeVerifier string
	Resource     string
}

func (a *App) exchangeAuthorizationCode(in exchangeCodeInput) (*IssuedTokens, error) {
	client, err := a.resolveClient(in.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, newHTTPError(401, "invalid_client", "unknown client")
	}

	a.mem.mu.Lock()
	code := a.mem.codes[in.Code]
	// One-time by construction: taken out of the map before it is checked, so a
	// replay finds nothing even if the checks below fail.
	delete(a.mem.codes, in.Code)
	a.mem.mu.Unlock()

	if code == nil {
		return nil, newHTTPError(400, "invalid_grant", "unknown code")
	}
	if code.ExpiresAt < nowMs() {
		return nil, newHTTPError(400, "invalid_grant", "code expired")
	}
	if code.ClientID != in.ClientID {
		return nil, newHTTPError(400, "invalid_grant", "code/client mismatch")
	}
	if code.RedirectURI != in.RedirectURI {
		return nil, newHTTPError(400, "invalid_grant", "redirect_uri mismatch")
	}
	if !verifyPKCE(code.CodeChallenge, in.CodeVerifier) {
		return nil, newHTTPError(400, "invalid_grant", "PKCE verification failed")
	}
	if err := a.checkResource(in.Resource); err != nil {
		return nil, err
	}

	return a.mintTokens(in.ClientID, code.Email, code.Scope), nil
}

type exchangeRefreshInput struct {
	ClientID     string
	RefreshToken string
}

func (a *App) exchangeRefreshToken(in exchangeRefreshInput) (*IssuedTokens, error) {
	client, err := a.resolveClient(in.ClientID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, newHTTPError(401, "invalid_client", "unknown client")
	}
	claims := a.verifyToken(in.RefreshToken, "refresh")
	if claims == nil {
		return nil, newHTTPError(400, "invalid_grant", "unknown or expired refresh token")
	}
	if claims.ClientID != in.ClientID {
		return nil, newHTTPError(400, "invalid_grant", "token/client mismatch")
	}

	// Still a FreeScout user? A token outliving the account would be a way in.
	known, err := a.bridge.knownUser(claims.Subject)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, newHTTPError(400, "invalid_grant", "user no longer has access")
	}

	return a.mintTokens(in.ClientID, claims.Subject, claims.Scope), nil
}

type tokenClaims struct {
	Kind     string `json:"k"`
	Subject  string `json:"s"`
	ClientID string `json:"c"`
	Scope    string `json:"sc"`
	Expires  int64  `json:"e"`
	Nonce    string `json:"n"`
}

func (a *App) mintTokens(clientID, email, scope string) *IssuedTokens {
	if scope == "" {
		scope = defaultScope
	}
	now := nowMs()
	return &IssuedTokens{
		AccessToken:  a.signToken(tokenClaims{Kind: "access", Subject: email, ClientID: clientID, Scope: scope, Expires: now + accessTTLMs, Nonce: genID("n")}),
		TokenType:    "Bearer",
		ExpiresIn:    accessTTLMs / 1000,
		RefreshToken: a.signToken(tokenClaims{Kind: "refresh", Subject: email, ClientID: clientID, Scope: scope, Expires: now + refreshTTLMs, Nonce: genID("n")}),
		Scope:        scope,
	}
}

func (a *App) signToken(claims tokenClaims) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	body := tokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(a.sign([]byte(body)))
}

func (a *App) verifyToken(token, kind string) *tokenClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return nil
	}
	want := a.sign([]byte(parts[0] + "." + parts[1]))
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	if claims.Kind != kind || claims.Expires < nowMs() || claims.Subject == "" {
		return nil
	}
	return &claims
}

func (a *App) sign(data []byte) []byte {
	mac := hmac.New(sha256.New, a.cfg.SigningKey)
	mac.Write(data)
	return mac.Sum(nil)
}

func (m *memory) sweepLocked() {
	now := nowMs()
	for id, p := range m.pending {
		if p.ExpiresAt < now {
			delete(m.pending, id)
		}
	}
	for id, c := range m.codes {
		if c.ExpiresAt < now {
			delete(m.codes, id)
		}
	}
}

func verifyPKCE(challenge, verifier string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func scopeIsSupported(scope string) bool {
	for _, s := range strings.Fields(scope) {
		if !contains(supportedScopes, s) {
			return false
		}
	}
	return true
}

// checkResource validates an RFC 8707 resource indicator, if the client sent
// one: tokens minted here are only ever valid for this MCP server.
func (a *App) checkResource(resource string) error {
	if resource == "" {
		return nil
	}
	want, _ := url.Parse(a.cfg.mcpResource())
	got, err := url.Parse(resource)
	if err != nil || got.Fragment != "" ||
		!strings.EqualFold(got.Scheme, want.Scheme) || !strings.EqualFold(got.Host, want.Host) ||
		strings.TrimRight(got.Path, "/") != strings.TrimRight(want.Path, "/") {
		return newHTTPError(400, "invalid_target", "resource must be "+a.cfg.mcpResource())
	}
	return nil
}

func isLoopback(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// redirectURIAllowed: exact match, except for loopback redirects, which native
// clients bind to an ephemeral port at runtime (RFC 8252 §7.3) — there the
// port is ignored.
func redirectURIAllowed(registered []string, presented string) bool {
	if contains(registered, presented) {
		return true
	}
	p, err := url.Parse(presented)
	if err != nil || !isLoopback(p) {
		return false
	}
	for _, r := range registered {
		ru, err := url.Parse(r)
		if err != nil || !isLoopback(ru) {
			continue
		}
		if ru.Hostname() == p.Hostname() && ru.Path == p.Path {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
