package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalWorkspace(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if output, err := exec.CommandContext(ctx, "git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".contextshare/project.json", `{
		"version": 1, "project": {"id":"test","name":"Test"},
		"sections":[{"id":"guide","title":"Rules","paths":["md/*"]}], "fallback_section":"guide"
	}`)
	write("client.go", "package client\n")
	write("md/editor.md", "# Запуск редактора Android\n\nДля складного окна учитывается разделяющий hinge. [Код](../client.go)\n\n"+strings.Repeat("Дополнительные условия. ", 30)+"OpenGL ES 3.0.")
	write("md/reports/publish.md", "# Девятый набор миниатюр\n\nИстория публикации. [Редактор](../editor.md).")
	w, err := openLocalWorkspace(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	find := func(query string) []localNode {
		t.Helper()
		data, err := w.load()
		if err != nil {
			t.Fatal(err)
		}
		result, err := w.findNodes(ctx, data, nodeListArgs{Query: query}, true)
		if err != nil {
			t.Fatal(err)
		}
		return result.Items
	}
	if hits := find("разделяющего шарнира"); len(hits) != 1 || hits[0].SourcePath != "md/editor.md" {
		t.Fatal("Russian inflection and hinge alias did not find the source")
	}
	if hits := find(`"OpenGL ES 3.0" -Vulkan`); len(hits) != 1 || !strings.Contains(hits[0].Body, "OpenGL ES 3.0") {
		t.Fatal("phrase preview did not find the match")
	}
	if hits := find("девятый набор миниатюр"); len(hits) != 1 || hits[0].Status != "historical" {
		t.Fatal("historical result is missing")
	}
	if len(w.snapshot.links) != 1 || len(w.snapshot.notes["md/editor.md"].Node.CodePaths) != 1 {
		t.Fatal("document and code links are missing")
	}
	write("md/editor.md", "# Проверка свежести\n\nТеперь Vulkan.")
	if len(find("OpenGL ES 3.0")) != 0 || len(find("проверка свежести")) != 1 {
		t.Fatal("index did not follow working tree edit")
	}
	in := batchArgs{ProjectID: "test", OperationID: "test-operation", Requests: []requestInput{{Title: "Fix", Body: "Reproduce", Kind: "bug", NodeID: "md/editor.md"}}}
	first, err := w.addLocalRequests(ctx, in)
	if err != nil || len(first.Items) != 1 {
		t.Fatalf("could not add: %v", err)
	}
	second, err := openLocalWorkspace(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	retry, err := second.addLocalRequests(ctx, in)
	if err != nil || len(retry.Items) != 1 || retry.Items[0] != first.Items[0] {
		t.Fatalf("request retry changed the stored result: %v", err)
	}
	in.Requests[0].Title = "Different"
	if _, err := second.addLocalRequests(ctx, in); !errors.Is(err, errConflict) {
		t.Fatalf("expected conflicting retry: %v", err)
	}
	change := localRequestChange{ProjectID: "test", RequestID: first.Items[0].ID, ExpectedVersion: 1, Criteria: "Tested", Platforms: []string{"web", "android"}}
	prepared, err := second.changeLocalRequest(ctx, change, "prepare")
	if err != nil || prepared.State != "ready" || prepared.Version != 2 {
		t.Fatalf("could not prepare: %v", err)
	}
	change.ExpectedVersion, change.Platform = prepared.Version, "android"
	claimed, err := w.changeLocalRequest(ctx, change, "claim")
	if err != nil || claimed.Work[0].State != "ready" || claimed.Work[1].State != "active" {
		t.Fatalf("claim changed the wrong platform: %v", err)
	}
	if _, err := second.changeLocalRequest(ctx, change, "claim"); !errors.Is(err, errConflict) {
		t.Fatalf("expected stale version conflict: %v", err)
	}
}
