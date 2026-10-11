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
	"mime/multipart"
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

	stateDraft     = 1
	statePublished = 2
	folderDrafts   = 30
)

type seed struct {
	MailboxID      int    `json:"mailbox_id"`
	ConversationID int    `json:"conversation_id"`
	CustomerEmail  string `json:"customer_email"`
	AdminEmail     string `json:"admin_email"`
	ColleagueEmail string `json:"colleague_email"`
	ThreadID       int    `json:"thread_id"`
	TextAttachment int    `json:"text_attachment_id"`
	ImgAttachment  int    `json:"image_attachment_id"`
	ImageBase64    string `json:"image_base64"`
	ColleagueDraft int    `json:"colleague_draft_id"`
	SecondMailbox  int    `json:"second_mailbox_id"`
	MergeConv      int    `json:"merge_conversation_id"`
	MoveConv       int    `json:"move_conversation_id"`
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

func callToolRaw(t *testing.T, name string, args map[string]any) map[string]any {
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
	return result
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
		"reply_to_conversation", "add_note", "set_status", "assign_conversation",
		"create_draft", "create_new_draft", "update_draft", "delete_draft",
		"send_draft", "forward_conversation", "find_customers", "move_conversation", "merge_conversations",
		"token", "api"} {
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

func TestAttachmentsAreListedAndDownloadable(t *testing.T) {
	fixture := seeded(t)

	conversation := callTool(t, "get_conversation", map[string]any{"id": fixture.ConversationID})
	for _, want := range []string{"angebot.txt", "dach.png", "image/png"} {
		if !strings.Contains(conversation, want) {
			t.Fatalf("get_conversation does not list %q: %s", want, truncate(conversation))
		}
	}

	text := callToolRaw(t, "get_attachment", map[string]any{"id": fixture.TextAttachment})
	textContent := contentOf(t, text)
	if len(textContent) != 1 || textContent[0]["type"] != "text" {
		t.Fatalf("the text file did not come back as text: %v", text)
	}
	if body, _ := textContent[0]["text"].(string); !strings.Contains(body, "Angebot über 5 m²") {
		t.Fatalf("the text file lost its content: %q", body)
	}

	image := callToolRaw(t, "get_attachment", map[string]any{"id": fixture.ImgAttachment})
	imageContent := contentOf(t, image)
	if len(imageContent) != 2 || imageContent[1]["type"] != "image" {
		t.Fatalf("the image did not come back as an image: %v", image)
	}
	if data, _ := imageContent[1]["data"].(string); data != fixture.ImageBase64 {
		t.Fatalf("the image bytes changed on the way")
	}
	if mime, _ := imageContent[1]["mimeType"].(string); mime != "image/png" {
		t.Fatalf("wrong mime type: %q", mime)
	}

	missing := callToolRaw(t, "get_attachment", map[string]any{"id": 9999})
	if missing["isError"] != true {
		t.Fatalf("an unknown attachment was not an error: %v", missing)
	}
}

func contentOf(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	raw, _ := result["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		out = append(out, item)
	}
	return out
}

func TestSettingsPageShowsTheEndpoint(t *testing.T) {
	browser := signedInBrowser(t)

	res, err := browser.Get(freescoutURL + "/app-settings/mcp")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("the settings section answered %d: %s", res.StatusCode, truncate(string(body)))
	}
	page := string(body)
	if !strings.Contains(page, freescoutURL+"/mcp") {
		t.Fatalf("the page does not show the endpoint address: %s", truncate(page))
	}
	if !strings.Contains(page, "Running") {
		t.Fatalf("the page does not report the server as running: %s", truncate(page))
	}
	if !strings.Contains(page, "Token configured") {
		t.Fatalf("the page does not report the bridge token: %s", truncate(page))
	}

	sidebar, err := browser.Get(freescoutURL + "/app-settings")
	if err != nil {
		t.Fatal(err)
	}
	sidebarBody, _ := io.ReadAll(sidebar.Body)
	sidebar.Body.Close()
	if !strings.Contains(string(sidebarBody), "app-settings/mcp") {
		t.Fatalf("the settings menu does not link the section: %s", truncate(string(sidebarBody)))
	}
}

func signedInBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	res, err := browser.Get(freescoutURL + "/zitadel/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || strings.Contains(string(body), `name="password"`) {
		t.Fatalf("could not sign in: %d %s", res.StatusCode, truncate(string(body)))
	}
	return browser
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

// ---- Drafts ----------------------------------------------------------------

type draftResult struct {
	Draft struct {
		DraftID         int    `json:"draft_id"`
		ConversationID  int    `json:"conversation_id"`
		NewConversation bool   `json:"new_conversation"`
		Sent            bool   `json:"sent"`
		URL             string `json:"url"`
		Attachments     []struct {
			ID       int    `json:"id"`
			Name     string `json:"name"`
			MimeType string `json:"mime_type"`
			Size     int    `json:"size"`
		} `json:"attachments"`
	} `json:"draft"`
}

func callDraftTool(t *testing.T, name string, args map[string]any) draftResult {
	t.Helper()
	var out draftResult
	text := callTool(t, name, args)
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s answered something that is not JSON: %s", name, truncate(text))
	}
	return out
}

// Everything that could mean "this went out": a queued job, a send status on
// the thread, or the thread being published.
func outgoingMail(t *testing.T) int {
	t.Helper()
	return queryInt(t, "select count(*) from jobs where payload like '%SendReplyToCustomer%'") +
		queryInt(t, "select count(*) from failed_jobs where payload like '%SendReplyToCustomer%'")
}

func attachmentBytes(t *testing.T, id int) []byte {
	t.Helper()
	result := callToolRaw(t, "get_attachment", map[string]any{"id": id})
	if result["isError"] == true {
		t.Fatalf("attachment %d cannot be read back: %s", id, toolText(t, result))
	}
	for _, item := range contentOf(t, result) {
		if data, ok := item["data"].(string); ok {
			raw, _ := base64.StdEncoding.DecodeString(data)
			return raw
		}
		if res, ok := item["resource"].(map[string]any); ok {
			raw, _ := base64.StdEncoding.DecodeString(res["blob"].(string))
			return raw
		}
	}
	text := toolText(t, result)
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return []byte(text[i+2:])
	}
	return nil
}

func TestReplyDraftIsAFreeScoutDraftAndNothingIsSent(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID
	mailBefore := outgoingMail(t)
	statusBefore := queryInt(t, fmt.Sprintf("select status from conversations where id = %d", id))
	pdf := []byte("%PDF-1.4\n% Angebot über 5 m²\n")

	draft := callDraftTool(t, "create_draft", map[string]any{
		"id":   id,
		"body": "Guten Tag,\n\nanbei das Angebot.\n\nViele Grüße",
		"cc":   []string{"statik@example.org"},
		"bcc":  "archiv@example.org",
		"attachments": []map[string]any{
			{"filename": "angebot.pdf", "content_base64": base64.StdEncoding.EncodeToString(pdf)},
			{"filename": "dach skizze.png", "content_base64": fixture.ImageBase64},
		},
	})
	threadID := draft.Draft.DraftID
	if threadID <= 0 || draft.Draft.ConversationID != id || draft.Draft.NewConversation || draft.Draft.Sent {
		t.Fatalf("unexpected answer: %+v", draft)
	}

	row := query(t, fmt.Sprintf(
		"select state, type, created_by_user_id, source_via, has_attachments, coalesce(send_status, 'null') from threads where id = %d", threadID))
	adminID := queryInt(t, fmt.Sprintf("select id from users where email = '%s'", fixture.AdminEmail))
	want := fmt.Sprintf("%d\t%d\t%d\t%d\t1\tnull", stateDraft, threadTypeMessage, adminID, personUser)
	if row != want {
		t.Fatalf("the draft thread is not stored like FreeScout's own drafts:\n got %q\nwant %q", row, want)
	}
	if to := query(t, fmt.Sprintf("select `to` from threads where id = %d", threadID)); !strings.Contains(to, fixture.CustomerEmail) {
		t.Fatalf("the draft is not addressed to the customer: %q", to)
	}
	if cc := query(t, fmt.Sprintf("select cc from threads where id = %d", threadID)); !strings.Contains(cc, "statik@example.org") {
		t.Fatalf("cc missing: %q", cc)
	}
	if bcc := query(t, fmt.Sprintf("select bcc from threads where id = %d", threadID)); !strings.Contains(bcc, "archiv@example.org") {
		t.Fatalf("bcc missing: %q", bcc)
	}
	if body := query(t, fmt.Sprintf("select body from threads where id = %d", threadID)); !strings.Contains(body, "anbei das Angebot.") || !strings.Contains(body, "<br>") {
		t.Fatalf("body not stored as FreeScout HTML: %q", body)
	}
	if got := queryInt(t, fmt.Sprintf(
		"select count(*) from conversation_folder cf join folders f on f.id = cf.folder_id where cf.conversation_id = %d and f.type = %d", id, folderDrafts)); got != 1 {
		t.Fatalf("the conversation is not in the Drafts folder")
	}

	if len(draft.Draft.Attachments) != 2 {
		t.Fatalf("expected two attachments, got %+v", draft.Draft.Attachments)
	}
	for _, a := range draft.Draft.Attachments {
		dbRow := query(t, fmt.Sprintf("select thread_id, embedded, user_id, file_dir is not null from attachments where id = %d", a.ID))
		if dbRow != fmt.Sprintf("%d\t0\t%d\t1", threadID, adminID) {
			t.Fatalf("attachment %s not bound to the draft like an editor upload: %q", a.Name, dbRow)
		}
	}
	byName := map[string]int{}
	for _, a := range draft.Draft.Attachments {
		byName[a.Name] = a.ID
	}
	if byName["angebot.pdf"] == 0 || byName["dach_skizze.png"] == 0 {
		t.Fatalf("file names not as FreeScout stores them: %+v", draft.Draft.Attachments)
	}
	if got := attachmentBytes(t, byName["angebot.pdf"]); string(got) != string(pdf) {
		t.Fatalf("the pdf on disk differs from what was sent: %q", got)
	}
	if mime := query(t, fmt.Sprintf("select mime_type from attachments where id = %d", byName["dach_skizze.png"])); mime != "image/png" {
		t.Fatalf("mime type not sniffed: %q", mime)
	}

	// get_conversation shows it as a draft, with its id for update_draft.
	conversation := callTool(t, "get_conversation", map[string]any{"id": id})
	if !strings.Contains(conversation, `"draft":true`) || !strings.Contains(conversation, "anbei das Angebot.") {
		t.Fatalf("get_conversation does not show the draft: %s", truncate(conversation))
	}

	// FreeScout's own conversation page shows it.
	page := fetchPage(t, fmt.Sprintf("%s/conversation/%d", freescoutURL, id))
	for _, want := range []string{"anbei das Angebot.", fmt.Sprintf(`class="thread thread-type-draft" id="thread-%d"`, threadID), "dach_skizze.png"} {
		if !strings.Contains(page, want) {
			t.Fatalf("FreeScout's conversation page does not show the draft (%q missing)", want)
		}
	}

	// update: new text, one file out, one file in.
	updated := callDraftTool(t, "update_draft", map[string]any{
		"draft_id":              threadID,
		"body":                  "Guten Tag,\n\nanbei das korrigierte Angebot.",
		"remove_attachment_ids": []int{byName["angebot.pdf"]},
		"attachments":           []map[string]any{{"filename": "angebot-v2.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("Version 2"))}},
	})
	names := []string{}
	for _, a := range updated.Draft.Attachments {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "dach_skizze.png,angebot-v2.txt" {
		t.Fatalf("attachments after update: %v", names)
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from attachments where id = %d", byName["angebot.pdf"])); got != 0 {
		t.Fatalf("the removed attachment is still there")
	}
	if body := query(t, fmt.Sprintf("select body from threads where id = %d", threadID)); !strings.Contains(body, "korrigierte") {
		t.Fatalf("body not updated: %q", body)
	}
	if cc := query(t, fmt.Sprintf("select cc from threads where id = %d", threadID)); !strings.Contains(cc, "statik@example.org") {
		t.Fatalf("an update without cc changed cc: %q", cc)
	}

	if text := callToolExpectingError(t, "update_draft", map[string]any{"draft_id": threadID, "subject": "neu"}); !strings.Contains(text, "subject") {
		t.Fatalf("a reply draft accepted a subject: %q", text)
	}

	if after := outgoingMail(t); after != mailBefore {
		t.Fatalf("a draft put mail into the queue (%d -> %d)", mailBefore, after)
	}
	if got := queryInt(t, fmt.Sprintf("select state from threads where id = %d", threadID)); got != stateDraft {
		t.Fatalf("the draft is no longer a draft: state %d", got)
	}
	if got := queryInt(t, fmt.Sprintf("select status from conversations where id = %d", id)); got != statusBefore {
		t.Fatalf("a draft changed the conversation status (%d -> %d)", statusBefore, got)
	}

	// delete: thread and files go, the conversation stays, and it stays in
	// Drafts because the colleague's draft is still there.
	callTool(t, "delete_draft", map[string]any{"draft_id": threadID})
	if got := queryInt(t, fmt.Sprintf("select count(*) from threads where id = %d", threadID)); got != 0 {
		t.Fatalf("the draft thread survived delete_draft")
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from attachments where thread_id = %d", threadID)); got != 0 {
		t.Fatalf("the draft's attachments survived delete_draft")
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from conversations where id = %d", id)); got != 1 {
		t.Fatalf("deleting a reply draft deleted the conversation")
	}
	if got := queryInt(t, fmt.Sprintf(
		"select count(*) from conversation_folder cf join folders f on f.id = cf.folder_id where cf.conversation_id = %d and f.type = %d", id, folderDrafts)); got != 1 {
		t.Fatalf("the conversation left Drafts although the colleague's draft is still open")
	}
}

func TestSomeoneElsesDraftIsOffLimits(t *testing.T) {
	fixture := seeded(t)
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"update_draft", map[string]any{"draft_id": fixture.ColleagueDraft, "body": "überschrieben"}},
		{"delete_draft", map[string]any{"draft_id": fixture.ColleagueDraft}},
	} {
		if text := callToolExpectingError(t, call.name, call.args); !strings.Contains(text, "someone else") {
			t.Fatalf("%s: unexpected error %q", call.name, text)
		}
	}
	if body := query(t, fmt.Sprintf("select body from threads where id = %d", fixture.ColleagueDraft)); body != "Entwurf der Kollegin" {
		t.Fatalf("the colleague's draft was changed: %q", body)
	}
}

func TestPublishedThreadsAreNotDrafts(t *testing.T) {
	fixture := seeded(t)
	if text := callToolExpectingError(t, "update_draft", map[string]any{"draft_id": fixture.ThreadID, "body": "x"}); !strings.Contains(text, "not a draft") {
		t.Fatalf("unexpected error %q", text)
	}
}

func TestNewConversationDraft(t *testing.T) {
	fixture := seeded(t)
	mailBefore := outgoingMail(t)
	conversationsBefore := queryInt(t, "select count(*) from conversations")

	draft := callDraftTool(t, "create_new_draft", map[string]any{
		"mailbox_id": fixture.MailboxID,
		"to":         []string{"architekt@example.org", "bauamt@example.org"},
		"cc":         "statik@example.org",
		"subject":    "Bauantrag Ökohaus",
		"body":       "Sehr geehrte Damen und Herren,\n\nanbei die Unterlagen.",
		"attachments": []map[string]any{
			// Downloaded by the MCP server; the fake login provider is the
			// only other web server in the pod.
			{"filename": "unterlagen.json", "url": oidcURL + "/.well-known/openid-configuration"},
		},
	})
	if !draft.Draft.NewConversation || draft.Draft.Sent || draft.Draft.ConversationID <= 0 {
		t.Fatalf("unexpected answer: %+v", draft)
	}
	cid, tid := draft.Draft.ConversationID, draft.Draft.DraftID

	conv := query(t, fmt.Sprintf(
		"select state, type, source_via, coalesce(customer_id, 'null'), has_attachments, mailbox_id from conversations where id = %d", cid))
	if conv != fmt.Sprintf("%d\t1\t%d\tnull\t1\t%d", stateDraft, personUser, fixture.MailboxID) {
		t.Fatalf("the conversation is not stored like FreeScout's own new-conversation drafts: %q", conv)
	}
	if subject := query(t, fmt.Sprintf("select subject from conversations where id = %d", cid)); subject != "Bauantrag Ökohaus" {
		t.Fatalf("subject %q", subject)
	}
	thread := query(t, fmt.Sprintf("select state, first, has_attachments from threads where id = %d", tid))
	if thread != fmt.Sprintf("%d\t1\t1", stateDraft) {
		t.Fatalf("thread %q", thread)
	}
	to := query(t, fmt.Sprintf("select `to` from threads where id = %d", tid))
	if !strings.Contains(to, "architekt@example.org") || !strings.Contains(to, "bauamt@example.org") {
		t.Fatalf("recipients %q", to)
	}
	if got := queryInt(t, fmt.Sprintf(
		"select count(*) from conversation_folder cf join folders f on f.id = cf.folder_id where cf.conversation_id = %d and f.type = %d", cid, folderDrafts)); got != 1 {
		t.Fatalf("the new draft is not in the Drafts folder")
	}
	if len(draft.Draft.Attachments) != 1 || !strings.Contains(string(attachmentBytes(t, draft.Draft.Attachments[0].ID)), `"issuer"`) {
		t.Fatalf("the downloaded attachment is wrong: %+v", draft.Draft.Attachments)
	}

	// A draft must not leak into search, which only lists real conversations.
	if search := callTool(t, "search_conversations", map[string]any{"query": "Bauantrag"}); strings.Contains(search, "Bauantrag Ökohaus") {
		t.Fatalf("an unsent draft shows up in search: %s", truncate(search))
	}

	// FreeScout opens it in its "New Conversation" editor.
	page := fetchPage(t, fmt.Sprintf("%s/conversation/%d", freescoutURL, cid))
	if !strings.Contains(page, "Bauantrag Ökohaus") || !strings.Contains(page, "architekt@example.org") {
		t.Fatalf("FreeScout does not show the new-conversation draft")
	}

	callDraftTool(t, "update_draft", map[string]any{"draft_id": tid, "subject": "Bauantrag Ökohaus (Ergänzung)", "to": "architekt@example.org"})
	if subject := query(t, fmt.Sprintf("select subject from conversations where id = %d", cid)); subject != "Bauantrag Ökohaus (Ergänzung)" {
		t.Fatalf("subject not updated: %q", subject)
	}
	if to := query(t, fmt.Sprintf("select `to` from threads where id = %d", tid)); strings.Contains(to, "bauamt") {
		t.Fatalf("recipients not updated: %q", to)
	}

	if after := outgoingMail(t); after != mailBefore {
		t.Fatalf("a draft put mail into the queue")
	}

	callTool(t, "delete_draft", map[string]any{"draft_id": tid})
	if got := queryInt(t, fmt.Sprintf("select count(*) from conversations where id = %d", cid)); got != 0 {
		t.Fatalf("discarding a new-conversation draft left its conversation behind")
	}
	if got := queryInt(t, "select count(*) from conversations"); got != conversationsBefore {
		t.Fatalf("conversation count %d -> %d", conversationsBefore, got)
	}
}

func TestDraftInputIsCheckedBeforeAnythingIsWritten(t *testing.T) {
	fixture := seeded(t)
	threadsBefore := queryInt(t, "select count(*) from threads")
	attachmentsBefore := queryInt(t, "select count(*) from attachments")

	for _, args := range []map[string]any{
		{"id": fixture.ConversationID, "body": "x", "cc": "kein-mail"},
		{"id": fixture.ConversationID, "body": "x", "cc": []string{"Kirsten Kundin <kundin@example.com>"}},
		{"id": fixture.ConversationID, "body": "x", "attachments": []map[string]any{{"filename": "a", "url": "http://127.0.0.1:9/nothing-listens"}}},
		{"id": 999999, "body": "x"},
	} {
		callToolExpectingError(t, "create_draft", args)
	}
	callToolExpectingError(t, "create_new_draft", map[string]any{
		"mailbox_id": 999999, "to": "a@example.org", "subject": "s", "body": "b",
	})

	if got := queryInt(t, "select count(*) from threads"); got != threadsBefore {
		t.Fatalf("a refused draft left threads behind (%d -> %d)", threadsBefore, got)
	}
	if got := queryInt(t, "select count(*) from attachments"); got != attachmentsBefore {
		t.Fatalf("a refused draft left attachments behind")
	}
}

// 19 MB through the whole path: MCP call, sidecar, multipart to the bridge,
// FreeScout's storage. Guards body limits and PHP's memory limit.
func TestLargeAttachmentFitsThroughTheWholePath(t *testing.T) {
	fixture := seeded(t)
	data := make([]byte, 19<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}

	draft := callDraftTool(t, "create_draft", map[string]any{
		"id": fixture.ConversationID, "body": "Große Datei anbei",
		"attachments": []map[string]any{{"filename": "plaene.bin", "content_base64": base64.StdEncoding.EncodeToString(data)}},
	})
	if len(draft.Draft.Attachments) != 1 || draft.Draft.Attachments[0].Size != len(data) {
		t.Fatalf("size on disk differs: %+v", draft.Draft.Attachments)
	}
	callTool(t, "delete_draft", map[string]any{"draft_id": draft.Draft.DraftID})

	tooBig := base64.StdEncoding.EncodeToString(make([]byte, 21<<20))
	text := callToolExpectingError(t, "create_draft", map[string]any{
		"id": fixture.ConversationID, "body": "x",
		"attachments": []map[string]any{{"filename": "zu-gross.bin", "content_base64": tooBig}},
	})
	if !strings.Contains(text, "20 MB") {
		t.Fatalf("unhelpful error: %q", text)
	}
}

func fetchPage(t *testing.T, address string) string {
	t.Helper()
	res, err := signedInBrowser(t).Get(address)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("%s answered %d", address, res.StatusCode)
	}
	return string(body)
}

// ---- Sending, forwarding, search, customers, move, merge -------------------

const (
	stateDeleted   = 3
	subtypeForward = 1
)

// waitForMail waits until FreeScout has handed a message for this thread to
// its mail queue (there is no SMTP server, so it goes no further).
func waitForMail(t *testing.T, threadID int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	needle := fmt.Sprintf("%%i:%d;%%", threadID)
	for {
		queued := queryInt(t, fmt.Sprintf("select count(*) from jobs where payload like '%%SendReplyToCustomer%%' and payload like '%s'", needle)) +
			queryInt(t, fmt.Sprintf("select count(*) from failed_jobs where payload like '%%SendReplyToCustomer%%' and payload like '%s'", needle)) +
			queryInt(t, fmt.Sprintf("select count(*) from threads where id = %d and send_status is not null", threadID))
		if queued > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread %d was never handed to the mail queue", threadID)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestReplyWithCcAndAttachment(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID

	text := callTool(t, "reply_to_conversation", map[string]any{
		"id":          id,
		"body":        "Anbei der Lieferschein.",
		"cc":          []string{"statik@example.org"},
		"bcc":         "archiv@example.org",
		"attachments": []map[string]any{{"filename": "lieferschein.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("Lieferschein 42"))}},
	})
	var out struct {
		ThreadID    int `json:"thread_id"`
		Attachments []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"attachments"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	if out.ThreadID <= 0 || len(out.Attachments) != 1 {
		t.Fatalf("unexpected answer: %s", text)
	}
	row := query(t, fmt.Sprintf("select state, type, has_attachments from threads where id = %d", out.ThreadID))
	if row != fmt.Sprintf("%d\t%d\t1", statePublished, threadTypeMessage) {
		t.Fatalf("reply thread %q", row)
	}
	if cc := query(t, fmt.Sprintf("select cc from threads where id = %d", out.ThreadID)); !strings.Contains(cc, "statik@example.org") {
		t.Fatalf("cc missing: %q", cc)
	}
	if bcc := query(t, fmt.Sprintf("select bcc from threads where id = %d", out.ThreadID)); !strings.Contains(bcc, "archiv@example.org") {
		t.Fatalf("bcc missing: %q", bcc)
	}
	if got := attachmentBytes(t, out.Attachments[0].ID); string(got) != "Lieferschein 42" {
		t.Fatalf("attachment content %q", got)
	}
	waitForMail(t, out.ThreadID)
}

func TestSendDraftSendsWhatIsStored(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID
	countBefore := queryInt(t, fmt.Sprintf("select threads_count from conversations where id = %d", id))

	draft := callDraftTool(t, "create_draft", map[string]any{
		"id": id, "body": "Entwurf, geprüft und freigegeben.",
		"attachments": []map[string]any{{"filename": "freigabe.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("ok"))}},
	})
	tid := draft.Draft.DraftID
	// A person edits the draft in FreeScout before it goes out.
	query(t, fmt.Sprintf("update threads set body = 'Von Hand korrigiert.' where id = %d", tid))

	callTool(t, "send_draft", map[string]any{"draft_id": tid})

	if row := query(t, fmt.Sprintf("select state, type, body from threads where id = %d", tid)); row != fmt.Sprintf("%d\t%d\tVon Hand korrigiert.", statePublished, threadTypeMessage) {
		t.Fatalf("the sent draft is %q", row)
	}
	if got := queryInt(t, fmt.Sprintf("select threads_count from conversations where id = %d", id)); got != countBefore+1 {
		t.Fatalf("threads_count %d -> %d", countBefore, got)
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from attachments where thread_id = %d", tid)); got != 1 {
		t.Fatalf("the draft's attachment did not stay with the sent message")
	}
	waitForMail(t, tid)

	if text := callToolExpectingError(t, "send_draft", map[string]any{"draft_id": tid}); !strings.Contains(text, "not a draft") {
		t.Fatalf("sending twice: %q", text)
	}
	if text := callToolExpectingError(t, "send_draft", map[string]any{"draft_id": fixture.ColleagueDraft}); !strings.Contains(text, "someone else") {
		t.Fatalf("someone else's draft: %q", text)
	}
}

func TestSendNewConversationDraft(t *testing.T) {
	fixture := seeded(t)
	draft := callDraftTool(t, "create_new_draft", map[string]any{
		"mailbox_id": fixture.MailboxID,
		"to":         []string{"architekt@example.org", "bauamt@example.org"},
		"subject":    "Termin Baubeginn",
		"body":       "Guten Tag,\n\nder Baubeginn ist am 1. März.",
	})
	cid, tid := draft.Draft.ConversationID, draft.Draft.DraftID

	callTool(t, "send_draft", map[string]any{"draft_id": tid, "status": "pending"})

	row := query(t, fmt.Sprintf("select state, status, customer_email, customer_id is not null from conversations where id = %d", cid))
	if row != fmt.Sprintf("%d\t%d\tarchitekt@example.org\t1", statePublished, statusPending) {
		t.Fatalf("the sent conversation is %q", row)
	}
	if cc := query(t, fmt.Sprintf("select cc from conversations where id = %d", cid)); !strings.Contains(cc, "bauamt@example.org") {
		t.Fatalf("the second recipient did not end up in cc, as FreeScout does it: %q", cc)
	}
	if got := queryInt(t, fmt.Sprintf("select state from threads where id = %d", tid)); got != statePublished {
		t.Fatalf("thread state %d", got)
	}
	if got := queryInt(t, fmt.Sprintf(
		"select count(*) from conversation_folder cf join folders f on f.id = cf.folder_id where cf.conversation_id = %d and f.type = %d", cid, folderDrafts)); got != 0 {
		t.Fatalf("the sent conversation is still in Drafts")
	}
	if search := callTool(t, "search_conversations", map[string]any{"query": "Termin Baubeginn"}); !strings.Contains(search, "Termin Baubeginn") {
		t.Fatalf("the sent conversation is not searchable")
	}
	waitForMail(t, tid)
}

func TestForwardAsDraftCopiesTheAttachments(t *testing.T) {
	fixture := seeded(t)
	id := fixture.ConversationID

	draft := callDraftTool(t, "forward_conversation", map[string]any{
		"id": id, "to": "statik@example.org", "body": "Zur Info.",
	})
	tid := draft.Draft.DraftID
	row := query(t, fmt.Sprintf("select state, subtype, conversation_id from threads where id = %d", tid))
	if row != fmt.Sprintf("%d\t%d\t%d", stateDraft, subtypeForward, id) {
		t.Fatalf("forward draft %q", row)
	}
	names := []string{}
	for _, a := range draft.Draft.Attachments {
		names = append(names, a.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "angebot.txt") || !strings.Contains(joined, "dach.png") {
		t.Fatalf("the conversation's files were not copied onto the forward draft: %v", names)
	}
	for _, a := range draft.Draft.Attachments {
		if a.ID == fixture.TextAttachment || a.ID == fixture.ImgAttachment {
			t.Fatalf("the draft took the original attachment instead of a copy")
		}
	}
	if text := callToolExpectingError(t, "send_draft", map[string]any{"draft_id": tid}); !strings.Contains(text, "forward") {
		t.Fatalf("send_draft on a forward draft: %q", text)
	}
	if got := query(t, fmt.Sprintf("select state, coalesce(send_status, 'null') from threads where id = %d", tid)); got != fmt.Sprintf("%d\tnull", stateDraft) {
		t.Fatalf("the forward draft went out: %q", got)
	}
	callTool(t, "delete_draft", map[string]any{"draft_id": tid})
	for _, original := range []int{fixture.TextAttachment, fixture.ImgAttachment} {
		if got := queryInt(t, fmt.Sprintf("select count(*) from attachments where id = %d", original)); got != 1 {
			t.Fatalf("discarding the forward draft removed an original attachment")
		}
	}
}

func TestForwardRightAway(t *testing.T) {
	fixture := seeded(t)
	text := callTool(t, "forward_conversation", map[string]any{
		"id": fixture.ConversationID, "to": "statik@example.org", "body": "Bitte prüfen.", "send": true,
	})
	var out struct {
		Forwarded int `json:"forwarded_conversation_id"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	if out.Forwarded <= 0 {
		t.Fatalf("no forwarded conversation in %s", text)
	}
	row := query(t, fmt.Sprintf("select customer_email, subject, has_attachments from conversations where id = %d", out.Forwarded))
	if row != "statik@example.org\tFwd: Lieferung Dach\t1" {
		t.Fatalf("forwarded conversation %q", row)
	}
	tid := queryInt(t, fmt.Sprintf("select id from threads where conversation_id = %d order by id desc limit 1", out.Forwarded))
	if got := queryInt(t, fmt.Sprintf("select count(*) from attachments where thread_id = %d", tid)); got < 2 {
		t.Fatalf("the attachments were not forwarded (%d)", got)
	}
	waitForMail(t, tid)
}

func TestSearchFindsMessageTextAndFilters(t *testing.T) {
	fixture := seeded(t)

	// Only in the customer's message, not in subject or preview of every row.
	if got := callTool(t, "search_conversations", map[string]any{"query": "Rechnung zum Dach"}); !strings.Contains(got, "Rechnung Dach") {
		t.Fatalf("message text is not searched: %s", truncate(got))
	}
	if got := callTool(t, "search_conversations", map[string]any{"from": fixture.CustomerEmail, "mailbox_id": fixture.MailboxID}); !strings.Contains(got, "Lieferung Dach") {
		t.Fatalf("from filter: %s", truncate(got))
	}
	for _, args := range []map[string]any{
		{"from": "niemand@example.org"},
		{"after": "2999-01-01"},
		{"before": "2000-01-01"},
		{"query": "Dach", "status": "spam"},
	} {
		if got := callTool(t, "search_conversations", args); !strings.Contains(got, `"total":0`) {
			t.Fatalf("%v should match nothing: %s", args, truncate(got))
		}
	}
	callToolExpectingError(t, "search_conversations", map[string]any{"after": "letzte Woche"})

	draft := callDraftTool(t, "create_new_draft", map[string]any{
		"mailbox_id": fixture.MailboxID, "to": "a@example.org", "subject": "Nur ein Entwurf", "body": "x",
	})
	if got := callTool(t, "search_conversations", map[string]any{"state": "draft"}); !strings.Contains(got, "Nur ein Entwurf") {
		t.Fatalf("state=draft does not list drafts: %s", truncate(got))
	}
	callTool(t, "delete_draft", map[string]any{"draft_id": draft.Draft.DraftID})
}

func TestFindCustomers(t *testing.T) {
	got := callTool(t, "find_customers", map[string]any{"query": "Kundin"})
	for _, want := range []string{"kundin@example.com", "Kirsten Kundin", "Lieferung Dach"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q missing: %s", want, truncate(got))
		}
	}
	if got := callTool(t, "find_customers", map[string]any{"query": "kundin@example"}); !strings.Contains(got, "Kirsten") {
		t.Fatalf("search by address: %s", truncate(got))
	}
}

func TestMoveConversation(t *testing.T) {
	fixture := seeded(t)
	callTool(t, "move_conversation", map[string]any{"id": fixture.MoveConv, "mailbox_id": fixture.SecondMailbox})
	if got := queryInt(t, fmt.Sprintf("select mailbox_id from conversations where id = %d", fixture.MoveConv)); got != fixture.SecondMailbox {
		t.Fatalf("mailbox_id %d", got)
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and type = 4", fixture.MoveConv)); got != 1 {
		t.Fatalf("FreeScout's history line for the move is missing")
	}
	callToolExpectingError(t, "move_conversation", map[string]any{"id": fixture.MoveConv, "mailbox_id": 999999})
}

func TestMergeConversations(t *testing.T) {
	fixture := seeded(t)
	threads := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d", fixture.MergeConv))

	callToolExpectingError(t, "merge_conversations", map[string]any{"id": fixture.ConversationID, "merge_ids": []int{fixture.ConversationID}})
	callTool(t, "merge_conversations", map[string]any{"id": fixture.ConversationID, "merge_ids": []int{fixture.MergeConv}})

	if got := queryInt(t, fmt.Sprintf("select state from conversations where id = %d", fixture.MergeConv)); got != stateDeleted {
		t.Fatalf("the merged conversation was not deleted: state %d", got)
	}
	if got := queryInt(t, fmt.Sprintf("select count(*) from threads where conversation_id = %d and body like '%%Rechnung zum Dach%%'", fixture.ConversationID)); got != 1 || threads < 1 {
		t.Fatalf("the merged messages did not move over")
	}
}

// ---- REST API ----------------------------------------------------------------

const apiURL = mcpURL + "/mcp/api/v1"

// restToken is what a client with a shell does first: ask the token tool.
func restToken(t *testing.T) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(callTool(t, "token", map[string]any{})), &out); err != nil || out.Token == "" {
		t.Fatalf("the token tool gave no token")
	}
	return out.Token
}

func rest(t *testing.T, method, path, token string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, apiURL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// form builds what curl -F sends.
func form(t *testing.T, fields [][2]string, files map[string][]byte) (io.Reader, string) {
	t.Helper()
	var buf strings.Builder
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = w.WriteField(f[0], f[1])
	}
	for name, data := range files {
		part, _ := w.CreateFormFile("file", name)
		_, _ = part.Write(data)
	}
	_ = w.Close()
	return strings.NewReader(buf.String()), w.FormDataContentType()
}

func TestRestDraftWithPdfLandsInFreeScout(t *testing.T) {
	fixture := seeded(t)
	token := restToken(t)
	mailBefore := outgoingMail(t)
	pdf := []byte("%PDF-1.4\n% Angebot per REST\n")

	body, ct := form(t, [][2]string{{"body", "Guten Tag,\n\nanbei das Angebot per REST."}, {"cc", "statik@example.org"}},
		map[string][]byte{"angebot-rest.pdf": pdf})
	status, raw := rest(t, "POST", fmt.Sprintf("/conversations/%d/drafts", fixture.ConversationID), token, body, ct)
	if status != 201 {
		t.Fatalf("POST drafts: HTTP %d %s", status, raw)
	}
	var draft draftResult
	if err := json.Unmarshal(raw, &draft); err != nil || draft.Draft.DraftID <= 0 || len(draft.Draft.Attachments) != 1 {
		t.Fatalf("unexpected answer: %s", raw)
	}
	a := draft.Draft.Attachments[0]
	if row := query(t, fmt.Sprintf("select thread_id, name, mime_type from attachments where id = %d", a.ID)); row != fmt.Sprintf("%d\tangebot-rest.pdf\tapplication/pdf", draft.Draft.DraftID) {
		t.Fatalf("attachment not stored on the draft: %q", row)
	}
	if state := queryInt(t, fmt.Sprintf("select state from threads where id = %d", draft.Draft.DraftID)); state != stateDraft {
		t.Fatalf("thread state %d", state)
	}

	// The file comes back byte for byte, as a file.
	status, got := rest(t, "GET", fmt.Sprintf("/attachments/%d", a.ID), token, nil, "")
	if status != 200 || string(got) != string(pdf) {
		t.Fatalf("download: HTTP %d %q", status, got)
	}

	// Upload first, attach by id through the MCP tool: the way for a client
	// that talks MCP but can run curl.
	body, ct = form(t, nil, map[string][]byte{"plaene.pdf": []byte("%PDF-1.4\n% Pläne\n")})
	status, raw = rest(t, "POST", "/uploads", token, body, ct)
	if status != 201 {
		t.Fatalf("upload: HTTP %d %s", status, raw)
	}
	uploadID := between(string(raw), `"upload_id":"`, `"`)
	updated := callDraftTool(t, "update_draft", map[string]any{
		"draft_id": draft.Draft.DraftID, "attachments": []map[string]any{{"upload_id": uploadID}},
	})
	if len(updated.Draft.Attachments) != 2 {
		t.Fatalf("the uploaded file was not attached: %+v", updated.Draft.Attachments)
	}
	callToolExpectingError(t, "update_draft", map[string]any{
		"draft_id": draft.Draft.DraftID, "attachments": []map[string]any{{"upload_id": uploadID}},
	})

	if status, raw := rest(t, "DELETE", fmt.Sprintf("/drafts/%d", draft.Draft.DraftID), token, nil, ""); status != 200 {
		t.Fatalf("delete: HTTP %d %s", status, raw)
	}
	if outgoingMail(t) != mailBefore {
		t.Fatalf("a REST draft sent mail")
	}
}

func TestRestNewDraftAndSearch(t *testing.T) {
	fixture := seeded(t)
	token := restToken(t)
	body, ct := form(t, [][2]string{
		{"mailbox_id", strconv.Itoa(fixture.MailboxID)}, {"to", "bauamt@example.org"},
		{"subject", "Bauantrag per REST"}, {"body", "Sehr geehrte Damen und Herren"},
	}, map[string][]byte{"antrag.pdf": []byte("%PDF-1.4\n% Antrag\n")})
	status, raw := rest(t, "POST", "/drafts", token, body, ct)
	if status != 201 {
		t.Fatalf("POST /drafts: HTTP %d %s", status, raw)
	}
	var draft draftResult
	_ = json.Unmarshal(raw, &draft)
	if !draft.Draft.NewConversation || len(draft.Draft.Attachments) != 1 {
		t.Fatalf("unexpected answer: %s", raw)
	}

	status, raw = rest(t, "GET", "/conversations?state=draft&fields=subject", token, nil, "")
	if status != 200 || !strings.Contains(string(raw), "Bauantrag per REST") || strings.Contains(string(raw), "preview") {
		t.Fatalf("search: HTTP %d %s", status, raw)
	}
	rest(t, "DELETE", fmt.Sprintf("/drafts/%d", draft.Draft.DraftID), token, nil, "")
}

func TestRestRefusesBadTokensAndForeignDrafts(t *testing.T) {
	fixture := seeded(t)

	if status, _ := rest(t, "GET", "/mailboxes", "", nil, ""); status != 401 {
		t.Fatalf("no token: HTTP %d", status)
	}
	// Signed with the right key but expired, and a current one with the wrong key.
	sign := func(key string, expires time.Time) string {
		claims, _ := json.Marshal(map[string]any{"k": "api", "s": "post@levinkeller.de", "c": clientID, "sc": "mcp", "e": expires.UnixMilli(), "n": "x"})
		body := "fs1." + base64.RawURLEncoding.EncodeToString(claims)
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(body))
		return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	if status, _ := rest(t, "GET", "/mailboxes", sign(signingKey, time.Now().Add(-time.Minute)), nil, ""); status != 401 {
		t.Fatalf("expired token: HTTP %d", status)
	}
	if status, _ := rest(t, "GET", "/mailboxes", sign("a-different-key-that-is-long-enough-00", time.Now().Add(time.Hour)), nil, ""); status != 401 {
		t.Fatalf("forged token: HTTP %d", status)
	}
	// The MCP access token is not a REST token.
	if status, _ := rest(t, "GET", "/mailboxes", connect(t), nil, ""); status != 401 {
		t.Fatalf("MCP access token accepted: HTTP %d", status)
	}

	token := restToken(t)
	if status, raw := rest(t, "GET", "/mailboxes", token, nil, ""); status != 200 || !strings.Contains(string(raw), "Ökohaus") {
		t.Fatalf("mailboxes: HTTP %d %s", status, raw)
	}
	status, raw := rest(t, "PATCH", fmt.Sprintf("/drafts/%d", fixture.ColleagueDraft), token, strings.NewReader(`{"body":"überschrieben"}`), "application/json")
	if status < 400 || status >= 500 || !strings.Contains(string(raw), "someone else") {
		t.Fatalf("a colleague's draft was writable: HTTP %d %s", status, raw)
	}
	if body := query(t, fmt.Sprintf("select body from threads where id = %d", fixture.ColleagueDraft)); body != "Entwurf der Kollegin" {
		t.Fatalf("the colleague's draft was changed: %q", body)
	}

	big, ct := form(t, [][2]string{{"body", "x"}}, map[string][]byte{"gross.bin": make([]byte, 21<<20)})
	if status, _ := rest(t, "POST", fmt.Sprintf("/conversations/%d/drafts", fixture.ConversationID), token, big, ct); status != 413 {
		t.Fatalf("21 MB: HTTP %d", status)
	}
}

func TestOpenAPIIsPublic(t *testing.T) {
	status, raw := rest(t, "GET", "/openapi.json", "", nil, "")
	if status != 200 || !strings.Contains(string(raw), `"openapi":"3.0.3"`) || !strings.Contains(string(raw), "/conversations/{id}/drafts") {
		t.Fatalf("openapi.json: HTTP %d %s", status, truncate(string(raw)))
	}
}
