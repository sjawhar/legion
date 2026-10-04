package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// writeSuggestionTimeout bounds how long a write waits on the fused search LEGION-550 runs
// after a new issue or ask commits, so a slow or down search never holds up a write that has
// already succeeded. search.go's own corpus latency gate (sjawhar/legion#1764) keeps an
// ordinary query's p95 near 150ms; twice that still treats a slow search as exceptional rather
// than ordinary, while bounding the worst case any single write waits.
const writeSuggestionTimeout = 300 * time.Millisecond

// suggestionRelatedCount is "the three items most like what was just filed" (LEGION-550's spec).
const suggestionRelatedCount = 3

// suggestionSearchDepth widens the fused search past suggestionRelatedCount so an answered ask
// a few ranks behind the top issues can still surface as Suggestions.Decision: the tuning
// favours catching a duplicate or a settled decision over precision.
const suggestionSearchDepth = 10

// suggestionQueryWordLimit bounds how many of a title or spec's distinct words feed
// suggestionQuery, so a long spec still produces one bounded query rather than an
// unboundedly large one.
const suggestionQueryWordLimit = 40

// suggestionQuery turns free-form text (a title, a spec, a question) into a query that favours
// recall: websearch_to_tsquery (search.go's runFusedSearch) ANDs plain unquoted words together,
// which a whole title-plus-spec would almost never satisfy against another issue's own words.
// Joining its distinct significant words with the literal operator "OR" instead asks
// websearch_to_tsquery for anything sharing even one of them, and ts_rank_cd still ranks a
// candidate sharing more of them higher — "the tuning favours catching a duplicate over
// precision" (LEGION-550's spec). The literal words "or" and "and" are dropped since re-joining
// them with the OR operator would read as a stray, empty disjunct.
func suggestionQuery(text string) string {
	seen := map[string]bool{}
	terms := make([]string, 0, suggestionQueryWordLimit)
	for _, word := range strings.Fields(text) {
		word = strings.ToLower(strings.Trim(word, ".,!?;:\"'()[]{}*_`"))
		if word == "" || word == "or" || word == "and" || seen[word] {
			continue
		}
		seen[word] = true
		terms = append(terms, word)
		if len(terms) >= suggestionQueryWordLimit {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// suggestionSource identifies what a set of suggestions was offered for: a newly created issue,
// or a newly created ask inside one (an ask on an unlinked project document has no issue to
// resolve activity against and is out of scope for now — computeSuggestionsForIssue is never
// called for one).
type suggestionSource struct {
	kind     string // "issue" or "ask"
	issueKey string
	askID    string // empty for an issue source
	actor    model.Actor
}

// computeSuggestions runs LEGION-550's write-time feedback over project: the fused search
// (sjawhar/legion#1764) for text, read within writeSuggestionTimeout. It never returns an error:
// a slow or failed search is reported as Suggestions.Missing instead, since nothing here may
// hold or fail a write that already committed. A blank text (an issue filed with no title, which
// the create route already refuses, or an ask with a blank question) returns nil rather than
// searching for nothing.
//
// excludeKind/excludeID keep the just-written row itself out of its own suggestions, since the
// transaction that created it already made it the newest, best-scoring match for its own words.
// excludeIssueKey, set only for an issue creation, additionally excludes every other item the
// new issue owns (its own seeded spec foremost): an issue's primary document is written and
// indexed in the same transaction as the issue, so without this a new issue's own spec is
// "related" to the issue it is the spec of. An ask's own question is never also stored as a
// document, so ask creation passes "" here.
func (s *server) computeSuggestions(
	ctx context.Context, project, text, excludeKind, excludeID, excludeIssueKey string,
) *model.Suggestions {
	searchText := suggestionQuery(text)
	if searchText == "" {
		return nil
	}
	searchCtx, cancel := context.WithTimeout(ctx, writeSuggestionTimeout)
	defer cancel()
	response, err := s.runFusedSearch(searchCtx, searchText, project, suggestionSearchDepth, 0)
	if err != nil {
		reason := "the search behind it failed"
		if searchCtx.Err() != nil {
			reason = "the search behind it did not answer within " + writeSuggestionTimeout.String()
		}
		slog.Warn("dispatch: write suggestions omitted", "error", err)
		return &model.Suggestions{Missing: reason}
	}

	suggestions := &model.Suggestions{Related: []model.WriteSuggestion{}}
	var askIDs []string
	for _, result := range response.Results {
		if result.Kind == excludeKind && result.ID == excludeID {
			continue
		}
		if excludeIssueKey != "" &&
			((result.Kind == "issue" && result.ID == excludeIssueKey) ||
				(result.Owner.Kind == "issue" && result.Owner.Key == excludeIssueKey)) {
			continue
		}
		if len(suggestions.Related) < suggestionRelatedCount {
			suggestions.Related = append(suggestions.Related, suggestionFromResult(result))
		}
		if result.Kind == "ask" {
			askIDs = append(askIDs, result.ID)
		}
	}
	if len(askIDs) > 0 {
		decision, err := s.firstAnsweredAsk(ctx, askIDs, response.Results)
		if err != nil {
			slog.Warn("dispatch: decision suggestion omitted", "error", err)
		} else {
			suggestions.Decision = decision
		}
	}
	return suggestions
}

func suggestionFromResult(result model.SearchResult) model.WriteSuggestion {
	return model.WriteSuggestion{
		Kind:     result.Kind,
		Owner:    result.Owner,
		Artifact: result.Artifact,
		ID:       result.ID,
		Snippet:  result.Snippet,
		Href:     result.Href,
	}
}

// firstAnsweredAsk returns the best-ranked askIDs entry that already has a human answer,
// enriched with who answered and when — "any past decision that matches" (LEGION-550's spec) —
// or nil if none of them does.
func (s *server) firstAnsweredAsk(
	ctx context.Context, askIDs []string, results []model.SearchResult,
) (*model.WriteSuggestion, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select id::text, answer->>'user', (answer->>'at')::timestamptz
		  from asks where id = any($1::uuid[]) and answer is not null
	`, askIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type answered struct {
		by string
		at time.Time
	}
	byID := map[string]answered{}
	for rows.Next() {
		var id, by string
		var at time.Time
		if err := rows.Scan(&id, &by, &at); err != nil {
			return nil, err
		}
		byID[id] = answered{by: by, at: at}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, result := range results {
		if result.Kind != "ask" {
			continue
		}
		found, ok := byID[result.ID]
		if !ok {
			continue
		}
		item := suggestionFromResult(result)
		at := found.at
		item.AnsweredBy = found.by
		item.AnsweredAt = &at
		return &item, nil
	}
	return nil, nil
}

// suggestedIssueKey is the issue a suggested item's activity would show up on: the item's own
// key for an issue, its owner's key for anything issue-owned, nil for a project-level document,
// ask, comment or message that the outcome sweep's direct-activity signal cannot evaluate.
func suggestedIssueKey(item model.WriteSuggestion) *string {
	if item.Kind == "issue" {
		return &item.ID
	}
	if item.Owner.Kind == "issue" && item.Owner.Key != "" {
		key := item.Owner.Key
		return &key
	}
	return nil
}

// persistSuggestions records every item a set of suggestions offered, so the outcome sweep has
// something to resolve and a later count has something to count. A write that already succeeded
// is never failed by this: a row that cannot be inserted is only logged.
func (s *server) persistSuggestions(ctx context.Context, source suggestionSource, suggestions *model.Suggestions) {
	if suggestions == nil {
		return
	}
	type row struct {
		role string
		rank int
		item model.WriteSuggestion
	}
	rows := make([]row, 0, len(suggestions.Related)+1)
	for i, item := range suggestions.Related {
		rows = append(rows, row{role: "related", rank: i, item: item})
	}
	if suggestions.Decision != nil {
		rows = append(rows, row{role: "decision", rank: 0, item: *suggestions.Decision})
	}
	for _, r := range rows {
		var askID any
		if source.askID != "" {
			askID = source.askID
		}
		if _, err := s.deps.Store.Pool.Exec(ctx, `
			insert into write_suggestions
				(source_kind, source_issue_key, source_ask_id, actor_kind, actor_id, role, rank,
				 suggested_kind, suggested_id, suggested_issue_key)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			source.kind, source.issueKey, askID, source.actor.Kind, source.actor.ID, r.role, r.rank,
			r.item.Kind, r.item.ID, suggestedIssueKey(r.item),
		); err != nil {
			slog.Warn("dispatch: write suggestion not recorded", "error", err)
		}
	}
}
