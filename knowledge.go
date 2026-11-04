package main

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

const nodeColumns = "id,section_id,coalesce(parent_id,''),title,kind,body,status,source_url,source_revision,coalesce(verified_at::text,''),version"
const nodePreview = "id,section_id,coalesce(parent_id,''),title,kind,left(body,220),status,source_url,source_revision,coalesce(verified_at::text,''),version"

func (s *store) projects(ctx context.Context, uid, only string) ([]project, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT p.id,p.name,p.overview,p.repository_url,m.role FROM cs.projects p JOIN cs.memberships m ON p.id=m.project_id WHERE m.user_id=$1 AND ($2='' OR p.id=$2) ORDER BY p.name,p.id LIMIT 100"), uid, only)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (project, error) {
		var p project
		err := row.Scan(&p.ID, &p.Name, &p.Overview, &p.RepositoryURL, &p.Role)
		return p, err
	})
}

func (s *store) project(ctx context.Context, uid, pid string) (project, error) {
	items, err := s.projects(ctx, uid, pid)
	if err != nil {
		return project{}, err
	}
	if pid == "" || len(items) != 1 {
		return project{}, errForbidden
	}
	return items[0], nil
}

func (s *store) createProject(ctx context.Context, uid string, p project) (string, error) {
	if !textOK(p.Name, 1, 160) || !textOK(p.Overview, 0, 6000) || !httpURL(p.RepositoryURL) {
		return "", errInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	id, err := s.insertProject(ctx, tx, uid, p)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *store) insertProject(ctx context.Context, tx pgx.Tx, uid string, p project) (string, error) {
	id := newID()
	if _, err := tx.Exec(ctx, s.q("INSERT INTO cs.projects(id,name,overview,repository_url) VALUES($1,$2,$3,$4)"), id, p.Name, p.Overview, p.RepositoryURL); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, s.q("INSERT INTO cs.memberships(project_id,user_id,role) VALUES($1,$2,'owner')"), id, uid); err != nil {
		return "", err
	}
	return id, nil
}

func (s *store) sections(ctx context.Context, pid string) ([]section, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT id,title,summary FROM cs.sections WHERE project_id=$1 ORDER BY position,title,id LIMIT 200"), pid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (section, error) {
		var v section
		err := row.Scan(&v.ID, &v.Title, &v.Summary)
		return v, err
	})
}

func (s *store) section(ctx context.Context, pid, id string) (section, error) {
	var v section
	err := s.pool.QueryRow(ctx, s.q("SELECT id,title,summary FROM cs.sections WHERE project_id=$1 AND id=$2"), pid, id).Scan(&v.ID, &v.Title, &v.Summary)
	return v, missing(err)
}

type nodeFilter struct {
	SectionID, Kind, Status, Query, Platform string
	Limit, Offset                            int
}

func (s *store) nodes(ctx context.Context, pid string, f nodeFilter) (page[node], error) {
	if f.Status == "" {
		f.Status = "current"
	}
	if f.Status != "all" && !allowed(f.Status, nodeStates) || f.Kind != "" && !allowed(f.Kind, nodeKinds) ||
		f.Platform != "" && !allowed(f.Platform, platforms) || !textOK(f.Query, 0, 240) {
		return page[node]{}, errInvalid
	}
	limit, offset := pageArgs(f.Limit, f.Offset)
	if query := strings.TrimSpace(f.Query); query != "" {
		return s.searchNodes(ctx, pid, f, query, limit, offset)
	}
	rows, err := s.pool.Query(ctx, s.q("SELECT "+nodePreview+" FROM cs.nodes n WHERE "+nodeFiltersSQL+
		" ORDER BY (status='current') DESC,title,id LIMIT $6 OFFSET $7"),
		pid, f.SectionID, f.Kind, f.Status, f.Platform, limit+1, offset)
	if err != nil {
		return page[node]{}, err
	}
	items, err := pgx.CollectRows(rows, nodeScan)
	return paginate(items, limit, offset), err
}

const nodeFiltersSQL = `n.project_id=$1
	AND ($2='' OR n.section_id=$2) AND ($3='' OR n.kind=$3) AND ($4='all' OR n.status=$4)
	AND ($5='' OR NOT EXISTS (SELECT 1 FROM cs.node_platforms np WHERE np.project_id=n.project_id AND np.node_id=n.id)
		OR EXISTS (SELECT 1 FROM cs.node_platforms np WHERE np.project_id=n.project_id AND np.node_id=n.id AND np.platform=$5))`

func (s *store) node(ctx context.Context, pid, id string) (node, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT "+nodeColumns+" FROM cs.nodes WHERE project_id=$1 AND id=$2"), pid, id)
	if err != nil {
		return node{}, err
	}
	v, err := pgx.CollectOneRow(rows, nodeScan)
	return v, missing(err)
}

func (s *store) children(ctx context.Context, pid, id string) ([]node, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT "+nodePreview+" FROM cs.nodes WHERE project_id=$1 AND parent_id=$2 ORDER BY title,id LIMIT 50"), pid, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, nodeScan)
}

func (s *store) nodePlatforms(ctx context.Context, pid, id string) ([]platformDetail, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT platform,implementation,verification,availability FROM cs.node_platforms WHERE project_id=$1 AND node_id=$2 ORDER BY platform"), pid, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (platformDetail, error) {
		var v platformDetail
		err := row.Scan(&v.Platform, &v.Implementation, &v.Verification, &v.Availability)
		return v, err
	})
}

func (s *store) related(ctx context.Context, pid, id, kind string, limit, offset int) (page[relatedNode], error) {
	if kind != "" && !allowed(kind, linkKinds) {
		return page[relatedNode]{}, errInvalid
	}
	limit, offset = pageArgs(limit, offset)
	rows, err := s.pool.Query(ctx, s.q(`SELECT n.id,n.section_id,n.title,n.kind,n.status,l.kind,
		CASE WHEN l.from_id=$2 THEN 'outgoing' ELSE 'incoming' END
		FROM cs.node_links l JOIN cs.nodes n ON n.project_id=l.project_id
		AND n.id=CASE WHEN l.from_id=$2 THEN l.to_id ELSE l.from_id END
		WHERE l.project_id=$1 AND (l.from_id=$2 OR l.to_id=$2) AND ($3='' OR l.kind=$3)
		ORDER BY n.title,l.kind,n.id LIMIT $4 OFFSET $5`), pid, id, kind, limit+1, offset)
	if err != nil {
		return page[relatedNode]{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (relatedNode, error) {
		var v relatedNode
		err := row.Scan(&v.NodeID, &v.SectionID, &v.Title, &v.Kind, &v.Status, &v.Relation, &v.Direction)
		return v, err
	})
	return paginate(items, limit, offset), err
}

func (s *store) insertSection(ctx context.Context, tx pgx.Tx, pid string, v section, position int) (string, error) {
	if !textOK(v.Title, 1, 160) || !textOK(v.Summary, 0, 2000) {
		return "", errInvalid
	}
	id := newID()
	_, err := tx.Exec(ctx, s.q("INSERT INTO cs.sections(id,project_id,title,summary,position) VALUES($1,$2,$3,$4,$5)"), id, pid, v.Title, v.Summary, position)
	return id, err
}

func (s *store) insertNode(ctx context.Context, tx pgx.Tx, pid string, v node) (string, error) {
	if !validNode(v) {
		return "", errInvalid
	}
	date, err := verifiedDate(v.VerifiedAt)
	if err != nil {
		return "", err
	}
	id := newID()
	tag, err := tx.Exec(ctx, s.q(`INSERT INTO cs.nodes(id,project_id,section_id,parent_id,title,kind,body,status,source_url,source_revision,verified_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11 FROM cs.sections WHERE project_id=$2 AND id=$3
		AND ($4::text IS NULL OR EXISTS(SELECT 1 FROM cs.nodes WHERE project_id=$2 AND section_id=$3 AND id=$4))`),
		id, pid, v.SectionID, nullString(v.ParentID), v.Title, v.Kind, v.Body, v.Status, v.SourceURL, v.SourceRevision, date)
	if err == nil && tag.RowsAffected() != 1 {
		return "", errMissing
	}
	return id, err
}

func validNode(v node) bool {
	return textOK(v.Title, 1, 160) && textOK(v.Body, 1, 16000) && textOK(v.SourceURL, 0, 2000) && textOK(v.SourceRevision, 0, 120) &&
		httpURL(v.SourceURL) && allowed(v.Kind, nodeKinds) && allowed(v.Status, nodeStates)
}

func (s *store) editNode(ctx context.Context, tx pgx.Tx, pid string, v node) error {
	if !validNode(v) || v.Version < 1 {
		return errInvalid
	}
	date, err := verifiedDate(v.VerifiedAt)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, s.q("UPDATE cs.nodes SET title=$4,kind=$5,body=$6,status=$7,source_url=$8,source_revision=$9,verified_at=$10,version=version+1,updated_at=now() WHERE project_id=$1 AND id=$2 AND version=$3"),
		pid, v.ID, v.Version, v.Title, v.Kind, v.Body, v.Status, v.SourceURL, v.SourceRevision, date)
	if err == nil && tag.RowsAffected() != 1 {
		return errConflict
	}
	return err
}

func (s *store) setPlatform(ctx context.Context, tx pgx.Tx, pid, id string, v platformDetail) error {
	if !allowed(v.Platform, platforms) || !textOK(v.Implementation, 1, 1000) || !textOK(v.Verification, 0, 1000) || !textOK(v.Availability, 0, 1000) {
		return errInvalid
	}
	tag, err := tx.Exec(ctx, s.q(`INSERT INTO cs.node_platforms(project_id,node_id,platform,implementation,verification,availability)
		SELECT $1,$2,$3,$4,$5,$6 FROM cs.nodes WHERE project_id=$1 AND id=$2
		ON CONFLICT(project_id,node_id,platform) DO UPDATE SET implementation=excluded.implementation,verification=excluded.verification,availability=excluded.availability`), pid, id, v.Platform, v.Implementation, v.Verification, v.Availability)
	if err == nil && tag.RowsAffected() != 1 {
		return errMissing
	}
	return err
}

func (s *store) link(ctx context.Context, tx pgx.Tx, pid, from, to, kind string) error {
	if from == to || from == "" || to == "" || !allowed(kind, linkKinds) {
		return errInvalid
	}
	_, err := tx.Exec(ctx, s.q(`INSERT INTO cs.node_links(project_id,from_id,to_id,kind) SELECT $1,$2,$3,$4
		WHERE EXISTS(SELECT 1 FROM cs.nodes WHERE project_id=$1 AND id=$2) AND EXISTS(SELECT 1 FROM cs.nodes WHERE project_id=$1 AND id=$3)
		ON CONFLICT DO NOTHING`), pid, from, to, kind)
	return err
}

func inTx(ctx context.Context, s *store, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func isUserError(err error) bool {
	return errors.Is(err, errInvalid) || errors.Is(err, errMissing) || errors.Is(err, errForbidden) || errors.Is(err, errConflict)
}
