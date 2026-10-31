package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// relatedCheckAllowance is the most a write waits on its related-item check: the
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
	// recorded; write_suggestions rows that cannot be written in a second are only logged, and a
	// related_checks row is retried once (relatedRecordRetryTimeout).
	relatedRecordTimeout = time.Second
	// relatedRecordRetryTimeout bounds the one retry of a related_checks row the first attempt
	// could not write in relatedRecordTimeout. The insert locks every table its foreign keys name
	// (asks, artifacts, issues), so a migration's ACCESS EXCLUSIVE lock on one of them holds it
	// up - just when the check, held up by the same lock, ran out of time and is the one the
	// records exist to count. Thirty seconds is well past the five a migration waits for a lock
	// (pgmigrate.LockTimeout), and the retry runs in the background, so no write waits on it.
	relatedRecordRetryTimeout = 30 * time.Second
	// relatedRecordRetryLimit is the most related_checks retries that run at once. Each holds, or
	// waits for, a connection of the shared pool for up to relatedRecordRetryTimeout, and a lock
	// that holds up the first attempts holds up every check's: without a bound, those retries
	// alone could take every connection the requests need.
	relatedRecordRetryLimit = 4
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
	// relatedKeywordLegs is search's keyword legs (searchKeywordLegs) ranked by the chosen
	// lexemes' weights (relatedCoordinationRank).
	relatedKeywordLegs = searchKeywordLegs(relatedCoordinationRank)
	// relatedKeywordQuery is the check's stage 1: search's keyword query (searchQueryBody) over the
	// chosen lexemes, with relatedKeywordLegs. Parameters: searchQueryBody's $1-$8, then $9 the
	// lexemes and $10 their idf.
	relatedKeywordQuery = relatedQOpener + relatedTermWeights(9) + searchQueryBody(relatedKeywordLegs)
	// relatedMeaningQuery is the check's stage 2: search's meaning query, which fuses the same
	// keyword legs with the meaning arms, over the same lexemes and the text's embedding.
	// Parameters: searchQueryMeaningBody's $1-$11, then $12 the lexemes and $13 their idf.
	relatedMeaningQuery = relatedQOpener + relatedTermWeights(12) + searchQueryMeaningBody(relatedKeywordLegs)
)

// relatedCoordinationRank ranks a keyword arm's item, searchColumn its search vector, by
// idf-weighted coordination in place of search's ts_rank_cd: the sum of the idf of every chosen
// lexeme the item holds, so an item holding three of the text's rare words outranks one
// repeating one of them, which cover density favours. On a project's closed duplicates it listed
// more of the issues they were closed into than ts_rank_cd over the same terms or the old
// 40-word OR query (TestRelatedRecallOnDuplicates). ts_rank_cd, normalised into [0, 1) and scaled
// by a thousandth, only orders items holding the same lexemes. Each lexeme is tested with @@
// against a tsquery of its own (relatedTermWeights), a binary search of the item's sorted vector,
// not by unnesting the vector, which costs a document's whole length.
func relatedCoordinationRank(searchColumn string) string {
	return "((select sum(w.idf) from term_weights w where " + searchColumn + " @@ w.tq) + 0.001 * ts_rank_cd(" + searchColumn + ", q.tsq, 32))"
}

// relatedTermWeights is the term_weights CTE relatedCoordinationRank reads: each chosen lexeme
// as a tsquery of its own and its idf, from $lexemes the chosen lexemes and $lexemes+1 their idf,
// in one order.
func relatedTermWeights(lexemes int) string {
	return fmt.Sprintf(`term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.idf from unnest($%d::text[], $%d::float8[]) w(lexeme, idf)
),
`, lexemes, lexemes+1)
}

// The check's outcomes and search modes, as Suggestions and related_checks hold them.
const (
	relatedFound   = "found"
	relatedNone    = "none"
	relatedNotMade = "not_made"

	relatedMeaningAndKeywords = "meaning_and_keywords"
	relatedKeywordsOnly       = "keywords_only"
)

// The reasons a check was not made, or searched by keywords alone: related_checks.reason's values
// (0139_related_checks).
const (
	relatedReasonBlankText         = "blank_text"
	relatedReasonTimeout           = "timeout"
	relatedReasonCancelled         = "cancelled"
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
	relatedCancelledMissing    = "the request that ran it ended before the check finished"
)

// errRelatedAllowanceEnded is the cause a check's allowance context ends with when its own
// deadline fires. A check whose context ended with any other cause stopped because the request
// that ran it ended first - its client leaving, for a write sent without an Idempotency-Key; for a
// keyed write, whose context no client's leaving ends (keyedWrites), the keyed run's own bound,
// keyedHandlerLimit - which says nothing of how long the search takes, so it is recorded as
// cancelled rather than timeout (GET /api/v1/related-checks counts only timeout as skipped for
// time).
var errRelatedAllowanceEnded = errors.New("the related-item check's allowance ended")

// relatedAllowance is the deadline the whole check runs under: relatedCheckAllowance on every
// server the deployment builds, since register sets no suggestionTimeout, and whatever a test set
// otherwise.
func (s *server) relatedAllowance() time.Duration {
	return cmp.Or(s.suggestionTimeout, relatedCheckAllowance)
}

// relatedBounds is the context a check's work runs under and the allowance it ends at: ctx
// bounded by relatedAllowance, ending with errRelatedAllowanceEnded as its cause so that a check
// whose context is done can tell its own deadline from the request that ran it going away
// (runRelatedCheck's callerGone). Every site that bounds a check uses it, so a decision block's
// per-check context inherits the edit's allowance with that same cause. The caller cancels, and
// names the allowance in what it answers.
func (s *server) relatedBounds(ctx context.Context) (context.Context, context.CancelFunc, time.Duration) {
	allowance := s.relatedAllowance()
	bounded, cancel := context.WithTimeoutCause(ctx, allowance, errRelatedAllowanceEnded)
	return bounded, cancel, allowance
}

// relatedCheckNotRun answers, and records unless input.record is false, a check its route decided
// not to make (reason and text say why), so that write's result still says no check was made.
func (s *server) relatedCheckNotRun(ctx context.Context, input relatedInput, reason, text string) *model.Suggestions {
	suggestions := notMadeSuggestions(text)
	if input.record {
		s.recordRelatedChecks(ctx, relatedCheckRecord{
			source: input.source, suggestions: suggestions,
			record: relatedRecord{project: input.project, reason: reason, terms: []string{}},
		})
	}
	return suggestions
}

// notMadeSuggestions is the answer of a check that was not made; text says why.
func notMadeSuggestions(text string) *model.Suggestions {
	return &model.Suggestions{Outcome: relatedNotMade, Related: []model.WriteSuggestion{}, Missing: text}
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
// decisionBlockCheckLimit blocks are checked, one after another and all under one allowance
// (relatedBounds), so an edit waits on its checks no longer than an issue create waits on its
// one; each block past them answers as not made, and all of those are recorded in one round trip.
// A block's source is the document's issue, or the project document itself when it has no issue,
// left out of its results as an ask's owner is. Nil when the edit added no block.
//
// One check at a time is what the edit's route is admitted for: a request may hold no more
// request-pool connections than its route's weight (apiRoute.Weight, concurrentReadWeight for
// every route that runs a check), and one check holds two of them, its keyword read and its
// meaning read side by side (store.ForConcurrentRead). Five checks at once asked for ten.
func (s *server) decisionBlockChecks(
	ctx context.Context, artifact model.Artifact, blocks []docs.AddedAskBlock, actor model.Actor,
) []decisionBlockCheck {
	if len(blocks) == 0 {
		return nil
	}
	bounded, cancel, _ := s.relatedBounds(ctx)
	defer cancel()
	checks := make([]decisionBlockCheck, len(blocks))
	var overflow []relatedCheckRecord
	for index, block := range blocks {
		source := suggestionSource{kind: "decision_block", blockID: block.ID, actor: actor}
		if artifact.IssueKey != nil {
			source.issueKey = *artifact.IssueKey
		} else {
			source.artifactID = artifact.ID
		}
		input := relatedInput{project: artifact.Project, text: block.Question, title: block.Question, source: source, record: true}
		checks[index] = decisionBlockCheck{BlockID: block.ID, Question: block.Question}
		if index >= decisionBlockCheckLimit {
			suggestions := notMadeSuggestions("more than five new decision blocks in one edit")
			checks[index].Suggestions = suggestions
			overflow = append(overflow, relatedCheckRecord{
				source: source, suggestions: suggestions,
				record: relatedRecord{project: artifact.Project, reason: relatedReasonTooManyBlocks, terms: []string{}},
			})
			continue
		}
		checks[index].Suggestions = s.relatedCheck(bounded, input)
	}
	if len(overflow) > 0 {
		s.recordRelatedChecks(ctx, overflow...)
	}
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
	text string
	// title is the part of text the chosen terms are written as (relatedTerm.Surface): a new
	// issue's or the no-write route's title, an ask's or decision block's question.
	title  string
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

// relatedRecord is what the related_checks row of one check holds besides its source and what
// the check answered (its outcome, search mode and how many items it listed).
type relatedRecord struct {
	project   string
	reason    string
	terms     []string
	keywordMS *int
	meaningMS *int
	totalMS   int
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
// as a write's, and the no-write related route. It never fails and never returns nil: what it did
// not do, it says in the result. It waits at most relatedAllowance.
//
// It resolves the project when the input does not name it, then starts two things at once: stage
// 1, which picks the text's rarest words (pickRelatedTerms) and runs search's keyword query over
// them, and, when an embedder is configured and keywords alone were not asked for, the text's
// embedding followed by stage 2, search's meaning query, which fuses the same keyword arms with
// the nearest vectors. Stage 2's answer within relatedMeaningAllowance is the result, searched by
// meaning and keywords; otherwise stage 1's is, searched by keywords only, with the reason. If
// stage 1 has not answered when the allowance ends, the check was not made, for time; if the
// request that ran it goes away first, it was not made, cancelled.
//
// The source's own items are left out, each owner keeps its best-ranked item, open owners come
// before done issues, and the best three are Related; the decision is the first answered ask in
// the search's own order. Unless record is false it then records what it offered and how it went
// (recordRelatedChecks).
func (s *server) relatedCheck(ctx context.Context, input relatedInput) *model.Suggestions {
	if s.relatedCheckEntered != nil {
		s.relatedCheckEntered()
	}
	started := time.Now()
	suggestions, record := s.runRelatedCheck(ctx, input, started)
	record.totalMS = int(time.Since(started).Milliseconds())
	if input.record {
		s.recordRelatedChecks(ctx, relatedCheckRecord{source: input.source, suggestions: suggestions, record: record})
	}
	return suggestions
}

// runRelatedCheck is relatedCheck up to its records: the result and the related_checks row's
// fields, all but the total time.
func (s *server) runRelatedCheck(
	ctx context.Context, input relatedInput, started time.Time,
) (*model.Suggestions, relatedRecord) {
	bounded, cancel, allowance := s.relatedBounds(ctx)
	defer cancel()
	record := relatedRecord{project: input.project, terms: []string{}}
	suggestions := &model.Suggestions{Related: []model.WriteSuggestion{}}
	notMade := func(reason, text string) (*model.Suggestions, relatedRecord) {
		suggestions.Outcome, suggestions.Missing = relatedNotMade, text
		record.reason = reason
		return suggestions, record
	}
	timedOut := "the search behind it did not answer within " + allowance.String()
	// callerGone reports a check stopped by its request going away, not by the allowance: a
	// decision block's checks run under the edit's one allowance, which ends theirs with the same
	// cause as their own.
	callerGone := func() bool {
		return bounded.Err() != nil && !errors.Is(context.Cause(bounded), errRelatedAllowanceEnded)
	}
	// ended answers a check whose allowance context is done: timed out (missing says what did not
	// answer) when the allowance ran out, cancelled when the request went away first.
	ended := func(missing string) (*model.Suggestions, relatedRecord) {
		if callerGone() {
			return notMade(relatedReasonCancelled, relatedCancelledMissing)
		}
		return notMade(relatedReasonTimeout, missing)
	}
	// searchFailed answers a stage 1 read that failed: ended when the allowance context was done
	// by then, failed otherwise.
	searchFailed := func(err error) (*model.Suggestions, relatedRecord) {
		slog.Warn("dispatch: related-item check not made", "error", err)
		if bounded.Err() != nil {
			return ended(timedOut)
		}
		return notMade(relatedReasonSearchFailed, relatedSearchFailedMissing)
	}

	if record.project == "" {
		project, err := s.resolveSuggestionProject(bounded, input.source)
		switch {
		case err != nil && bounded.Err() != nil:
			slog.Warn("dispatch: related-item check not made", "error", err)
			return ended("the project lookup behind it did not answer within " + allowance.String())
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

	// Stage 1 runs on a cancel of its own so that a check which answered without it does not
	// leave it holding a request-pool connection past the answer (its end below).
	keywordStage, endKeyword := context.WithCancel(bounded)
	defer endKeyword()
	keywordCtx, err := store.ForConcurrentRead(keywordStage)
	if err != nil {
		slog.Warn("dispatch: related-item check not made", "error", err)
		return notMade(relatedReasonSearchFailed, relatedSearchFailedMissing)
	}
	exclusion := relatedExclusion{issueKey: cmp.Or(input.source.issueKey, input.excludeIssue), artifactID: input.source.artifactID}
	stagesStarted := time.Now()
	pick := &relatedPick{ready: make(chan struct{})}
	keyword := make(chan relatedStage, 1)
	go func() {
		pick.terms, pick.err = s.pickRelatedTerms(keywordCtx, s.deps.Store.Pool, project, input.text, input.title, exclusion)
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
	var endMeaning context.CancelFunc
	// Stage 2's goroutine holds a request-pool connection for its query, so the check ends its
	// context and waits for the stage before it returns, whichever return that is: its allowance
	// running out, the request going away, stage 1 failing. Left running, it would still hold
	// that connection while the edit's next decision-block check ran, which is admitted for its
	// own two connections and not for these as well (decisionBlockChecks). A stage already taken
	// off the channel leaves nothing to wait for.
	defer func() {
		if meaning == nil {
			return
		}
		endMeaning()
		<-meaning
	}()
	reason := ""
	switch {
	case input.keywordsOnly:
		reason = relatedReasonKeywordsRequested
	case s.deps.Embedder == nil:
		reason = relatedReasonNoEmbedder
	default:
		meaningCtx, cancelMeaning := context.WithDeadline(bounded, started.Add(meaningAllowance))
		meaningRead, err := store.ForConcurrentRead(meaningCtx)
		if err != nil {
			cancelMeaning()
			reason = relatedReasonMeaningFailed
			break
		}
		meaningDone, endMeaning = meaningCtx.Done(), cancelMeaning
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
		return ended(timedOut)
	}
	if pick.err != nil {
		return searchFailed(pick.err)
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
			meaning = nil
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
	// A meaning stage cut short by the request going away did not time out, and nobody waits on
	// the keyword stage's answer.
	if callerGone() {
		return notMade(relatedReasonCancelled, relatedCancelledMissing)
	}
	if answered {
		suggestions.Search = relatedMeaningAndKeywords
		// Stage 1's time is recorded when it has answered by now; its answer is not waited for.
		// One still running is ended instead of left to the allowance, so a check that has
		// answered holds no request-pool connection: the next check of the same edit is admitted
		// for its own two, not for these as well (decisionBlockChecks).
		select {
		case stage := <-keyword:
			record.keywordMS = milliseconds(stage.took)
		default:
			endKeyword()
			<-keyword
		}
	} else {
		select {
		case stage := <-keyword:
			record.keywordMS = milliseconds(stage.took)
			if stage.err != nil {
				return searchFailed(stage.err)
			}
			answer = stage
		case <-bounded.Done():
			return ended(timedOut)
		}
		suggestions.Search = relatedKeywordsOnly
		suggestions.SearchNote = relatedKeywordsOnlyNote(reason, meaningAllowance)
		record.reason = reason
	}

	s.fillRelatedItems(bounded, input, answer.response.Results, suggestions)
	suggestions.Outcome = relatedNone
	if len(suggestions.Related) > 0 {
		suggestions.Outcome = relatedFound
	}
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
		return relatedStage{reason: reason}
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
	if s.meaningSearchEntered != nil {
		s.meaningSearchEntered()
	}
	if _, err := tx.Exec(ctx, relatedEFSearch); err != nil {
		return model.SearchResponse{}, err
	}
	return s.relatedSearch(ctx, tx, relatedMeaningQuery, project, picked, embed.Literal(vector),
		searchMeaningFloor, s.deps.Embedder.Identity())
}

// relatedSearch runs one of the check's two queries with the parameters search passes its own
// ($1 the chosen lexemes as a tsquery, $4 the first term for the snippet window), then meaning,
// the meaning query's further three ($9 the vector, $10 searchMeaningFloor, $11 the embedder's
// identity), none for the keyword query, then the chosen lexemes and their idf, the keyword
// arms' weights. An empty tsquery (no term chosen) matches nothing, so the meaning query then
// ranks by meaning alone.
func (s *server) relatedSearch(
	ctx context.Context, q queryer, query, project string, picked relatedTerms, meaning ...any,
) (model.SearchResponse, error) {
	snippetTerm := ""
	if len(picked.Terms) > 0 {
		snippetTerm = cmp.Or(picked.Terms[0].Surface, picked.Terms[0].Lexeme)
	}
	lexemes := make([]string, len(picked.Terms))
	weights := make([]float64, len(picked.Terms))
	for index, term := range picked.Terms {
		lexemes[index], weights[index] = term.Lexeme, term.IDF
	}
	args := []any{picked.TSQuery, project, suggestionSearchDepth, snippetTerm, headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK}
	args = append(append(args, meaning...), lexemes, weights)
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()
	// A document hit's link opens the document with these words highlighted.
	return scanSearchRows(rows, relatedSearchQuery(picked.Terms), suggestionSearchDepth, 0)
}

// relatedSearchQuery is Suggestions.SearchQuery: the best relatedSearchQueryTerms chosen terms,
// each as the title spells it where one word of the title yields it, else its lexeme.
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
// excludes is left out first; the rest carry their ask state (attachAskStates). A failure to read
// those states leaves every item's nil, and so no Decision, rather than failing the check or
// keeping the states read before it. Each owner (an issue, or a project document)
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
		// attachAskStates sets each row's state before it reads resolved asks' answers, so a
		// failure partway would leave a resolved ask that was answered showing no answer.
		slog.Warn("dispatch: related items carry no ask state", "error", err)
		for index := range candidates {
			candidates[index].Ask = nil
		}
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

// relatedCheckRecord is one check's records: its source, what it answered, and the rest of its
// related_checks row.
type relatedCheckRecord struct {
	source      suggestionSource
	suggestions *model.Suggestions
	record      relatedRecord
}

// relatedCheckInsert writes one related_checks row; a row whose id exists is left as it is.
const relatedCheckInsert = `
	insert into related_checks
		(id, project_key, source_kind, source_issue_key, source_artifact_id, source_ask_id, source_block_id,
		 actor_kind, actor_id, outcome, search, reason, terms, keyword_ms, meaning_ms, total_ms, related_count)
	values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	on conflict (id) do nothing
`

// recordRelatedChecks writes what each check offered and how it went, on a context of its own
// taken from context.WithoutCancel(ctx) and bounded by relatedRecordTimeout: a check that ran out
// of its allowance is recorded too, and a write that already succeeded is never failed by its
// records, so a row that cannot be written is only logged.
//
// An issue or ask source gets its write_suggestions rows, which the outcome sweep resolves
// (persistSuggestions). Every source with a project and an owner gets one related_checks row: its
// outcome, search mode, reason, the lexemes it searched for, the stages' and the whole check's
// times and how many items it listed. A check whose project could not be resolved has no row to
// write, since the row belongs to a project. The rows go in one round trip (store.Pool.SendBatch),
// so an edit's decision blocks past decisionBlockCheckLimit, however many, cost one.
//
// related_checks rows the first attempt could not write for a reason a retry can cure
// (relatedRecordRetryable) are tried once more in the background, on the server's lifetime
// bounded by relatedRecordRetryTimeout, and waitBackground waits them out. At most
// relatedRecordRetryLimit retries run at once, each holding or waiting on a connection of the
// shared pool; past them the rows are dropped and logged, so a lock that holds up every check's
// records cannot take the pool from the requests. Each row's id is chosen here and the insert does
// nothing where it exists, since the first attempt's statement may still land after its context
// gave up on it: pgx cancels a statement whose context ends only by a cancel request it sends
// afterwards, and a lock released before that request arrives lets the statement run.
func (s *server) recordRelatedChecks(ctx context.Context, rows ...relatedCheckRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relatedRecordTimeout)
	defer cancel()
	var inserts [][]any
	for _, row := range rows {
		source, record, suggestions := row.source, row.record, row.suggestions
		if source.kind == "issue" || source.kind == "ask" {
			s.persistSuggestions(ctx, source, suggestions)
		}
		if record.project == "" || (source.issueKey == "" && source.artifactID == "") {
			continue
		}
		artifactID := source.artifactID
		if source.issueKey != "" {
			artifactID = ""
		}
		inserts = append(inserts, []any{
			uuid.New(), record.project, source.kind, nullableText(source.issueKey), nullableText(artifactID),
			nullableText(source.askID), nullableText(source.blockID), source.actor.Kind, source.actor.ID,
			suggestions.Outcome, nullableText(suggestions.Search), nullableText(record.reason), record.terms,
			record.keywordMS, record.meaningMS, record.totalMS, len(suggestions.Related),
		})
	}
	if len(inserts) == 0 {
		return
	}
	err := s.insertRelatedChecks(ctx, inserts)
	if err == nil {
		return
	}
	if !relatedRecordRetryable(err) {
		slog.Warn("dispatch: related-item check not recorded", "rows", len(inserts), "error", err)
		return
	}
	if s.relatedRecordRetrying.Add(1) > relatedRecordRetryLimit {
		s.relatedRecordRetrying.Add(-1)
		slog.Warn("dispatch: related-item check not recorded: too many records are already being retried",
			"rows", len(inserts), "error", err)
		return
	}
	s.detached.goDetached(func() {
		defer s.relatedRecordRetrying.Add(-1)
		ctx, cancel := context.WithTimeout(s.lifetime, relatedRecordRetryTimeout)
		defer cancel()
		if err := s.insertRelatedChecks(ctx, inserts); err != nil {
			slog.Warn("dispatch: related-item check not recorded", "rows", len(inserts), "error", err)
		}
	})
}

// insertRelatedChecks writes inserts' related_checks rows in one round trip. A batch is built for
// each attempt: pgx keeps what it learned of a statement on the batch's queued queries, which
// belongs to the connection that sent it. The rows are no write of the request that ran the
// check: they record a write that has already committed and marked its key, so they are made
// outside its transaction, and a failure is only logged.
func (s *server) insertRelatedChecks(ctx context.Context, inserts [][]any) error {
	batch := &pgx.Batch{}
	for _, args := range inserts {
		batch.Queue(relatedCheckInsert, args...)
	}
	return s.deps.Store.Pool.SendBatch(ctx, batch).Close()
}

// relatedRecordRetryable says whether a related_checks insert that failed may succeed when tried
// again: one that ran out of time, waiting on a lock, or that Postgres refused for a lock or a
// serialization conflict (SQLSTATE classes 55 and 40). Any other refusal, such as a constraint
// the row breaks (class 23), would only be refused again.
func relatedRecordRetryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && (strings.HasPrefix(pgError.Code, "40") || strings.HasPrefix(pgError.Code, "55"))
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
