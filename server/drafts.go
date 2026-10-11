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
	UploadID      string `json:"upload_id"`
	MimeType      string `json:"mime_type"`
}

func draftToolDefs() []map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	addresses := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	attachments := map[string]any{
		"type": "array",
		"description": "Files to attach, at most 20 MB together. Give each file one of upload_id, " +
			"url or content_base64. For a local file do not inline it as base64: get a REST token " +
			"with the token tool, upload it with curl -F file=@x.pdf to the uploads endpoint " +
			"(see the api tool) and pass the upload_id it answers with.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename":       str("File name as the recipient will see it, e.g. angebot.pdf. Optional with upload_id."),
				"upload_id":      str("Id of a file uploaded through the REST API (POST /mcp/api/v1/uploads). Preferred for local files."),
				"content_base64": str("The file's bytes, base64-encoded. Only for small generated files."),
				"url":            str("Instead of content_base64: a public https URL to download the file from."),
				"mime_type":      str("Optional. Detected from the bytes when left out."),
			},
		},
	}
	never := " This never sends anything: the draft waits in FreeScout until a person reviews and sends it."
	signature := " Do not add a signature: FreeScout appends the mailbox's signature when the mail goes out."
	status := map[string]any{"type": "string", "enum": []string{"active", "pending", "closed"}, "description": "Optional status to set along with sending."}

	return []map[string]any{
		{
			"name": "reply_to_conversation",
			"description": "Send a reply to the customer. This really sends an email — do not call it " +
				"to draft or to think out loud; use create_draft for a reply a person should check first. " +
				"The text is sent as written, so write it as the agent whose account is connected." + signature,
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          num("Conversation id."),
					"body":        str("The message to the customer, plain text."),
					"cc":          addresses("Optional Cc addresses."),
					"bcc":         addresses("Optional Bcc addresses."),
					"attachments": attachments,
					"status":      status,
				},
				"required": []string{"id", "body"},
			},
		},
		{
			"name": "send_draft",
			"description": "Send a draft you created, as it is stored in FreeScout now (a person may have " +
				"edited it there). This really sends an email: only call it when the person explicitly " +
				"asked to send this draft after reviewing it. Forward drafts are sent from FreeScout.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"draft_id": num("draft_id from create_draft or create_new_draft."),
					"status":   status,
				},
				"required": []string{"draft_id"},
			},
		},
		{
			"name": "forward_conversation",
			"description": "Forward a conversation to someone. By default this only saves a forward draft " +
				"in the conversation for a person to review and send; with send=true it is sent right away " +
				"(one recipient, no extra attachments), as FreeScout's Forward does.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":                  num("Conversation id."),
					"to":                  addresses("Recipient address(es); exactly one when send is true."),
					"cc":                  addresses("Optional Cc addresses."),
					"bcc":                 addresses("Optional Bcc addresses."),
					"body":                str("Optional note above the forwarded messages, plain text."),
					"include_attachments": map[string]any{"type": "boolean", "description": "Forward the conversation's attachments too. Default true."},
					"attachments":         attachments,
					"send":                map[string]any{"type": "boolean", "description": "Send right away instead of saving a draft. Default false."},
				},
				"required": []string{"id", "to"},
			},
		},
		{
			"name": "create_draft",
			"description": "Write a reply to a conversation as a draft." + never +
				" It shows up in the conversation and in the mailbox's Drafts folder." + signature,
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
				" It shows up in the mailbox's Drafts folder." + signature,
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

// loadAttachments decodes, downloads or fetches from the upload store every
// file named in list, and appends the files that came as real uploads. It
// returns the upload ids it used, for the caller to delete after success.
func (a *App) loadAttachments(list []attachmentArg, uploaded []draftFile, email string) ([]draftFile, []string, error) {
	if len(list)+len(uploaded) > maxDraftAttachments {
		return nil, nil, fmt.Errorf("at most %d attachments per draft", maxDraftAttachments)
	}
	files := make([]draftFile, 0, len(list)+len(uploaded))
	var used []string
	total := 0
	for _, f := range uploaded {
		if len(f.Data) == 0 {
			return nil, nil, fmt.Errorf("%s is empty", f.Name)
		}
		total += len(f.Data)
		if total > maxDraftAttachmentBytes {
			return nil, nil, fmt.Errorf("attachments are larger than %d MB together", maxDraftAttachmentBytes>>20)
		}
		files = append(files, f)
	}
	for i, item := range list {
		name := strings.TrimSpace(item.Filename)
		uploadID := strings.TrimSpace(item.UploadID)
		hasData, hasURL, hasUpload := item.ContentBase64 != "", strings.TrimSpace(item.URL) != "", uploadID != ""
		if name == "" && !hasUpload {
			return nil, nil, fmt.Errorf("attachment %d has no filename", i+1)
		}
		label := name
		if label == "" {
			label = uploadID
		}
		if n := btoi(hasData) + btoi(hasURL) + btoi(hasUpload); n != 1 {
			return nil, nil, fmt.Errorf("%s: give exactly one of upload_id, url or content_base64", label)
		}

		var data []byte
		mimeType := strings.TrimSpace(item.MimeType)
		if hasUpload {
			up, content, err := a.uploads.read(email, uploadID)
			if err != nil {
				return nil, nil, err
			}
			data = content
			if name == "" {
				name = up.Name
			}
			if mimeType == "" {
				mimeType = up.MimeType
			}
			used = append(used, uploadID)
		} else if hasData {
			decoded, err := decodeBase64(item.ContentBase64)
			if err != nil {
				return nil, nil, fmt.Errorf("%s is not valid base64", name)
			}
			data = decoded
		} else {
			downloaded, contentType, err := a.downloadAttachment(strings.TrimSpace(item.URL), maxDraftAttachmentBytes-total)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %v", name, err)
			}
			data = downloaded
			if mimeType == "" {
				mimeType = contentType
			}
		}
		if len(data) == 0 {
			return nil, nil, fmt.Errorf("%s is empty", name)
		}
		total += len(data)
		if total > maxDraftAttachmentBytes {
			return nil, nil, fmt.Errorf("attachments are larger than %d MB together", maxDraftAttachmentBytes>>20)
		}
		files = append(files, draftFile{Name: name, MimeType: mimeType, Data: data})
	}
	return files, used, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
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
