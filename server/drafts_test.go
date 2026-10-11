package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func lastCall(t *testing.T, bridge *fakeBridge) bridgeCall {
	t.Helper()
	if len(bridge.calls) == 0 {
		t.Fatalf("the bridge was never called")
	}
	return bridge.calls[len(bridge.calls)-1]
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCreateDraftSendsFieldsAndFilesToTheDraftRoute(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	pdf := []byte("%PDF-1.7 Angebot über 5 m²")

	result := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
		"id":   7,
		"body": "Guten Tag,\n\nanbei das Angebot.",
		"cc":   "bau@example.org; statik@example.org",
		"attachments": []map[string]any{
			{"filename": "angebot.pdf", "content_base64": base64.StdEncoding.EncodeToString(pdf)},
		},
	}), agent)
	if result["isError"] == true {
		t.Fatalf("create_draft failed: %s", toolText(t, result))
	}

	call := lastCall(t, bridge)
	if call.Method != "POST" || call.Path != "conversations/7/draft" {
		t.Fatalf("unexpected bridge call: %s %s", call.Method, call.Path)
	}
	if strings.Contains(call.Path, "reply") {
		t.Fatalf("a draft went to the reply route")
	}
	if call.Body["body"] != "Guten Tag,\n\nanbei das Angebot." {
		t.Fatalf("body altered: %q", call.Body["body"])
	}
	cc, _ := call.Body["cc"].([]any)
	if len(cc) != 2 || cc[0] != "bau@example.org" || cc[1] != "statik@example.org" {
		t.Fatalf("cc not split into a list: %v", call.Body["cc"])
	}
	if len(call.Files) != 1 || !bytes.Equal(call.Files[0].Data, pdf) {
		t.Fatalf("the file did not arrive byte for byte: %+v", call.Files)
	}
	meta, _ := call.Body["attachments"].([]any)
	first, _ := meta[0].(map[string]any)
	if len(meta) != 1 || first["filename"] != "angebot.pdf" {
		t.Fatalf("attachment metadata missing: %v", call.Body["attachments"])
	}
	if call.ActingUser != agent {
		t.Fatalf("acting user %q", call.ActingUser)
	}
}

func TestCreateNewDraftNeedsRecipientSubjectAndBody(t *testing.T) {
	app, _, bridge, _, _ := setup(t)

	for _, args := range []map[string]any{
		{"to": []string{"a@example.org"}, "subject": "s", "body": "b"},
		{"mailbox_id": 1, "subject": "s", "body": "b"},
		{"mailbox_id": 1, "to": []string{"a@example.org"}, "body": "b"},
		{"mailbox_id": 1, "to": []string{"a@example.org"}, "subject": "s", "body": "  "},
	} {
		if result := app.dispatchTool("create_new_draft", mustJSON(t, args), agent); result["isError"] != true {
			t.Fatalf("accepted %v", args)
		}
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("the bridge was called for invalid input")
	}

	result := app.dispatchTool("create_new_draft", mustJSON(t, map[string]any{
		"mailbox_id": 1, "to": []string{"kundin@example.com"}, "subject": "Angebot", "body": "Hallo",
	}), agent)
	if result["isError"] == true {
		t.Fatalf("create_new_draft failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if call.Path != "drafts" || call.Body["subject"] != "Angebot" || call.Body["mailbox_id"] != float64(1) {
		t.Fatalf("unexpected call: %s %v", call.Path, call.Body)
	}
	if to, _ := call.Body["to"].([]any); len(to) != 1 || to[0] != "kundin@example.com" {
		t.Fatalf("to not passed: %v", call.Body["to"])
	}
}

func TestUpdateDraftOnlySendsWhatWasGiven(t *testing.T) {
	app, _, bridge, _, _ := setup(t)

	result := app.dispatchTool("update_draft", mustJSON(t, map[string]any{
		"draft_id": 5, "cc": []string{}, "remove_attachment_ids": []int{11},
	}), agent)
	if result["isError"] == true {
		t.Fatalf("update_draft failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if call.Path != "drafts/5" {
		t.Fatalf("path %s", call.Path)
	}
	if _, ok := call.Body["body"]; ok {
		t.Fatalf("an untouched body was sent along and would be overwritten: %v", call.Body)
	}
	if _, ok := call.Body["to"]; ok {
		t.Fatalf("untouched recipients were sent along: %v", call.Body)
	}
	if cc, ok := call.Body["cc"].([]any); !ok || len(cc) != 0 {
		t.Fatalf("an explicit empty cc must reach the bridge to clear it: %v", call.Body)
	}
	if ids, _ := call.Body["remove_attachment_ids"].([]any); len(ids) != 1 || ids[0] != float64(11) {
		t.Fatalf("remove_attachment_ids lost: %v", call.Body)
	}

	for _, args := range []map[string]any{{"draft_id": 5}, {"body": "x"}, {"draft_id": 5, "body": " "}} {
		if result := app.dispatchTool("update_draft", mustJSON(t, args), agent); result["isError"] != true {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestDeleteDraft(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	if result := app.dispatchTool("delete_draft", json.RawMessage(`{"draft_id":5}`), agent); result["isError"] == true {
		t.Fatalf("delete_draft failed: %s", toolText(t, result))
	}
	if call := lastCall(t, bridge); call.Method != "POST" || call.Path != "drafts/5/delete" {
		t.Fatalf("unexpected call %s %s", call.Method, call.Path)
	}
	if result := app.dispatchTool("delete_draft", json.RawMessage(`{}`), agent); result["isError"] != true {
		t.Fatalf("accepted a delete without draft_id")
	}
}

func TestBadAttachmentsNeverReachTheBridge(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	big := base64.StdEncoding.EncodeToString(make([]byte, maxDraftAttachmentBytes/2+1))

	for name, attachments := range map[string][]map[string]any{
		"no filename":     {{"content_base64": "aGFsbG8="}},
		"no content":      {{"filename": "a.txt"}},
		"content and url": {{"filename": "a.txt", "content_base64": "aGFsbG8=", "url": "https://example.org/a"}},
		"not base64":      {{"filename": "a.txt", "content_base64": "@@@ nope"}},
		"empty file":      {{"filename": "a.txt", "content_base64": ""}, {"filename": "b.txt", "content_base64": "="}},
		"too large":       {{"filename": "a.bin", "content_base64": big}, {"filename": "b.bin", "content_base64": big}},
		"plain http":      {{"filename": "a.txt", "url": "ftp://example.org/a"}},
	} {
		result := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
			"id": 7, "body": "hallo", "attachments": attachments,
		}), agent)
		if result["isError"] != true {
			t.Fatalf("%s: accepted", name)
		}
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("the bridge was called %d times for bad attachments", len(bridge.calls))
	}
}

func TestBase64InTheShapesModelsProduce(t *testing.T) {
	want := []byte{0xfb, 0xff, 0x00, 'h', 'a', 'l', 'l', 'o'}
	std := base64.StdEncoding.EncodeToString(want)
	for _, encoded := range []string{
		std,
		std[:4] + "\n" + std[4:],
		base64.RawStdEncoding.EncodeToString(want),
		base64.URLEncoding.EncodeToString(want),
		"data:application/octet-stream;base64," + std,
	} {
		got, err := decodeBase64(encoded)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%q decoded to %v, %v", encoded, got, err)
		}
	}
}

func TestAttachmentByURL(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plan.pdf" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.7 Plan"))
	}))
	defer files.Close()

	result := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
		"id": 7, "body": "Plan anbei",
		"attachments": []map[string]any{{"filename": "plan.pdf", "url": files.URL + "/plan.pdf"}},
	}), agent)
	if result["isError"] == true {
		t.Fatalf("create_draft with url failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if len(call.Files) != 1 || string(call.Files[0].Data) != "%PDF-1.7 Plan" {
		t.Fatalf("downloaded file did not arrive: %+v", call.Files)
	}
	meta, _ := call.Body["attachments"].([]any)
	if first, _ := meta[0].(map[string]any); first["mime_type"] != "application/pdf" {
		t.Fatalf("the served content type was not passed on: %v", meta)
	}

	missing := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
		"id": 7, "body": "x", "attachments": []map[string]any{{"filename": "x.pdf", "url": files.URL + "/nope"}},
	}), agent)
	if missing["isError"] != true || !strings.Contains(toolText(t, missing), "404") {
		t.Fatalf("a 404 download was not reported: %v", missing)
	}
}

// The sidecar shares the pod with the bridge and sits inside the cluster: a
// URL must never be a way to make it fetch from either.
func TestAttachmentURLsCannotReachInternalAddresses(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	app.cimdAllowLocal = false

	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an internal address was fetched: %s", r.URL)
	}))
	defer internal.Close()

	for _, target := range []string{
		internal.URL + "/secret",
		"https://127.0.0.1/mcp-bridge/users",
		"https://[::1]/x",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/x",
		"http://example.org/plain-http",
	} {
		result := app.dispatchTool("create_draft", mustJSON(t, map[string]any{
			"id": 7, "body": "x", "attachments": []map[string]any{{"filename": "x", "url": target}},
		}), agent)
		if result["isError"] != true {
			t.Fatalf("%s was accepted", target)
		}
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("the bridge was called")
	}
}

func TestDraftToolsSayTheyNeverSend(t *testing.T) {
	for _, def := range toolDefs() {
		name, _ := def["name"].(string)
		if name != "create_draft" && name != "create_new_draft" && name != "update_draft" {
			continue
		}
		if desc, _ := def["description"].(string); !strings.Contains(desc, "never sends") {
			t.Fatalf("%s does not say that it never sends: %q", name, desc)
		}
	}
}

func TestOversizedCallIsRefusedClearly(t *testing.T) {
	app, server, _, _, _ := setup(t)
	token := app.mintTokens("client", agent, "").AccessToken

	body := bytes.Repeat([]byte("a"), maxRPCBody+10)
	req, _ := http.NewRequest("POST", server.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 413 {
		t.Fatalf("want 413, got %d", res.StatusCode)
	}
}
