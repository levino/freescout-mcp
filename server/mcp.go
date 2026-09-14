package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
)

const mcpProtocolVersion = "2025-06-18"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func jsonRPCResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "result": result}
}

func jsonRPCError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "error": map[string]any{"code": code, "message": message}}
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

var bearerRe = regexp.MustCompile(`^Bearer\s+(\S+)$`)

// The 401 plus this header is the whole protocol signal that makes Claude show
// its connect card and start OAuth; scope tells it what to ask for.
func (a *App) wwwAuthenticate(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate",
		`Bearer error="invalid_token", error_description="Authentication required", `+
			`resource_metadata="`+a.cfg.BaseURL+`/.well-known/oauth-protected-resource/mcp", `+
			`scope="`+defaultScope+`"`)
}

func (a *App) mountMcp(mux *http.ServeMux) {
	mux.HandleFunc("OPTIONS /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.bearerUser(r); !ok {
			a.wwwAuthenticate(w)
			http.Error(w, "missing or invalid token", 401)
			return
		}
		// No server-initiated stream: everything this server does is a reply.
		http.Error(w, "Method Not Allowed", 405)
	})

	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		email, ok := a.bearerUser(r)
		if !ok {
			a.wwwAuthenticate(w)
			writeJSON(w, 401, jsonRPCError(nil, -32001, "missing or invalid token"))
			return
		}

		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var req jsonRPCRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, 400, jsonRPCError(nil, -32700, "parse error"))
			return
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			writeJSON(w, 200, jsonRPCError(req.ID, -32600, "invalid request"))
			return
		}

		result, rpcErr := a.dispatchRPC(&req, email)
		if rpcErr != nil {
			writeJSON(w, 200, jsonRPCError(req.ID, rpcErr.code, rpcErr.message))
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, 200, jsonRPCResult(req.ID, result))
	})
}

type rpcError struct {
	code    int
	message string
}

func (a *App) dispatchRPC(req *jsonRPCRequest, email string) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "freescout", "version": version},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Name == "" {
			return nil, &rpcError{-32602, "tools/call requires name"}
		}
		return a.dispatchTool(p.Name, p.Arguments, email), nil
	default:
		return nil, &rpcError{-32601, "unknown method: " + req.Method}
	}
}

func (a *App) bearerUser(r *http.Request) (string, bool) {
	m := bearerRe.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil {
		return "", false
	}
	claims := a.verifyToken(m[1], "access")
	if claims == nil {
		return "", false
	}
	return claims.Subject, true
}
