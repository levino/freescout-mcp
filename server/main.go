package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

const version = "0.1.0"

type App struct {
	cfg    *Config
	http   *http.Client
	bridge *Bridge
	mem    *memory
	cimd   *cimdCache
	oidc   *oidcState

	// Tests point the client_id at an http test server; production never does.
	cimdAllowLocal bool
}

func NewApp(cfg *Config) *App {
	client := &http.Client{Timeout: 20 * time.Second}
	return &App{
		cfg:            cfg,
		cimdAllowLocal: cfg.InsecureAllowLocalClients,
		http:           client,
		bridge:         &Bridge{baseURL: cfg.BridgeURL, token: cfg.BridgeToken, http: client},
		mem:            newMemory(),
		cimd:           newCimdCache(),
		oidc:           &oidcState{},
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.mountOauth(mux)
	a.mountMcp(mux)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": version})
	})

	// Anything else on this host belongs to FreeScout itself, which sits in
	// front of us in the ingress; a request landing here is a misroute.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	return logRequests(mux)
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	app := NewApp(cfg)
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logJSON("info", "freescout-mcp starting", map[string]any{
		"version": version, "listen": cfg.Listen, "resource": cfg.mcpResource(),
	})
	if cfg.InsecureAllowLocalClients {
		logJSON("warn", "INSECURE_ALLOW_LOCAL_CLIENTS is set: client metadata may be fetched from loopback addresses. This belongs in tests only.", nil)
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		// No query string: it carries codes and tokens.
		logJSON("info", "request", map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": rec.status,
			"ms": time.Since(started).Milliseconds(),
		})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func logJSON(level, message string, fields map[string]any) {
	entry := map[string]any{"level": level, "msg": message, "ts": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range fields {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = os.Stdout.Write(append(line, '\n'))
}
