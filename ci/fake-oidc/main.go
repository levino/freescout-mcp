// Command fake-oidc stands in for ZITADEL in the end-to-end test. It signs
// nothing and verifies nothing, so it must never run near a real deployment.
package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	issuer := env("ISSUER", "http://127.0.0.1:8082")
	email := env("EMAIL", "post@levinkeller.de")
	audience := env("AUDIENCE", "freescout-mcp")
	listen := env("LISTEN", ":8082")

	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"end_session_endpoint":   issuer + "/end-session",
		})
	})

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		redirect, err := url.Parse(r.URL.Query().Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		q := redirect.Query()
		q.Set("code", "fake-code")
		q.Set("state", r.URL.Query().Get("state"))
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		// Both clients check that the audience is their own client id, so it
		// follows whoever is asking.
		_ = r.ParseForm()
		aud := r.PostForm.Get("client_id")
		if aud == "" {
			if id, _, ok := r.BasicAuth(); ok {
				aud, _ = url.QueryUnescape(id)
			}
		}
		if aud == "" {
			aud = audience
		}

		claims := map[string]any{
			"sub": "fake-subject", "email": email, "email_verified": true,
			"name": "Test Agent", "iss": issuer, "aud": aud,
			"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		}
		payload, _ := json.Marshal(claims)
		writeJSON(w, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".not-a-signature",
		})
	})

	mux.HandleFunc("/client-metadata.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"client_id":                  issuer + "/client-metadata.json",
			"client_name":                "End-to-end test client",
			"redirect_uris":              []string{"http://localhost/callback", issuer + "/callback"},
			"token_endpoint_auth_method": "none",
			"grant_types":                []string{"authorization_code", "refresh_token"},
		})
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": r.URL.Query().Get("code"), "state": r.URL.Query().Get("state")})
	})

	log.Printf("fake-oidc listening on %s as %s", listen, issuer)
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
