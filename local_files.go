package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const localDir = ".contextshare"

type localSection struct {
	section
	Paths []string `json:"paths"`
}

type localConfig struct {
	Version         int            `json:"version"`
	Project         project        `json:"project"`
	Sections        []localSection `json:"sections"`
	FallbackSection string         `json:"fallback_section"`
}

type localNode struct {
	node
	SourcePath string   `json:"source_path,omitempty"`
	FirstLine  int      `json:"first_line,omitempty"`
	LastLine   int      `json:"last_line,omitempty"`
	CodePaths  []string `json:"code_paths,omitempty"`
}

type localNote struct {
	Node      localNode        `json:"node"`
	Platforms []platformDetail `json:"platforms"`
}

type localBatch struct {
	OperationID string        `json:"operation_id"`
	InputHash   string        `json:"input_hash,omitempty"`
	Items       []requestItem `json:"items"`
}

type localSnapshot struct {
	hash      string
	config    localConfig
	notes     map[string]localNote
	requests  map[string]requestItem
	batches   map[string]localBatch
	links     []seedLink
	documents int
}

func decodeLocal(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

// Only tracked-format project files enter the index. The os.Root limits all
// reads to the selected worktree even if a repository contains symlinks.
func readLocalFiles(root *os.Root) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	read := func(name string) error {
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		const maxFileSize = 2 << 20
		data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
		if err == nil && len(data) > maxFileSize {
			err = fmt.Errorf("file exceeds 2 MiB")
		}
		if err == nil {
			files[name] = data
		}
		return err
	}
	for _, name := range []string{"README.md", localDir + "/project.json", localDir + "/links.json"} {
		if err := read(name); err != nil && (name == localDir+"/project.json" || !os.IsNotExist(err)) {
			return nil, "", fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, dir := range []string{"md", localDir + "/notes", localDir + "/requests"} {
		ext := ".json"
		if dir == "md" {
			ext = ".md"
		}
		if err := fs.WalkDir(root.FS(), dir, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				if name == dir && os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.Type().IsRegular() && strings.EqualFold(path.Ext(name), ext) {
				return read(name)
			}
			return nil
		}); err != nil {
			return nil, "", err
		}
	}
	h := sha256.New()
	for _, name := range sortedLocalKeys(files) {
		fmt.Fprintf(h, "%s\000%x\n", name, sha256.Sum256(files[name]))
	}
	return files, fmt.Sprintf("%x", h.Sum(nil)), nil
}

func sortedLocalKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func buildLocalSnapshot(root *os.Root, files map[string][]byte, hash string) (*localSnapshot, error) {
	data := &localSnapshot{hash: hash, notes: map[string]localNote{}, requests: map[string]requestItem{}, batches: map[string]localBatch{}}
	if err := decodeLocal(files[localDir+"/project.json"], &data.config); err != nil {
		return nil, fmt.Errorf("project.json: %w", err)
	}
	config := data.config
	if config.Version != 1 || !textOK(config.Project.ID, 1, 120) || !textOK(config.Project.Name, 1, 160) {
		return nil, fmt.Errorf("unsupported local project format")
	}
	sections := map[string]bool{}
	for _, s := range config.Sections {
		if s.ID == "" || s.Title == "" || sections[s.ID] {
			return nil, fmt.Errorf("missing or duplicate section %q", s.ID)
		}
		sections[s.ID] = true
	}
	if !sections[config.FallbackSection] {
		return nil, fmt.Errorf("unknown fallback section")
	}
	for _, name := range sortedLocalKeys(files) {
		switch {
		case name == "README.md" || strings.HasPrefix(name, "md/"):
			data.documents++
			for _, n := range localMarkdownNodes(name, string(files[name]), config.sectionFor(name)) {
				if data.notes[n.ID].Node.ID != "" {
					return nil, fmt.Errorf("%s: duplicate node ID %q", name, n.ID)
				}
				data.notes[n.ID] = localNote{Node: n}
			}
		case strings.HasPrefix(name, localDir+"/notes/"):
			var note localNote
			if err := decodeLocal(files[name], &note); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			n := note.Node
			if n.ID == "" || data.notes[n.ID].Node.ID != "" || !sections[n.SectionID] || !validNode(n.node) {
				return nil, fmt.Errorf("%s: invalid or duplicate note", name)
			}
			data.notes[n.ID] = note
		case strings.HasPrefix(name, localDir+"/requests/"):
			var batch localBatch
			if err := decodeLocal(files[name], &batch); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if batch.OperationID == "" || len(batch.Items) == 0 {
				return nil, fmt.Errorf("%s: empty request batch", name)
			}
			data.batches[name] = batch
			for _, item := range batch.Items {
				if item.ID == "" || data.requests[item.ID].ID != "" || !allowed(item.State, requestStates) ||
					!allowed(item.Kind, requestKinds) || item.Version < 1 || (item.SectionID != "" && !sections[item.SectionID]) {
					return nil, fmt.Errorf("%s: invalid or duplicate request", name)
				}
				data.requests[item.ID] = item
			}
		}
	}
	var recorded []seedLink
	if content, ok := files[localDir+"/links.json"]; ok {
		if err := decodeLocal(content, &recorded); err != nil {
			return nil, fmt.Errorf("links.json: %w", err)
		}
	}
	edges := map[seedLink]bool{}
	add := func(link seedLink) {
		if link.From != link.To && data.notes[link.From].Node.ID != "" && data.notes[link.To].Node.ID != "" && allowed(link.Kind, linkKinds) {
			edges[link] = true
		}
	}
	for _, link := range recorded {
		add(link)
	}
	codeExists := map[string]bool{}
	for id, note := range data.notes {
		n := &note.Node
		if n.SourcePath == "" {
			continue
		}
		code := map[string]bool{}
		for _, match := range markdownLocalLink.FindAllStringSubmatch(n.Body, -1) {
			dest, fragment, ok := resolveLocalLink(n.SourcePath, match[1])
			if !ok {
				continue
			}
			target := dest
			if fragment != "" && data.notes[dest+"#"+fragment].Node.ID != "" {
				target += "#" + fragment
			}
			add(seedLink{From: id, To: target, Kind: "related"})
			if allowed(path.Ext(dest), []string{".go", ".kt", ".kts", ".vue", ".js", ".ts", ".py", ".rs"}) {
				exists, known := codeExists[dest]
				if !known {
					stat, err := root.Stat(dest)
					exists = err == nil && stat.Mode().IsRegular()
					codeExists[dest] = exists
				}
				if exists {
					code[dest] = true
				}
			}
		}
		n.CodePaths = sortedLocalKeys(code)
		data.notes[id] = note
	}
	for link := range edges {
		data.links = append(data.links, link)
	}
	sort.Slice(data.links, func(i, j int) bool {
		a, b := data.links[i], data.links[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})
	return data, nil
}

func (c localConfig) sectionFor(name string) string {
	result, best := c.FallbackSection, 0
	for _, section := range c.Sections {
		for _, pattern := range section.Paths {
			if prefix, ok := strings.CutSuffix(pattern, "*"); (ok && strings.HasPrefix(name, prefix)) || name == pattern {
				score := len(prefix)
				if !ok {
					score++
				}
				if score > best {
					result, best = section.ID, score
				}
			}
		}
	}
	return result
}

var markdownLocalLink = regexp.MustCompile(`\]\((<?[^\s)<>]+>?)`)
var markdownLocalHeading = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.+?)[ \t]*#*[ \t]*$`)
var markdownLocalFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

func resolveLocalLink(source, target string) (string, string, bool) {
	u, err := url.Parse(strings.Trim(target, "<>"))
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "/") {
		return "", "", false
	}
	dest := source
	if u.Path != "" {
		dest = path.Join(path.Dir(source), u.Path)
	}
	return dest, u.Fragment, fs.ValidPath(dest) && dest != "."
}

func localMarkdownNodes(name, raw, sectionID string) []localNode {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	title := path.Base(name)
	status, kind := "current", "guide"
	if strings.HasPrefix(name, "md/reports/") || strings.Contains(raw, "Статус: указатель исторического отчёта") {
		status, kind = "historical", "evidence"
	}
	type heading struct {
		line   int
		title  string
		anchor string
	}
	var headings []heading
	count := map[string]int{}
	start := 0
	var fence string
	for i, line := range lines {
		if match := markdownLocalFence.FindStringSubmatch(line); match != nil {
			mark := match[1]
			if fence == "" {
				fence = mark
			} else if mark[0] == fence[0] && len(mark) >= len(fence) && strings.TrimSpace(strings.TrimSpace(line)[len(mark):]) == "" {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		match := markdownLocalHeading.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if match[1] == "#" && start == 0 {
			title, start = strings.TrimSpace(match[2]), i+1
		}
		anchor := localSlug(match[2])
		index := count[anchor]
		count[anchor]++
		if index > 0 {
			anchor = fmt.Sprintf("%s-%d", anchor, index)
		}
		if match[1] == "##" && len(raw) > 5000 {
			headings = append(headings, heading{i, match[2], anchor})
		}
	}
	var result []localNode
	makeNode := func(id, parent, label string, first, last int, body string) {
		result = append(result, localNode{
			node:       node{ID: id, ParentID: parent, SectionID: sectionID, Title: label, Body: body, Kind: kind, Status: status, Version: 1},
			SourcePath: name, FirstLine: first + 1, LastLine: last,
		})
	}
	if len(headings) == 0 {
		makeNode(name, "", title, 0, len(lines), strings.TrimSpace(raw))
		return result
	}
	intro := strings.TrimSpace(strings.Join(lines[start:headings[0].line], "\n"))
	if intro == "" {
		var titles []string
		for _, h := range headings {
			titles = append(titles, h.title)
		}
		intro = strings.Join(titles, "\n")
	}
	makeNode(name, "", title, 0, headings[0].line, intro)
	for i, h := range headings {
		last := len(lines)
		if i+1 < len(headings) {
			last = headings[i+1].line
		}
		makeNode(name+"#"+h.anchor, name, title+" · "+h.title, h.line, last, strings.TrimSpace(strings.Join(lines[h.line:last], "\n")))
	}
	return result
}

func localSlug(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			return unicode.ToLower(r)
		case r == ' ':
			return '-'
		default:
			return -1
		}
	}, s)
}
