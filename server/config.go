package main

import (
	"errors"
	"os"
	"strings"
)

// Config is env-only; there is no config file.
type Config struct {
	// BaseURL is the public origin of this server, e.g.
	// https://shared-inbox.levinkeller.de. Everything the OAuth discovery
	// documents advertise is derived from it.
	BaseURL string
	Listen  string

	// FreeScout side: the module in this pod.
	BridgeURL   string
	BridgeToken string

	// Upstream login (ZITADEL).
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCScopes       string

	// HMAC key for the tokens we mint. Without it nothing starts: tokens would
	// be forgeable.
	SigningKey []byte

	AppName string

	// Test-only: allows http client metadata documents on loopback addresses,
	// which the end-to-end harness serves. Never set this in production — it
	// is what stops a client_id from pointing into the cluster's own network.
	InsecureAllowLocalClients bool
}

func LoadConfig() (*Config, error) {
	c := &Config{
		BaseURL:          strings.TrimRight(env("BASE_URL", ""), "/"),
		Listen:           env("LISTEN", ":8080"),
		BridgeURL:        strings.TrimRight(env("BRIDGE_URL", "http://127.0.0.1"), "/"),
		BridgeToken:      env("BRIDGE_TOKEN", ""),
		OIDCIssuer:       strings.TrimRight(env("OIDC_ISSUER", ""), "/"),
		OIDCClientID:     env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("OIDC_CLIENT_SECRET", ""),
		OIDCScopes:       env("OIDC_SCOPES", "openid profile email"),
		SigningKey:       []byte(env("TOKEN_SIGNING_KEY", "")),
		AppName:          env("APP_NAME", "FreeScout"),

		InsecureAllowLocalClients: env("INSECURE_ALLOW_LOCAL_CLIENTS", "") == "true",
	}

	var missing []string
	for name, value := range map[string]string{
		"BASE_URL":          c.BaseURL,
		"BRIDGE_TOKEN":      c.BridgeToken,
		"OIDC_ISSUER":       c.OIDCIssuer,
		"OIDC_CLIENT_ID":    c.OIDCClientID,
		"TOKEN_SIGNING_KEY": string(c.SigningKey),
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, errors.New("missing environment variables: " + strings.Join(missing, ", "))
	}
	if len(c.SigningKey) < 32 {
		return nil, errors.New("TOKEN_SIGNING_KEY must be at least 32 bytes")
	}
	if !strings.HasPrefix(c.BaseURL, "https://") && !strings.HasPrefix(c.BaseURL, "http://127.0.0.1") {
		return nil, errors.New("BASE_URL must be https (or http://127.0.0.1 for tests)")
	}

	return c, nil
}

func (c *Config) callbackURL() string { return c.BaseURL + "/oauth/callback" }

// mcpResource is the canonical resource URI of this MCP server (RFC 8707).
func (c *Config) mcpResource() string { return c.BaseURL + "/mcp" }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
