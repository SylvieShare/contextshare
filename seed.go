package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/jackc/pgx/v5"
)

type seedNode struct {
	Key    string `json:"key"`
	Parent string `json:"parent,omitempty"`
	node
	Platforms []platformDetail `json:"platforms,omitempty"`
}

type seedSection struct {
	Key string `json:"key"`
	section
	Nodes []seedNode `json:"nodes"`
}

type seedLink struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type seedBundle struct {
	Project  project        `json:"project"`
	Sections []seedSection  `json:"sections"`
	Links    []seedLink     `json:"links"`
	Requests []requestInput `json:"requests"`
}

func readSeed(path string) (*seedBundle, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var seed seedBundle
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&seed); err != nil {
		return nil, err
	}
	if !textOK(seed.Project.Name, 1, 160) || !textOK(seed.Project.Overview, 0, 6000) || !httpURL(seed.Project.RepositoryURL) {
		return nil, errInvalid
	}
	return &seed, nil
}

func (s *store) applySeed(ctx context.Context, tx pgx.Tx, uid string, seed *seedBundle) error {
	if seed == nil {
		return nil
	}
	pid, err := s.insertProject(ctx, tx, uid, seed.Project)
	if err != nil {
		return err
	}
	sections, nodes := map[string]string{}, map[string]string{}
	for position, v := range seed.Sections {
		if v.Key == "" || sections[v.Key] != "" {
			return errInvalid
		}
		sid, err := s.insertSection(ctx, tx, pid, v.section, position)
		if err != nil {
			return err
		}
		sections[v.Key] = sid
		for _, row := range v.Nodes {
			if row.Key == "" || nodes[row.Key] != "" || row.Parent != "" && nodes[row.Parent] == "" {
				return errInvalid
			}
			row.SectionID, row.ParentID = sid, nodes[row.Parent]
			id, err := s.insertNode(ctx, tx, pid, row.node)
			if err != nil {
				return err
			}
			nodes[row.Key] = id
			for _, p := range row.Platforms {
				if err := s.setPlatform(ctx, tx, pid, id, p); err != nil {
					return err
				}
			}
		}
	}
	for _, l := range seed.Links {
		if err := s.link(ctx, tx, pid, nodes[l.From], nodes[l.To], l.Kind); err != nil {
			return err
		}
	}
	for _, r := range seed.Requests {
		if r.SectionID != "" && sections[r.SectionID] == "" || r.NodeID != "" && nodes[r.NodeID] == "" {
			return errInvalid
		}
		r.SectionID, r.NodeID = sections[r.SectionID], nodes[r.NodeID]
		if _, err := s.insertRequest(ctx, tx, uid, pid, r); err != nil {
			return err
		}
	}
	return s.audit(ctx, tx, uid, pid, "project.import", "pilot")
}
