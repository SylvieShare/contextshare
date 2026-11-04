package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	errInvalid   = errors.New("некорректные параметры")
	errForbidden = errors.New("нет доступа")
	errMissing   = errors.New("запись не найдена")
	errConflict  = errors.New("запись изменилась или действие уже выполнено")
)

const scopeRead = "project:read"
const scopeDraft = "requests:add"
const scopeWork = "requests:work"

var platforms = []string{"web", "android", "backend"}
var nodeKinds = []string{"feature", "rule", "decision", "limitation", "guide", "evidence"}
var nodeStates = []string{"current", "planned", "historical"}
var linkKinds = []string{"related", "depends_on", "verified_by", "supersedes"}
var requestKinds = []string{"feature", "bug", "idea"}
var requestStates = []string{"inbox", "ready", "active", "done", "archived"}

type project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Overview      string `json:"overview,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty"`
	Role          string `json:"role,omitempty"`
}

type section struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type node struct {
	ID             string `json:"id"`
	SectionID      string `json:"section_id"`
	ParentID       string `json:"parent_id,omitempty"`
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	Body           string `json:"body"`
	Status         string `json:"status"`
	SourceURL      string `json:"source_url,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	VerifiedAt     string `json:"verified_at,omitempty"`
	Version        int64  `json:"version"`
}

type platformDetail struct {
	Platform       string `json:"platform"`
	Implementation string `json:"implementation"`
	Verification   string `json:"verification"`
	Availability   string `json:"availability"`
}

type relatedNode struct {
	NodeID    string `json:"node_id"`
	SectionID string `json:"section_id"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Relation  string `json:"relation"`
	Direction string `json:"direction"`
}

type requestItem struct {
	ID        string     `json:"id"`
	SectionID string     `json:"section_id,omitempty"`
	NodeID    string     `json:"node_id,omitempty"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Kind      string     `json:"kind"`
	State     string     `json:"state"`
	Criteria  string     `json:"criteria,omitempty"`
	Version   int64      `json:"version"`
	Work      []workPart `json:"work,omitempty"`
}

type workPart struct {
	Platform string `json:"platform"`
	State    string `json:"state"`
	Evidence string `json:"evidence,omitempty"`
}

type requestInput struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Kind      string   `json:"kind"`
	SectionID string   `json:"section_id,omitempty"`
	NodeID    string   `json:"node_id,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
}

type page[T any] struct {
	Items      []T  `json:"items"`
	NextOffset *int `json:"next_offset,omitempty"`
}

func pageArgs(limit, offset int) (int, int) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if offset > 10000 {
		offset = 10000
	}
	return limit, offset
}

func paginate[T any](items []T, limit, offset int) page[T] {
	if items == nil {
		items = []T{}
	}
	if len(items) <= limit {
		return page[T]{Items: items}
	}
	next := offset + limit
	return page[T]{Items: items[:limit], NextOffset: &next}
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func randomToken(prefix string) string {
	var b [32]byte
	rand.Read(b[:])
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

func digest(value string) []byte {
	h := sha256.Sum256([]byte(value))
	return h[:]
}

func httpURL(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && u.Hostname() != "" && u.User == nil && (u.Scheme == "http" || u.Scheme == "https")
}

func textOK(value string, min, max int) bool {
	n := len(strings.TrimSpace(value))
	return n >= min && n <= max
}

func allowed(value string, options []string) bool { return slices.Contains(options, value) }

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nodeScan(row pgx.CollectableRow) (node, error) {
	var n node
	err := row.Scan(&n.ID, &n.SectionID, &n.ParentID, &n.Title, &n.Kind, &n.Body, &n.Status, &n.SourceURL, &n.SourceRevision, &n.VerifiedAt, &n.Version)
	return n, err
}

func requestScan(row pgx.CollectableRow) (requestItem, error) {
	var r requestItem
	err := row.Scan(&r.ID, &r.SectionID, &r.NodeID, &r.Title, &r.Body, &r.Kind, &r.State, &r.Criteria, &r.Version)
	return r, err
}

func missing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errMissing
	}
	return err
}

func (s *store) role(ctx context.Context, userID, projectID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, s.q("SELECT role FROM cs.memberships WHERE project_id=$1 AND user_id=$2"), projectID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errForbidden
	}
	return role, err
}

func (s *store) audit(ctx context.Context, tx pgx.Tx, userID, projectID, action, entity string) error {
	_, err := tx.Exec(ctx, s.q("INSERT INTO cs.audit(project_id,user_id,action,entity_id) VALUES($1,$2,$3,$4)"), projectID, userID, action, entity)
	return err
}

func verifiedDate(value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, errInvalid
	}
	return t, nil
}
