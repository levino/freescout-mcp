package main

import (
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
		"reply_to_conversation", "add_note", "set_status", "assign_conversation"} {
		if !names[want] {
			t.Fatalf("tool %s missing", want)
		}
	}
}
