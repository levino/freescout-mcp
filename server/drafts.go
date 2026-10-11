package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Together, not per file: this is what ends up in one outgoing mail. The PHP
// side checks the same numbers again (modules/Mcp/Drafts.php).
const (
	maxDraftAttachmentBytes = 20 << 20
	maxDraftAttachments     = 20
)

// emailList takes either ["a@x", "b@y"] or "a@x, b@y"; models produce both.
type emailList []string

func (l *emailList) UnmarshalJSON(raw []byte) error {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		*l = list
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err != nil {
		return errors.New("addresses must be a string or a list of strings")
	}
	*l = nil
	for _, part := range strings.FieldsFunc(single, func(r rune) bool { return r == ',' || r == ';' }) {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

type attachmentArg struct {
	Filename      string `json:"filename"`
	ContentBase64 string `json:"content_base64"`
	URL           string `json:"url"`
	MimeType      string `json:"mime_type"`
}

type draftArgs struct {
	ID                  int64           `json:"id"`
	DraftID             int64           `json:"draft_id"`
	MailboxID           int64           `json:"mailbox_id"`
	To                  emailList       `json:"to"`
	Cc                  emailList       `json:"cc"`
	Bcc                 emailList       `json:"bcc"`
	Subject             string          `json:"subject"`
	Body                string          `json:"body"`
	Attachments         []attachmentArg `json:"attachments"`
	RemoveAttachmentIDs []int64         `json:"remove_attachment_ids"`
}

func draftToolDefs() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	addresses := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	attachments := map[string]any{
		"type": "array",
		"description": "Files to attach, at most 20 MB together. Give each file either content_base64 " +
			"or a public https url the server downloads it from.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename":       str("File name as the recipient will see it, e.g. angebot.pdf."),
				"content_base64": str("The file's bytes, base64-encoded."),
				"url":            str("Instead of content_base64: a public https URL to download the file from."),
				"mime_type":      str("Optional. Detected from the bytes when left out."),
			},
			"required": []string{"filename"},
		},
	}
	never := " This never sends anything: the draft waits in FreeScout until a person reviews and sends it."

	return []map[string]any{
		{
			"name": "create_draft",
			"description": "Write a reply to a conversation as a draft." + never +
				" It shows up in the conversation and in the mailbox's Drafts folder.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          num("Conversation id."),
					"body":        str("The reply to the customer, plain text."),
					"cc":          addresses("Optional Cc addresses."),
					"bcc":         addresses("Optional Bcc addresses."),
					"attachments": attachments,
				},
				"required": []string{"id", "body"},
			},
		},
		{
			"name": "create_new_draft",
			"description": "Write a new email, starting a new conversation, as a draft." + never +
				" It shows up in the mailbox's Drafts folder.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"mailbox_id":  num("The mailbox to send from later (see list_mailboxes)."),
					"to":          addresses("Recipient addresses."),
					"cc":          addresses("Optional Cc addresses."),
					"bcc":         addresses("Optional Bcc addresses."),
					"subject":     str("Subject line."),
					"body":        str("The message, plain text."),
					"attachments": attachments,
				},
				"required": []string{"mailbox_id", "to", "subject", "body"},
			},
		},
		{
			"name": "update_draft",
			"description": "Change a draft you created. Only the fields you pass change; attachments are " +
				"added, and removed by id. to and subject can only change on a new-conversation draft." + never,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"draft_id":              num("draft_id from create_draft or create_new_draft."),
					"to":                    addresses("New recipients (new-conversation drafts only)."),
					"cc":                    addresses("New Cc addresses; an empty list removes them."),
					"bcc":                   addresses("New Bcc addresses; an empty list removes them."),
					"subject":               str("New subject (new-conversation drafts only)."),
					"body":                  str("New text, plain text. Replaces the old text."),
					"attachments":           attachments,
					"remove_attachment_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Ids of this draft's attachments to remove."},
				},
				"required": []string{"draft_id"},
			},
		},
		{
			"name": "delete_draft",
			"description": "Discard a draft you created. A new-conversation draft disappears with its " +
				"conversation; a reply draft leaves the conversation as it was.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"draft_id": num("draft_id from create_draft or create_new_draft.")},
				"required":   []string{"draft_id"},
			},
		},
	}
}

func isDraftTool(name string) bool {
	switch name {
	case "create_draft", "create_new_draft", "update_draft", "delete_draft":
		return true
	}
	return false
}

func (a *App) dispatchDraftTool(name string, rawArgs json.RawMessage, email string) map[string]any {
	var args draftArgs
	present := map[string]json.RawMessage{}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &present); err != nil {
			return toolError("arguments are not valid JSON: " + err.Error())
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return toolError("arguments: " + err.Error())
		}
	}

	switch name {
	case "create_draft":
		if args.ID <= 0 || strings.TrimSpace(args.Body) == "" {
			return toolError("id and body are required")
		}
		files, err := a.loadAttachments(args.Attachments)
		if err != nil {
			return toolError(err.Error())
		}
		payload := map[string]any{"body": args.Body, "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc)}
		return a.toolResult(a.bridge.postDraft("conversations/"+strconv.FormatInt(args.ID, 10)+"/draft", payload, files, email))

	case "create_new_draft":
		if args.MailboxID <= 0 || len(args.To) == 0 || strings.TrimSpace(args.Subject) == "" || strings.TrimSpace(args.Body) == "" {
			return toolError("mailbox_id, to, subject and body are required")
		}
		files, err := a.loadAttachments(args.Attachments)
		if err != nil {
			return toolError(err.Error())
		}
		payload := map[string]any{
			"mailbox_id": args.MailboxID, "to": nonNil(args.To), "cc": nonNil(args.Cc), "bcc": nonNil(args.Bcc),
			"subject": args.Subject, "body": args.Body,
		}
		return a.toolResult(a.bridge.postDraft("drafts", payload, files, email))

	case "update_draft":
		if args.DraftID <= 0 {
			return toolError("draft_id is required")
		}
		files, err := a.loadAttachments(args.Attachments)
		if err != nil {
			return toolError(err.Error())
		}
		// Only what was passed, so the bridge can tell "leave it" from "empty it".
		payload := map[string]any{}
		if _, ok := present["body"]; ok {
			if strings.TrimSpace(args.Body) == "" {
				return toolError("body cannot be empty; use delete_draft to discard the draft")
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
		if len(payload) == 0 && len(files) == 0 {
			return toolError("nothing to change")
		}
		return a.toolResult(a.bridge.postDraft("drafts/"+strconv.FormatInt(args.DraftID, 10), payload, files, email))

	case "delete_draft":
		if args.DraftID <= 0 {
			return toolError("draft_id is required")
		}
		return a.toolResult(a.bridge.post("drafts/"+strconv.FormatInt(args.DraftID, 10)+"/delete", map[string]any{}, email))
	}
	return toolError("unknown tool: " + name)
}

func nonNil(list emailList) []string {
	if list == nil {
		return []string{}
	}
	return list
}

type draftFile struct {
	Name     string
	MimeType string
	Data     []byte
}

// loadAttachments decodes or downloads every file before the bridge is
// called, so a bad file fails the call without leaving half a draft.
func (a *App) loadAttachments(list []attachmentArg) ([]draftFile, error) {
	if len(list) > maxDraftAttachments {
		return nil, fmt.Errorf("at most %d attachments per draft", maxDraftAttachments)
	}
	files := make([]draftFile, 0, len(list))
	total := 0
	for i, item := range list {
		name := strings.TrimSpace(item.Filename)
		if name == "" {
			return nil, fmt.Errorf("attachment %d has no filename", i+1)
		}
		hasData, hasURL := item.ContentBase64 != "", strings.TrimSpace(item.URL) != ""
		if hasData == hasURL {
			return nil, fmt.Errorf("%s: give either content_base64 or url", name)
		}

		var data []byte
		mimeType := strings.TrimSpace(item.MimeType)
		if hasData {
			decoded, err := decodeBase64(item.ContentBase64)
			if err != nil {
				return nil, fmt.Errorf("%s is not valid base64", name)
			}
			data = decoded
		} else {
			downloaded, contentType, err := a.downloadAttachment(strings.TrimSpace(item.URL), maxDraftAttachmentBytes-total)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", name, err)
			}
			data = downloaded
			if mimeType == "" {
				mimeType = contentType
			}
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%s is empty", name)
		}
		total += len(data)
		if total > maxDraftAttachmentBytes {
			return nil, fmt.Errorf("attachments are larger than %d MB together", maxDraftAttachmentBytes>>20)
		}
		files = append(files, draftFile{Name: name, MimeType: mimeType, Data: data})
	}
	return files, nil
}

// Models wrap long base64 and sometimes use the URL alphabet or drop padding.
func decodeBase64(s string) ([]byte, error) {
	if i := strings.Index(s, ";base64,"); strings.HasPrefix(s, "data:") && i > 0 {
		s = s[i+len(";base64,"):]
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := enc.DecodeString(s); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("invalid base64")
}

var errBlockedAddress = errors.New("refusing to download from a non-public address")

// downloadAttachment fetches a file from the internet for a draft. The sidecar
// sits next to the bridge and inside the cluster, so the address is checked at
// connect time — after DNS, for every redirect — and never reaches loopback,
// private or link-local ranges. INSECURE_ALLOW_LOCAL_CLIENTS lifts that for tests.
func (a *App) downloadAttachment(raw string, limit int) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, "", errors.New("not a usable URL")
	}
	if u.Scheme != "https" && !(a.cimdAllowLocal && u.Scheme == "http") {
		return nil, "", errors.New("only https URLs are downloaded")
	}

	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if a.cimdAllowLocal {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || blockedIP(ip) {
				return errBlockedAddress
			}
			return nil
		},
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !(a.cimdAllowLocal && req.URL.Scheme == "http") {
				return errors.New("redirected away from https")
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "freescout-mcp-attachments/1")

	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errBlockedAddress) {
			return nil, "", errBlockedAddress
		}
		return nil, "", errors.New("download failed: " + err.Error())
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, "", fmt.Errorf("download failed: HTTP %d", res.StatusCode)
	}
	if res.ContentLength > int64(limit) {
		return nil, "", fmt.Errorf("attachments are larger than %d MB together", maxDraftAttachmentBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		return nil, "", errors.New("download failed: " + err.Error())
	}
	if len(data) > limit {
		return nil, "", fmt.Errorf("attachments are larger than %d MB together", maxDraftAttachmentBytes>>20)
	}

	contentType := ""
	if mediaType, _, err := mime.ParseMediaType(res.Header.Get("Content-Type")); err == nil {
		contentType = mediaType
	}
	return data, contentType, nil
}
