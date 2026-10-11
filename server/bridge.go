package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

type Bridge struct {
	baseURL string
	token   string
	http    *http.Client
}

type bridgeError struct {
	Status  int
	Message string
}

func (e *bridgeError) Error() string {
	return fmt.Sprintf("freescout responded %d: %s", e.Status, e.Message)
}

func (b *Bridge) get(path string, query url.Values, actingUser string) (map[string]any, error) {
	u := b.baseURL + "/mcp-bridge/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	return b.do(req, actingUser)
}

func (b *Bridge) post(path string, body map[string]any, actingUser string) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", b.baseURL+"/mcp-bridge/"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return b.do(req, actingUser)
}

// postDraft sends the draft fields as JSON in the form field `payload` and the
// files as real uploads in `files[]`, so PHP streams them to disk and
// FreeScout stores them the way its own upload does. payload["attachments"]
// describes the files in the same order.
func (b *Bridge) postDraft(path string, payload map[string]any, files []draftFile, actingUser string) (map[string]any, error) {
	meta := make([]map[string]string, 0, len(files))
	for _, f := range files {
		meta = append(meta, map[string]string{"filename": f.Name, "mime_type": f.MimeType})
	}
	payload["attachments"] = meta
	fields, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("payload", string(fields)); err != nil {
		return nil, err
	}
	for _, f := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[]"; filename="%s"`, quoteFileName(f.Name)))
		header.Set("Content-Type", "application/octet-stream")
		part, err := form.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := form.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", b.baseURL+"/mcp-bridge/"+path, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	return b.do(req, actingUser)
}

// The name that counts travels in the payload; this one only has to parse.
func quoteFileName(name string) string {
	return strings.NewReplacer("\\", "_", `"`, "_", "\r", "_", "\n", "_").Replace(name)
}

func (b *Bridge) do(req *http.Request, actingUser string) (map[string]any, error) {
	req.Header.Set("X-Mcp-Bridge-Token", b.token)
	req.Header.Set("Accept", "application/json")
	if actingUser != "" {
		req.Header.Set("X-Mcp-Acting-User", actingUser)
	}

	res, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, &bridgeError{Status: res.StatusCode, Message: "unreadable response from FreeScout"}
	}
	if res.StatusCode >= 400 {
		msg, _ := decoded["error"].(string)
		if msg == "" {
			msg = strconv.Itoa(res.StatusCode)
		}
		return nil, &bridgeError{Status: res.StatusCode, Message: msg}
	}
	return decoded, nil
}

// The bridge wants an acting user on every call, so this one asks on behalf of
// the very address it is checking; the module answers 403 for an unknown one.
func (b *Bridge) knownUser(email string) (bool, error) {
	res, err := b.get("users", nil, email)
	if err != nil {
		var be *bridgeError
		if ok := asBridgeError(err, &be); ok && be.Status == 403 {
			return false, nil
		}
		return false, err
	}
	users, _ := res["users"].([]any)
	for _, entry := range users {
		user, _ := entry.(map[string]any)
		if user == nil {
			continue
		}
		if addr, _ := user["email"].(string); addr == email {
			return true, nil
		}
	}
	return false, nil
}

func asBridgeError(err error, target **bridgeError) bool {
	be, ok := err.(*bridgeError)
	if ok {
		*target = be
	}
	return ok
}
