package main

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Respect explicit phrase, OR and exclusion syntax. Ordinary questions may
// still match when some of their words are absent from the document.
func allowPartialSearch(query string) bool {
	if strings.ContainsRune(query, '"') {
		return false
	}
	for _, word := range strings.Fields(query) {
		if strings.EqualFold(word, "or") || (len(word) > 1 && word[0] == '-') {
			return false
		}
	}
	return true
}

const nodeSearchOrder = `title_literal DESC, exact_match DESC, matched_terms DESC, title_terms DESC,
	(status='current') DESC, (body_position > 0) DESC,
	CASE WHEN title_literal THEN char_length(title) ELSE 0 END, relevance DESC, title, id`

const nodeSearchSQL = `
	WITH parsed AS (
		SELECT lower($6::text) AS literal,
		       websearch_to_tsquery('russian', $6) AS exact,
		       tsvector_to_array(to_tsvector('russian', regexp_replace($6, '[^[:alnum:]_]+', ' ', 'g'))) AS terms
	), queries AS (
		SELECT parsed.*,
		       coalesce((SELECT string_agg(quote_literal(term), ' | ') FROM unnest(terms) AS words(term)), '')::tsquery AS partial
		FROM parsed
	), candidates AS (
		SELECT n.*, q.partial, q.exact,
		       strpos(lower(n.title), q.literal) > 0 AS title_literal,
		       strpos(lower(n.body), q.literal) AS body_position,
		       n.search_document @@ q.exact AS exact_match,
		       cardinality(q.terms) AS term_count,
		       (SELECT count(*) FROM unnest(q.terms) AS words(term)
		        WHERE term = ANY(tsvector_to_array(n.search_document))) AS matched_terms,
		       (SELECT count(*) FROM unnest(q.terms) AS words(term)
		        WHERE term = ANY(tsvector_to_array(to_tsvector('russian', n.title)))) AS title_terms
		FROM cs.nodes n CROSS JOIN queries q
		WHERE ` + nodeFiltersSQL + `
		AND (n.search_document @@ q.exact OR ($7::boolean AND
		     (n.search_document @@ q.partial OR strpos(lower(n.title), q.literal) > 0
		      OR strpos(lower(n.body), q.literal) > 0)))
	), ranked AS (
		SELECT *, CASE WHEN $7 THEN partial ELSE exact END AS headline_query,
		       ts_rank_cd(search_document, CASE WHEN $7 THEN partial ELSE exact END, 32) AS relevance
		FROM candidates
		WHERE exact_match OR ($7 AND (title_literal OR body_position > 0
			OR matched_terms >= least(2, greatest(1, term_count - 1))))
		ORDER BY ` + nodeSearchOrder + `
		LIMIT $8 OFFSET $9
	)
	SELECT id, section_id, coalesce(parent_id, ''), title, kind,
		CASE
			WHEN body_position > 0 THEN
				CASE WHEN body_position > 71 THEN '…' ELSE '' END ||
				substr(body, greatest(1, body_position - 70), 280) ||
				CASE WHEN char_length(body) > greatest(1, body_position - 70) + 279 THEN '…' ELSE '' END
			WHEN to_tsvector('russian', body) @@ headline_query THEN
				left(ts_headline('russian', body, headline_query,
					'MaxFragments=1, MinWords=10, MaxWords=32, StartSel=«, StopSel=»'), 360)
			ELSE left(body, 220)
		END,
		status, source_url, source_revision, coalesce(verified_at::text, ''), version
	FROM ranked
	ORDER BY ` + nodeSearchOrder

func (s *store) searchNodes(ctx context.Context, pid string, f nodeFilter, query string, limit, offset int) (page[node], error) {
	rows, err := s.pool.Query(ctx, s.q(nodeSearchSQL), pid, f.SectionID, f.Kind, f.Status, f.Platform,
		query, allowPartialSearch(query), limit+1, offset)
	if err != nil {
		return page[node]{}, err
	}
	items, err := pgx.CollectRows(rows, nodeScan)
	return paginate(items, limit, offset), err
}
