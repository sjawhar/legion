package api

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
// or a newly created ask on an issue or on an unlinked project document. Exactly one of issueKey
// and artifactID is set.
type suggestionSource struct {
	kind       string // "issue" or "ask"
	issueKey   string
	artifactID string // an ask on an unlinked project document
	askID      string // empty for an issue source
	actor      model.Actor
}

// owns reports whether result is the source itself or anything its owner holds: the new issue, or
// the issue or project document the new ask sits on, and everything inside that owner (its spec,
// its other asks, comments and messages). Each is the best match for the source's own words, so
// suggesting one says nothing, and acting on it (a reply on the ask's own issue) must never count
// as acting on a suggestion.
func (source suggestionSource) owns(result model.SearchResult) bool {
	if source.askID != "" && result.Kind == "ask" && result.ID == source.askID {
		return true
	}
	if source.issueKey != "" {
		return (result.Kind == "issue" && result.ID == source.issueKey) ||
			(result.Owner.Kind == "issue" && result.Owner.Key == source.issueKey)
	}
	return (result.Kind == "document" && result.ID == source.artifactID) ||
		(result.Owner.Kind == "document" && result.Owner.ArtifactID == source.artifactID)
}

// closedOwner reports whether result belongs to an issue that is done. Keyword search ranks an
// issue already closed as a duplicate above the open issue it was closed into whenever its words
// are closer to the new filing's, which they often are (a duplicate restates the bug the way the
// next duplicate will); the open issue is the one an agent can act on.
func closedOwner(result model.SearchResult) bool {
	return result.Owner.Kind == "issue" && result.Owner.Status == "done"
}

// computeAndPersistSuggestions runs LEGION-550's write-time feedback — resolving project when
// the caller does not already know it, the fused search (sjawhar/legion#1764), the decision
// lookup, and persisting every row the sweep later reads — all under one writeSuggestionTimeout
// deadline derived once here, so nothing downstream of the write's own commit can hold the
// response past that single bound. Two earlier rounds each bounded one more step and left
// another on the request's unbounded context: round 2 bounded the search and missed the decision
// lookup (measured blocking ~1.2s behind a table lock); this round folds the ask route's
// project-key lookup in too (measured blocking ~2s behind a lock on `issues`), since it ran
// between the write's commit and this call on the same unbounded context. project is empty for
// an ask, whose owning issue or project document `source` already names; an issue creation
// already knows its project and passes it directly, needing no lookup. It never returns an
// error: a slow or failed search, or a slow or failed project lookup, is reported as
// Suggestions.Missing instead, since nothing here may hold or fail a write that already
// committed. A blank text (an issue filed with no title, which the create route already refuses,
// or an ask with a blank question) returns nil rather than searching for nothing.
func (s *server) computeAndPersistSuggestions(
	ctx context.Context, project, text string, source suggestionSource,
) *model.Suggestions {
	searchText := suggestionQuery(text)
	if searchText == "" {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, writeSuggestionTimeout)
	defer cancel()
	if project == "" {
		resolved, err := s.resolveSuggestionProject(bounded, source)
		if err != nil {
			reason := "the project behind it could not be resolved"
			if bounded.Err() != nil {
				reason = "the project lookup behind it did not answer within " + writeSuggestionTimeout.String()
			}
			slog.Warn("dispatch: write suggestions omitted", "error", err)
			return &model.Suggestions{Related: []model.WriteSuggestion{}, Missing: reason}
		}
		project = resolved
	}
	suggestions := s.computeSuggestions(bounded, project, searchText, source)
	s.persistSuggestions(bounded, source, suggestions)
	return suggestions
}

// resolveSuggestionProject reads the project a source's own owner belongs to, on ctx: the same
// bounded deadline computeAndPersistSuggestions derives for everything else, so this lookup can
// no longer block a write past that bound the way it did when it ran before computing suggestions
// at all, on the request's own unbounded context.
func (s *server) resolveSuggestionProject(ctx context.Context, source suggestionSource) (string, error) {
	var project string
	var err error
	switch {
	case source.issueKey != "":
		err = s.deps.Store.Pool.QueryRow(ctx, `select project_key from issues where key = $1`, source.issueKey).Scan(&project)
	case source.artifactID != "":
		err = s.deps.Store.Pool.QueryRow(ctx, `select coalesce(project_key, '') from artifacts where id = $1`, source.artifactID).Scan(&project)
	}
	return project, err
}

// computeSuggestions runs the fused search and the decision lookup under ctx, which the caller
// has already bounded. Everything source owns is left out (suggestionSource.owns). Related takes
// the best suggestionRelatedCount of the rest with every hit on an open owner ahead of every hit
// on a done issue, keeping the search's order within each; Decision is the first answered ask in
// the search's own order, deliberately not reordered by owner status — an answered question
// stays the best match for the question asked whether or not its issue has since closed, so
// exempting it from the open-before-done rule (unlike reviewed Round 2's claim that Decision was
// simply "unaffected" by construction, which was true only because no test had two decision
// candidates split across open and done owners).
func (s *server) computeSuggestions(
	ctx context.Context, project, searchText string, source suggestionSource,
) *model.Suggestions {
	response, err := s.runFusedSearch(ctx, searchText, project, suggestionSearchDepth, 0)
	if err != nil {
		reason := "the search behind it failed"
		if ctx.Err() != nil {
			reason = "the search behind it did not answer within " + writeSuggestionTimeout.String()
		}
		slog.Warn("dispatch: write suggestions omitted", "error", err)
		return &model.Suggestions{Related: []model.WriteSuggestion{}, Missing: reason}
	}

	var candidates []model.SearchResult
	var askIDs []string
	for _, result := range response.Results {
		if source.owns(result) {
			continue
		}
		candidates = append(candidates, result)
		if result.Kind == "ask" {
			askIDs = append(askIDs, result.ID)
		}
	}

	related := append([]model.SearchResult(nil), candidates...)
	slices.SortStableFunc(related, func(a, b model.SearchResult) int {
		return cmp.Compare(boolRank(closedOwner(a)), boolRank(closedOwner(b)))
	})
	suggestions := &model.Suggestions{Related: []model.WriteSuggestion{}}
	for _, result := range related[:min(len(related), suggestionRelatedCount)] {
		suggestions.Related = append(suggestions.Related, suggestionFromResult(result))
	}
	if len(askIDs) > 0 {
		// candidates, not related: the decision is the best-ranked answered ask in the search's
		// own order, never reordered by whether its issue is open or done.
		decision, err := s.firstAnsweredAsk(ctx, askIDs, candidates)
		if err != nil {
			slog.Warn("dispatch: decision suggestion omitted", "error", err)
		} else {
			suggestions.Decision = decision
		}
	}
	return suggestions
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
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
// something to resolve and a later count has something to count. Runs on ctx, the same bounded
// deadline computeAndPersistSuggestions derived for the search and decision lookup: a write that
// already succeeded is never failed by this, so a row that cannot be inserted — deadline
// exceeded included — is only logged. One round trip (store.Pool.SendBatch), not one per row:
// up to four rows (three related plus one decision) previously cost up to four.
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
	if len(rows) == 0 {
		return
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
			insert into write_suggestions
				(source_kind, source_issue_key, source_artifact_id, source_ask_id, actor_kind, actor_id,
				 role, rank, suggested_kind, suggested_id, suggested_issue_key)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`,
			source.kind, nullable(source.issueKey), nullable(source.artifactID), nullable(source.askID),
			source.actor.Kind, source.actor.ID, r.role, r.rank, r.item.Kind, r.item.ID, suggestedIssueKey(r.item),
		)
	}
	results := s.deps.Store.Pool.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			slog.Warn("dispatch: write suggestion not recorded", "error", err)
			break
		}
	}
	_ = results.Close()
}
