//go:build e2e

// End-to-end test against a real FreeScout. Run it with ci/e2e.sh.
//
// Every write is checked in the database afterwards: a tool that answers "ok"
// while nothing reached the helpdesk is the failure this exists to catch.
package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	freescoutURL = "http://127.0.0.1:8080"
	mcpURL       = "http://127.0.0.1:8081"
	oidcURL      = "http://127.0.0.1:8082"

	clientID    = oidcURL + "/client-metadata.json"
	redirectURI = "http://localhost/callback"
	verifier    = "e2e-code-verifier-0123456789-abcdefghij"

	signingKey = "e2e-signing-key-that-is-long-enough-000"

	// From FreeScout's app/Conversation.php and app/Thread.php.
	statusActive  = 1
	statusPending = 2
	statusClosed  = 3

	threadTypeCustomer = 1
	threadTypeMessage  = 2
	threadTypeNote     = 3

	personCustomer = 1
	personUser     = 2
)

type seed struct {
	MailboxID      int    `json:"mailbox_id"`
	ConversationID int    `json:"conversation_id"`
	CustomerEmail  string `json:"customer_email"`
	AdminEmail     string `json:"admin_email"`
	ColleagueEmail string `json:"colleague_email"`
}

var (
	fixture     seed
	fixtureOnce sync.Once
	tokenOnce   sync.Once
	accessToken string
)

func seeded(t *testing.T) seed {
	t.Helper()
	fixtureOnce.Do(func() {
		raw, err := os.ReadFile("../.stage/seed.json")
		if err != nil {
			t.Fatalf("no seed data — run ci/e2e.sh up first: %v", err)
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("seed data: %v", err)
		}
	})
	return fixture
}

func query(t *testing.T, sql string) string {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", "../compose.yml", "exec", "-T", "mariadb",
		"mariadb", "-u", "root", "-proot-for-tests", "freescout", "-N", "-B", "-e", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %q failed: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func queryInt(t *testing.T, sql string) int {
	t.Helper()
	raw := query(t, sql)
	if raw == "" || raw == "NULL" {
		return -1
	}
	value, err := strconv.Atoi(strings.Fields(raw)[0])
	if err != nil {
		t.Fatalf("query %q returned %q, not a number", sql, raw)
	}
	return value
}

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func connect(t *testing.T) string {
	t.Helper()
	tokenOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		browser := &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if strings.HasPrefix(req.URL.String(), redirectURI) {
					return http.ErrUseLastResponse
				}
				if len(via) > 12 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}

		q := url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"scope":                 {"mcp"},
			"state":                 {"e2e-state"},
			"code_challenge":        {challenge(verifier)},
			"code_challenge_method": {"S256"},
			"resource":              {mcpURL + "/mcp"},
		}
		res, err := browser.Get(mcpURL + "/oauth/authorize?" + q.Encode())
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("expected the consent page, got %d: %s", res.StatusCode, body)
		}
		if !strings.Contains(string(body), "post@levinkeller.de") {
			t.Fatalf("the consent page does not name the logged-in user: %s", body)
		}
		authzID := between(string(body), `name="authz_id" value="`, `"`)
		if authzID == "" {
			t.Fatalf("no authz_id on the consent page")
		}

		approve, err := browser.PostForm(mcpURL+"/oauth/approve", url.Values{"authz_id": {authzID}})
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		defer approve.Body.Close()
		if approve.StatusCode != http.StatusFound {
			raw, _ := io.ReadAll(approve.Body)
			t.Fatalf("approve did not redirect: %d %s", approve.StatusCode, raw)
		}
		location, _ := url.Parse(approve.Header.Get("Location"))
		if location.Query().Get("state") != "e2e-state" {
			t.Fatalf("state was not echoed back")
		}
		code := location.Query().Get("code")
		if code == "" {
			t.Fatalf("no code in %s", location)
		}

		form := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {clientID},
			"code":          {code},
			"redirect_uri":  {redirectURI},
			"code_verifier": {verifier},
			"resource":      {mcpURL + "/mcp"},
		}
		tokenRes, err := http.PostForm(mcpURL+"/oauth/token", form)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		defer tokenRes.Body.Close()
		var tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		raw, _ := io.ReadAll(tokenRes.Body)
		if tokenRes.StatusCode != 200 {
			t.Fatalf("token exchange: %d %s", tokenRes.StatusCode, raw)
		}
		_ = json.Unmarshal(raw, &tokens)
		if tokens.AccessToken == "" || tokens.RefreshToken == "" {
			t.Fatalf("no tokens in %s", raw)
		}
		accessToken = tokens.AccessToken
	})
	return accessToken
}

func rpc(t *testing.T, token string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", mcpURL+"/mcp", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rpc: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return res.StatusCode, decoded
}

func callTool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	status, body := rpc(t, connect(t), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if status != 200 {
		t.Fatalf("%s: HTTP %d", name, status)
	}
	result, _ := body["result"].(map[string]any)
	if result == nil {
		t.Fatalf("%s: no result in %v", name, body)
	}
	text := toolText(t, result)
	if result["isError"] == true {
		t.Fatalf("%s reported an error: %s", name, text)
	}
	return text
}

func callToolExpectingError(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	_, body := rpc(t, connect(t), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	result, _ := body["result"].(map[string]any)
	if result == nil || result["isError"] != true {
		t.Fatalf("%s was expected to fail, got %v", name, body)
	}
	return toolText(t, result)
}

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		raw, _ := json.Marshal(result)
		t.Fatalf("no content in %s", raw)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func TestBothModulesAreInstalledAndActive(t *testing.T) {
	for _, alias := range []string{"mcp", "zitadel"} {
		if got := queryInt(t, fmt.Sprintf("select active from modules where alias = '%s'", alias)); got != 1 {
			t.Fatalf("module %s is not active in the modules table (got %d)", alias, got)
		}
	}
}

func TestUnauthenticatedMcpPointsAtTheAuthorizationServer(t *testing.T) {
	req, _ := http.NewRequest("POST", mcpURL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
	challenge := res.Header.Get("WWW-Authenticate")
	for _, want := range []string{`error="invalid_token"`, "oauth-protected-resource/mcp", `scope="mcp"`} {
		if !strings.Contains(challenge, want) {
			t.Fatalf("challenge %q lacks %q", challenge, want)
		}
	}

	for _, path := range []string{
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		res, err := http.Get(mcpURL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d", path, res.StatusCode)
		}
	}
}

func TestForgedTokenIsRejected(t *testing.T) {
	claims, _ := json.Marshal(map[string]any{
		"k": "access", "s": "post@levinkeller.de", "c": clientID, "sc": "mcp",
		"e": time.Now().Add(time.Hour).UnixMilli(), "n": "forged",
	})
	body := "fs1." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte("a-different-key-that-is-long-enough-00"))
	mac.Write([]byte(body))
	forged := body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	if status, _ := rpc(t, forged, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); status != 401 {
		t.Fatalf("a token signed with the wrong key was accepted: HTTP %d", status)
	}

	// The real token still works, so the check above proves something.
	if status, _ := rpc(t, connect(t), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); status != 200 {
		t.Fatalf("the real token was rejected: HTTP %d", status)
	}
	_ = signingKey
}

func TestBridgeIsUnreachableFromOutsideThePod(t *testing.T) {
	for _, headers := range []map[string]string{
		{},
		{"X-Forwarded-For": "127.0.0.1"},
		{"X-Real-IP": "127.0.0.1"},
	} {
		req, _ := http.NewRequest("GET", freescoutURL+"/mcp-bridge/users?as_email=post@levinkeller.de", nil)
		req.Header.Set("X-Mcp-Bridge-Token", "bridge-token-for-tests")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatalf("the bridge answered %d from outside the pod (headers %v): %s", res.StatusCode, headers, body)
		}
	}
}

func TestToolsListOffersTheWholeSurface(t *testing.T) {
	status, body := rpc(t, connect(t), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if status != 200 {
		t.Fatalf("tools/list: HTTP %d", status)
	}
	result, _ := body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	found := map[string]bool{}
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		name, _ := tool["name"].(string)
		found[name] = true
	}
	for _, want := range []string{"list_mailboxes", "search_conversations", "get_conversation",
		"reply_to_conversation", "add_note", "set_status", "assign_conversation"} {
		if !found[want] {
			t.Fatalf("tool %s missing from tools/list", want)
		}
	}
}

func TestReadingTheHelpdesk(t *testing.T) {
	fixture := seeded(t)

	mailboxes := callTool(t, "list_mailboxes", map[string]any{})
	if !strings.Contains(mailboxes, "Ökohaus") {
		t.Fatalf("the seeded mailbox is missing: %s", mailboxes)
	}

	search := callTool(t, "search_conversations", map[string]any{"query": "Dach"})
	if !strings.Contains(search, "Lieferung Dach") {
		t.Fatalf("the seeded conversation was not found: %s", search)
	}

	conversation := callTool(t, "get_conversation", map[string]any{"id": fixture.ConversationID})
	if !strings.Contains(conversation, "wann kann das Dach geliefert werden?") {
		t.Fatalf("the customer's message is missing: %s", conversation)
	}
	if strings.Contains(conversation, "<div>") {
		t.Fatalf("the model is being handed raw HTML: %s", conversation)
	}
	if !strings.Contains(conversation, fixture.CustomerEmail) {
		t.Fatalf("the customer address is missing: %s", conversation)
	}
}

func TestReplyReallyLandsInTheHelpdesk(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID

	before := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeMessage))

	callTool(t, "reply_to_conversation", map[string]any{
		"id":     id,
		"body":   "Guten Tag,\n\ndas Dach kommt am Dienstag.\n\nViele Grüße",
		"status": "pending",
	})

	after := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeMessage))
	if after != before+1 {
		t.Fatalf("no reply thread was written (%d -> %d)", before, after)
	}

	body := query(t, fmt.Sprintf(
		"select body from threads where conversation_id = %d and type = %d order by id desc limit 1", id, threadTypeMessage))
	if !strings.Contains(body, "das Dach kommt am Dienstag.") {
		t.Fatalf("the reply body did not arrive intact: %q", body)
	}
	if !strings.Contains(body, "<br>") {
		t.Fatalf("line breaks were lost on the way into the mail: %q", body)
	}

	author := queryInt(t, fmt.Sprintf(
		"select created_by_user_id from threads where conversation_id = %d and type = %d order by id desc limit 1", id, threadTypeMessage))
	adminID := queryInt(t, fmt.Sprintf("select id from users where email = '%s'", fixture.AdminEmail))
	if author != adminID {
		t.Fatalf("the reply was written as user %d, expected the connected user %d", author, adminID)
	}

	if got := queryInt(t, fmt.Sprintf("select status from conversations where id = %d", id)); got != statusPending {
		t.Fatalf("the status did not follow the reply: got %d, want %d", got, statusPending)
	}
	if got := queryInt(t, fmt.Sprintf("select last_reply_from from conversations where id = %d", id)); got != personUser {
		t.Fatalf("last_reply_from is %d, expected the agent (%d)", got, personUser)
	}

	// FreeScout queues the outgoing mail rather than sending it inline, so the
	// most this can assert is that something was handed to the queue.
	deadline := time.Now().Add(30 * time.Second)
	for {
		queued := queryInt(t, "select count(*) from jobs") +
			queryInt(t, "select count(*) from failed_jobs") +
			queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d and send_status is not null", id, threadTypeMessage))
		if queued > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reply was stored but never handed to the mail queue")
		}
		time.Sleep(2 * time.Second)
	}
}

func TestNoteStaysInternal(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID

	messagesBefore := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeMessage))
	notesBefore := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeNote))

	callTool(t, "add_note", map[string]any{"id": id, "body": "Lieferant sagt Dienstag zu."})

	notesAfter := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeNote))
	messagesAfter := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = %d", id, threadTypeMessage))

	if notesAfter != notesBefore+1 {
		t.Fatalf("the note was not written (%d -> %d)", notesBefore, notesAfter)
	}
	if messagesAfter != messagesBefore {
		t.Fatalf("the note turned into a message to the customer (%d -> %d)", messagesBefore, messagesAfter)
	}
	body := query(t, fmt.Sprintf(
		"select body from threads where conversation_id = %d and type = %d order by id desc limit 1", id, threadTypeNote))
	if !strings.Contains(body, "Lieferant sagt Dienstag zu.") {
		t.Fatalf("the note body is wrong: %q", body)
	}
}

func TestStatusAndAssignment(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID

	callTool(t, "set_status", map[string]any{"id": id, "status": "closed"})
	if got := queryInt(t, fmt.Sprintf("select status from conversations where id = %d", id)); got != statusClosed {
		t.Fatalf("status is %d, want %d", got, statusClosed)
	}

	callTool(t, "assign_conversation", map[string]any{"id": id, "assignee_email": fixture.ColleagueEmail})
	colleagueID := queryInt(t, fmt.Sprintf("select id from users where email = '%s'", fixture.ColleagueEmail))
	if got := queryInt(t, fmt.Sprintf("select user_id from conversations where id = %d", id)); got != colleagueID {
		t.Fatalf("assigned to %d, expected %d", got, colleagueID)
	}

	callTool(t, "assign_conversation", map[string]any{"id": id, "assignee_email": ""})
	if got := queryInt(t, fmt.Sprintf("select user_id from conversations where id = %d", id)); got != -1 {
		t.Fatalf("unassigning left user_id = %d", got)
	}

	callTool(t, "set_status", map[string]any{"id": id, "status": "active"})
	if got := queryInt(t, fmt.Sprintf("select status from conversations where id = %d", id)); got != statusActive {
		t.Fatalf("status is %d, want %d", got, statusActive)
	}
}

func TestUnknownAssigneeIsRefusedWithoutTouchingTheConversation(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID
	before := queryInt(t, fmt.Sprintf("select coalesce(user_id, -1) from conversations where id = %d", id))

	text := callToolExpectingError(t, "assign_conversation", map[string]any{
		"id": id, "assignee_email": "niemand@example.org",
	})
	if !strings.Contains(text, "unknown assignee") {
		t.Fatalf("unhelpful error: %q", text)
	}
	if after := queryInt(t, fmt.Sprintf("select coalesce(user_id, -1) from conversations where id = %d", id)); after != before {
		t.Fatalf("the failed assignment changed the conversation (%d -> %d)", before, after)
	}
}

func TestMissingConversationIsAToolErrorNotACrash(t *testing.T) {
	text := callToolExpectingError(t, "get_conversation", map[string]any{"id": 999999})
	if !strings.Contains(text, "not found") {
		t.Fatalf("unhelpful error: %q", text)
	}
}

func TestWebLoginGoesThroughZitadel(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	noFollow := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := noFollow.Get(freescoutURL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("/login answered %d, expected a redirect to ZITADEL", res.StatusCode)
	}
	if location := res.Header.Get("Location"); !strings.Contains(location, "/zitadel/login") {
		t.Fatalf("/login redirected to %q", location)
	}

	// The escape hatch is the way back in when the login provider is down.
	local, err := noFollow.Get(freescoutURL + "/login?local=1")
	if err != nil {
		t.Fatal(err)
	}
	localBody, _ := io.ReadAll(local.Body)
	local.Body.Close()
	if local.StatusCode != 200 || !strings.Contains(string(localBody), "password") {
		t.Fatalf("/login?local=1 did not show the built-in form: %d", local.StatusCode)
	}

	landing, err := browser.Get(freescoutURL + "/zitadel/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(landing.Body)
	landing.Body.Close()
	if landing.StatusCode != 200 {
		t.Fatalf("the login flow ended with %d: %s", landing.StatusCode, body)
	}
	if strings.Contains(string(body), "kein aktives Benutzerkonto") {
		t.Fatalf("the login was refused: %s", body)
	}

	home, err := browser.Get(freescoutURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	homeBody, _ := io.ReadAll(home.Body)
	home.Body.Close()
	if home.StatusCode != 200 {
		t.Fatalf("the home page answered %d after the login", home.StatusCode)
	}
	if strings.Contains(string(homeBody), `name="password"`) {
		t.Fatalf("still on the login form after the ZITADEL round trip")
	}
	if !strings.Contains(string(homeBody), "Ökohaus") {
		t.Fatalf("the logged-in page does not show the mailbox: %s", truncate(string(homeBody)))
	}
}

// FreeScout turns PHP deprecations into exceptions, so a construct that is
// merely frowned upon in 8.5 takes a whole route down with it.
func TestNeitherModuleLoggedAnError(t *testing.T) {
	out, err := exec.Command("docker", "compose", "-f", "../compose.yml", "exec", "-T", "freescout",
		"sh", "-c", "grep -a 'production.ERROR' /data/storage/logs/laravel.log || true").CombinedOutput()
	if err != nil {
		t.Fatalf("reading the log failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Modules\\Mcp") || strings.Contains(line, "Modules\\Zitadel") ||
			strings.Contains(line, "[zitadel]") || strings.Contains(line, "[mcp]") {
			t.Errorf("module error in the FreeScout log:\n%s", truncateLine(line))
		}
	}
}

func truncateLine(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}
