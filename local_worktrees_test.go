package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestWorktreeRoutingIsolation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	primary := filepath.Join(dir, "primary")
	secondary := filepath.Join(dir, "secondary")
	git := func(args ...string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet", primary)
	if err := os.MkdirAll(filepath.Join(primary, ".contextshare", "wiki"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"version":1,"wiki_dir":".contextshare/wiki","project":{"id":"test","name":"Test"},"sections":[{"id":"guide","title":"Rules","paths":[]}],"fallback_section":"guide"}`
	if err := os.WriteFile(filepath.Join(primary, ".contextshare", "project.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(".contextshare", "wiki", "rule.md")
	if err := os.WriteFile(filepath.Join(primary, page), []byte("# Primary rule\n\nOriginal.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", primary, "add", "--", ".contextshare")
	git("-C", primary, "-c", "user.name=ContextShare Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "Fixture")
	git("-C", primary, "worktree", "add", "--quiet", "-b", "secondary", secondary)
	if err := os.WriteFile(filepath.Join(secondary, page), []byte("# Secondary rule\n\nLocal change.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := openLocalWorkspace(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := w.localServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "worktree-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, args any, dst any) *mcp.CallToolResult {
		t.Helper()
		out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || out.IsError {
			t.Fatalf("%s failed: %v %+v", name, err, out)
		}
		raw, err := json.Marshal(out.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
		return out
	}
	var first, second worktreeResult
	call("select_worktree", worktreeSelection{Path: primary}, &first)
	call("select_worktree", worktreeSelection{Path: secondary}, &second)
	if first.ProjectID == second.ProjectID || first.Workspace.Commit == "" || !second.Workspace.Dirty {
		t.Fatal("worktree identity or revision missing")
	}
	var a, b wikiDocument
	call("get_wiki_document", wikiReadArgs{ProjectID: first.ProjectID, Path: "rule.md"}, &a)
	call("get_wiki_document", wikiReadArgs{ProjectID: second.ProjectID, Path: "rule.md"}, &b)
	if !strings.Contains(a.Content, "Primary") || !strings.Contains(b.Content, "Secondary") ||
		a.Workspace.Path != first.Workspace.Path || b.Workspace.Path != second.Workspace.Path {
		t.Fatal("parallel worktree selections interfered")
	}
	var changed wikiDocument
	call("write_wiki_document", wikiWriteArgs{ProjectID: second.ProjectID, Path: "rule.md", Expected: b.SHA256, Find: "Local change.", Replace: "Selected worktree only."}, &changed)
	if !changed.Workspace.Dirty || changed.Workspace.Path != second.Workspace.Path {
		t.Fatal("write response did not identify the modified worktree")
	}
	call("get_wiki_document", wikiReadArgs{ProjectID: first.ProjectID, Path: "rule.md"}, &a)
	if strings.Contains(a.Content, "Selected worktree") {
		t.Fatal("write changed the default worktree")
	}
	out, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "write_wiki_document",
		Arguments: wikiWriteArgs{ProjectID: "test", Path: "rule.md", Expected: a.SHA256, Find: "Original.", Replace: "Wrong default"}})
	if err != nil || !out.IsError {
		t.Fatal("unscoped MCP write was accepted")
	}
}

func TestNearbySearchTerms(t *testing.T) {
	terms := localQueryTerms("Как обозначается владение навыком?")
	if len(terms) != 3 || terms[0] != "marker" {
		t.Fatalf("question intent polluted content terms: %v", terms)
	}
	start, end := localTermWindow([]localTermPosition{
		{"skill", 1, 2}, {"proficiency", 70, 71}, {"skill", 71, 72},
	})
	if start != 70 || end != 72 {
		t.Fatalf("wrong relevant passage %d..%d", start, end)
	}
}
