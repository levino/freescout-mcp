package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Bridge talks to the FreeScout module over the pod's loopback interface.
// It is the only way this server touches the helpdesk: no SQL, no IMAP, no
// duplicated business logic.
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
		// A FreeScout stack trace is HTML; do not hand that to the model.
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

// knownUser reports whether this email belongs to an active FreeScout user.
// FreeScout's own user list is the access list for this server: whoever may
// not open the helpdesk may not drive it through Claude either.
func (b *Bridge) knownUser(email string) (bool, error) {
	// The bridge needs an acting user for every call, including this one, so
	// the lookup asks on behalf of the very address it is checking. An unknown
	// address is rejected by the module with 403.
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
