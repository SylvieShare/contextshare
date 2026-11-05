package main

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var localQueryParts = regexp.MustCompile(`(-?)(?:"([^"]+)"|([^\s"]+))`)
var localWords = regexp.MustCompile(`[\p{L}\p{N}]+`)

func localSearchExpression(query string, aliases ...map[string]string) string {
	if allowPartialSearch(query) {
		terms := map[string]bool{}
		for _, term := range localQueryTerms(query, aliases...) {
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
		terms := localTerms(part[2]+part[3], aliases...)
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
	words := localQueryTerms(query, data.aliases)
	for i := range result {
		result[i].Body = localExcerpt(result[i].Body, query, words, data.aliases)
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
		node         localNode
		exactTitle   bool
		exactBody    bool
		weight       int
		window       int
		topicDensity float64
		bm25         float64
	}
	hits := map[string]hit{}
	needles := map[string]bool{}
	for _, term := range localQueryTerms(query, data.aliases) {
		needles[term] = true
	}
	partial := allowPartialSearch(query)
	folded := strings.ToLower(query)
	expression := localSearchExpression(query, data.aliases)
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
			exactTitle := strings.Contains(strings.ToLower(n.Node.Title), folded) || len(needles) > 0 && titleHits == len(needles)
			exactBody := len(needles) > 1 && strings.Contains(strings.ToLower(n.Node.Body), folded)
			if !partial || exactTitle || exactBody || coverage >= min(2, len(needles)) {
				terms := strings.Fields(body)
				positions := make([]localTermPosition, 0)
				for i, term := range terms {
					if needles[term] {
						positions = append(positions, localTermPosition{term, i, i + 1})
					}
				}
				start, end := localTermWindow(positions)
				density := 0.0
				if len(needles) == 1 {
					density = float64(len(positions)) / math.Sqrt(float64(max(1, len(terms))))
				}
				hits[id] = hit{node: n.Node, exactTitle: exactTitle, exactBody: exactBody, weight: coverage + 2*titleHits,
					window: end - start, topicDensity: density, bm25: rank}
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
		if a.topicDensity != b.topicDensity {
			return a.topicDensity > b.topicDensity
		}
		if a.window != b.window {
			return a.window < b.window
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

func localExcerpt(body, query string, words []string, aliases ...map[string]string) string {
	if query == "" {
		return string([]rune(body)[:min(220, utf8.RuneCountInString(body))])
	}
	position := strings.Index(strings.ToLower(body), strings.ToLower(query))
	if position < 0 {
		wanted := map[string]bool{}
		for _, word := range words {
			wanted[word] = true
		}
		var positions []localTermPosition
		for _, span := range localWords.FindAllStringIndex(body, -1) {
			for _, term := range localTerms(body[span[0]:span[1]], aliases...) {
				if wanted[term] {
					positions = append(positions, localTermPosition{term, span[0], span[1]})
				}
			}
		}
		if len(positions) > 0 {
			position, _ = localTermWindow(positions)
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

func localQueryTerms(query string, aliases ...map[string]string) []string {
	terms := localTerms(query, aliases...)
	words := strings.Fields(strings.ToLower(query))
	if len(words) > 0 && allowed(words[0], []string{"как", "где", "почему", "чем", "что"}) {
		filler := map[string]bool{}
		for _, word := range []string{"означает", "показывается", "выглядит", "работает", "найти", "расположено", "называется", "устроено"} {
			for _, term := range localTerms(word, aliases...) {
				filler[term] = true
			}
		}
		filtered := make([]string, 0, len(terms))
		for _, term := range terms {
			if !filler[term] {
				filtered = append(filtered, term)
			}
		}
		if len(filtered) > 0 {
			return filtered
		}
	}
	return terms
}

type localTermPosition struct {
	term       string
	start, end int
}

// Select the shortest passage covering all distinct query terms present in
// this document. Repeated words do not outweigh a nearby matching phrase.
func localTermWindow(positions []localTermPosition) (int, int) {
	if len(positions) == 0 {
		return 0, 0
	}
	needed := map[string]bool{}
	for _, p := range positions {
		needed[p.term] = true
	}
	counts := map[string]int{}
	start, end := positions[0].start, positions[len(positions)-1].end
	left := 0
	for _, right := range positions {
		counts[right.term]++
		for len(counts) == len(needed) {
			first := positions[left]
			if right.end-first.start < end-start {
				start, end = first.start, right.end
			}
			counts[first.term]--
			if counts[first.term] == 0 {
				delete(counts, first.term)
			}
			left++
		}
	}
	return start, end
}
