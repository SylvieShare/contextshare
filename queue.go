package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

const requestColumns = "id,coalesce(section_id,''),coalesce(node_id,''),title,body,kind,state,criteria,version"
const requestPreview = "id,coalesce(section_id,''),coalesce(node_id,''),title,left(body,220),kind,state,left(criteria,220),version"

type requestFilter struct {
	State, SectionID, Platform, Kind string
	Limit, Offset                    int
}

type addedRequest struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (s *store) requests(ctx context.Context, pid string, f requestFilter) (page[requestItem], error) {
	if f.State != "" && !allowed(f.State, requestStates) || f.Kind != "" && !allowed(f.Kind, requestKinds) || f.Platform != "" && !allowed(f.Platform, platforms) {
		return page[requestItem]{}, errInvalid
	}
	limit, offset := pageArgs(f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, s.q(`SELECT `+requestPreview+` FROM cs.requests r
		WHERE project_id=$1 AND ($2='' AND state<>'archived' OR state=$2 OR ($2='ready' AND $5<>'' AND state='active'))
		AND ($3='' OR section_id=$3) AND ($4='' OR kind=$4)
		AND ($5='' OR EXISTS(SELECT 1 FROM cs.request_work w WHERE w.project_id=r.project_id AND w.request_id=r.id AND w.platform=$5
			AND ($2<>'ready' OR w.state='ready')))
		ORDER BY created_at DESC,id LIMIT $6 OFFSET $7`), pid, f.State, f.SectionID, f.Kind, f.Platform, limit+1, offset)
	if err != nil {
		return page[requestItem]{}, err
	}
	items, err := pgx.CollectRows(rows, requestScan)
	return paginate(items, limit, offset), err
}

func (s *store) request(ctx context.Context, pid, id string) (requestItem, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT "+requestColumns+" FROM cs.requests WHERE project_id=$1 AND id=$2"), pid, id)
	if err != nil {
		return requestItem{}, err
	}
	item, err := pgx.CollectOneRow(rows, requestScan)
	if err != nil {
		return item, missing(err)
	}
	rows, err = s.pool.Query(ctx, s.q("SELECT platform,state,evidence FROM cs.request_work WHERE project_id=$1 AND request_id=$2 ORDER BY platform"), pid, id)
	if err != nil {
		return item, err
	}
	item.Work, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (workPart, error) {
		var p workPart
		err := row.Scan(&p.Platform, &p.State, &p.Evidence)
		return p, err
	})
	return item, err
}

func (s *store) insertRequest(ctx context.Context, tx pgx.Tx, uid, pid string, v requestInput) (addedRequest, error) {
	if !textOK(v.Title, 1, 160) || !textOK(v.Body, 1, 10000) || !allowed(v.Kind, requestKinds) || len(v.Platforms) > len(platforms) {
		return addedRequest{}, errInvalid
	}
	if v.NodeID != "" {
		var sectionID string
		if err := tx.QueryRow(ctx, s.q("SELECT section_id FROM cs.nodes WHERE project_id=$1 AND id=$2"), pid, v.NodeID).Scan(&sectionID); err != nil {
			return addedRequest{}, missing(err)
		}
		if v.SectionID != "" && v.SectionID != sectionID {
			return addedRequest{}, errInvalid
		}
		v.SectionID = sectionID
	}
	result := addedRequest{ID: newID(), Title: strings.TrimSpace(v.Title)}
	tag, err := tx.Exec(ctx, s.q(`INSERT INTO cs.requests(id,project_id,section_id,node_id,author_id,title,body,kind)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE $3::text IS NULL OR EXISTS(SELECT 1 FROM cs.sections WHERE project_id=$2 AND id=$3)`),
		result.ID, pid, nullString(v.SectionID), nullString(v.NodeID), uid, result.Title, v.Body, v.Kind)
	if err != nil {
		return addedRequest{}, err
	}
	if tag.RowsAffected() != 1 {
		return addedRequest{}, errMissing
	}
	if err := s.insertParts(ctx, tx, pid, result.ID, v.Platforms); err != nil {
		return addedRequest{}, err
	}
	return result, s.audit(ctx, tx, uid, pid, "request.add", result.ID)
}

func (s *store) insertParts(ctx context.Context, tx pgx.Tx, pid, id string, choices []string) error {
	if len(choices) > len(platforms) {
		return errInvalid
	}
	seen := map[string]bool{}
	for _, p := range choices {
		if !allowed(p, platforms) || seen[p] {
			return errInvalid
		}
		seen[p] = true
		if _, err := tx.Exec(ctx, s.q("INSERT INTO cs.request_work(project_id,request_id,platform,state) VALUES($1,$2,$3,'ready')"), pid, id, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) addRequests(ctx context.Context, uid, pid, operationID string, items []requestInput) ([]addedRequest, error) {
	if !textOK(operationID, 1, 120) || len(items) < 1 || len(items) > 20 {
		return nil, errInvalid
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	inputHash := sha256.Sum256(payload)
	var result []addedRequest
	err = inTx(ctx, s, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, s.q("INSERT INTO cs.request_batches(project_id,user_id,operation_id,input_hash,response) VALUES($1,$2,$3,$4,'[]') ON CONFLICT DO NOTHING"), pid, uid, operationID, inputHash[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var savedHash, saved []byte
			if err := tx.QueryRow(ctx, s.q("SELECT input_hash,response FROM cs.request_batches WHERE project_id=$1 AND user_id=$2 AND operation_id=$3"), pid, uid, operationID).Scan(&savedHash, &saved); err != nil {
				return err
			}
			if subtle.ConstantTimeCompare(savedHash, inputHash[:]) != 1 {
				return errConflict
			}
			return json.Unmarshal(saved, &result)
		}
		for _, v := range items {
			added, err := s.insertRequest(ctx, tx, uid, pid, v)
			if err != nil {
				return err
			}
			result = append(result, added)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, s.q("UPDATE cs.request_batches SET response=$4::jsonb WHERE project_id=$1 AND user_id=$2 AND operation_id=$3"), pid, uid, operationID, string(encoded))
		return err
	})
	return result, err
}

func (s *store) prepareRequest(ctx context.Context, uid, pid, id, criteria string, version int64, choices []string) error {
	if !textOK(criteria, 1, 6000) || len(choices) == 0 {
		return errInvalid
	}
	return inTx(ctx, s, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, s.q("UPDATE cs.requests SET state='ready',criteria=$4,version=version+1,updated_at=now() WHERE project_id=$1 AND id=$2 AND version=$3 AND state='inbox'"), pid, id, version, criteria)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errConflict
		}
		if _, err := tx.Exec(ctx, s.q("DELETE FROM cs.request_work WHERE project_id=$1 AND request_id=$2"), pid, id); err != nil {
			return err
		}
		if err := s.insertParts(ctx, tx, pid, id, choices); err != nil {
			return err
		}
		return s.audit(ctx, tx, uid, pid, "request.prepare", id)
	})
}

func (s *store) changePart(ctx context.Context, uid, pid, id, platform, action, evidence string) error {
	if !allowed(platform, platforms) {
		return errInvalid
	}
	return inTx(ctx, s, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, s.q("SELECT state FROM cs.requests WHERE project_id=$1 AND id=$2 FOR UPDATE"), pid, id).Scan(&current); err != nil {
			return missing(err)
		}
		if current != "ready" && current != "active" {
			return errConflict
		}
		var tagAffected int64
		switch action {
		case "claim":
			tag, err := tx.Exec(ctx, s.q("UPDATE cs.request_work SET state='active',assignee_id=$4 WHERE project_id=$1 AND request_id=$2 AND platform=$3 AND state='ready'"), pid, id, platform, uid)
			if err != nil {
				return err
			}
			tagAffected = tag.RowsAffected()
		case "finish":
			if !textOK(evidence, 1, 2000) {
				return errInvalid
			}
			tag, err := tx.Exec(ctx, s.q("UPDATE cs.request_work SET state='done',evidence=$5 WHERE project_id=$1 AND request_id=$2 AND platform=$3 AND assignee_id=$4 AND state='active'"), pid, id, platform, uid, evidence)
			if err != nil {
				return err
			}
			tagAffected = tag.RowsAffected()
		case "release":
			tag, err := tx.Exec(ctx, s.q("UPDATE cs.request_work SET state='ready',assignee_id=NULL WHERE project_id=$1 AND request_id=$2 AND platform=$3 AND state='active' AND (assignee_id=$4 OR EXISTS(SELECT 1 FROM cs.memberships WHERE project_id=$1 AND user_id=$4 AND role='owner'))"), pid, id, platform, uid)
			if err != nil {
				return err
			}
			tagAffected = tag.RowsAffected()
		default:
			return errInvalid
		}
		if tagAffected != 1 {
			return errConflict
		}
		if _, err := tx.Exec(ctx, s.q(`UPDATE cs.requests r SET state=(
			SELECT CASE WHEN bool_and(w.state='done') THEN 'done' WHEN bool_and(w.state='ready') THEN 'ready' ELSE 'active' END
			FROM cs.request_work w WHERE w.project_id=r.project_id AND w.request_id=r.id
		),version=version+1,updated_at=now() WHERE project_id=$1 AND id=$2`), pid, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, uid, pid, "request."+action+"."+platform, id)
	})
}

func (s *store) queueCounts(ctx context.Context, pid string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, s.q("SELECT state,count(*) FROM cs.requests WHERE project_id=$1 GROUP BY state"), pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		result[state] = count
	}
	return result, rows.Err()
}
