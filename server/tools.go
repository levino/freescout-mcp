package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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
				"arguments it returns the most recent conversations.",
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
				},
			},
		},
		{
			"name":        "get_conversation",
			"description": "Read one conversation with its full message history, including internal notes.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": num("Conversation id from search_conversations.")},
				"required":   []string{"id"},
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
				"anything else as a file the client can save. Attachment ids come from get_conversation.",
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
	return append(defs, draftToolDefs()...)
}

type toolArgs struct {
	ID            int64   `json:"id"`
	Query         string  `json:"query"`
	After         string  `json:"after"`
	Before        string  `json:"before"`
	From          string  `json:"from"`
	To            string  `json:"to"`
	State         string  `json:"state"`
	MergeIDs      []int64 `json:"merge_ids"`
	Status        string  `json:"status"`
	MailboxID     int64   `json:"mailbox_id"`
	Limit         int64   `json:"limit"`
	Offset        int64   `json:"offset"`
	Body          string  `json:"body"`
	AssigneeEmail string  `json:"assignee_email"`
}

func (a *App) dispatchTool(name string, rawArgs json.RawMessage, email string) map[string]any {
	if isDraftTool(name) {
		return a.dispatchDraftTool(name, rawArgs, email)
	}

	var args toolArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return toolError("arguments are not valid JSON: " + err.Error())
		}
	}

	switch name {
	case "list_mailboxes":
		return a.toolResult(a.bridge.get("mailboxes", nil, email))

	case "search_conversations":
		q := url.Values{}
		if args.Query != "" {
			q.Set("query", args.Query)
		}
		if args.Status != "" {
			q.Set("status", args.Status)
		}
		if args.MailboxID > 0 {
			q.Set("mailbox_id", strconv.FormatInt(args.MailboxID, 10))
		}
		if args.Limit > 0 {
			q.Set("limit", strconv.FormatInt(args.Limit, 10))
		}
		if args.Offset > 0 {
			q.Set("offset", strconv.FormatInt(args.Offset, 10))
		}
		for key, value := range map[string]string{"after": args.After, "before": args.Before, "from": args.From, "to": args.To, "state": args.State} {
			if value = strings.TrimSpace(value); value != "" {
				q.Set(key, value)
			}
		}
		return a.toolResult(a.bridge.get("conversations", q, email))

	case "get_conversation":
		if args.ID <= 0 {
			return toolError("id is required")
		}
		return a.toolResult(a.bridge.get("conversations/"+strconv.FormatInt(args.ID, 10), nil, email))

	case "get_attachment":
		if args.ID <= 0 {
			return toolError("id is required")
		}
		payload, err := a.bridge.get("attachments/"+strconv.FormatInt(args.ID, 10), nil, email)
		if err != nil {
			return a.toolResult(nil, err)
		}
		return attachmentResult(payload)

	case "add_note":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return toolError("id and body are required")
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/note",
			map[string]any{"body": args.Body}, email))

	case "set_status":
		if args.ID <= 0 || args.Status == "" {
			return toolError("id and status are required")
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/status",
			map[string]any{"status": args.Status}, email))

	case "assign_conversation":
		if args.ID <= 0 {
			return toolError("id is required")
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/assign",
			map[string]any{"assignee_email": args.AssigneeEmail}, email))

	case "find_customers":
		if len(strings.TrimSpace(args.Query)) < 2 {
			return toolError("query needs at least two characters")
		}
		return a.toolResult(a.bridge.get("customers", url.Values{"query": {strings.TrimSpace(args.Query)}}, email))

	case "move_conversation":
		if args.ID <= 0 || args.MailboxID <= 0 {
			return toolError("id and mailbox_id are required")
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/move",
			map[string]any{"mailbox_id": args.MailboxID}, email))

	case "merge_conversations":
		if args.ID <= 0 || len(args.MergeIDs) == 0 {
			return toolError("id and merge_ids are required")
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/merge",
			map[string]any{"merge_ids": args.MergeIDs}, email))

	default:
		return toolError("unknown tool: " + name)
	}
}

func (a *App) toolResult(payload map[string]any, err error) map[string]any {
	if err != nil {
		var be *bridgeError
		if asBridgeError(err, &be) {
			return toolError(be.Message)
		}
		logJSON("error", "bridge call failed", map[string]any{"err": err.Error()})
		return toolError("the helpdesk did not answer")
	}
	pretty, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return toolError("could not encode result")
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(pretty)}},
	}
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
