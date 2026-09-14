package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func toolDefs() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

	return []map[string]any{
		{
			"name":        "list_mailboxes",
			"description": "List the shared mailboxes you have access to.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name": "search_conversations",
			"description": "List conversations, newest reply first. Use it to find a thread before " +
				"reading or answering it. Without arguments it returns the most recent conversations.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":      str("Free text matched against subject, customer address and preview."),
					"status":     map[string]any{"type": "string", "enum": []string{"active", "pending", "closed", "spam"}, "description": "Restrict to one status."},
					"mailbox_id": num("Restrict to one mailbox (see list_mailboxes)."),
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
			"name": "reply_to_conversation",
			"description": "Send a reply to the customer. This really sends an email — do not call it " +
				"to draft or to think out loud. The text is sent as written, so write it as the " +
				"agent whose account is connected.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     num("Conversation id."),
					"body":   str("The message to the customer, plain text."),
					"status": map[string]any{"type": "string", "enum": []string{"active", "pending", "closed"}, "description": "Optional status to set along with the reply."},
				},
				"required": []string{"id", "body"},
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
}

type toolArgs struct {
	ID            int64  `json:"id"`
	Query         string `json:"query"`
	Status        string `json:"status"`
	MailboxID     int64  `json:"mailbox_id"`
	Limit         int64  `json:"limit"`
	Offset        int64  `json:"offset"`
	Body          string `json:"body"`
	AssigneeEmail string `json:"assignee_email"`
}

func (a *App) dispatchTool(name string, rawArgs json.RawMessage, email string) map[string]any {
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
		return a.toolResult(a.bridge.get("conversations", q, email))

	case "get_conversation":
		if args.ID <= 0 {
			return toolError("id is required")
		}
		return a.toolResult(a.bridge.get("conversations/"+strconv.FormatInt(args.ID, 10), nil, email))

	case "reply_to_conversation":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return toolError("id and body are required")
		}
		payload := map[string]any{"body": args.Body}
		if args.Status != "" {
			payload["status"] = args.Status
		}
		return a.toolResult(a.bridge.post("conversations/"+strconv.FormatInt(args.ID, 10)+"/reply", payload, email))

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
