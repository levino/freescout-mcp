package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// The business logic behind every MCP tool and every REST route. Both front
// ends turn their input into the same JSON arguments and call operation; what
// differs is only how the answer is wrapped (a tool result, or an HTTP
// response). Keep it that way: a check added here protects both.

// inputError is a problem with the caller's arguments, found before FreeScout
// was asked anything. REST answers it with 400, a tool with isError.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

func invalid(msg string) error { return &inputError{msg} }

// opArgs is the union of every operation's arguments.
type opArgs struct {
	ID                  int64           `json:"id"`
	DraftID             int64           `json:"draft_id"`
	MailboxID           int64           `json:"mailbox_id"`
	Query               string          `json:"query"`
	After               string          `json:"after"`
	Before              string          `json:"before"`
	From                string          `json:"from"`
	State               string          `json:"state"`
	MergeIDs            []int64         `json:"merge_ids"`
	Status              string          `json:"status"`
	Limit               int64           `json:"limit"`
	Offset              int64           `json:"offset"`
	AssigneeEmail       string          `json:"assignee_email"`
	Fields              emailList       `json:"fields"`
	LastThreads         int             `json:"last_threads"`
	To                  emailList       `json:"to"`
	Cc                  emailList       `json:"cc"`
	Bcc                 emailList       `json:"bcc"`
	Subject             string          `json:"subject"`
	Body                string          `json:"body"`
	Attachments         []attachmentArg `json:"attachments"`
	RemoveAttachmentIDs []int64         `json:"remove_attachment_ids"`
	Send                bool            `json:"send"`
	IncludeAttachments  *bool           `json:"include_attachments"`
}

// operation runs one named operation as email. files are attachments that
// arrived as real uploads (REST multipart); they count against the same
// limits as the ones named in args.attachments.
func (a *App) operation(name string, rawArgs json.RawMessage, files []draftFile, email string) (map[string]any, error) {
	var args opArgs
	present := map[string]json.RawMessage{}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &present); err != nil {
			return nil, invalid("arguments are not valid JSON: " + err.Error())
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, invalid("arguments: " + err.Error())
		}
	}
	id := strconv.FormatInt(args.ID, 10)
	draftID := strconv.FormatInt(args.DraftID, 10)

	switch name {
	case "list_mailboxes":
		return a.bridge.get("mailboxes", nil, email)

	case "search_conversations":
		q := url.Values{}
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
		to := ""
		if len(args.To) > 0 {
			to = args.To[0]
		}
		for key, value := range map[string]string{"query": args.Query, "after": args.After, "before": args.Before, "from": args.From, "to": to, "state": args.State} {
			if value = strings.TrimSpace(value); value != "" {
				q.Set(key, value)
			}
		}
		payload, err := a.bridge.get("conversations", q, email)
		if err != nil {
			return nil, err
		}
		return project(payload, args.Fields, 0), nil

	case "get_conversation":
		if args.ID <= 0 {
			return nil, invalid("id is required")
		}
		payload, err := a.bridge.get("conversations/"+id, nil, email)
		if err != nil {
			return nil, err
		}
		return project(payload, args.Fields, args.LastThreads), nil

	case "get_attachment":
		if args.ID <= 0 {
			return nil, invalid("id is required")
		}
		return a.bridge.get("attachments/"+id, nil, email)

	case "add_note":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return nil, invalid("id and body are required")
		}
		return a.bridge.post("conversations/"+id+"/note", map[string]any{"body": args.Body}, email)

	case "set_status":
		if args.ID <= 0 || args.Status == "" {
			return nil, invalid("id and status are required")
		}
		return a.bridge.post("conversations/"+id+"/status", map[string]any{"status": args.Status}, email)

	case "assign_conversation":
		if args.ID <= 0 {
			return nil, invalid("id is required")
		}
		return a.bridge.post("conversations/"+id+"/assign", map[string]any{"assignee_email": args.AssigneeEmail}, email)

	case "find_customers":
		if len(strings.TrimSpace(args.Query)) < 2 {
			return nil, invalid("query needs at least two characters")
		}
		return a.bridge.get("customers", url.Values{"query": {strings.TrimSpace(args.Query)}}, email)

	case "move_conversation":
		if args.ID <= 0 || args.MailboxID <= 0 {
			return nil, invalid("id and mailbox_id are required")
		}
		return a.bridge.post("conversations/"+id+"/move", map[string]any{"mailbox_id": args.MailboxID}, email)

	case "merge_conversations":
		if args.ID <= 0 || len(args.MergeIDs) == 0 {
			return nil, invalid("id and merge_ids are required")
		}
		return a.bridge.post("conversations/"+id+"/merge", map[string]any{"merge_ids": args.MergeIDs}, email)

	case "create_draft":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return nil, invalid("id and body are required")
		}
		payload := map[string]any{"body": args.Body, "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc)}
		return a.withAttachments(args.Attachments, files, email, func(all []draftFile) (map[string]any, error) {
			return a.bridge.postDraft("conversations/"+id+"/draft", payload, all, email)
		})

	case "create_new_draft":
		if args.MailboxID <= 0 || len(args.To) == 0 || strings.TrimSpace(args.Subject) == "" || strings.TrimSpace(args.Body) == "" {
			return nil, invalid("mailbox_id, to, subject and body are required")
		}
		payload := map[string]any{
			"mailbox_id": args.MailboxID, "to": nonNil(args.To), "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc),
			"subject": args.Subject, "body": args.Body,
		}
		return a.withAttachments(args.Attachments, files, email, func(all []draftFile) (map[string]any, error) {
			return a.bridge.postDraft("drafts", payload, all, email)
		})

	case "update_draft":
		if args.DraftID <= 0 {
			return nil, invalid("draft_id is required")
		}
		// Only what was passed, so the bridge can tell "leave it" from "empty it".
		payload := map[string]any{}
		if _, ok := present["body"]; ok {
			if strings.TrimSpace(args.Body) == "" {
				return nil, invalid("body cannot be empty; use delete_draft to discard the draft")
			}
			payload["body"] = args.Body
		}
		if _, ok := present["subject"]; ok {
			payload["subject"] = args.Subject
		}
		for key, list := range map[string]emailList{"to": args.To, "cc": args.Cc, "bcc": args.Bcc} {
			if _, ok := present[key]; ok {
				payload[key] = nonNil(list)
			}
		}
		if len(args.RemoveAttachmentIDs) > 0 {
			payload["remove_attachment_ids"] = args.RemoveAttachmentIDs
		}
		if len(payload) == 0 && len(args.Attachments) == 0 && len(files) == 0 {
			return nil, invalid("nothing to change")
		}
		return a.withAttachments(args.Attachments, files, email, func(all []draftFile) (map[string]any, error) {
			return a.bridge.postDraft("drafts/"+draftID, payload, all, email)
		})

	case "reply_to_conversation":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return nil, invalid("id and body are required")
		}
		payload := map[string]any{"body": args.Body, "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc)}
		if args.Status != "" {
			payload["status"] = args.Status
		}
		return a.withAttachments(args.Attachments, files, email, func(all []draftFile) (map[string]any, error) {
			return a.bridge.postDraft("conversations/"+id+"/reply", payload, all, email)
		})

	case "send_draft":
		if args.DraftID <= 0 {
			return nil, invalid("draft_id is required")
		}
		payload := map[string]any{}
		if args.Status != "" {
			payload["status"] = args.Status
		}
		return a.bridge.post("drafts/"+draftID+"/send", payload, email)

	case "forward_conversation":
		if args.ID <= 0 || len(args.To) == 0 {
			return nil, invalid("id and to are required")
		}
		if args.Send && len(args.To) != 1 {
			return nil, invalid("forward to exactly one address when send is true")
		}
		if args.Send && (len(args.Attachments) > 0 || len(files) > 0) {
			return nil, invalid("extra attachments only on a forward draft (send=false)")
		}
		include := args.IncludeAttachments == nil || *args.IncludeAttachments
		payload := map[string]any{
			"to": nonNil(args.To), "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc), "body": args.Body,
			"send": args.Send, "include_attachments": include,
		}
		return a.withAttachments(args.Attachments, files, email, func(all []draftFile) (map[string]any, error) {
			return a.bridge.postDraft("conversations/"+id+"/forward", payload, all, email)
		})

	case "delete_draft":
		if args.DraftID <= 0 {
			return nil, invalid("draft_id is required")
		}
		return a.bridge.post("drafts/"+draftID+"/delete", map[string]any{}, email)
	}
	return nil, invalid("unknown operation: " + name)
}

// withAttachments loads every file before the bridge is called, so a bad file
// fails the call without leaving half a draft, and deletes the temporary
// uploads it used once FreeScout has stored them. A failed call keeps them
// for another try.
func (a *App) withAttachments(list []attachmentArg, files []draftFile, email string, write func([]draftFile) (map[string]any, error)) (map[string]any, error) {
	loaded, used, err := a.loadAttachments(list, files, email)
	if err != nil {
		return nil, invalid(err.Error())
	}
	result, err := write(loaded)
	if err == nil {
		for _, id := range used {
			a.uploads.remove(email, id)
		}
	}
	return result, err
}

// project keeps only the named keys of the objects in a bridge answer: of
// each element of a top-level list (search results, customers) and of a
// top-level object (one conversation). Counters like total stay. lastThreads
// keeps only that many of a conversation's newest messages.
func project(payload map[string]any, fields []string, lastThreads int) map[string]any {
	if conversation, ok := payload["conversation"].(map[string]any); ok && lastThreads > 0 {
		if threads, ok := conversation["threads"].([]any); ok && len(threads) > lastThreads {
			conversation["threads"] = threads[len(threads)-lastThreads:]
			conversation["threads_omitted"] = len(threads) - lastThreads
		}
	}
	if len(fields) == 0 {
		return payload
	}
	keep := map[string]bool{"id": true}
	for _, f := range fields {
		keep[strings.TrimSpace(f)] = true
	}
	pick := func(v any) any {
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		out := map[string]any{}
		for k, val := range obj {
			if keep[k] || k == "threads_omitted" {
				out[k] = val
			}
		}
		return out
	}
	for key, value := range payload {
		switch v := value.(type) {
		case []any:
			for i := range v {
				v[i] = pick(v[i])
			}
		case map[string]any:
			payload[key] = pick(v)
		}
	}
	return payload
}
