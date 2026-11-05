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
		"sections":[{"id":"guide","title":"Rules","paths":["md/*"]}], "fallback_section":"guide",
		"search_aliases":[["характеристики","статы"]]
	}`)
	write("client.go", "package client\n")
	write("md/editor.md", "# Запуск редактора Android\n\nДля складного окна учитывается разделяющий hinge. [Код](../client.go)\n\n"+strings.Repeat("Дополнительные условия. ", 30)+"OpenGL ES 3.0.")
	write("md/reports/publish.md", "# Девятый набор миниатюр\n\nИстория публикации. [Редактор](../editor.md).")
	write("md/stats.md", "# Статы персонажа\n\nОписание карточек.")
	write("md/panel.md", "# Карточки листа\n\n"+strings.Repeat("Навыки используют модификатор и владение. ", 8))
	write("md/misc.md", "# Прочее\n\nСсылка на навыки.")
	write("md/skill-marker.md", "# Карточки листа\n\nВладение навыком отмечается одной меткой.")
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
	if hits := find("характеристики"); len(hits) != 1 || hits[0].SourcePath != "md/stats.md" {
		t.Fatal("project search aliases did not resolve the topic")
	}
	if hits := find("навыки"); len(hits) < 2 || hits[0].SourcePath != "md/panel.md" {
		t.Fatal("single-term search preferred a passing mention over the main topic")
	}
	if hits := find("Как обозначается владение навыком?"); len(hits) == 0 || hits[0].SourcePath != "md/skill-marker.md" {
		t.Fatal("question search missed the proficiency marker")
	}
	if hits := find(`"OpenGL ES 3.0" -Vulkan`); len(hits) != 1 || !strings.Contains(hits[0].Body, "OpenGL ES 3.0") {
		t.Fatal("phrase preview did not find the match")
	}
	if hits := find("девятый набор миниатюр"); len(hits) != 0 {
		t.Fatal("default search included history")
	}
	history, err := w.findNodes(ctx, w.snapshot, nodeListArgs{Query: "девятый набор миниатюр", Status: "historical"}, true)
	if err != nil || len(history.Items) != 1 || history.Items[0].Status != "historical" {
		t.Fatalf("explicit history result is missing: %v", err)
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

func TestWikiWrites(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	if output, err := exec.CommandContext(ctx, "git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.Mkdir(filepath.Join(dir, ".contextshare"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `{"version":1,"wiki_dir":".contextshare/wiki","project":{"id":"wiki","name":"Wiki"},"sections":[{"id":"guide","title":"Guide","paths":[]}],"fallback_section":"guide"}`
	if err := os.WriteFile(filepath.Join(dir, ".contextshare", "project.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := openLocalWorkspace(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	created, err := w.writeWiki(ctx, wikiWriteArgs{ProjectID: "wiki", Path: "models.md", Content: "# Модели\n\nПервый рецепт.\n"})
	if err != nil || created.Path != ".contextshare/wiki/models.md" {
		t.Fatalf("create failed: %v", err)
	}
	data, err := w.load()
	if err != nil {
		t.Fatal(err)
	}
	part, err := w.readWikiRange(data.config, wikiReadArgs{Path: created.Path, StartLine: 3, LineCount: 1})
	if err != nil || part.SHA256 != created.SHA256 || part.Content != "Первый рецепт.\n" {
		t.Fatalf("range with full-file revision: %+v, %v", part, err)
	}
	edit := wikiWriteArgs{ProjectID: "wiki", Path: created.Path, Expected: created.SHA256, Find: "Первый", Replace: "Второй"}
	updated, err := w.writeWiki(ctx, edit)
	if err != nil || updated.SHA256 == created.SHA256 {
		t.Fatalf("fragment update failed: %v", err)
	}
	if _, err := w.writeWiki(ctx, edit); !errors.Is(err, errConflict) {
		t.Fatalf("stale edit must conflict: %v", err)
	}
	if _, err := w.writeWiki(ctx, wikiWriteArgs{ProjectID: "wiki", Path: "../README.md", Content: "# Escape"}); !errors.Is(err, errInvalid) {
		t.Fatalf("path escape must fail: %v", err)
	}
	if _, err := w.writeWiki(ctx, wikiWriteArgs{ProjectID: "wiki", Path: "README.md", Content: "# Wiki\n\n[Модели](models.md)\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.deleteWiki(ctx, wikiDeleteArgs{ProjectID: "wiki", Path: updated.Path, Expected: updated.SHA256}); !errors.Is(err, errConflict) {
		t.Fatalf("incoming link must prevent deletion: %v", err)
	}
	saved, err := w.writeWikiAttachment(ctx, wikiAttachmentWriteArgs{ProjectID: "wiki", Path: "model.json", Content: `{"sourceCode":"TILE-1"}`})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := w.readWikiAttachment(data.config, "model.json")
	if err != nil || !strings.Contains(attachment.Content, "TILE-1") || attachment.SHA256 != saved.SHA256 {
		t.Fatalf("manifest read failed: %+v, %v", attachment, err)
	}
	if _, err := w.writeWikiAttachment(ctx, wikiAttachmentWriteArgs{ProjectID: "wiki", Path: "model.json", Content: `{}`}); !errors.Is(err, errConflict) {
		t.Fatalf("manifest replacement without a version must conflict: %v", err)
	}
	link := wikiLinkArgs{ProjectID: "wiki", From: ".contextshare/wiki/README.md", To: created.Path, Kind: "related"}
	if _, err := w.setWikiLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	link.Remove = true
	if _, err := w.setWikiLink(ctx, link); err != nil {
		t.Fatal(err)
	}
}
