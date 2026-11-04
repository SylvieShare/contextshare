package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPagesAndOAuthRedirects(t *testing.T) {
	if _, err := newApp(config{base: "http://127.0.0.1:8080"}, &store{}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://chatgpt.com/connector_platform_oauth_redirect", "http://127.0.0.1:31337/callback", "https://chatgpt.com/connector/oauth/AB_CD-1"} {
		if !allowedRedirect(raw) {
			t.Errorf("expected allowed callback %s", raw)
		}
	}
	for _, raw := range []string{"https://chatgpt.com.evil.example/connector_platform_oauth_redirect", "https://chatgpt.com@evil.example/connector_platform_oauth_redirect", "https://chatgpt.com/connector/oauth/../elsewhere", "http://127.0.0.1.example/callback", "https://chatgpt.com/connector_platform_oauth_redirect?to=evil", "http://192.168.0.1/callback"} {
		if allowedRedirect(raw) {
			t.Errorf("unexpected callback acceptance %s", raw)
		}
	}
	if !registeredRedirect("http://127.0.0.1/callback", "http://127.0.0.1:49123/callback") ||
		registeredRedirect("https://chatgpt.com/connector/oauth/first", "https://chatgpt.com/connector/oauth/second") {
		t.Fatal("redirect matching must allow only loopback port differences")
	}
	if safeReturn("//example.org") != "/" || safeReturn("/oauth/authorize?client_id=x") != "/oauth/authorize?client_id=x" {
		t.Fatal("login return path is not restricted")
	}
}

type fixtureHTTP struct {
	t      *testing.T
	client *http.Client
	base   string
}

func (f fixtureHTTP) do(method, path, contentType, body, token string, want int) (http.Header, []byte) {
	f.t.Helper()
	r, err := http.NewRequest(method, f.base+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if path == "/mcp" {
		r.Header.Set("Accept", "application/json, text/event-stream")
	}
	res, err := f.client.Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	if res.StatusCode != want {
		f.t.Fatalf("%s %s returned %d; expected %d", method, path, res.StatusCode, want)
	}
	return res.Header, data
}

func (f fixtureHTTP) post(path string, form url.Values, want int) (http.Header, []byte) {
	f.t.Helper()
	u, _ := url.Parse(f.base)
	for _, cookie := range f.client.Jar.Cookies(u) {
		if cookie.Name == "contextshare_csrf" {
			form.Set("csrf", cookie.Value)
		}
	}
	return f.do("POST", path, "application/x-www-form-urlencoded", form.Encode(), "", want)
}

func (f fixtureHTTP) tool(token, name string, args any, fail bool) json.RawMessage {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	_, data := f.do("POST", "/mcp", "application/json", string(body), token, 200)
	var output struct {
		Result struct {
			IsError    bool            `json:"isError"`
			Structured json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct{ Code int } `json:"error"`
	}
	if json.Unmarshal(data, &output) != nil || output.Error != nil || output.Result.IsError != fail {
		f.t.Fatalf("unexpected MCP response to %s: protocol error=%v tool error=%t", name, output.Error, output.Result.IsError)
	}
	return output.Result.Structured
}

func TestPostgresMCPIntegration(t *testing.T) {
	if os.Getenv("CONTEXTSHARE_RUN_INTEGRATION") != "1" {
		t.Skip("isolated PostgreSQL schema is required")
	}
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile("^ctx_test_[a-f0-9]{12}$").MatchString(cfg.schema) {
		t.Fatal("refusing to test outside an isolated schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Close()
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := db.pool.Exec(clean, "DROP SCHEMA "+pgx.Identifier{cfg.schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error("could not drop the isolated test schema")
		}
	}()
	invite := randomToken("test_invite_")
	if err := db.migrate(ctx, digest(invite)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	cfg.base = "http://" + server.Listener.Addr().String()
	cfg.secure = false
	a, err := newApp(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = a.routes()
	server.Start()
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	f := fixtureHTTP{t, client, server.URL}
	f.do("GET", "/register", "", "", "", 200)
	f.post("/register", url.Values{"email": {"pilot@example.invalid"}, "password": {"example-long-password"}, "invite": {invite}}, 303)
	f.do("GET", "/", "", "", "", 200)
	if a.seed != nil {
		var nodes, requests int
		if err := db.pool.QueryRow(ctx, db.q("SELECT count(*) FROM cs.nodes")).Scan(&nodes); err != nil {
			t.Fatal(err)
		}
		if err := db.pool.QueryRow(ctx, db.q("SELECT count(*) FROM cs.requests")).Scan(&requests); err != nil {
			t.Fatal(err)
		}
		wantNodes := 0
		for _, section := range a.seed.Sections {
			wantNodes += len(section.Nodes)
		}
		if nodes != wantNodes || requests != len(a.seed.Requests) {
			t.Fatal("pilot import did not preserve the expected records")
		}
		if a.seed.Project.Name == "DnD Share" {
			var pilotID string
			if err := db.pool.QueryRow(ctx, db.q("SELECT id FROM cs.projects WHERE name=$1"), a.seed.Project.Name).Scan(&pilotID); err != nil {
				t.Fatal(err)
			}
			_, tokenPage := f.post("/p/"+pilotID+"/access/tokens", url.Values{"label": {"Read-only pilot"}}, 200)
			token := regexp.MustCompile("ctx_[A-Za-z0-9_-]{43}").FindString(string(tokenPage))
			if token == "" {
				t.Fatal("missing pilot key")
			}
			overview := f.tool(token, "get_project_overview", projectArgs{pilotID}, false)
			var info overviewResult
			if json.Unmarshal(overview, &info) != nil || len(info.Sections) != 3 {
				t.Fatal("missing pilot sections")
			}
			var matches page[node]
			if json.Unmarshal(f.tool(token, "search", nodeListArgs{ProjectID: pilotID, Query: "OpenGL", Platform: "android"}, false), &matches) != nil || len(matches.Items) == 0 || matches.Items[0].SourceRevision == "" {
				t.Fatal("pilot search did not retrieve a sourced Android graphics requirement")
			}
			t.Log("DnD Share pilot: overview and a sourced Android graphics requirement found in two MCP calls")
		}
	}
	h, _ := f.post("/projects", url.Values{"name": {"Pilot Project"}, "overview": {"Current project context"}}, 303)
	pid := strings.TrimPrefix(h.Get("Location"), "/p/")
	h, _ = f.post("/projects", url.Values{"name": {"Another private project"}}, 303)
	other := strings.TrimPrefix(h.Get("Location"), "/p/")
	h, _ = f.post("/p/"+pid+"/sections", url.Values{"title": {"Character sheet"}}, 303)
	sid := strings.TrimPrefix(h.Get("Location"), "/p/"+pid+"/sections/")
	h, _ = f.post("/p/"+pid+"/sections/"+sid+"/nodes", url.Values{"title": {"Dice modes"}, "kind": {"rule"}, "status": {"current"}, "body": {"One mode per roll."}}, 303)
	nid := strings.TrimPrefix(h.Get("Location"), "/p/"+pid+"/nodes/")
	h, _ = f.post("/p/"+pid+"/sections/"+sid+"/nodes", url.Values{"title": {"Ability checks"}, "kind": {"feature"}, "status": {"current"}, "body": {"Read the dice rule."}, "parent_id": {nid}}, 303)
	linked := strings.TrimPrefix(h.Get("Location"), "/p/"+pid+"/nodes/")
	f.post("/p/"+pid+"/nodes/"+linked+"/links", url.Values{"kind": {"depends_on"}, "target_id": {nid}}, 303)
	f.do("GET", "/p/"+pid+"/nodes/"+nid, "", "", "", 200)
	_, tokenPage := f.post("/p/"+pid+"/access/tokens", url.Values{"label": {"MCP test"}, "scopes": {scopeDraft, scopeWork}}, 200)
	pat := regexp.MustCompile("ctx_[A-Za-z0-9_-]{43}").FindString(string(tokenPage))
	if pat == "" {
		t.Fatal("missing prepared token")
	}
	f.tool(pat, "get_usage_guide", map[string]any{}, false)
	if out := f.tool(pat, "get_project_overview", projectArgs{pid}, false); !strings.Contains(string(out), "Pilot Project") {
		t.Fatal("missing project overview")
	}
	f.tool(pat, "get_project_overview", projectArgs{other}, true)
	if out := f.tool(pat, "get_related_nodes", relationArgs{ProjectID: pid, NodeID: nid}, false); !strings.Contains(string(out), "Ability checks") {
		t.Fatal("missing incoming link")
	}
	batch := batchArgs{ProjectID: pid, OperationID: "same-action", Requests: []requestInput{
		{Title: "Update modes", Body: "Use one mode.", Kind: "feature", NodeID: nid, Platforms: []string{"web", "android"}},
		{Title: "Check menu", Body: "Open and close.", Kind: "bug", SectionID: sid},
	}}
	first := f.tool(pat, "add_requests", batch, false)
	if retry := f.tool(pat, "add_requests", batch, false); string(retry) != string(first) {
		t.Fatal("retry produced a different batch")
	}
	var added struct {
		Items []addedRequest `json:"items"`
	}
	if json.Unmarshal(first, &added) != nil || len(added.Items) != 2 {
		t.Fatal("missing created requests")
	}
	id := added.Items[0].ID
	f.post("/p/"+pid+"/queue/"+id+"/prepare", url.Values{"version": {"1"}, "criteria": {"Web and Android can switch immediately."}, "platforms": {"web", "android"}}, 303)
	g, err := db.tokenGrant(ctx, pat)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() { results <- db.changePart(ctx, g.UserID, pid, id, "web", "claim", "") })
	}
	group.Wait()
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("expected exactly one web claim")
	}
	f.tool(pat, "claim_request", claimArgs{pid, id, "android"}, false)
	h, _ = f.post("/p/"+pid+"/sections", url.Values{"title": {"Rules moved"}}, 303)
	destination := strings.TrimPrefix(h.Get("Location"), "/p/"+pid+"/sections/")
	f.post("/p/"+pid+"/nodes/"+nid+"/move", url.Values{"version": {"1"}, "target_section": {destination}}, 303)
	moved := f.tool(pat, "get_node", nodeArgs{pid, nid}, false)
	var result nodeResult
	if json.Unmarshal(moved, &result) != nil || result.Node.SectionID != destination || len(result.Children) != 1 || result.Children[0].ID != linked {
		t.Fatal("move must preserve the node and its subtree")
	}
	var item requestItem
	if json.Unmarshal(f.tool(pat, "get_request", requestArgs{pid, id}, false), &item) != nil || item.SectionID != destination || item.NodeID != nid {
		t.Fatal("move must preserve linked requests")
	}
	f.testOAuth(pid)
	f.post("/p/"+pid+"/access/revoke", url.Values{"grant_id": {g.ID}}, 303)
	f.do("POST", "/mcp", "application/json", "{}", pat, 401)
}

func (f fixtureHTTP) testOAuth(pid string) {
	f.t.Helper()
	redirect := "https://chatgpt.com/connector_platform_oauth_redirect"
	registration, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"})
	_, data := f.do("POST", "/oauth/register", "application/json", string(registration), "", 201)
	var client struct {
		ID string `json:"client_id"`
	}
	if json.Unmarshal(data, &client) != nil || client.ID == "" {
		f.t.Fatal("missing OAuth client")
	}
	verifier := strings.Repeat("x", 43)
	hash := sha256.Sum256([]byte(verifier))
	values := url.Values{"client_id": {client.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {"example-state"},
		"scope": {scopeRead + " " + scopeDraft}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])},
		"code_challenge_method": {"S256"}, "resource": {f.base + "/mcp"}}
	consent, _ := f.do("GET", "/oauth/authorize?"+values.Encode(), "", "", "", 200)
	if !strings.Contains(consent.Get("Content-Security-Policy"), "form-action 'self' "+redirect) {
		f.t.Fatal("consent page must allow navigation to the registered OAuth callback")
	}
	values.Set("project_id", pid)
	values.Set("allow_draft", "yes")
	values.Set("decision", "allow")
	h, _ := f.post("/oauth/authorize", values, 303)
	callback, err := url.Parse(h.Get("Location"))
	if err != nil || callback.Query().Get("state") != "example-state" || callback.Query().Get("iss") != f.base || callback.Query().Get("code") == "" {
		f.t.Fatal("invalid OAuth callback")
	}
	tokenRequest := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "code": {callback.Query().Get("code")}, "code_verifier": {strings.Repeat("z", 43)}, "resource": {f.base + "/mcp"}}
	f.do("POST", "/oauth/token", "application/x-www-form-urlencoded", tokenRequest.Encode(), "", 400)
	tokenRequest.Set("code_verifier", verifier)
	_, body := f.do("POST", "/oauth/token", "application/x-www-form-urlencoded", tokenRequest.Encode(), "", 200)
	var token struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if json.Unmarshal(body, &token) != nil || token.Access == "" || token.Refresh == "" {
		f.t.Fatal("missing OAuth tokens")
	}
	f.tool(token.Access, "get_project_overview", projectArgs{pid}, false)
	f.do("POST", "/oauth/token", "application/x-www-form-urlencoded", tokenRequest.Encode(), "", 400)
	refreshRequest := url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {token.Refresh}, "resource": {f.base + "/mcp"}}
	f.do("POST", "/oauth/token", "application/x-www-form-urlencoded", refreshRequest.Encode(), "", 200)
	f.do("POST", "/oauth/token", "application/x-www-form-urlencoded", refreshRequest.Encode(), "", 400)
}
