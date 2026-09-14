package main

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 {
		raw, _ := json.Marshal(result)
		t.Fatalf("no content in %s", raw)
	}
	text, _ := content[0]["text"].(string)
	return text
}

func TestReplyReachesTheBridgeUnchanged(t *testing.T) {
	app, _, bridge, _, _ := setup(t)

	result := app.dispatchTool("reply_to_conversation",
		json.RawMessage(`{"id":7,"body":"Guten Tag,\n\ndas Dach ist bestellt.","status":"pending"}`), agent)
	if result["isError"] == true {
		t.Fatalf("reply failed: %s", toolText(t, result))
	}

	call := bridge.calls[len(bridge.calls)-1]
	if call.Method != "POST" || call.Path != "conversations/7/reply" {
		t.Fatalf("unexpected bridge call: %s %s", call.Method, call.Path)
	}
	if call.Body["body"] != "Guten Tag,\n\ndas Dach ist bestellt." {
		t.Fatalf("body was altered: %q", call.Body["body"])
	}
	if call.Body["status"] != "pending" {
		t.Fatalf("status not passed on: %v", call.Body["status"])
	}
	if call.ActingUser != agent {
		t.Fatalf("acting user %q", call.ActingUser)
	}
}

func TestHelpdeskErrorsReachTheModelAsToolErrors(t *testing.T) {
	app, _, _, _, _ := setup(t)

	result := app.dispatchTool("reply_to_conversation", json.RawMessage(`{"id":9,"body":"hallo"}`), agent)
	if result["isError"] != true {
		t.Fatalf("a 404 from FreeScout was not surfaced: %v", result)
	}
	if text := toolText(t, result); !strings.Contains(text, "not found") {
		t.Fatalf("error text unhelpful: %q", text)
	}
}

func TestRepliesRequireBodyAndId(t *testing.T) {
	app, _, _, _, _ := setup(t)

	for _, args := range []string{`{"id":7,"body":"   "}`, `{"body":"hallo"}`, `{}`} {
		result := app.dispatchTool("reply_to_conversation", json.RawMessage(args), agent)
		if result["isError"] != true {
			t.Fatalf("accepted %s", args)
		}
	}
}

func TestSearchPassesFiltersThrough(t *testing.T) {
	app, _, bridge, _, _ := setup(t)

	app.dispatchTool("search_conversations",
		json.RawMessage(`{"query":"Dach","status":"active","mailbox_id":1,"limit":5,"offset":10}`), agent)

	call := bridge.calls[len(bridge.calls)-1]
	want := url.Values{"query": {"Dach"}, "status": {"active"}, "mailbox_id": {"1"}, "limit": {"5"}, "offset": {"10"}}
	for key, value := range want {
		if call.Query.Get(key) != value[0] {
			t.Fatalf("%s = %q, want %q", key, call.Query.Get(key), value[0])
		}
	}
}

func TestUnknownToolIsAnError(t *testing.T) {
	app, _, _, _, _ := setup(t)
	if result := app.dispatchTool("delete_everything", json.RawMessage(`{}`), agent); result["isError"] != true {
		t.Fatalf("unknown tool accepted")
	}
}

func TestLoopbackRedirectsIgnoreThePort(t *testing.T) {
	registered := []string{"http://localhost/callback", "http://127.0.0.1/callback"}

	for _, ok := range []string{
		"http://localhost/callback",
		"http://localhost:53211/callback",
		"http://127.0.0.1:8976/callback",
	} {
		if !redirectURIAllowed(registered, ok) {
			t.Fatalf("rejected a legitimate loopback redirect: %s", ok)
		}
	}
	for _, bad := range []string{
		"http://localhost:53211/other",
		"https://evil.example/callback",
		"http://evil.example/callback",
	} {
		if redirectURIAllowed(registered, bad) {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestToolListIsStable(t *testing.T) {
	names := map[string]bool{}
	for _, def := range toolDefs() {
		name, _ := def["name"].(string)
		if name == "" {
			t.Fatalf("tool without a name: %v", def)
		}
		if names[name] {
			t.Fatalf("duplicate tool %s", name)
		}
		names[name] = true
		if _, ok := def["inputSchema"]; !ok {
			t.Fatalf("tool %s has no inputSchema", name)
		}
	}
	for _, want := range []string{"list_mailboxes", "search_conversations", "get_conversation",
		"reply_to_conversation", "add_note", "set_status", "assign_conversation", "get_attachment"} {
		if !names[want] {
			t.Fatalf("tool %s missing", want)
		}
	}
}

func TestAttachmentsComeBackInAUsableShape(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d})

	image := attachmentResult(map[string]any{"attachment": map[string]any{
		"id": float64(3), "name": "dach.png", "mime_type": "image/png", "base64": png,
	}})
	content, _ := image["content"].([]map[string]any)
	if len(content) != 2 || content[1]["type"] != "image" {
		t.Fatalf("an image did not come back as an image: %v", image)
	}
	if content[1]["data"] != png || content[1]["mimeType"] != "image/png" {
		t.Fatalf("image payload altered: %v", content[1])
	}

	text := attachmentResult(map[string]any{"attachment": map[string]any{
		"id": float64(4), "name": "angebot.txt", "mime_type": "text/plain",
		"base64": base64.StdEncoding.EncodeToString([]byte("Angebot über 5 m²")),
	}})
	textContent, _ := text["content"].([]map[string]any)
	if len(textContent) != 1 || !strings.Contains(textContent[0]["text"].(string), "Angebot über 5 m²") {
		t.Fatalf("text did not come back readable: %v", text)
	}

	pdf := attachmentResult(map[string]any{"attachment": map[string]any{
		"id": float64(5), "name": "plan.pdf", "mime_type": "application/pdf",
		"base64": base64.StdEncoding.EncodeToString([]byte("%PDF-1.7")),
	}})
	pdfContent, _ := pdf["content"].([]map[string]any)
	if len(pdfContent) != 2 || pdfContent[1]["type"] != "resource" {
		t.Fatalf("a pdf did not come back as a resource: %v", pdf)
	}
	resource, _ := pdfContent[1]["resource"].(map[string]any)
	if resource["uri"] != "freescout://attachment/5" || resource["mimeType"] != "application/pdf" {
		t.Fatalf("resource metadata wrong: %v", resource)
	}

	// A text/* file that is not valid UTF-8 must not be pasted into the answer.
	broken := attachmentResult(map[string]any{"attachment": map[string]any{
		"id": float64(6), "name": "kaputt.csv", "mime_type": "text/csv",
		"base64": base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00}),
	}})
	brokenContent, _ := broken["content"].([]map[string]any)
	if len(brokenContent) != 2 || brokenContent[1]["type"] != "resource" {
		t.Fatalf("invalid utf-8 was not handed over as a file: %v", broken)
	}

	if empty := attachmentResult(map[string]any{}); empty["isError"] != true {
		t.Fatalf("a missing attachment was not an error")
	}
	if noData := attachmentResult(map[string]any{"attachment": map[string]any{"name": "x"}}); noData["isError"] != true {
		t.Fatalf("an empty attachment was not an error")
	}
}

func TestAttachmentNeedsAnId(t *testing.T) {
	app, _, _, _, _ := setup(t)
	if result := app.dispatchTool("get_attachment", json.RawMessage(`{}`), agent); result["isError"] != true {
		t.Fatalf("accepted a call without an id")
	}
}
