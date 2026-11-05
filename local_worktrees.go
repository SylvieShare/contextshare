package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type worktreeInfo struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Dirty  bool   `json:"dirty"`
}

type localMetadata struct {
	Workspace worktreeInfo `json:"workspace"`
}

func (m *localMetadata) setWorkspace(info worktreeInfo) { m.Workspace = info }

type worktreeSelection struct {
	Path string `json:"path"`
}

type worktreeResult struct {
	localMetadata
	ProjectID string `json:"project_id"`
}

type worktreeList struct {
	Default   string         `json:"default_path"`
	Worktrees []worktreeInfo `json:"worktrees"`
}

func (w *localWorkspace) info(ctx context.Context) (worktreeInfo, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", w.path, "status", "--porcelain=v2", "--branch", "--untracked-files=normal").Output()
	if err != nil {
		return worktreeInfo{}, fmt.Errorf("read Git revision: %w", err)
	}
	info := worktreeInfo{Path: w.path}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			info.Commit = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.head "):
			info.Branch = strings.TrimPrefix(line, "# branch.head ")
		case line != "" && !strings.HasPrefix(line, "# "):
			info.Dirty = true
		}
	}
	return info, nil
}

func scopedProjectID(projectID, path string) string {
	hash := sha256.Sum256([]byte(filepath.Clean(path)))
	return fmt.Sprintf("%s@%x", projectID, hash[:8])
}

func (w *localWorkspace) acceptsProject(id, canonical string) bool {
	return id == canonical || id == scopedProjectID(canonical, w.path)
}

func (w *localWorkspace) resolveProject(ctx context.Context, id string) (*localWorkspace, error) {
	if w.acceptsProject(id, w.projectID) {
		return w, nil
	}
	w.routesMu.Lock()
	selected := w.routes[id]
	w.routesMu.Unlock()
	if selected == nil {
		return nil, fmt.Errorf("unknown workspace project_id: call select_worktree for the task's absolute path")
	}
	return selected, nil
}

func (w *localWorkspace) worktreePaths(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", w.path, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, field := range strings.Split(string(out), "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			paths = append(paths, filepath.Clean(filepath.FromSlash(strings.TrimPrefix(field, "worktree "))))
		}
	}
	return paths, nil
}

func (w *localWorkspace) selectWorktree(ctx context.Context, requested string) (*localWorkspace, string, error) {
	if !filepath.IsAbs(requested) {
		return nil, "", fmt.Errorf("select_worktree requires an absolute worktree path")
	}
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return nil, "", err
	}
	paths, err := w.worktreePaths(ctx)
	if err != nil {
		return nil, "", err
	}
	found := false
	for _, path := range paths {
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil && filepath.Clean(canonical) == filepath.Clean(resolved) {
			found = true
			break
		}
	}
	if !found {
		return nil, "", fmt.Errorf("path is not a worktree of the connected repository")
	}
	w.routesMu.Lock()
	defer w.routesMu.Unlock()
	for id, selected := range w.routes {
		if filepath.Clean(selected.path) == filepath.Clean(resolved) {
			return selected, id, nil
		}
	}
	selected := w
	if filepath.Clean(w.path) != filepath.Clean(resolved) {
		selected, err = openLocalWorkspace(ctx, resolved)
		if err != nil {
			return nil, "", err
		}
	}
	selected.mu.Lock()
	data, err := selected.load()
	selected.mu.Unlock()
	if err != nil {
		if selected != w {
			selected.close()
		}
		return nil, "", err
	}
	id := scopedProjectID(data.config.Project.ID, selected.path)
	if selected != w {
		if w.routes == nil {
			w.routes = map[string]*localWorkspace{}
		}
		w.routes[id] = selected
	}
	return selected, id, nil
}

func (w *localWorkspace) registerWorktrees(server *mcp.Server) {
	no := false
	mcp.AddTool(server, &mcp.Tool{Name: "list_worktrees", Description: "List this repository's worktrees that contain ContextShare data. Select the worktree used for the current task.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &no}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, worktreeList, error) {
			out := worktreeList{Default: w.path, Worktrees: []worktreeInfo{}}
			paths, err := w.worktreePaths(ctx)
			if err != nil {
				return nil, out, err
			}
			for _, path := range paths {
				if _, err := os.Stat(filepath.Join(path, localDir, "project.json")); err != nil {
					continue
				}
				info, err := (&localWorkspace{path: path}).info(ctx)
				if err != nil {
					return nil, out, err
				}
				out.Worktrees = append(out.Worktrees, info)
			}
			return nil, out, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "select_worktree", Description: "Bind an absolute Git worktree path to a project_id for this task. Pass the returned project_id in all later calls. Other tasks keep their own selection.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &no}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in worktreeSelection) (*mcp.CallToolResult, worktreeResult, error) {
			var out worktreeResult
			selected, id, err := w.selectWorktree(ctx, in.Path)
			if err != nil {
				return nil, out, err
			}
			out.ProjectID = id
			out.Workspace, err = selected.info(ctx)
			return nil, out, err
		})
}

func workspaceResult[O any](out *O, info worktreeInfo) (*mcp.CallToolResult, error) {
	if result, ok := any(out).(interface{ setWorkspace(worktreeInfo) }); ok {
		result.setWorkspace(info)
	}
	raw, err := workspaceJSON(out, info)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Meta:    mcp.Meta{"workspace": info},
		Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}, nil
}

func workspaceJSON(out any, info worktreeInfo) ([]byte, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	values["workspace"] = info
	return json.Marshal(values)
}
