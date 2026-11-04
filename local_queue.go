package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type localAdded struct {
	Items []addedRequest `json:"items"`
}

type localRequestChange struct {
	ProjectID       string   `json:"project_id"`
	RequestID       string   `json:"request_id"`
	ExpectedVersion int64    `json:"expected_version"`
	Platform        string   `json:"platform,omitempty"`
	Platforms       []string `json:"platforms,omitempty"`
	Criteria        string   `json:"criteria,omitempty"`
}

func (w *localWorkspace) registerLocalQueue(server *mcp.Server) {
	localTool(server, w, "list_requests", "Read the local queue without claiming any work.", true, true,
		func(in requestListArgs) string { return in.ProjectID },
		func(_ context.Context, data *localSnapshot, in requestListArgs) (page[requestItem], error) {
			if in.State != "" && !allowed(in.State, requestStates) || in.Kind != "" && !allowed(in.Kind, requestKinds) ||
				in.Platform != "" && !allowed(in.Platform, platforms) {
				return page[requestItem]{}, errInvalid
			}
			var out []requestItem
			for _, item := range data.requests {
				if in.State != "" && item.State != in.State || in.SectionID != "" && item.SectionID != in.SectionID ||
					in.Kind != "" && item.Kind != in.Kind {
					continue
				}
				if in.Platform != "" && !requestHasPlatform(item, in.Platform) {
					continue
				}
				out = append(out, item)
			}
			sort.Slice(out, func(i, j int) bool {
				if out[i].Title != out[j].Title {
					return out[i].Title < out[j].Title
				}
				return out[i].ID < out[j].ID
			})
			return localPage(out, in.Limit, in.Offset), nil
		})
	localTool(server, w, "get_request", "Read a request and its platform work.", true, true,
		func(in requestArgs) string { return in.ProjectID },
		func(_ context.Context, data *localSnapshot, in requestArgs) (requestItem, error) {
			item, ok := data.requests[in.RequestID]
			if !ok {
				return requestItem{}, errMissing
			}
			return item, nil
		})
	localTool(server, w, "add_requests", "Write explicitly requested ideas to the local Git inbox. Reuse operation_id when retrying.", false, true,
		func(in batchArgs) string { return in.ProjectID },
		func(ctx context.Context, _ *localSnapshot, in batchArgs) (localAdded, error) {
			return w.addLocalRequests(ctx, in)
		})
	for _, action := range []string{"prepare", "claim"} {
		description := "Prepare an inbox request with acceptance criteria and platform work. Only after the user asks."
		if action == "claim" {
			description = "Claim a ready platform part when the user explicitly asks to take the work."
		}
		localTool(server, w, action+"_request", description, false, false,
			func(in localRequestChange) string { return in.ProjectID },
			func(ctx context.Context, _ *localSnapshot, in localRequestChange) (requestItem, error) {
				return w.changeLocalRequest(ctx, in, action)
			})
	}
}

func requestHasPlatform(item requestItem, platform string) bool {
	for _, part := range item.Work {
		if part.Platform == platform {
			return true
		}
	}
	return false
}

func localWork(choices []string) ([]workPart, error) {
	var parts []workPart
	seen := map[string]bool{}
	for _, p := range choices {
		if !allowed(p, platforms) || seen[p] {
			return nil, errInvalid
		}
		seen[p] = true
		parts = append(parts, workPart{Platform: p, State: "ready"})
	}
	return parts, nil
}

func (w *localWorkspace) addLocalRequests(ctx context.Context, in batchArgs) (localAdded, error) {
	if !textOK(in.OperationID, 1, 120) || len(in.Requests) == 0 || len(in.Requests) > 20 {
		return localAdded{}, errInvalid
	}
	payload, err := json.Marshal(in.Requests)
	if err != nil {
		return localAdded{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	key := sha256.Sum256([]byte(in.OperationID))
	name := fmt.Sprintf("%s/requests/%x.json", localDir, key[:16])
	var out localAdded
	err = w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if data.config.Project.ID != in.ProjectID {
			return errForbidden
		}
		if batch, ok := data.batches[name]; ok {
			if batch.OperationID != in.OperationID || batch.InputHash != hash {
				return errConflict
			}
			for _, item := range batch.Items {
				out.Items = append(out.Items, addedRequest{ID: item.ID, Title: item.Title})
			}
			return nil
		}
		batch := localBatch{OperationID: in.OperationID, InputHash: hash}
		for _, input := range in.Requests {
			if !textOK(input.Title, 1, 160) || !textOK(input.Body, 1, 10000) || !allowed(input.Kind, requestKinds) {
				return errInvalid
			}
			if input.NodeID != "" {
				n, ok := data.notes[input.NodeID]
				if !ok || input.SectionID != "" && input.SectionID != n.Node.SectionID {
					return errInvalid
				}
				input.SectionID = n.Node.SectionID
			}
			if input.SectionID != "" {
				found := false
				for _, s := range data.config.Sections {
					found = found || s.ID == input.SectionID
				}
				if !found {
					return errInvalid
				}
			}
			parts, err := localWork(input.Platforms)
			if err != nil {
				return err
			}
			item := requestItem{ID: newID(), SectionID: input.SectionID, NodeID: input.NodeID, Title: strings.TrimSpace(input.Title),
				Body: input.Body, Kind: input.Kind, State: "inbox", Version: 1, Work: parts}
			batch.Items = append(batch.Items, item)
			out.Items = append(out.Items, addedRequest{ID: item.ID, Title: item.Title})
		}
		return w.saveLocalBatch(name, batch)
	})
	return out, err
}

func (w *localWorkspace) changeLocalRequest(ctx context.Context, in localRequestChange, action string) (requestItem, error) {
	var changed requestItem
	err := w.writeLock(ctx, func(_ *sql.Conn) error {
		data, err := w.load()
		if err != nil {
			return err
		}
		if data.config.Project.ID != in.ProjectID {
			return errForbidden
		}
		for name, batch := range data.batches {
			for i, item := range batch.Items {
				if item.ID != in.RequestID {
					continue
				}
				if item.Version != in.ExpectedVersion {
					return errConflict
				}
				switch action {
				case "prepare":
					if item.State != "inbox" || len(in.Platforms) == 0 || !textOK(in.Criteria, 1, 10000) {
						return errInvalid
					}
					item.Work, err = localWork(in.Platforms)
					if err != nil {
						return err
					}
					item.State, item.Criteria = "ready", strings.TrimSpace(in.Criteria)
				case "claim":
					if item.State != "ready" && item.State != "active" {
						return errConflict
					}
					found := false
					item.Work = append([]workPart(nil), item.Work...)
					for j, part := range item.Work {
						if part.Platform == in.Platform && part.State == "ready" {
							item.Work[j].State, found = "active", true
						}
					}
					if !found {
						return errConflict
					}
					item.State = "active"
				default:
					return errInvalid
				}
				item.Version++
				batch.Items = append([]requestItem(nil), batch.Items...)
				batch.Items[i], changed = item, item
				return w.saveLocalBatch(name, batch)
			}
		}
		return errMissing
	})
	return changed, err
}

func (w *localWorkspace) saveLocalBatch(name string, batch localBatch) error {
	body, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return err
	}
	dir := localDir + "/requests"
	if err := w.root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp := dir + "/.pending-" + newID()
	f, err := w.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer w.root.Remove(temp)
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return w.root.Rename(temp, name)
}
