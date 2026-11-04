package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxWikiDocumentSize = 2 << 20

type wikiReadArgs struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	LineCount int    `json:"line_count,omitempty"`
}

type wikiWriteArgs struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Expected  string `json:"expected_sha256,omitempty"`
	Content   string `json:"content,omitempty"`
	Find      string `json:"find,omitempty"`
	Replace   string `json:"replace,omitempty"`
}

type wikiDocument struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content,omitempty"`
	Lines   int    `json:"lines,omitempty"`
}

type wikiValidation struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
}

type wikiLinkArgs struct {
	ProjectID string `json:"project_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Kind      string `json:"kind"`
	Remove    bool   `json:"remove,omitempty"`
}

type wikiDeleteArgs struct {
	ProjectID string `json:"project_id"`
	Path      string `json:"path"`
	Expected  string `json:"expected_sha256"`
}

type wikiAttachment struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int    `json:"bytes"`
	Content string `json:"content,omitempty"`
}

type wikiAttachmentWriteArgs struct {
	ProjectID  string `json:"project_id"`
	Path       string `json:"path"`
	Expected   string `json:"expected_sha256,omitempty"`
	Content    string `json:"content,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
}

var wikiAttachmentTypes = []string{".json", ".csv", ".png", ".webp"}

func (w *localWorkspace) registerLocalWiki(server *mcp.Server) {
	localTool(server, w, "get_wiki_document", "Read wiki Markdown and its full-file SHA-256. Use start_line and line_count to read a range.", true, true,
		func(in wikiReadArgs) string { return in.ProjectID },
		func(_ context.Context, data *localSnapshot, in wikiReadArgs) (wikiDocument, error) {
			return w.readWikiRange(data.config, in)
		})
	localTool(server, w, "write_wiki_document", "Create or update wiki Markdown. For updates pass the full-file expected_sha256. Supply content, or find and replace a unique existing fragment.", false, false,
		func(in wikiWriteArgs) string { return in.ProjectID },
		func(ctx context.Context, _ *localSnapshot, in wikiWriteArgs) (wikiDocument, error) {
			return w.writeWiki(ctx, in)
		})
	localTool(server, w, "delete_wiki_document", "Remove a wiki document by SHA-256 after removing its incoming links. Use only when requested.", false, false,
		func(in wikiDeleteArgs) string { return in.ProjectID },
		func(ctx context.Context, _ *localSnapshot, in wikiDeleteArgs) (wikiDocument, error) {
			return w.deleteWiki(ctx, in)
		})
	localTool(server, w, "get_wiki_attachment", "Read a JSON or CSV attachment, or locate an image by path and SHA-256.", true, true,
		func(in wikiReadArgs) string { return in.ProjectID },
		func(_ context.Context, data *localSnapshot, in wikiReadArgs) (wikiAttachment, error) {
			return w.readWikiAttachment(data.config, in.Path)
		})
	localTool(server, w, "write_wiki_attachment", "Attach JSON, CSV, PNG or WebP. Provide text or a worktree-relative source_path. For replacements pass the existing SHA-256.", false, false,
		func(in wikiAttachmentWriteArgs) string { return in.ProjectID },
		func(ctx context.Context, _ *localSnapshot, in wikiAttachmentWriteArgs) (wikiAttachment, error) {
			return w.writeWikiAttachment(ctx, in)
		})
	localTool(server, w, "validate_wiki", "Check the complete wiki structure, links, anchors and stored ContextShare relationships.", true, true,
		func(in projectArgs) string { return in.ProjectID },
		func(ctx context.Context, data *localSnapshot, _ projectArgs) (wikiValidation, error) {
			return w.validateWiki(ctx, data)
		})
	localTool(server, w, "set_wiki_link", "Add or remove a typed relationship between existing node IDs.", false, true,
		func(in wikiLinkArgs) string { return in.ProjectID },
		func(ctx context.Context, _ *localSnapshot, in wikiLinkArgs) (seedLink, error) {
			return w.setWikiLink(ctx, in)
		})
}

func wikiPath(config localConfig, requested string, extensions ...string) (string, error) {
	if !fs.ValidPath(requested) || strings.ContainsAny(requested, "\\:") || !allowed(path.Ext(requested), extensions) {
		return "", errInvalid
	}
	wiki := config.wikiDir()
	if strings.HasPrefix(requested, wiki+"/") {
		return requested, nil
	}
	if strings.HasPrefix(requested, ".") {
		return "", errInvalid
	}
	return wiki + "/" + requested, nil
}

func (w *localWorkspace) readWiki(config localConfig, requested string) (wikiDocument, error) {
	name, err := wikiPath(config, requested, ".md")
	if err != nil {
		return wikiDocument{}, err
	}
	file, err := w.root.Open(name)
	if err != nil {
		return wikiDocument{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxWikiDocumentSize+1))
	if err != nil {
		return wikiDocument{}, err
	}
	if len(content) > maxWikiDocumentSize || !utf8.Valid(content) {
		return wikiDocument{}, errInvalid
	}
	return wikiDocument{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(content)), Content: string(content)}, nil
}

func (w *localWorkspace) readWikiRange(config localConfig, in wikiReadArgs) (wikiDocument, error) {
	doc, err := w.readWiki(config, in.Path)
	if err != nil {
		return wikiDocument{}, err
	}
	lines := strings.SplitAfter(doc.Content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	doc.Lines = len(lines)
	if in.StartLine < 0 || in.LineCount < 0 || in.StartLine > len(lines) {
		return wikiDocument{}, errInvalid
	}
	if in.StartLine > 0 || in.LineCount > 0 {
		start := max(0, in.StartLine-1)
		count := in.LineCount
		if count == 0 {
			count = 120
		}
		doc.Content = strings.Join(lines[start:start+min(count, len(lines)-start)], "")
	}
	return doc, nil
}

func (w *localWorkspace) writeWiki(ctx context.Context, in wikiWriteArgs) (wikiDocument, error) {
	if in.Find != "" && (in.Content != "" || in.Expected == "") || in.Find == "" && in.Replace != "" {
		return wikiDocument{}, errInvalid
	}
	var result wikiDocument
	err := w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if data.config.Project.ID != in.ProjectID {
			return errForbidden
		}
		name, err := wikiPath(data.config, in.Path, ".md")
		if err != nil {
			return err
		}
		old, err := w.readWiki(data.config, name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if old.SHA256 != in.Expected {
			return fmt.Errorf("wiki revision changed or create/update mismatch: %w", errConflict)
		}
		content := in.Content
		if in.Find != "" {
			if strings.Count(old.Content, in.Find) != 1 {
				return fmt.Errorf("expected one occurrence of the fragment: %w", errConflict)
			}
			content = strings.Replace(old.Content, in.Find, in.Replace, 1)
		}
		if len(content) > maxWikiDocumentSize || !utf8.ValidString(content) || !strings.HasPrefix(strings.TrimSpace(content), "# ") {
			return fmt.Errorf("wiki document must start with a Markdown title and fit within 2 MiB")
		}
		body := []byte(content)
		if err := w.saveLocalFile(name, body); err != nil {
			return err
		}
		result = wikiDocument{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
		return nil
	})
	return result, err
}

func (w *localWorkspace) deleteWiki(ctx context.Context, in wikiDeleteArgs) (wikiDocument, error) {
	var result wikiDocument
	err := w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if in.ProjectID != data.config.Project.ID {
			return errForbidden
		}
		doc, err := w.readWiki(data.config, in.Path)
		if err != nil {
			return err
		}
		if in.Expected != doc.SHA256 || doc.Path == data.config.wikiDir()+"/README.md" {
			return errConflict
		}
		belongs := func(id string) bool { return id == doc.Path || strings.HasPrefix(id, doc.Path+"#") }
		for _, link := range data.links {
			if belongs(link.To) && !belongs(link.From) {
				return fmt.Errorf("incoming wiki link from %s: %w", link.From, errConflict)
			}
		}
		for _, request := range data.requests {
			if belongs(request.NodeID) {
				return fmt.Errorf("linked request %s: %w", request.ID, errConflict)
			}
		}
		links, err := w.recordedWikiLinks()
		if err != nil {
			return err
		}
		for _, link := range links {
			if belongs(link.From) || belongs(link.To) {
				return fmt.Errorf("typed wiki link %s → %s: %w", link.From, link.To, errConflict)
			}
		}
		if err := w.root.Remove(doc.Path); err != nil {
			return err
		}
		result = wikiDocument{Path: doc.Path, SHA256: doc.SHA256}
		return nil
	})
	return result, err
}

func (w *localWorkspace) readWikiAttachment(config localConfig, requested string) (wikiAttachment, error) {
	name, err := wikiPath(config, requested, wikiAttachmentTypes...)
	if err != nil {
		return wikiAttachment{}, err
	}
	f, err := w.root.Open(name)
	if err != nil {
		return wikiAttachment{}, err
	}
	defer f.Close()
	h := sha256.New()
	result := wikiAttachment{Path: name}
	if allowed(path.Ext(name), []string{".json", ".csv"}) {
		raw, err := io.ReadAll(io.LimitReader(f, maxWikiDocumentSize+1))
		if err != nil {
			return wikiAttachment{}, err
		}
		if len(raw) > maxWikiDocumentSize || !utf8.Valid(raw) {
			return wikiAttachment{}, errInvalid
		}
		h.Write(raw)
		result.Bytes, result.Content = len(raw), string(raw)
	} else {
		size, err := io.Copy(h, f)
		if err != nil {
			return wikiAttachment{}, err
		}
		result.Bytes = int(size)
	}
	result.SHA256 = fmt.Sprintf("%x", h.Sum(nil))
	return result, nil
}

func (w *localWorkspace) writeWikiAttachment(ctx context.Context, in wikiAttachmentWriteArgs) (wikiAttachment, error) {
	if (in.Content == "") == (in.SourcePath == "") {
		return wikiAttachment{}, errInvalid
	}
	var result wikiAttachment
	err := w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if data.config.Project.ID != in.ProjectID {
			return errForbidden
		}
		name, err := wikiPath(data.config, in.Path, wikiAttachmentTypes...)
		if err != nil {
			return err
		}
		old, err := w.readWikiAttachment(data.config, name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if old.SHA256 != in.Expected {
			return errConflict
		}
		ext := path.Ext(name)
		limit := maxWikiDocumentSize
		if ext == ".png" || ext == ".webp" {
			limit = 64 << 20
		}
		body := []byte(in.Content)
		if in.SourcePath != "" {
			if !fs.ValidPath(in.SourcePath) || strings.ContainsAny(in.SourcePath, "\\:") || strings.HasPrefix(in.SourcePath, ".git") {
				return errInvalid
			}
			f, err := w.root.Open(in.SourcePath)
			if err != nil {
				return err
			}
			body, err = io.ReadAll(io.LimitReader(f, int64(limit+1)))
			f.Close()
			if err != nil {
				return err
			}
		}
		if len(body) == 0 || len(body) > limit ||
			ext == ".json" && (!json.Valid(body) || !utf8.Valid(body)) || ext == ".csv" && !utf8.Valid(body) ||
			ext == ".png" && http.DetectContentType(body) != "image/png" ||
			ext == ".webp" && http.DetectContentType(body) != "image/webp" {
			return errInvalid
		}
		if err := w.saveLocalFile(name, body); err != nil {
			return err
		}
		result = wikiAttachment{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(body)), Bytes: len(body)}
		return nil
	})
	return result, err
}

func (w *localWorkspace) recordedWikiLinks() ([]seedLink, error) {
	raw, err := w.root.ReadFile(localDir + "/links.json")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var links []seedLink
	err = decodeLocal(raw, &links)
	return links, err
}

func (w *localWorkspace) setWikiLink(ctx context.Context, in wikiLinkArgs) (seedLink, error) {
	link := seedLink{From: in.From, To: in.To, Kind: in.Kind}
	err := w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if data.config.Project.ID != in.ProjectID {
			return errForbidden
		}
		if link.From == link.To || !allowed(link.Kind, linkKinds) ||
			!in.Remove && (data.notes[link.From].Node.ID == "" || data.notes[link.To].Node.ID == "") {
			return errInvalid
		}
		links, err := w.recordedWikiLinks()
		if err != nil {
			return err
		}
		for i, old := range links {
			if old == link {
				if !in.Remove {
					return nil
				}
				links = append(links[:i], links[i+1:]...)
				break
			}
		}
		if !in.Remove {
			links = append(links, link)
		}
		body, err := json.MarshalIndent(links, "", "  ")
		if err != nil {
			return err
		}
		return w.saveLocalFile(localDir+"/links.json", append(body, '\n'))
	})
	return link, err
}

func (w *localWorkspace) validateWiki(ctx context.Context, data *localSnapshot) (wikiValidation, error) {
	links, err := w.recordedWikiLinks()
	if err != nil {
		return wikiValidation{}, err
	}
	var failures []string
	for _, link := range links {
		if !allowed(link.Kind, linkKinds) || link.From == link.To ||
			data.notes[link.From].Node.ID == "" || data.notes[link.To].Node.ID == "" {
			failures = append(failures, fmt.Sprintf("invalid relationship %s → %s (%s)", link.From, link.To, link.Kind))
		}
	}
	for _, request := range data.requests {
		if request.NodeID != "" && data.notes[request.NodeID].Node.ID == "" {
			failures = append(failures, fmt.Sprintf("request %s points to missing node %s", request.ID, request.NodeID))
		}
	}
	if len(failures) > 0 {
		return wikiValidation{Output: strings.Join(failures, "\n")}, nil
	}
	python := "python3"
	if runtime.GOOS == "windows" {
		python = "python"
	}
	cmd := exec.CommandContext(ctx, python, "-X", "utf8", "scripts/check-wiki.py", "--strict")
	cmd.Dir = w.path
	output, err := cmd.CombinedOutput()
	if err != nil && len(output) == 0 {
		return wikiValidation{}, err
	}
	return wikiValidation{OK: err == nil, Output: strings.TrimSpace(string(output))}, nil
}

func (w *localWorkspace) wikiCLI(ctx context.Context, args []string) error {
	if len(args) != 2 || args[0] != "wiki" || !allowed(args[1], []string{"get", "write", "validate"}) {
		return fmt.Errorf("usage: contextshare local --project PATH wiki get|write|validate")
	}
	data, err := w.load()
	if err != nil {
		return err
	}
	var result any
	switch args[1] {
	case "get":
		var in wikiReadArgs
		if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
			return err
		}
		result, err = w.readWikiRange(data.config, in)
	case "write":
		var in wikiWriteArgs
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 4*maxWikiDocumentSize)).Decode(&in); err != nil {
			return err
		}
		in.ProjectID = data.config.Project.ID
		result, err = w.writeWiki(ctx, in)
	case "validate":
		result, err = w.validateWiki(ctx, data)
	}
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if validation, ok := result.(wikiValidation); ok && !validation.OK {
		return fmt.Errorf("wiki validation failed")
	}
	return nil
}
