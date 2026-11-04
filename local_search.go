package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var localQueryParts = regexp.MustCompile(`(-?)(?:"([^"]+)"|([^\s"]+))`)
var localWords = regexp.MustCompile(`[\p{L}\p{N}]+`)

func localSearchExpression(query string) string {
	if allowPartialSearch(query) {
		terms := map[string]bool{}
		for _, term := range localTerms(query) {
			terms[term] = true
		}
		quoted := sortedLocalKeys(terms)
		for i := range quoted {
			quoted[i] = `"` + quoted[i] + `"`
		}
		return strings.Join(quoted, " OR ")
	}
	var alternatives, required, excluded []string
	finish := func() {
		if len(required) > 0 {
			group := strings.Join(required, " AND ")
			if len(excluded) > 0 {
				group = "(" + group + ") NOT (" + strings.Join(excluded, " OR ") + ")"
			}
			alternatives = append(alternatives, "("+group+")")
		}
		required, excluded = nil, nil
	}
	for _, part := range localQueryParts.FindAllStringSubmatch(query, -1) {
		if part[1] == "" && strings.EqualFold(part[3], "or") {
			finish()
			continue
		}
		terms := localTerms(part[2] + part[3])
		if len(terms) == 0 {
			continue
		}
		clause := `"` + strings.Join(terms, " ") + `"`
		if part[1] == "-" {
			excluded = append(excluded, clause)
		} else {
			required = append(required, clause)
		}
	}
	finish()
	return strings.Join(alternatives, " OR ")
}

func matchesLocalNode(n localNote, in nodeListArgs) bool {
	if in.Status != "all" && n.Node.Status != in.Status ||
		in.SectionID != "" && n.Node.SectionID != in.SectionID ||
		in.Kind != "" && n.Node.Kind != in.Kind {
		return false
	}
	if in.Platform != "" && len(n.Platforms) > 0 {
		for _, p := range n.Platforms {
			if p.Platform == in.Platform {
				return true
			}
		}
		return false
	}
	return true
}

func (w *localWorkspace) findNodes(ctx context.Context, data *localSnapshot, in nodeListArgs, search bool) (page[localNode], error) {
	if in.Status == "" {
		in.Status = "current"
		if search {
			in.Status = "all"
		}
	}
	if !textOK(in.Query, 0, 240) || in.Status != "all" && !allowed(in.Status, nodeStates) ||
		in.Kind != "" && !allowed(in.Kind, nodeKinds) || in.Platform != "" && !allowed(in.Platform, platforms) {
		return page[localNode]{}, errInvalid
	}
	query := strings.TrimSpace(in.Query)
	limit, offset := pageArgs(in.Limit, in.Offset)
	var result []localNode
	if query == "" {
		if search {
			return page[localNode]{}, errInvalid
		}
		for _, n := range data.notes {
			if matchesLocalNode(n, in) {
				result = append(result, n.Node)
			}
		}
		sort.Slice(result, func(i, j int) bool { return localNodeLess(result[i], result[j]) })
	} else {
		var err error
		result, err = w.rankLocalNodes(ctx, data, in, query)
		if err != nil {
			return page[localNode]{}, err
		}
	}
	offset = min(offset, len(result))
	result = result[offset:min(offset+limit+1, len(result))]
	words := localTerms(query)
	for i := range result {
		result[i].Body = localExcerpt(result[i].Body, query, words)
		result[i].CodePaths = nil
	}
	return paginate(result, limit, offset), nil
}

func localNodeLess(a, b localNode) bool {
	if (a.Status == "current") != (b.Status == "current") {
		return a.Status == "current"
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return a.ID < b.ID
}

func (w *localWorkspace) rankLocalNodes(ctx context.Context, data *localSnapshot, in nodeListArgs, query string) ([]localNode, error) {
	if err := w.index(ctx, data); err != nil {
		return nil, err
	}
	type hit struct {
		node       localNode
		exactTitle bool
		exactBody  bool
		weight     int
		bm25       float64
	}
	hits := map[string]hit{}
	needles := map[string]bool{}
	for _, term := range localTerms(query) {
		needles[term] = true
	}
	partial := allowPartialSearch(query)
	folded := strings.ToLower(query)
	expression := localSearchExpression(query)
	if expression != "" {
		rows, err := w.db.QueryContext(ctx, "SELECT id,title,body,bm25(node_search,0,5,1) FROM node_search WHERE node_search MATCH ?", expression)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, title, body string
			var rank float64
			if err := rows.Scan(&id, &title, &body, &rank); err != nil {
				return nil, err
			}
			n, ok := data.notes[id]
			if !ok || !matchesLocalNode(n, in) {
				continue
			}
			title, body = " "+title+" ", " "+body+" "
			coverage, titleHits := 0, 0
			for term := range needles {
				word := " " + term + " "
				if strings.Contains(title, word) {
					titleHits++
					coverage++
				} else if strings.Contains(body, word) {
					coverage++
				}
			}
			exactTitle := strings.Contains(strings.ToLower(n.Node.Title), folded)
			exactBody := strings.Contains(strings.ToLower(n.Node.Body), folded)
			if !partial || exactTitle || exactBody || coverage >= min(2, len(needles)) {
				hits[id] = hit{node: n.Node, exactTitle: exactTitle, exactBody: exactBody, weight: coverage + 2*titleHits, bm25: rank}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	// Keep literal searches for partial IDs and punctuation that tokenization
	// would otherwise lose. Explicit boolean and phrase syntax stays exact.
	if partial {
		for id, n := range data.notes {
			if _, found := hits[id]; found || !matchesLocalNode(n, in) {
				continue
			}
			title := strings.Contains(strings.ToLower(n.Node.Title), folded)
			body := strings.Contains(strings.ToLower(n.Node.Body), folded)
			if title || body {
				hits[id] = hit{node: n.Node, exactTitle: title, exactBody: body}
			}
		}
	}
	if expression == "" && !partial {
		return nil, fmt.Errorf("search needs at least one positive term")
	}
	ordered := make([]hit, 0, len(hits))
	for _, match := range hits {
		ordered = append(ordered, match)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.exactTitle != b.exactTitle {
			return a.exactTitle
		}
		if a.exactTitle && len(a.node.Title) != len(b.node.Title) {
			return len(a.node.Title) < len(b.node.Title)
		}
		if a.exactBody != b.exactBody {
			return a.exactBody
		}
		if a.weight != b.weight {
			return a.weight > b.weight
		}
		if (a.node.Status == "current") != (b.node.Status == "current") {
			return a.node.Status == "current"
		}
		if a.bm25 != b.bm25 {
			return a.bm25 < b.bm25
		}
		return localNodeLess(a.node, b.node)
	})
	result := make([]localNode, 0, len(ordered))
	for _, hit := range ordered {
		result = append(result, hit.node)
	}
	return result, nil
}

func localExcerpt(body, query string, words []string) string {
	if query == "" {
		return string([]rune(body)[:min(220, utf8.RuneCountInString(body))])
	}
	position := strings.Index(strings.ToLower(body), strings.ToLower(query))
	if position < 0 {
		wanted := map[string]bool{}
		for _, word := range words {
			wanted[word] = true
		}
		best := 0
		for _, span := range localWords.FindAllStringIndex(body, -1) {
			for _, term := range localTerms(body[span[0]:span[1]]) {
				if wanted[term] && len(term) > best {
					position, best = span[0], len(term)
				}
			}
		}
	}
	runes := []rune(body)
	start := 0
	if position >= 0 && position <= len(body) {
		start = max(0, utf8.RuneCountInString(body[:position])-75)
	}
	end := min(start+300, len(runes))
	preview := string(runes[start:end])
	if start != 0 {
		preview = "…" + preview
	}
	if end != len(runes) {
		preview += "…"
	}
	return preview
}
