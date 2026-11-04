package main

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Move the whole subtree in one statement. Links and request IDs remain stable.
func (s *store) moveSubtree(ctx context.Context, uid, pid, id, target string, version int64) error {
	if target == "" || version < 1 {
		return errInvalid
	}
	return inTx(ctx, s, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "move:"+pid); err != nil {
			return err
		}
		var current string
		err := tx.QueryRow(ctx, s.q("SELECT section_id FROM cs.nodes WHERE project_id=$1 AND id=$2 AND version=$3 FOR UPDATE"), pid, id, version).Scan(&current)
		if err != nil {
			return missing(err)
		}
		if current == target {
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx, s.q("SELECT EXISTS(SELECT 1 FROM cs.sections WHERE project_id=$1 AND id=$2)"), pid, target).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errMissing
		}
		_, err = tx.Exec(ctx, s.q(`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM cs.nodes WHERE project_id=$1 AND id=$2
			UNION ALL SELECT n.id FROM cs.nodes n JOIN subtree s ON n.parent_id=s.id WHERE n.project_id=$1
		), moved AS (
			UPDATE cs.nodes n SET section_id=$3,parent_id=CASE WHEN n.id=$2 THEN NULL ELSE n.parent_id END,
				version=version+1,updated_at=now()
			FROM subtree s WHERE n.project_id=$1 AND n.id=s.id RETURNING n.id
		)
		UPDATE cs.requests r SET section_id=$3,version=version+1,updated_at=now()
		FROM moved m WHERE r.project_id=$1 AND r.node_id=m.id`), pid, id, target)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, uid, pid, "node.move", id)
	})
}

func (a *app) moveNode(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PathValue("node")
	version, _ := strconv.ParseInt(r.PostForm.Get("version"), 10, 64)
	if err := a.db.moveSubtree(r.Context(), u.ID, p.ID, id, r.PostForm.Get("target_section"), version); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/nodes/"+id), 303)
}
