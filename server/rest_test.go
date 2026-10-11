package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func apiToken(t *testing.T, app *App, email string) string {
	t.Helper()
	result := app.dispatchToolFor("token", nil, &tokenClaims{Subject: email, ClientID: "client", Scope: "mcp"})
	var out struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal([]byte(toolText(t, result)), &out); err != nil || out.Token == "" {
		t.Fatalf("token tool: %s", toolText(t, result))
	}
	if !strings.HasSuffix(out.BaseURL, "/mcp/api/v1") {
		t.Fatalf("base_url %q", out.BaseURL)
	}
	return out.Token
}

func apiCall(t *testing.T, server *httptest.Server, method, path, token string, body io.Reader, contentType string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, server.URL+"/mcp/api/v1"+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res, raw
}

type formPart struct {
	name, filename string
	data           []byte
}

func multipartBody(t *testing.T, parts ...formPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename != "" {
			fw, _ := w.CreateFormFile(p.name, p.filename)
			fw.Write(p.data)
		} else {
			w.WriteField(p.name, string(p.data))
		}
	}
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestRestNeedsAnAPITokenAndOnlyThat(t *testing.T) {
	app, server, _, _, _ := setup(t)

	if res, _ := apiCall(t, server, "GET", "/me", "", nil, ""); res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: HTTP %d", res.StatusCode)
	}
	// The MCP access token does not open the REST API, and the other way round.
	access := app.mintTokens("client", agent, "mcp").AccessToken
	if res, _ := apiCall(t, server, "GET", "/me", access, nil, ""); res.StatusCode != 401 {
		t.Fatalf("an MCP access token was accepted by REST: HTTP %d", res.StatusCode)
	}
	token := apiToken(t, app, agent)
	if res, _ := rpc(t, server, token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); res.StatusCode != 401 {
		t.Fatalf("a REST token was accepted by /mcp: HTTP %d", res.StatusCode)
	}

	expired := app.signToken(tokenClaims{Kind: "api", Subject: agent, Expires: nowMs() - 1000, Nonce: "n"})
	if res, _ := apiCall(t, server, "GET", "/me", expired, nil, ""); res.StatusCode != 401 {
		t.Fatalf("expired token: HTTP %d", res.StatusCode)
	}
	if res, _ := apiCall(t, server, "GET", "/me", token+"x", nil, ""); res.StatusCode != 401 {
		t.Fatalf("tampered token: HTTP %d", res.StatusCode)
	}

	res, raw := apiCall(t, server, "GET", "/me", token, nil, "")
	if res.StatusCode != 200 || !strings.Contains(string(raw), agent) {
		t.Fatalf("/me: %d %s", res.StatusCode, raw)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("API answers may be cached")
	}
}

func TestRestTokenLastsAnHour(t *testing.T) {
	app, _, _, _, _ := setup(t)
	claims := app.verifyToken(apiToken(t, app, agent), "api")
	if claims == nil || claims.Subject != agent || claims.ClientID != "client" || claims.Scope != "mcp" {
		t.Fatalf("claims %+v", claims)
	}
	if left := time.Duration(claims.Expires-nowMs()) * time.Millisecond; left > time.Hour || left < 59*time.Minute {
		t.Fatalf("token lifetime %v", left)
	}
}

func TestRestDraftWithMultipartAttachment(t *testing.T) {
	app, server, bridge, _, _ := setup(t)
	token := apiToken(t, app, agent)
	pdf := []byte("%PDF-1.7 Angebot")

	body, ct := multipartBody(t,
		formPart{name: "mailbox_id", data: []byte("1")},
		formPart{name: "to", data: []byte("kunde@example.org")},
		formPart{name: "cc", data: []byte("a@example.org")},
		formPart{name: "cc", data: []byte("b@example.org")},
		formPart{name: "subject", data: []byte("Ihr Angebot")},
		formPart{name: "body", data: []byte("123")},
		formPart{name: "file", filename: "angebot.pdf", data: pdf},
	)
	res, raw := apiCall(t, server, "POST", "/drafts", token, body, ct)
	if res.StatusCode != 201 {
		t.Fatalf("POST /drafts: %d %s", res.StatusCode, raw)
	}
	call := lastCall(t, bridge)
	if call.Path != "drafts" || call.ActingUser != agent {
		t.Fatalf("bridge call %s as %s", call.Path, call.ActingUser)
	}
	if call.Body["mailbox_id"] != float64(1) || call.Body["subject"] != "Ihr Angebot" || call.Body["body"] != "123" {
		t.Fatalf("fields: %v", call.Body)
	}
	if cc, _ := call.Body["cc"].([]any); len(cc) != 2 {
		t.Fatalf("repeated cc not a list: %v", call.Body["cc"])
	}
	if len(call.Files) != 1 || !bytes.Equal(call.Files[0].Data, pdf) || call.Files[0].Name != "angebot.pdf" {
		t.Fatalf("file: %+v", call.Files)
	}
}

func TestRestPathBeatsBodyAndJSONWorks(t *testing.T) {
	app, server, bridge, _, _ := setup(t)
	token := apiToken(t, app, agent)
	res, raw := apiCall(t, server, "POST", "/conversations/7/drafts", token,
		strings.NewReader(`{"id": 999, "body": "Hallo", "cc": "a@example.org"}`), "application/json")
	if res.StatusCode != 201 {
		t.Fatalf("%d %s", res.StatusCode, raw)
	}
	if call := lastCall(t, bridge); call.Path != "conversations/7/draft" {
		t.Fatalf("the body overrode the path: %s", call.Path)
	}

	if res, _ := apiCall(t, server, "POST", "/conversations/7/drafts", token, strings.NewReader(`{}`), "application/json"); res.StatusCode != 400 {
		t.Fatalf("missing body: HTTP %d", res.StatusCode)
	}
	if res, _ := apiCall(t, server, "POST", "/conversations/abc/drafts", token, strings.NewReader(`{}`), "application/json"); res.StatusCode != 404 {
		t.Fatalf("non-numeric id: HTTP %d", res.StatusCode)
	}
}

func TestRestKeepsFreeScoutsRefusal(t *testing.T) {
	app, server, _, _, _ := setup(t)
	// Not a FreeScout user: the bridge refuses, and so does the API.
	stranger := apiToken(t, app, "fremd@example.org")
	if res, _ := apiCall(t, server, "GET", "/mailboxes", stranger, nil, ""); res.StatusCode != 403 {
		t.Fatalf("a stranger got HTTP %d", res.StatusCode)
	}
	token := apiToken(t, app, agent)
	if res, _ := apiCall(t, server, "POST", "/conversations/9/reply", token, strings.NewReader(`{"body":"x"}`), "application/json"); res.StatusCode != 404 {
		t.Fatalf("missing conversation: HTTP %d", res.StatusCode)
	}
}

func TestRestTooLargeIs413(t *testing.T) {
	app, server, bridge, _, _ := setup(t)
	token := apiToken(t, app, agent)
	before := len(bridge.calls)
	body, ct := multipartBody(t,
		formPart{name: "body", data: []byte("x")},
		formPart{name: "file", filename: "gross.bin", data: make([]byte, 21<<20)},
	)
	if res, raw := apiCall(t, server, "POST", "/conversations/7/drafts", token, body, ct); res.StatusCode != 413 {
		t.Fatalf("21 MB: %d %s", res.StatusCode, raw)
	}
	body, ct = multipartBody(t, formPart{name: "file", filename: "gross.bin", data: make([]byte, 21<<20)})
	if res, _ := apiCall(t, server, "POST", "/uploads", token, body, ct); res.StatusCode != 413 {
		t.Fatalf("21 MB upload: HTTP %d", res.StatusCode)
	}
	if len(bridge.calls) != before {
		t.Fatalf("an oversized request reached the bridge")
	}
}

func TestUploadThenAttachByID(t *testing.T) {
	app, server, bridge, _, _ := setup(t)
	token := apiToken(t, app, agent)
	pdf := []byte("%PDF-1.7 Pläne")

	body, ct := multipartBody(t, formPart{name: "file", filename: "plaene.pdf", data: pdf})
	res, raw := apiCall(t, server, "POST", "/uploads", token, body, ct)
	if res.StatusCode != 201 {
		t.Fatalf("upload: %d %s", res.StatusCode, raw)
	}
	var out struct {
		Uploads []struct {
			ID   string `json:"upload_id"`
			Name string `json:"filename"`
			Size int    `json:"size"`
		} `json:"uploads"`
	}
	json.Unmarshal(raw, &out)
	if len(out.Uploads) != 1 || out.Uploads[0].Size != len(pdf) || !uploadIDRe.MatchString(out.Uploads[0].ID) {
		t.Fatalf("upload answer: %s", raw)
	}
	id := out.Uploads[0].ID

	// Someone else cannot use it.
	if _, _, err := app.uploads.read("fremd@example.org", id); err == nil {
		t.Fatalf("another user could read the upload")
	}

	// The MCP tool takes it by id; the file name comes from the upload.
	result := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
		"id": 7, "body": "Anbei", "attachments": []map[string]any{{"upload_id": id}},
	}), agent)
	if result["isError"] == true {
		t.Fatalf("create_draft: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if len(call.Files) != 1 || !bytes.Equal(call.Files[0].Data, pdf) || call.Files[0].Name != "plaene.pdf" {
		t.Fatalf("file: %+v", call.Files)
	}
	// Used once, then gone.
	if _, _, err := app.uploads.read(agent, id); err == nil {
		t.Fatalf("the upload survived its use")
	}
	again := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
		"id": 7, "body": "Anbei", "attachments": []map[string]any{{"upload_id": id}},
	}), agent)
	if again["isError"] != true {
		t.Fatalf("a used upload could be attached again")
	}
}

func TestUploadsExpireAndStayOutOfPaths(t *testing.T) {
	store, err := newUploadStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store.now = func() time.Time { return now }
	u, err := store.put(agent, "../../etc/passwd", "", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.path, "..") || !strings.HasPrefix(u.path, store.dir) {
		t.Fatalf("client name reached the path: %s", u.path)
	}
	if _, _, err := store.read(agent, "../"+u.ID); err == nil {
		t.Fatalf("a crafted id was accepted")
	}
	now = now.Add(uploadTTL + time.Second)
	if _, _, err := store.read(agent, u.ID); err == nil {
		t.Fatalf("an expired upload was still readable")
	}
	for i := 0; i < maxUploadsPerUser; i++ {
		if _, err := store.put(agent, "f", "", []byte("x")); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	if _, err := store.put(agent, "f", "", []byte("x")); err == nil {
		t.Fatalf("no limit on pending uploads")
	}
	if _, err := store.put("other@example.org", "f", "", []byte("x")); err != nil {
		t.Fatalf("one user's uploads blocked another: %v", err)
	}
}

func TestRestAttachmentIsTheFileItself(t *testing.T) {
	app, server, _, _, _ := setup(t)
	res, raw := apiCall(t, server, "GET", "/attachments/3", apiToken(t, app, agent), nil, "")
	if res.StatusCode != 200 || string(raw) != "%PDF-1.7 plan" {
		t.Fatalf("%d %q", res.StatusCode, raw)
	}
	if res.Header.Get("Content-Type") != "application/pdf" || !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers %v", res.Header)
	}
}

func TestFieldsAndLastThreadsKeepAnswersSmall(t *testing.T) {
	app, server, _, _, _ := setup(t)
	token := apiToken(t, app, agent)
	_, raw := apiCall(t, server, "GET", "/conversations?fields=subject", token, nil, "")
	if strings.Contains(string(raw), "preview") || !strings.Contains(string(raw), `"subject":"Dach"`) || !strings.Contains(string(raw), `"total":1`) {
		t.Fatalf("fields not applied: %s", raw)
	}
	_, raw = apiCall(t, server, "GET", "/conversations/7?last_threads=1", token, nil, "")
	if strings.Contains(string(raw), "eins") || !strings.Contains(string(raw), "drei") || !strings.Contains(string(raw), `"threads_omitted":2`) {
		t.Fatalf("last_threads not applied: %s", raw)
	}

	// The search tool is compact by default.
	text := toolText(t, app.dispatchTool("search_conversations", nil, agent))
	if strings.Contains(text, "preview") || !strings.Contains(text, "Dach") {
		t.Fatalf("search tool not compact: %s", text)
	}
	text = toolText(t, app.dispatchTool("search_conversations", mustJSON(t, map[string]any{"fields": []string{"preview"}}), agent))
	if !strings.Contains(text, "Lange Vorschau") {
		t.Fatalf("fields ignored by the tool: %s", text)
	}
}

func TestOpenAPIListsEveryRoute(t *testing.T) {
	app, server, _, _, _ := setup(t)
	res, raw := apiCall(t, server, "GET", "/openapi.json", "", nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("openapi.json: HTTP %d", res.StatusCode)
	}
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, route := range app.apiRoutes {
		method, path, _ := strings.Cut(route, " ")
		if _, ok := doc.Paths[path][strings.ToLower(method)]; !ok {
			t.Errorf("%s missing from openapi.json", route)
		}
	}
	documented := 0
	for _, ops := range doc.Paths {
		documented += len(ops)
	}
	if documented != len(app.apiRoutes) {
		t.Errorf("openapi.json documents %d operations, %d are mounted", documented, len(app.apiRoutes))
	}
}

func TestTokenAndAPIToolsAreListed(t *testing.T) {
	names := map[string]bool{}
	for _, def := range toolDefs() {
		names[def["name"].(string)] = true
	}
	if !names["token"] || !names["api"] {
		t.Fatalf("token or api tool missing")
	}
	app, _, _, _, _ := setup(t)
	if guide := toolText(t, app.dispatchTool("api", nil, agent)); !strings.Contains(guide, "openapi.json") || !strings.Contains(guide, "-F file=@") {
		t.Fatalf("api guide: %s", guide)
	}
}
