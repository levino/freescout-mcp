package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// The REST API: the same operations as the MCP tools (ops.go), for clients
// that can run curl. MCP stays the way in — the token tool hands out a
// one-hour bearer token for this API — and everything heavy goes here: files
// as multipart instead of base64 in a tool call, attachments as plain bytes,
// many conversations without a model reading every answer.
//
// It lives under /mcp so the ingress rule that already sends /mcp to the
// sidecar covers it; /api belongs to FreeScout.
const apiBase = "/mcp/api/v1"

const apiTokenTTLMs = 60 * 60 * 1000

func (a *App) mintAPIToken(session *tokenClaims) (string, int64) {
	scope := session.Scope
	if scope == "" {
		scope = defaultScope
	}
	expires := nowMs() + apiTokenTTLMs
	return a.signToken(tokenClaims{Kind: "api", Subject: session.Subject, ClientID: session.ClientID, Scope: scope, Expires: expires, Nonce: genID("n")}), expires
}

func (a *App) tokenTool(session *tokenClaims) map[string]any {
	token, _ := a.mintAPIToken(session)
	out, _ := json.Marshal(map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_in": apiTokenTTLMs / 1000,
		"base_url":   a.cfg.BaseURL + apiBase,
		"openapi":    a.cfg.BaseURL + apiBase + "/openapi.json",
		"usage":      `curl -H "Authorization: Bearer $TOKEN" ` + a.cfg.BaseURL + apiBase + "/mailboxes",
	})
	return textResult(string(out))
}

func (a *App) apiGuide() string {
	base := a.cfg.BaseURL + apiBase
	return "FreeScout REST API, base " + base + "\n" +
		"Auth: Authorization: Bearer <token from the token tool>, valid 1 h, acts as you.\n" +
		"OpenAPI 3: " + base + "/openapi.json\n\n" +
		"GET    /mailboxes\n" +
		"GET    /conversations?query=&status=&mailbox_id=&after=&before=&from=&to=&state=&limit=&offset=&fields=a,b\n" +
		"GET    /conversations/{id}?fields=&last_threads=\n" +
		"POST   /conversations/{id}/drafts      reply draft (JSON or multipart), never sends\n" +
		"POST   /conversations/{id}/reply       SENDS a reply\n" +
		"POST   /conversations/{id}/forward     forward draft; send=true SENDS\n" +
		"POST   /conversations/{id}/notes|status|assign|move|merge\n" +
		"POST   /drafts                         new-conversation draft, never sends\n" +
		"PATCH  /drafts/{id}   DELETE /drafts/{id}   POST /drafts/{id}/send (SENDS)\n" +
		"GET    /customers?query=\n" +
		"GET    /attachments/{id}               the file itself\n" +
		"POST   /uploads                        multipart; answers upload_id, usable for 1 h\n\n" +
		"Write routes take the same fields as the MCP tools, as JSON or as multipart form fields; " +
		"every file part of a multipart request becomes an attachment (20 MB together).\n\n" +
		"Examples:\n" +
		"curl -H \"Authorization: Bearer $TOKEN\" '" + base + "/conversations?query=Angebot&fields=subject,status'\n" +
		"curl -H \"Authorization: Bearer $TOKEN\" -F mailbox_id=1 -F to=kunde@example.org -F subject='Ihr Angebot' \\\n" +
		"     -F body='Guten Tag, anbei das Angebot.' -F file=@angebot.pdf " + base + "/drafts\n" +
		"curl -H \"Authorization: Bearer $TOKEN\" -F body='Anbei die Pläne.' -F file=@plan.pdf " + base + "/conversations/123/drafts\n" +
		"curl -H \"Authorization: Bearer $TOKEN\" -o datei.pdf " + base + "/attachments/456\n"
}

func (a *App) mountAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiBase+"/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		writeJSON(w, 200, a.openAPI())
	})

	route := func(pattern string, handler func(w http.ResponseWriter, r *http.Request, email string)) {
		method, path, _ := strings.Cut(pattern, " ")
		a.apiRoutes = append(a.apiRoutes, method+" "+path)
		mux.HandleFunc(method+" "+apiBase+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			session := a.bearerClaims(r, "api")
			if session == nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="freescout-api", error="invalid_token"`)
				apiError(w, 401, "missing, invalid or expired token; get one with the MCP tool token")
				return
			}
			handler(w, r, session.Subject)
		})
	}
	// op wires a route to an operation: arguments from the query (GET) or the
	// body, path parameters on top so the body cannot override them.
	op := func(pattern, name string, pathParam string, created bool) {
		route(pattern, func(w http.ResponseWriter, r *http.Request, email string) {
			args, files, err := requestArgs(r)
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) || errors.Is(err, errTooLarge) {
					apiError(w, 413, fmt.Sprintf("request larger than %d MB of attachments", maxDraftAttachmentBytes>>20))
					return
				}
				apiError(w, 400, err.Error())
				return
			}
			if pathParam != "" {
				id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
				if err != nil || id <= 0 {
					apiError(w, 404, "not found")
					return
				}
				args[pathParam] = id
			}
			raw, _ := json.Marshal(args)
			payload, err := a.operation(name, raw, files, email)
			status := 200
			if created {
				status = 201
			}
			a.writeOpResult(w, status, payload, err)
		})
	}

	op("GET /mailboxes", "list_mailboxes", "", false)
	op("GET /conversations", "search_conversations", "", false)
	op("GET /conversations/{id}", "get_conversation", "id", false)
	op("POST /conversations/{id}/notes", "add_note", "id", true)
	op("POST /conversations/{id}/status", "set_status", "id", false)
	op("POST /conversations/{id}/assign", "assign_conversation", "id", false)
	op("POST /conversations/{id}/move", "move_conversation", "id", false)
	op("POST /conversations/{id}/merge", "merge_conversations", "id", false)
	op("POST /conversations/{id}/drafts", "create_draft", "id", true)
	op("POST /conversations/{id}/reply", "reply_to_conversation", "id", true)
	op("POST /conversations/{id}/forward", "forward_conversation", "id", true)
	op("POST /drafts", "create_new_draft", "", true)
	op("PATCH /drafts/{id}", "update_draft", "draft_id", false)
	op("DELETE /drafts/{id}", "delete_draft", "draft_id", false)
	op("POST /drafts/{id}/send", "send_draft", "draft_id", false)
	op("GET /customers", "find_customers", "", false)

	route("GET /me", func(w http.ResponseWriter, r *http.Request, email string) {
		writeJSON(w, 200, map[string]any{"email": email})
	})

	route("GET /attachments/{id}", func(w http.ResponseWriter, r *http.Request, email string) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			apiError(w, 404, "not found")
			return
		}
		payload, err := a.operation("get_attachment", json.RawMessage(fmt.Sprintf(`{"id":%d}`, id)), nil, email)
		if err != nil {
			a.writeOpResult(w, 200, nil, err)
			return
		}
		attachment, _ := payload["attachment"].(map[string]any)
		encoded, _ := attachment["base64"].(string)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if attachment == nil || err != nil {
			apiError(w, 502, "the helpdesk returned no usable attachment")
			return
		}
		name, _ := attachment["name"].(string)
		mimeType, _ := attachment["mime_type"].(string)
		w.Header().Set("Content-Type", mimeOrDefault(mimeType))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		_, _ = w.Write(data)
	})

	route("POST /uploads", func(w http.ResponseWriter, r *http.Request, email string) {
		if !isMultipart(r) {
			apiError(w, 400, "send the files as multipart/form-data, e.g. curl -F file=@angebot.pdf")
			return
		}
		_, files, err := requestArgs(r)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) || errors.Is(err, errTooLarge) {
				apiError(w, 413, fmt.Sprintf("larger than %d MB", maxDraftAttachmentBytes>>20))
				return
			}
			apiError(w, 400, err.Error())
			return
		}
		if len(files) == 0 {
			apiError(w, 400, "no file in the request")
			return
		}
		var stored []*upload
		for _, f := range files {
			if len(f.Data) == 0 {
				apiError(w, 400, f.Name+" is empty")
				return
			}
			u, err := a.uploads.put(email, f.Name, f.MimeType, f.Data)
			if err != nil {
				for _, done := range stored {
					a.uploads.remove(email, done.ID)
				}
				if errors.Is(err, errUploadLimit) {
					apiError(w, 429, err.Error())
					return
				}
				logJSON("error", "storing upload failed", map[string]any{"err": err.Error()})
				apiError(w, 500, "could not store the file")
				return
			}
			stored = append(stored, u)
		}
		writeJSON(w, 201, map[string]any{"uploads": stored})
	})

	route("DELETE /uploads/{id}", func(w http.ResponseWriter, r *http.Request, email string) {
		if !a.uploads.remove(email, r.PathValue("id")) {
			apiError(w, 404, "upload not found")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc(apiBase+"/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 404, "no such endpoint; see "+apiBase+"/openapi.json")
	})
}

func apiError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// writeOpResult maps an operation's outcome to HTTP: the caller's mistakes
// are 400, what FreeScout refused keeps FreeScout's status (403, 404, 422),
// and a helpdesk that does not answer is 502.
func (a *App) writeOpResult(w http.ResponseWriter, status int, payload map[string]any, err error) {
	if err == nil {
		writeJSON(w, status, payload)
		return
	}
	var ie *inputError
	if errors.As(err, &ie) {
		apiError(w, 400, ie.msg)
		return
	}
	var be *bridgeError
	if asBridgeError(err, &be) && be.Status >= 400 && be.Status < 500 && be.Status != 401 {
		apiError(w, be.Status, be.Message)
		return
	}
	logJSON("error", "bridge call failed", map[string]any{"err": err.Error()})
	apiError(w, 502, "the helpdesk did not answer")
}

var errTooLarge = errors.New("too large")

func isMultipart(r *http.Request) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mediaType == "multipart/form-data"
}

// Form fields that are text even when they look like JSON ("123" as a body).
var textFields = map[string]bool{
	"body": true, "subject": true, "status": true, "query": true, "assignee_email": true,
	"after": true, "before": true, "from": true, "state": true,
}

// requestArgs collects the arguments of a REST call: the query string, then a
// JSON body or multipart form fields on top. A multipart field "payload" may
// hold all of them as one JSON object; every file part is an attachment.
func requestArgs(r *http.Request) (map[string]any, []draftFile, error) {
	args := map[string]any{}
	for key, values := range r.URL.Query() {
		args[key] = coerceField(key, values)
	}
	if r.Method == "GET" || r.Method == "DELETE" && r.ContentLength <= 0 {
		return args, nil, nil
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxRPCBody)

	if !isMultipart(r) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, nil, err
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return args, nil, nil
		}
		body := map[string]any{}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, nil, errors.New("body must be a JSON object or multipart/form-data")
		}
		for k, v := range body {
			args[k] = v
		}
		return args, nil, nil
	}

	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, errors.New("unreadable multipart body")
	}
	var files []draftFile
	total := 0
	fields := map[string][]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return nil, nil, err
			}
			return nil, nil, errors.New("unreadable multipart body")
		}
		if part.FileName() != "" {
			data, err := io.ReadAll(io.LimitReader(part, int64(maxDraftAttachmentBytes-total)+1))
			if err != nil {
				return nil, nil, err
			}
			total += len(data)
			if total > maxDraftAttachmentBytes {
				return nil, nil, errTooLarge
			}
			files = append(files, draftFile{Name: part.FileName(), MimeType: partMimeType(part.Header.Get("Content-Type")), Data: data})
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, 1<<20))
		if err != nil {
			return nil, nil, err
		}
		fields[part.FormName()] = append(fields[part.FormName()], string(value))
	}
	if payload, ok := fields["payload"]; ok {
		body := map[string]any{}
		if err := json.Unmarshal([]byte(payload[0]), &body); err != nil {
			return nil, nil, errors.New("the payload field must be a JSON object")
		}
		for k, v := range body {
			args[k] = v
		}
		delete(fields, "payload")
	}
	for key, values := range fields {
		args[key] = coerceField(key, values)
	}
	return args, files, nil
}

// coerceField turns form or query values into JSON values: numbers, booleans
// and lists where they parse as such, text otherwise. A repeated field is a
// list (-F cc=a@x -F cc=b@y).
func coerceField(key string, values []string) any {
	one := func(v string) any {
		if textFields[key] {
			return v
		}
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			switch parsed.(type) {
			case float64, bool, []any:
				return parsed
			}
		}
		return v
	}
	if len(values) == 1 {
		return one(values[0])
	}
	list := make([]any, 0, len(values))
	for _, v := range values {
		list = append(list, one(v))
	}
	return list
}

// curl labels every file it does not know as application/octet-stream; that
// says nothing, and FreeScout sniffs the bytes when no type is given.
func partMimeType(header string) string {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || mediaType == "application/octet-stream" {
		return ""
	}
	return mediaType
}
