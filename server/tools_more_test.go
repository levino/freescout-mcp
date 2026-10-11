package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestReplyCarriesCcBccAndFiles(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	result := app.dispatchTool("reply_to_conversation", mustJSON(t, map[string]any{
		"id": 7, "body": "Anbei.", "cc": "statik@example.org", "bcc": []string{"archiv@example.org"},
		"attachments": []map[string]any{{"filename": "a.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("A"))}},
	}), agent)
	if result["isError"] == true {
		t.Fatalf("reply failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if call.Path != "conversations/7/reply" || len(call.Files) != 1 || string(call.Files[0].Data) != "A" {
		t.Fatalf("unexpected call %s %+v", call.Path, call.Files)
	}
	if cc, _ := call.Body["cc"].([]any); len(cc) != 1 || cc[0] != "statik@example.org" {
		t.Fatalf("cc %v", call.Body["cc"])
	}
	if bcc, _ := call.Body["bcc"].([]any); len(bcc) != 1 {
		t.Fatalf("bcc %v", call.Body["bcc"])
	}
}

func TestSendDraftIsExplicitAndOnlyForDrafts(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	if result := app.dispatchTool("send_draft", json.RawMessage(`{}`), agent); result["isError"] != true {
		t.Fatalf("send_draft without draft_id accepted")
	}
	if result := app.dispatchTool("send_draft", json.RawMessage(`{"draft_id":5,"status":"pending"}`), agent); result["isError"] == true {
		t.Fatalf("send_draft failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if call.Path != "drafts/5/send" || call.Body["status"] != "pending" {
		t.Fatalf("unexpected call %s %v", call.Path, call.Body)
	}
	for _, def := range toolDefs() {
		if def["name"] == "send_draft" {
			if desc, _ := def["description"].(string); !strings.Contains(desc, "really sends") || !strings.Contains(desc, "explicitly") {
				t.Fatalf("send_draft does not warn: %q", desc)
			}
		}
	}
}

func TestForwardDefaultsToADraftWithAttachments(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	if result := app.dispatchTool("forward_conversation", mustJSON(t, map[string]any{"id": 7, "to": "statik@example.org"}), agent); result["isError"] == true {
		t.Fatalf("forward failed: %s", toolText(t, result))
	}
	call := lastCall(t, bridge)
	if call.Path != "conversations/7/forward" || call.Body["send"] != false || call.Body["include_attachments"] != true {
		t.Fatalf("a forward must default to an unsent draft with attachments: %v", call.Body)
	}

	calls := len(bridge.calls)
	for _, args := range []map[string]any{
		{"id": 7},
		{"id": 7, "to": []string{"a@example.org", "b@example.org"}, "send": true},
		{"id": 7, "to": "a@example.org", "send": true, "attachments": []map[string]any{{"filename": "a", "content_base64": "QQ=="}}},
	} {
		if result := app.dispatchTool("forward_conversation", mustJSON(t, args), agent); result["isError"] != true {
			t.Fatalf("accepted %v", args)
		}
	}
	if len(bridge.calls) != calls {
		t.Fatalf("invalid forwards reached the bridge")
	}
}

func TestSearchPassesTheNewFiltersThrough(t *testing.T) {
	app, _, bridge, _, _ := setup(t)
	app.dispatchTool("search_conversations", mustJSON(t, map[string]any{
		"query": "Dach", "after": "2026-01-01", "before": "2026-12-31", "from": "kundin@", "to": "info@", "state": "draft",
	}), agent)
	call := lastCall(t, bridge)
	for key, want := range map[string]string{"after": "2026-01-01", "before": "2026-12-31", "from": "kundin@", "to": "info@", "state": "draft"} {
		if got := call.Query.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestCustomersMoveAndMerge(t *testing.T) {
	app, _, bridge, _, _ := setup(t)

	if result := app.dispatchTool("find_customers", json.RawMessage(`{"query":"k"}`), agent); result["isError"] != true {
		t.Fatalf("a one-letter customer search was accepted")
	}
	app.dispatchTool("find_customers", json.RawMessage(`{"query":"Kundin"}`), agent)
	if call := lastCall(t, bridge); call.Path != "customers" || call.Query.Get("query") != "Kundin" {
		t.Fatalf("unexpected call %s %v", call.Path, call.Query)
	}

	app.dispatchTool("move_conversation", json.RawMessage(`{"id":7,"mailbox_id":2}`), agent)
	if call := lastCall(t, bridge); call.Path != "conversations/7/move" || call.Body["mailbox_id"] != float64(2) {
		t.Fatalf("unexpected call %s %v", call.Path, call.Body)
	}

	app.dispatchTool("merge_conversations", json.RawMessage(`{"id":7,"merge_ids":[8,9]}`), agent)
	if call := lastCall(t, bridge); call.Path != "conversations/7/merge" {
		t.Fatalf("unexpected call %s", call.Path)
	} else if ids, _ := call.Body["merge_ids"].([]any); len(ids) != 2 {
		t.Fatalf("merge_ids %v", call.Body)
	}

	for name, args := range map[string]string{"move_conversation": `{"id":7}`, "merge_conversations": `{"id":7,"merge_ids":[]}`} {
		if result := app.dispatchTool(name, json.RawMessage(args), agent); result["isError"] != true {
			t.Fatalf("%s accepted %s", name, args)
		}
	}
}
