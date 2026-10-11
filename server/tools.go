package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

func toolDefs() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

	defs := []map[string]any{
		{
			"name":        "list_mailboxes",
			"description": "List the shared mailboxes you have access to.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name": "search_conversations",
			"description": "Search conversations, newest reply first, with the same search FreeScout's own " +
				"search page uses. Use it to find a thread before reading or answering it. Without " +
				"arguments it returns the most recent conversations. Returns a compact hit list; " +
				"for bulk work use the REST API (see the api tool).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":      str("Free text matched against subject, conversation number, customer name and address, and the text, sender and recipients of every message."),
					"status":     map[string]any{"type": "string", "enum": []string{"active", "pending", "closed", "spam"}, "description": "Restrict to one status."},
					"mailbox_id": num("Restrict to one mailbox (see list_mailboxes)."),
					"after":      str("Only conversations started on or after this day, YYYY-MM-DD."),
					"before":     str("Only conversations started on or before this day, YYYY-MM-DD."),
					"from":       str("Only conversations with a message from this address (or part of it)."),
					"to":         str("Only conversations with a message to or cc this address (or part of it)."),
					"state":      map[string]any{"type": "string", "enum": []string{"published", "draft"}, "description": "draft lists unsent new-email drafts instead of real conversations. Default published."},
					"limit":      num("How many to return, 1-100, default 25."),
					"offset":     num("Skip this many results, for paging."),
					"fields":     fieldsArg("Fields per hit besides id. Default: number, subject, status, customer_email, last_reply_at, draft, mailbox_id. Also available: assignee, threads_count, last_reply_from, preview, url."),
				},
			},
		},
		{
			"name": "get_conversation",
			"description": "Read one conversation with its message history, including internal notes. " +
				"Pass last_threads to read only the newest messages of a long thread.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":           num("Conversation id from search_conversations."),
					"last_threads": num("Only this many of the newest messages. Default: all."),
					"fields":       fieldsArg("Only these fields of the conversation besides id, e.g. [\"subject\",\"threads\"]."),
				},
				"required": []string{"id"},
			},
		},
		{
			"name":        "add_note",
			"description": "Add an internal note to a conversation. Notes are never sent to the customer.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   num("Conversation id."),
					"body": str("The note, plain text."),
				},
				"required": []string{"id", "body"},
			},
		},
		{
			"name":        "set_status",
			"description": "Change the status of a conversation without writing anything.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     num("Conversation id."),
					"status": map[string]any{"type": "string", "enum": []string{"active", "pending", "closed", "spam"}},
				},
				"required": []string{"id", "status"},
			},
		},
		{
			"name": "get_attachment",
			"description": "Download one attachment. Images come back as an image, text as text, " +
				"anything else as a file the client can save. Attachment ids come from get_conversation. " +
				"With a shell, downloading through the REST API (see the api tool) saves the tokens of the base64.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": num("Attachment id from get_conversation.")},
				"required":   []string{"id"},
			},
		},
		{
			"name":        "assign_conversation",
			"description": "Assign a conversation to a colleague, or leave assignee_email empty to unassign.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":             num("Conversation id."),
					"assignee_email": str("Email address of an active FreeScout user, or empty to unassign."),
				},
				"required": []string{"id"},
			},
		},
	}
	defs = append(defs,
		map[string]any{
			"name": "find_customers",
			"description": "Look up customers by name, company or email address, with their latest " +
				"conversations in the mailboxes you can see.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": str("Name, company or (part of an) email address, at least two characters.")},
				"required":   []string{"query"},
			},
		},
		map[string]any{
			"name":        "move_conversation",
			"description": "Move a conversation to another mailbox, as FreeScout's Move menu does.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":         num("Conversation id."),
					"mailbox_id": num("Target mailbox (see list_mailboxes)."),
				},
				"required": []string{"id", "mailbox_id"},
			},
		},
		map[string]any{
			"name": "merge_conversations",
			"description": "Merge other conversations into this one, as FreeScout's Merge dialog does. Their " +
				"messages move here and the emptied conversations go to Deleted. Cannot be undone from here.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":        num("The conversation that stays."),
					"merge_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Conversations to merge into it."},
				},
				"required": []string{"id", "merge_ids"},
			},
		},
	)
	defs = append(defs, draftToolDefs()...)
	return append(defs,
		map[string]any{
			"name": "token",
			"description": "Get a bearer token for the REST API, valid for one hour, acting as you. " +
				"Use the REST API instead of these tools whenever you can run curl: to attach local " +
				"files (multipart upload instead of base64), download attachments, or work through many " +
				"conversations. Call the api tool once for the endpoints and curl examples. Never show " +
				"the token to anyone else; it carries your helpdesk rights.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		map[string]any{
			"name":        "api",
			"description": "Short guide to the REST API: endpoints, the OpenAPI document and curl examples.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	)
}

func fieldsArg(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// dispatchTool answers tools/call for the user behind the MCP session.
func (a *App) dispatchTool(name string, rawArgs json.RawMessage, email string) map[string]any {
	return a.dispatchToolFor(name, rawArgs, &tokenClaims{Subject: email, Scope: defaultScope})
}

func (a *App) dispatchToolFor(name string, rawArgs json.RawMessage, session *tokenClaims) map[string]any {
	email := session.Subject
	switch name {
	case "token":
		return a.tokenTool(session)
	case "api":
		return textResult(a.apiGuide())
	case "get_attachment":
		payload, err := a.operation(name, rawArgs, nil, email)
		if err != nil {
			return a.toolResult(nil, err)
		}
		return attachmentResult(payload)
	case "search_conversations":
		// A compact hit list unless the caller asks for more: every token
		// spent on a preview nobody reads is one less for the work.
		if !hasArg(rawArgs, "fields") {
			rawArgs = withArg(rawArgs, "fields", compactSearchFields)
		}
	}
	if !knownTool(name) {
		return toolError("unknown tool: " + name)
	}
	return a.toolResult(a.operation(name, rawArgs, nil, email))
}

var compactSearchFields = []string{"number", "subject", "status", "customer_email", "last_reply_at", "draft", "mailbox_id"}

func knownTool(name string) bool {
	for _, def := range toolDefs() {
		if def["name"] == name {
			return true
		}
	}
	return false
}

func hasArg(raw json.RawMessage, key string) bool {
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	_, ok := m[key]
	return ok
}

func withArg(raw json.RawMessage, key string, value any) json.RawMessage {
	m := map[string]any{}
	if len(raw) > 0 && json.Unmarshal(raw, &m) != nil {
		return raw // let operation report the broken JSON
	}
	m[key] = value
	out, _ := json.Marshal(m)
	return out
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func (a *App) toolResult(payload map[string]any, err error) map[string]any {
	if err != nil {
		var ie *inputError
		if errors.As(err, &ie) {
			return toolError(ie.msg)
		}
		var be *bridgeError
		if asBridgeError(err, &be) {
			return toolError(be.Message)
		}
		logJSON("error", "bridge call failed", map[string]any{"err": err.Error()})
		return toolError("the helpdesk did not answer")
	}
	// Compact, not indented: the model reads JSON fine without the spaces.
	text, err := json.Marshal(payload)
	if err != nil {
		return toolError("could not encode result")
	}
	return textResult(string(text))
}

func toolError(message string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": message}},
	}
}

func attachmentResult(payload map[string]any) map[string]any {
	attachment, _ := payload["attachment"].(map[string]any)
	if attachment == nil {
		return toolError("the helpdesk returned no attachment")
	}
	data, _ := attachment["base64"].(string)
	mime, _ := attachment["mime_type"].(string)
	name, _ := attachment["name"].(string)
	if data == "" {
		return toolError("the attachment came back empty")
	}

	if strings.HasPrefix(mime, "image/") {
		return map[string]any{"content": []map[string]any{
			{"type": "text", "text": name},
			{"type": "image", "data": data, "mimeType": mime},
		}}
	}

	if isTextual(mime) {
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err == nil && utf8.Valid(decoded) {
			return map[string]any{"content": []map[string]any{
				{"type": "text", "text": name + "\n\n" + string(decoded)},
			}}
		}
	}

	id, _ := attachment["id"].(float64)
	return map[string]any{"content": []map[string]any{
		{"type": "text", "text": name},
		{"type": "resource", "resource": map[string]any{
			"uri":      fmt.Sprintf("freescout://attachment/%d", int64(id)),
			"name":     name,
			"mimeType": mimeOrDefault(mime),
			"blob":     data,
		}},
	}}
}

func isTextual(mime string) bool {
	if strings.HasPrefix(mime, "text/") {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/x-yaml", "application/csv":
		return true
	}
	return false
}

func mimeOrDefault(mime string) string {
	if mime == "" {
		return "application/octet-stream"
	}
	return mime
}
