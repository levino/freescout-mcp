package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cimdMinTTL     = time.Minute
	cimdMaxTTL     = 24 * time.Hour
	cimdDefaultTTL = time.Hour
	cimdStaleMax   = 7 * 24 * time.Hour
	cimdMaxBody    = 16 << 10
)

type cimdEntry struct {
	client    *OAuthClient
	expires   time.Time
	fetchedAt time.Time
}

type cimdCache struct {
	mu      sync.Mutex
	entries map[string]cimdEntry
}

func newCimdCache() *cimdCache { return &cimdCache{entries: map[string]cimdEntry{}} }

func isClientIDURL(id string, allowHTTP bool) bool {
	u, err := url.Parse(id)
	if err != nil {
		return false
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return false
	}
	if u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Path != "" && u.Path != "/"
}

func blockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// CGNAT 100.64.0.0/10 (tailnets live here) and IPv6 ULA fc00::/7.
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1]&0xc0 == 64
	}
	return len(ip) == 16 && ip[0]&0xfe == 0xfc
}

func (a *App) cimdHostAllowed(host string) error {
	if a.cimdAllowLocal {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return fmt.Errorf("%s resolves to a non-public address", host)
		}
	}
	return nil
}

type clientMetadataDoc struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (a *App) fetchClientMetadata(clientID string) (*OAuthClient, time.Duration, error) {
	u, _ := url.Parse(clientID)
	if err := a.cimdHostAllowed(u.Hostname()); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest("GET", clientID, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "freescout-mcp-cimd/1")

	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, 0, fmt.Errorf("metadata document: HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, cimdMaxBody+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > cimdMaxBody {
		return nil, 0, errors.New("metadata document too large")
	}

	var doc clientMetadataDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, fmt.Errorf("metadata document is not JSON: %w", err)
	}
	if doc.ClientID != clientID {
		return nil, 0, fmt.Errorf("metadata document client_id %q does not match %q", doc.ClientID, clientID)
	}
	if len(doc.RedirectURIs) == 0 {
		return nil, 0, errors.New("metadata document has no redirect_uris")
	}
	for _, r := range doc.RedirectURIs {
		if err := validateRedirectURI(r); err != nil {
			return nil, 0, err
		}
		ru, _ := url.Parse(r)
		if !isLoopback(ru) && !(ru.Scheme == u.Scheme && strings.EqualFold(ru.Host, u.Host)) {
			return nil, 0, fmt.Errorf("redirect_uri %s is neither same-origin with the client_id nor loopback", r)
		}
	}
	if m := doc.TokenEndpointAuthMethod; m != "" && m != "none" {
		return nil, 0, fmt.Errorf("token_endpoint_auth_method %q not supported (public clients with PKCE only)", m)
	}

	return &OAuthClient{
		ClientID:     clientID,
		ClientName:   strings.TrimSpace(doc.ClientName),
		RedirectURIs: doc.RedirectURIs,
		fetchedAt:    nowMs(),
	}, cacheTTL(res.Header.Get("Cache-Control")), nil
}

func validateRedirectURI(uri string) error {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme == "" {
		return newHTTPError(400, "invalid_redirect_uri", "not a URL: "+uri)
	}
	if parsed.Scheme == "http" {
		h := parsed.Hostname()
		if h != "localhost" && h != "127.0.0.1" {
			return newHTTPError(400, "invalid_redirect_uri", "http redirect URIs must use localhost/127.0.0.1")
		}
	}
	return nil
}

func cacheTTL(cc string) time.Duration {
	ttl := cimdDefaultTTL
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if strings.HasPrefix(part, "max-age=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(part, "max-age=")); err == nil {
				ttl = time.Duration(n) * time.Second
			}
		}
		if part == "no-store" || part == "no-cache" {
			ttl = cimdMinTTL
		}
	}
	if ttl < cimdMinTTL {
		ttl = cimdMinTTL
	}
	if ttl > cimdMaxTTL {
		ttl = cimdMaxTTL
	}
	return ttl
}

// On a failed fetch the last good copy is served for up to cimdStaleMax: a
// refresh token must not die because the metadata host had a bad minute.
func (a *App) resolveClient(clientID string) (*OAuthClient, error) {
	if !isClientIDURL(clientID, a.cimdAllowLocal) {
		return nil, nil
	}

	a.cimd.mu.Lock()
	entry, cached := a.cimd.entries[clientID]
	a.cimd.mu.Unlock()
	if cached && time.Now().Before(entry.expires) {
		return entry.client, nil
	}

	client, ttl, err := a.fetchClientMetadata(clientID)
	if err == nil {
		a.cimd.mu.Lock()
		a.cimd.entries[clientID] = cimdEntry{client: client, expires: time.Now().Add(ttl), fetchedAt: time.Now()}
		a.cimd.mu.Unlock()
		return client, nil
	}

	logJSON("warn", "client metadata fetch failed", map[string]any{"client_id": clientID, "err": err.Error()})
	if cached && time.Since(entry.fetchedAt) < cimdStaleMax {
		a.cimd.mu.Lock()
		entry.expires = time.Now().Add(cimdMinTTL)
		a.cimd.entries[clientID] = entry
		a.cimd.mu.Unlock()
		return entry.client, nil
	}
	return nil, nil
}
