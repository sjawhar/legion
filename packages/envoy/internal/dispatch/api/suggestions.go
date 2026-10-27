package api

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// relatedCheckAllowance is the most a write waits on its related-item check (INFRA-568): the
	// meaning search's relatedMeaningAllowance and the time the keyword search may still need
	// after it. At its end only the keyword search can still be running, and the check answers
	// that it was not made rather than hold the write longer.
	relatedCheckAllowance = 5 * time.Second
	// relatedMeaningAllowance is the time from the check's start that the meaning search, the
	// text's embedding and then the meaning query, has to answer. Past it the check answers its
	// keyword search's result and says it searched by keywords only. searchEmbedTimeout bounds
	// the embedding alone at the same three seconds, so a slow model service costs a check no
	// more than it costs a search.
	relatedMeaningAllowance = 3 * time.Second
	// relatedRecordTimeout bounds the check's records (write_suggestions and related_checks). It
	// runs on a context of its own, not the check's, so a check that ran out of time is still
	// recorded, and a record that cannot be written in a second is only logged.
	relatedRecordTimeout = time.Second
	// suggestionRelatedCount is "the three items most like what was just filed" (LEGION-550's spec).
	suggestionRelatedCount = 3
	// suggestionSearchDepth widens the search past suggestionRelatedCount so an answered ask a few
	// ranks behind the top issues can still surface as Suggestions.Decision, and so three distinct
	// owners remain once an issue and its spec collapse into one item.
	suggestionSearchDepth = 10
	// relatedSearchQueryTerms is how many of the chosen terms Suggestions.SearchQuery holds: enough
	// for a reader's own dispatch search to find what the check found, few enough that search's
	// AND of them still matches something.
	relatedSearchQueryTerms = 3
	// decisionBlockCheckLimit is how many of one edit's new decision blocks are checked; each past
	// it answers that its check was not made (decisionBlockChecks).
	decisionBlockCheckLimit = 5
)

// relatedEFSearch sets hnsw.ef_search for the meaning query's transaction: how many nearest
// vectors the HNSW index hands back before the project, kind and model filters run. pgvector's
// default, 40, can be spent entirely on other projects' vectors, leaving out the project's own
// nearest items; 400 keeps them at the cost of a wider index walk, paid once per check.
const relatedEFSearch = `set local hnsw.ef_search = 400`

var (
	// relatedKeywordQuery is the check's stage 1: search's keyword query (searchQueryBody) over the
	// chosen lexemes (relatedQOpener), so it ranks exactly as search does.
	relatedKeywordQuery = relatedQOpener + searchQueryBody
	// relatedMeaningQuery is the check's stage 2: search's meaning query, which fuses the same
	// keyword arms with the meaning arms, over the same lexemes and the text's embedding.
	relatedMeaningQuery = relatedQOpener + searchQueryMeaningBody
)

// The check's outcomes and search modes, as Suggestions and related_checks hold them.
const (
	relatedFound   = "found"
	relatedNone    = "none"
	relatedNotMade = "not_made"

	relatedMeaningAndKeywords = "meaning_and_keywords"
	relatedKeywordsOnly       = "keywords_only"
)

// The reasons a check was not made, or searched by keywords alone: related_checks.reason's values
// (0121_related_checks).
const (
	relatedReasonBlankText         = "blank_text"
	relatedReasonTimeout           = "timeout"
	relatedReasonSearchFailed      = "search_failed"
	relatedReasonAlreadyExists     = "already_exists"
	relatedReasonProjectUnresolved = "project_unresolved"
	relatedReasonNoEmbedder        = "no_embedder"
	relatedReasonMeaningTimeout    = "meaning_timeout"
	relatedReasonEmbedFailed       = "embed_failed"
	relatedReasonMeaningFailed     = "meaning_failed"
	relatedReasonKeywordsRequested = "keywords_only_requested"
	relatedReasonTooManyBlocks     = "too_many_blocks"
)

// What a reader is told for the reasons more than one step of the check can reach.
const (
	relatedBlankTextMissing    = "the text holds no word to search for"
	relatedSearchFailedMissing = "the search behind it failed"
)

// relatedAllowance is the deadline the whole check runs under: relatedCheckAllowance on every
// server the deployment builds, since register sets no suggestionTimeout, and whatever a test set
// otherwise.
func (s *server) relatedAllowance() time.Duration {
	return cmp.Or(s.suggestionTimeout, relatedCheckAllowance)
}

// relatedCheckNotRun answers, and records unless input.record is false, a check its route decided
// not to make (reason and text say why), so that write's result still says no check was made.
func (s *server) relatedCheckNotRun(ctx context.Context, input relatedInput, reason, text string) *model.Suggestions {
	suggestions := &model.Suggestions{Outcome: relatedNotMade, Related: []model.WriteSuggestion{}, Missing: text}
	if input.record {
		s.recordRelatedCheck(ctx, input, suggestions, relatedRecord{
			project: input.project, outcome: relatedNotMade, reason: reason, terms: []string{},
		})
	}
	return suggestions
}

// withSuggestions wraps a write's payload with its advice carrying the check's result, or, when
// writeAdvice answered nil (a project document has no issue state, or the write-advice read
// failed), with suggestionsOnlyAdvice: every issue and ask write answers its check.
func withSuggestions(payload any, advice *writeAdvice, suggestions *model.Suggestions) any {
	if advice != nil {
		advice.Suggestions = suggestions
		return withAdvice(payload, advice)
	}
	return withRawAdvice(payload, suggestionsOnlyAdvice{Suggestions: suggestions})
}

// decisionBlockChecks runs the related-item check for each decision block a document edit added,
// once the edit has committed, so a block whose question restates a decision already made or work
// already filed is answered with it while the writer is still at the document. The first
// decisionBlockCheckLimit blocks are checked at once, all under one relatedAllowance, so an edit
// waits on its checks no longer than an issue create waits on its one; each block past them
// answers, and is recorded, as not made. A block's source is the document's issue, or the project
// document itself when it has no issue, left out of its results as an ask's owner is. Nil when the
// edit added no block.
func (s *server) decisionBlockChecks(
	ctx context.Context, artifact model.Artifact, blocks []docs.AddedAskBlock, actor model.Actor,
) []decisionBlockCheck {
	if len(blocks) == 0 {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, s.relatedAllowance())
	defer cancel()
	checks := make([]decisionBlockCheck, len(blocks))
	var running sync.WaitGroup
	for index, block := range blocks {
		source := suggestionSource{kind: "decision_block", blockID: block.ID, actor: actor}
		if artifact.IssueKey != nil {
			source.issueKey = *artifact.IssueKey
		} else {
			source.artifactID = artifact.ID
		}
		input := relatedInput{project: artifact.Project, text: block.Question, source: source, record: true}
		checks[index] = decisionBlockCheck{BlockID: block.ID, Question: block.Question}
		if index >= decisionBlockCheckLimit {
			checks[index].Suggestions = s.relatedCheckNotRun(
				ctx, input, relatedReasonTooManyBlocks, "more than five new decision blocks in one edit",
			)
			continue
		}
		running.Go(func() { checks[index].Suggestions = s.relatedCheck(bounded, input) })
	}
	running.Wait()
	return checks
}

// suggestionSource identifies what a check was run for: a newly created issue, a newly created
// ask on an issue or on an unlinked project document, or a decision block a document edit added.
// At most one of issueKey and artifactID is set: the issue, or the project document the ask or
// block sits on. A check run for no source (the no-write route) sets neither.
type suggestionSource struct {
	kind       string // "issue", "ask" or "decision_block"
	issueKey   string
	artifactID string // an ask or decision block on an unlinked project document
	askID      string // set for an ask source alone
	blockID    string // set for a decision block source alone
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
		return ownedByIssue(result, source.issueKey)
	}
	if source.artifactID != "" {
		return (result.Kind == "document" && result.ID == source.artifactID) ||
			(result.Owner.Kind == "document" && result.Owner.ArtifactID == source.artifactID)
	}
	return false
}

// ownedByIssue reports whether result is the issue key or anything that issue owns.
func ownedByIssue(result model.SearchResult, key string) bool {
	return (result.Kind == "issue" && result.ID == key) || (result.Owner.Kind == "issue" && result.Owner.Key == key)
}

// relatedInput is one related-item check's question.
type relatedInput struct {
	// project is the project to search; empty resolves it from source's owner.
	project string
	// text is what is checked: a new issue's title and spec, an ask's or decision block's question,
	// or the no-write route's title and text.
	text   string
	source suggestionSource
	// excludeIssue leaves that issue and everything it owns out of the results and out of the
	// terms' document frequencies, as an issue source's own issue is (the no-write route's
	// exclude_issue).
	excludeIssue string
	// keywordsOnly skips the meaning search.
	keywordsOnly bool
	// record writes the check's records: write_suggestions rows for an issue or ask source and one
	// related_checks row. The no-write route leaves it false.
	record bool
}

// excludes reports whether result is left out of the check's items: the source's own, or the
// excluded issue's.
func (input relatedInput) excludes(result model.SearchResult) bool {
	return input.source.owns(result) || (input.excludeIssue != "" && ownedByIssue(result, input.excludeIssue))
}

// relatedRecord is what the related_checks row of one check holds besides its source.
type relatedRecord struct {
	project   string
	outcome   string
	search    string
	reason    string
	terms     []string
	keywordMS *int
	meaningMS *int
	totalMS   int
	related   int
}

// relatedStage is one search stage's answer and how long it took from the stages' start (stage 1
// starts with the term selection, stage 2 with the embedding, both at once).
type relatedStage struct {
	response model.SearchResponse
	err      error
	reason   string // stage 2's reason for keywords only when it did not answer
	took     time.Duration
}

// relatedPick is stage 1's term selection as the check and stage 2 both read it: ready is closed
// once terms and err are set, and neither is written after.
type relatedPick struct {
	ready chan struct{}
	terms relatedTerms
	err   error
}

// relatedCheck is the related-item check every issue create, ask and new decision block runs
// (INFRA-568), and the no-write related route. It never fails and never returns nil: what it did
// not do, it says in the result. It waits at most relatedAllowance.
//
// It resolves the project when the input does not name it, then starts two things at once: stage
// 1, which picks the text's rarest words (pickRelatedTerms) and runs search's keyword query over
// them, and, when an embedder is configured and keywords alone were not asked for, the text's
// embedding followed by stage 2, search's meaning query, which fuses the same keyword arms with
// the nearest vectors. Stage 2's answer within relatedMeaningAllowance is the result, searched by
// meaning and keywords; otherwise stage 1's is, searched by keywords only, with the reason. If
// stage 1 has not answered when the allowance ends, the check was not made.
//
// The source's own items are left out, each owner keeps its best-ranked item, open owners come
// before done issues, and the best three are Related; the decision is the first answered ask in
// the search's own order. Unless record is false it then records what it offered and how it went
// (recordRelatedCheck).
func (s *server) relatedCheck(ctx context.Context, input relatedInput) *model.Suggestions {
	started := time.Now()
	suggestions, record := s.runRelatedCheck(ctx, input, started)
	record.totalMS = int(time.Since(started).Milliseconds())
	if input.record {
		s.recordRelatedCheck(ctx, input, suggestions, record)
	}
	return suggestions
}

// runRelatedCheck is relatedCheck up to its records: the result and the related_checks row's
// fields, all but the total time.
func (s *server) runRelatedCheck(
	ctx context.Context, input relatedInput, started time.Time,
) (*model.Suggestions, relatedRecord) {
	allowance := s.relatedAllowance()
	bounded, cancel := context.WithTimeout(ctx, allowance)
	defer cancel()
	record := relatedRecord{project: input.project, terms: []string{}}
	suggestions := &model.Suggestions{Related: []model.WriteSuggestion{}}
	notMade := func(reason, text string) (*model.Suggestions, relatedRecord) {
		suggestions.Outcome, suggestions.Missing = relatedNotMade, text
		record.outcome, record.reason = relatedNotMade, reason
		return suggestions, record
	}
	timedOut := "the search behind it did not answer within " + allowance.String()

	if record.project == "" {
		project, err := s.resolveSuggestionProject(bounded, input.source)
		switch {
		case err != nil && bounded.Err() != nil:
			slog.Warn("dispatch: related-item check not made", "error", err)
			return notMade(relatedReasonTimeout, "the project lookup behind it did not answer within "+allowance.String())
		case err != nil:
			slog.Warn("dispatch: related-item check not made", "error", err)
			return notMade(relatedReasonProjectUnresolved, "the project behind it could not be resolved")
		case project == "":
			// A document no project holds (a conversation's file): the terms' statistics and the
			// search are both per project, and searching every project would weigh words against
			// none of them.
			return notMade(relatedReasonProjectUnresolved, "the item it sits on belongs to no project")
		}
		record.project = project
	}
	project := record.project
	// A blank text has nothing to pick terms from; the same answer for stopwords alone comes from
	// the picker, which tokenizes as stored vectors are built.
	if strings.TrimSpace(input.text) == "" {
		return notMade(relatedReasonBlankText, relatedBlankTextMissing)
	}

	keywordCtx, err := store.ForConcurrentRead(bounded)
	if err != nil {
		slog.Warn("dispatch: related-item check not made", "error", err)
		return notMade(relatedReasonSearchFailed, relatedSearchFailedMissing)
	}
	exclusion := relatedExclusion{issueKey: cmp.Or(input.source.issueKey, input.excludeIssue), artifactID: input.source.artifactID}
	stagesStarted := time.Now()
	pick := &relatedPick{ready: make(chan struct{})}
	keyword := make(chan relatedStage, 1)
	go func() {
		pick.terms, pick.err = s.pickRelatedTerms(keywordCtx, s.deps.Store.Pool, project, input.text, exclusion)
		close(pick.ready)
		stage := relatedStage{err: pick.err}
		switch {
		case pick.err != nil || pick.terms.NoLexemes:
		case len(pick.terms.Terms) == 0:
			// Every lexeme was dropped (too common, noise, or held by the source alone): no
			// keyword matches anything, so there is no query to run.
			stage.response = model.SearchResponse{Results: []model.SearchResult{}, Limit: suggestionSearchDepth}
		default:
			stage.response, stage.err = s.relatedSearch(keywordCtx, s.deps.Store.Pool, relatedKeywordQuery, project, pick.terms)
		}
		stage.took = time.Since(stagesStarted)
		keyword <- stage
	}()

	// Stage 2 runs on a context of its own ending at relatedMeaningAllowance from the check's
	// start, so the embedding and the meaning query both stop there and leave the rest of the
	// allowance to stage 1.
	meaningAllowance := min(relatedMeaningAllowance, allowance)
	var meaning chan relatedStage
	var meaningDone <-chan struct{}
	reason := ""
	switch {
	case input.keywordsOnly:
		reason = relatedReasonKeywordsRequested
	case s.deps.Embedder == nil:
		reason = relatedReasonNoEmbedder
	default:
		meaningCtx, cancelMeaning := context.WithDeadline(bounded, started.Add(meaningAllowance))
		defer cancelMeaning()
		meaningRead, err := store.ForConcurrentRead(meaningCtx)
		if err != nil {
			reason = relatedReasonMeaningFailed
			break
		}
		meaningDone = meaningCtx.Done()
		meaning = make(chan relatedStage, 1)
		go func() {
			stage := s.relatedMeaningStage(meaningRead, project, input.text, pick)
			stage.took = time.Since(stagesStarted)
			meaning <- stage
		}()
	}

	select {
	case <-pick.ready:
	case <-bounded.Done():
		return notMade(relatedReasonTimeout, timedOut)
	}
	if pick.err != nil {
		slog.Warn("dispatch: related-item check not made", "error", pick.err)
		if bounded.Err() != nil {
			return notMade(relatedReasonTimeout, timedOut)
		}
		return notMade(relatedReasonSearchFailed, relatedSearchFailedMissing)
	}
	if pick.terms.NoLexemes {
		return notMade(relatedReasonBlankText, relatedBlankTextMissing)
	}
	for _, term := range pick.terms.Terms {
		record.terms = append(record.terms, term.Lexeme)
	}
	suggestions.SearchQuery = relatedSearchQuery(pick.terms.Terms)

	var answer relatedStage
	answered := false
	if meaning != nil {
		select {
		case stage := <-meaning:
			record.meaningMS = milliseconds(stage.took)
			if stage.reason == "" {
				answer, answered = stage, true
			} else {
				reason = stage.reason
			}
		case <-meaningDone:
			record.meaningMS = milliseconds(time.Since(stagesStarted))
			reason = relatedReasonMeaningTimeout
		}
	}
	if answered {
		suggestions.Search = relatedMeaningAndKeywords
		// Stage 1's time is recorded when it has answered by now; it is not waited for.
		select {
		case stage := <-keyword:
			record.keywordMS = milliseconds(stage.took)
		default:
		}
	} else {
		select {
		case stage := <-keyword:
			record.keywordMS = milliseconds(stage.took)
			if stage.err != nil {
				slog.Warn("dispatch: related-item check not made", "error", stage.err)
				if bounded.Err() != nil {
					return notMade(relatedReasonTimeout, timedOut)
				}
				return notMade(relatedReasonSearchFailed, relatedSearchFailedMissing)
			}
			answer = stage
		case <-bounded.Done():
			return notMade(relatedReasonTimeout, timedOut)
		}
		suggestions.Search = relatedKeywordsOnly
		suggestions.SearchNote = relatedKeywordsOnlyNote(reason, meaningAllowance)
		record.reason = reason
	}
	record.search = suggestions.Search

	s.fillRelatedItems(bounded, input, answer.response.Results, suggestions)
	record.related = len(suggestions.Related)
	record.outcome = relatedNone
	if len(suggestions.Related) > 0 {
		record.outcome = relatedFound
	}
	suggestions.Outcome = record.outcome
	return suggestions, record
}

// relatedMeaningStage is stage 2: the text's embedding, started at once, then, once stage 1 has
// picked the terms, the meaning query over them. Its reason is empty when it answered and names
// why it did not otherwise; ctx ends at the meaning allowance.
func (s *server) relatedMeaningStage(ctx context.Context, project, text string, pick *relatedPick) relatedStage {
	failed := func(reason string, err error) relatedStage {
		if ctx.Err() != nil {
			reason = relatedReasonMeaningTimeout
		}
		if err != nil {
			slog.Warn("dispatch: related-item check searches by keywords only", "reason", reason, "error", err)
		}
		return relatedStage{reason: reason, err: err}
	}
	vector, err := s.embedQuery(ctx, text)
	if err != nil {
		return failed(relatedReasonEmbedFailed, err)
	}
	select {
	case <-pick.ready:
	case <-ctx.Done():
		return failed(relatedReasonMeaningTimeout, nil)
	}
	if pick.err != nil || pick.terms.NoLexemes {
		// The check answers not made without reading this.
		return failed(relatedReasonMeaningFailed, nil)
	}
	response, err := s.relatedMeaningSearch(ctx, project, pick.terms, vector)
	if err != nil {
		return failed(relatedReasonMeaningFailed, err)
	}
	return relatedStage{response: response}
}

// relatedMeaningSearch runs relatedMeaningQuery in a read transaction of its own, the one place
// relatedEFSearch applies.
func (s *server) relatedMeaningSearch(
	ctx context.Context, project string, picked relatedTerms, vector []float32,
) (model.SearchResponse, error) {
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, relatedEFSearch); err != nil {
		return model.SearchResponse{}, err
	}
	return s.relatedSearch(ctx, tx, relatedMeaningQuery, project, picked, embed.Literal(vector),
		searchMeaningFloor, s.deps.Embedder.Identity())
}

// relatedSearch runs one of the check's two queries with the parameters search passes its own
// ($1 the chosen lexemes as a tsquery, $4 the first term for the snippet window); meaning is the
// meaning query's further three ($9 the vector, $10 searchMeaningFloor, $11 the embedder's
// identity), none for the keyword query. An empty tsquery (no term chosen) matches nothing, so the
// meaning query then ranks by meaning alone.
func (s *server) relatedSearch(
	ctx context.Context, q queryer, query, project string, picked relatedTerms, meaning ...any,
) (model.SearchResponse, error) {
	snippetTerm := ""
	if len(picked.Terms) > 0 {
		snippetTerm = cmp.Or(picked.Terms[0].Surface, picked.Terms[0].Lexeme)
	}
	args := append([]any{picked.TSQuery, project, suggestionSearchDepth, snippetTerm, headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}, meaning...)
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()
	// A document hit's link opens the document with these words highlighted.
	return scanSearchRows(rows, relatedSearchQuery(picked.Terms), suggestionSearchDepth, 0)
}

// relatedSearchQuery is Suggestions.SearchQuery: the best relatedSearchQueryTerms chosen terms,
// each as the text spells it where one word of the text yields it, else its lexeme.
func relatedSearchQuery(terms []relatedTerm) string {
	words := make([]string, 0, relatedSearchQueryTerms)
	for _, term := range terms[:min(len(terms), relatedSearchQueryTerms)] {
		words = append(words, cmp.Or(term.Surface, term.Lexeme))
	}
	return strings.Join(words, " ")
}

// relatedKeywordsOnlyNote is Suggestions.SearchNote: why the check searched by keywords alone.
func relatedKeywordsOnlyNote(reason string, meaningAllowance time.Duration) string {
	switch reason {
	case relatedReasonNoEmbedder:
		return "meaning search is off on this server"
	case relatedReasonMeaningTimeout:
		return "meaning search did not answer within " + meaningAllowance.String()
	case relatedReasonEmbedFailed:
		return "the text could not be embedded for meaning search"
	case relatedReasonMeaningFailed:
		return "meaning search failed"
	case relatedReasonKeywordsRequested:
		return "keywords only was asked for"
	}
	return ""
}

func milliseconds(d time.Duration) *int {
	ms := int(d.Milliseconds())
	return &ms
}

// fillRelatedItems turns the search's results into Related and Decision. Everything the input
// excludes is left out first; the rest carry their ask state (attachAskStates), which a failure
// to read leaves nil rather than failing the check. Each owner (an issue, or a project document)
// keeps only its best-ranked item, so an issue matched through its title, its spec and a comment
// fills one place, not three. Related then takes the best suggestionRelatedCount with every item
// on an open owner ahead of every item on a done issue, keeping the search's order within each;
// Decision is the first answered ask in the search's own order, deliberately not reordered by
// owner status: an answered question stays the best match for the question asked whether or not
// its issue has since closed.
func (s *server) fillRelatedItems(
	ctx context.Context, input relatedInput, results []model.SearchResult, suggestions *model.Suggestions,
) {
	candidates := make([]model.SearchResult, 0, len(results))
	for _, result := range results {
		if !input.excludes(result) {
			candidates = append(candidates, result)
		}
	}
	if err := s.attachAskStates(ctx, s.deps.Store.Pool, candidates); err != nil {
		slog.Warn("dispatch: related items carry no ask state", "error", err)
	}

	seen := map[string]bool{}
	related := make([]model.SearchResult, 0, len(candidates))
	for _, result := range candidates {
		owner := result.Owner.Kind + ":" + cmp.Or(result.Owner.Key, result.Owner.ArtifactID)
		if !seen[owner] {
			seen[owner] = true
			related = append(related, result)
		}
	}
	slices.SortStableFunc(related, func(a, b model.SearchResult) int {
		return cmp.Compare(boolRank(closedOwner(a)), boolRank(closedOwner(b)))
	})
	for _, result := range related[:min(len(related), suggestionRelatedCount)] {
		suggestions.Related = append(suggestions.Related, suggestionFromResult(result))
	}
	for _, result := range candidates {
		if result.Ask == nil || result.Ask.State != "answered" || result.Ask.Answer == nil {
			continue
		}
		item := suggestionFromResult(result)
		at := result.Ask.Answer.At
		item.AnsweredBy, item.AnsweredAt = result.Ask.Answer.User, &at
		suggestions.Decision = &item
		break
	}
}

// resolveSuggestionProject reads the project a source's own owner belongs to, on ctx: the check's
// own bounded deadline, so this lookup cannot hold a write past it.
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

// closedOwner reports whether result belongs to an issue that is done. Keyword search ranks an
// issue already closed as a duplicate above the open issue it was closed into whenever its words
// are closer to the new filing's, which they often are (a duplicate restates the bug the way the
// next duplicate will); the open issue is the one an agent can act on.
func closedOwner(result model.SearchResult) bool {
	return result.Owner.Kind == "issue" && result.Owner.Status == "done"
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
		Ask:      result.Ask,
	}
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

// nullableText is value, or SQL null for an empty one.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// recordRelatedCheck writes what a check offered and how it went, on a context of its own taken
// from context.WithoutCancel(ctx) and bounded by relatedRecordTimeout: a check that ran out of its
// allowance is recorded too, and a write that already succeeded is never failed by its records,
// so a row that cannot be written is only logged.
//
// An issue or ask source gets its write_suggestions rows, which the outcome sweep resolves
// (persistSuggestions). Every source with a project and an owner gets one related_checks row: its
// outcome, search mode, reason, the lexemes it searched for, the stages' and the whole check's
// times and how many items it listed. A check whose project could not be resolved has no row to
// write, since the row belongs to a project.
func (s *server) recordRelatedCheck(
	ctx context.Context, input relatedInput, suggestions *model.Suggestions, record relatedRecord,
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relatedRecordTimeout)
	defer cancel()
	source := input.source
	if source.kind == "issue" || source.kind == "ask" {
		s.persistSuggestions(ctx, source, suggestions)
	}
	if record.project == "" || (source.issueKey == "" && source.artifactID == "") {
		return
	}
	artifactID := source.artifactID
	if source.issueKey != "" {
		artifactID = ""
	}
	if _, err := s.deps.Store.Pool.Exec(ctx, `
		insert into related_checks
			(project_key, source_kind, source_issue_key, source_artifact_id, source_ask_id, source_block_id,
			 actor_kind, actor_id, outcome, search, reason, terms, keyword_ms, meaning_ms, total_ms, related_count)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	`,
		record.project, source.kind, nullableText(source.issueKey), nullableText(artifactID), nullableText(source.askID),
		nullableText(source.blockID), source.actor.Kind, source.actor.ID, record.outcome, nullableText(record.search),
		nullableText(record.reason), record.terms, record.keywordMS, record.meaningMS, record.totalMS, record.related,
	); err != nil {
		slog.Warn("dispatch: related-item check not recorded", "error", err)
	}
}

// persistSuggestions records every item a check offered, so the outcome sweep has something to
// resolve and a later count has something to count, in one round trip (store.Pool.SendBatch):
// up to four rows, three related plus one decision. A row that cannot be inserted is only logged.
func (s *server) persistSuggestions(ctx context.Context, source suggestionSource, suggestions *model.Suggestions) {
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
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
			insert into write_suggestions
				(source_kind, source_issue_key, source_artifact_id, source_ask_id, actor_kind, actor_id,
				 role, rank, suggested_kind, suggested_id, suggested_issue_key)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`,
			source.kind, nullableText(source.issueKey), nullableText(source.artifactID), nullableText(source.askID),
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
