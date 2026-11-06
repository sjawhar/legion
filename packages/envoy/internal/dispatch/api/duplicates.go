package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

// duplicateCandidateLimit is how many candidates the gate names: the five a 409 has always named.
const duplicateCandidateLimit = 5

// The two bars the near-copy score is read against. A score is the share of the new text's picked
// rarest-word weight that a candidate holds, so both are fractions of one.
//
// TestNearCopyRefusalReplay measures them: it replays every create of INFRA from 2026-10-20 to
// 2026-11-04 against the project as it stood at each create, over the corpus
// `near-copy-corpus.jsonl` attached to dispatch://INFRA-1160, and reports the false-refusal and
// nudge rates at each bar. It holds the two constants to the ceilings the spec gives them, but
// it is env-gated and CI never runs it (near_copy_replay_test.go), so nothing fails when the
// numbers below go stale. A change to the term picker, to the project's term statistics or to
// the scoring statement invalidates this calibration, and the replay is the recalibration to
// run by hand after it.
const (
	// duplicateRefuseScore is the score at or above which a create is refused 409
	// POSSIBLE_DUPLICATE. At most 5% of the creates the replay judges not duplicates may reach it
	// (dispatch://INFRA-1160/spec#b-r2). Measured over 1,159 INFRA creates, with the project's
	// term statistics refreshed every ten minutes of replayed time as a live server refreshes
	// them (dispatch://INFRA-1160/artifact/near-copy-replay-md, version 3): 55 refused, 43 of
	// them not duplicates, 3.76% of the 1,144 creates not marked. It is not the lowest bar under
	// that ceiling - the next bar down, 0.75, refuses 57 that are not duplicates, 4.98%, which
	// holds too - and the architect kept 0.80 for the margin 3.76% leaves: the two creates 0.75
	// refuses and 0.80 does not are both above the nudge bar, so their filer is told of the
	// issue either way.
	duplicateRefuseScore = 0.80
	// duplicateNudgeScore is the score at or above which the closest open issue is named to the
	// filer as a likely duplicate, without refusing the create. At most 15% of the replay's
	// creates may reach it (dispatch://INFRA-1160/spec#b-r4). Measured on the same replay: 131
	// creates nudged, 11.30%. The next bar down, 0.55, nudges 15.53%, over that ceiling.
	duplicateNudgeScore = 0.60
	// duplicateMinTerms is how many terms the new text must carry before the gate scores it at
	// all. The score is a share of the picked terms, and pickRelatedTerms keeps only a term some
	// other item of the project holds (a term no other item holds has df 0 and is dropped), and
	// drops one more than a tenth of the project holds as common. So a text that yields one or
	// two terms scores 1.0 against any issue that holds them, and a two-word title would be
	// refused as a near copy of an issue it shares one word with. Three is what the title rule
	// this replaces asked for as well (`shared >= 3`).
	//
	// Below it the gate does not score at all: it refuses only an open issue whose title and
	// spec hold the same lexemes as the new text's, which is duplicateEqualLexemesQuery. A short
	// text is a near copy only when its words are the same ones.
	duplicateMinTerms = 3
)

// duplicateIssueKeyLexeme matches a lexeme shaped like a Dispatch issue key, lower-cased as a
// vector holds it.
var duplicateIssueKeyLexeme = regexp.MustCompile(`^[a-z][a-z0-9_]*-[0-9]+$`)

// duplicateGateRollback bounds the gate's own rollback, which runs after the read whatever
// happened to it. It is not the read's bound: it ends a rollback the database will not take,
// so the handler is never held by one.
const duplicateGateRollback = 2 * time.Second

// duplicateKeyLexemesQuery is what the issues trigger makes of a key of project $1
// (0057_issues_search_trigger): the compound lexeme, the project's own word and the number.
const duplicateKeyLexemesQuery = `select array(select l.lexeme from unnest(to_tsvector('english', search_text($1 || '-0'))) l)`

// duplicateKeyLexemes reads, as a set, the lexemes an issue key of project contributes to
// issues.search. The statement reads no table, so it costs the gate one round trip of a
// constant expression inside the transaction it already holds.
//
// Every such lexeme is dropped from the picked terms before the gate scores anything, so neither
// the score nor its denominator counts one, and neither does duplicateMinTerms. `issues.search`
// holds the issue's own key, so `infra-431` is a lexeme with the rarest weight in the project,
// and `skill://dispatch-first` tells a filer to cite the nearest issue it ruled out, so a new
// spec routinely holds the key of the issue most like it: left in, that one citation would hand
// the cited issue the heaviest term of the query and the gate would refuse the create for citing
// its neighbour. Postgres parses `infra-431` into the compound and its two parts, so the
// project's own word goes with it, and the number is noise the picker already drops. The
// project's own word is in every issue of the project, so in a project of more than
// relatedCommonFloor items the picker drops it as too common anyway; dropping it here makes the
// rule exact rather than incidental.
func duplicateKeyLexemes(ctx context.Context, q queryer, project string) (map[string]bool, error) {
	var lexemes []string
	if err := q.QueryRow(ctx, duplicateKeyLexemesQuery, project).Scan(&lexemes); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(lexemes))
	for _, lexeme := range lexemes {
		set[lexeme] = true
	}
	return set, nil
}

// duplicateScoreQuery scores the project's open issues against the new text's picked rarest
// words: the share of the picked weight that a candidate's title vector and its primary
// document's latest vector hold together.
//
// The statement drives from term_weights, one row per picked lexeme and its idf, into
// issues.search and into document_search (migration 20261101182447, one row per document holding
// its latest version's vector), so each term is one probe of a GIN index and no candidate's spec
// vector is read more than once per term. project_key, the status and the ancestors are filters
// on the rows those index scans produced, never the driving predicate.
//
// Not the obvious shape - one tsquery to find the candidates, then relatedCoordinationRank's
// per-candidate `sum(idf) from term_weights where search @@ tq` - because that tests each
// candidate's spec vector once per term, so its cost grows with the length of every candidate's
// spec, and a project's specs are long. The per-term join does its work inside the index and is
// bounded by the picker's own relatedMatchBudget.
//
// A term that matches both a candidate's title and its spec counts once: the two arms are joined
// with `union`, which takes distinct (key, lexeme, idf) before the sum, and idf is a function of
// the lexeme.
//
// A candidate's headline marks the words of its title the new text shares: its query is the
// shared lexemes, as the title rule it replaces built it.
//
// Parameters: $1 project, $2 the picked lexemes, $3 their idf, $4 the new issue's ancestors,
// $5 the lowest score to return, $6 the headline options.
var duplicateScoreQuery = `
with term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.lexeme, w.idf
    from unnest($2::text[], $3::float8[]) w(lexeme, idf)
),
picked as (select coalesce(sum(idf), 0) as weight from term_weights),
hits as (
  select i.key, w.lexeme, w.idf
    from term_weights w
    join issues i on i.search @@ w.tq
   where i.project_key = $1 and i.status <> 'done' and i.key <> all($4::text[])
  union
  select i.key, w.lexeme, w.idf
    from term_weights w
    join document_search d on d.search @@ w.tq
    join artifacts a on a.id = d.artifact_id and a.kind = 'doc' and a.is_primary
    join issues i on i.key = a.issue_key
   where i.project_key = $1 and i.status <> 'done' and i.key <> all($4::text[])
),
scored as (
  select h.key, sum(h.idf) / nullif((select weight from picked), 0) as score,
         count(*)::int as shared, array_agg(h.lexeme) as shared_lex
    from hits h
   group by h.key
)
select s.key, i.title, i.status, s.shared, s.score,
       ts_headline('english', search_text(i.title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(s.shared_lex) x)), $6) as headline
  from scored s join issues i on i.key = s.key
 where s.score >= $5
 order by s.score desc, i.updated_at desc
 limit ` + strconv.Itoa(duplicateCandidateLimit) + `
`

// duplicateEqualLexemesQuery finds the project's open issues, other than the new issue's
// ancestors, whose title and spec hold exactly the lexemes the new title and spec hold. It is the
// whole of the gate below duplicateMinTerms, where there is too little text to score a share of:
// a short text is a near copy only when its words are the same ones, so "Held by a stopped agent"
// and "Held by a running agent" are two issues and a second copy of either is one.
//
// Each side's lexemes are built the way the row that stores them was built, or the comparison
// would answer on a difference in construction rather than in words. A title's are
// title_lexemes() (migration 0088), the function the issues trigger writes issues.title_lexemes
// with. A spec's are the document vector artifact_versions_search() builds, which 0058's trigger
// writes on every version and 20261101182454's trigger copies into document_search for the
// primary document's latest version: search_vector, with an empty head, over search_text of the
// stored markdown with its ask blocks stripped, as 0087 rewrote that function (0058 built the
// same vector with to_tsvector, which search_vector only leaves for text too long to hold in one
// tsvector). The spec this side reads is the new one as Dispatch would store it, rendered by
// duplicateStoredSpec, because the stored vector is of the rendering and not of the request's
// markdown. tsvector_to_array answers a vector's lexemes sorted and each once, as title_lexemes
// does, so array equality is set equality here.
//
// A spec is compared whether or not either side has one, which covers the architect's rule for
// both: with no spec on either side the two empty sets are equal and the titles decide, and with
// one on either side the specs must hold the same lexemes too. An issue whose primary document
// has no vector - a file upload, a document of ask blocks alone - is read as the empty set, which
// is what document_search holds for it.
//
// A title that yields no lexemes at all, every word of it a stopword, matches nothing: there is
// no text to be a copy of. Without that guard it would name every other such issue in the
// project.
//
// Parameters: $1 project, $2 the new title, $3 the new spec as it would be stored, $4 the new
// issue's ancestors, $5 the headline options.
var duplicateEqualLexemesQuery = `
with new_text as (
  select title_lexemes($2) as title_lex,
         tsvector_to_array(search_vector('',
           regexp_replace(search_text($3), '(?s):::ask\{.*?:::', '', 'g'))) as spec_lex
)
select i.key, i.title, i.status, cardinality(n.title_lex)::int as shared, 1::float8 as score,
       ts_headline('english', search_text(i.title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(n.title_lex) x)), $5) as headline
  from new_text n
  join issues i
    on i.project_key = $1 and i.status <> 'done' and i.key <> all($4::text[])
   and i.title_lexemes = n.title_lex
  left join artifacts a on a.issue_key = i.key and a.kind = 'doc' and a.is_primary
  left join document_search d on d.artifact_id = a.id
 where cardinality(n.title_lex) > 0
   and coalesce(tsvector_to_array(d.search), '{}') = n.spec_lex
 order by i.updated_at desc
 limit ` + strconv.Itoa(duplicateCandidateLimit) + `
`

// duplicateStoredSpec is the new spec as a create would store it: the parse and the render
// createIssue runs over it (docs' SeedText, pmdoc.ParseWritten over no live document, taken as
// an upload is taken, then pmdoc.Render), without writing anything. Nothing else of the seed is
// run here, since nothing else of it changes the markdown.
//
// duplicateEqualLexemesQuery compares the new spec with document_search.search, the vector of
// the markdown the seed stored, and a rendering differs from the text it was parsed from
// wherever the renderer normalises one: an ordered list is renumbered, and Postgres keeps a
// list's numbers as lexemes, so `1.` `1.` is stored as `1.` `2.` and an exact copy of that spec
// would hold different lexemes from the row it copies. An empty spec is what createIssue seeds
// for a spec of whitespace alone.
//
// Markdown the parse or the render refuses is handed on as it came. The create that follows
// refuses it with the error of its own seed, which says what is wrong with it; the gate answers
// on the text it was given rather than failing a create the seed would have let through.
func duplicateStoredSpec(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return ""
	}
	tree, _, err := pmdoc.ParseWritten(spec, nil)
	if err != nil {
		return spec
	}
	tree = pmdoc.StripAnchorMarks(tree)
	pmdoc.StripServerOwnedAttrs(tree)
	stored, err := pmdoc.Render(tree)
	if err != nil {
		return spec
	}
	return stored
}

// scoredDuplicate is one candidate with the near-copy score that found it. The score is the
// gate's own reading and is not part of what a client is told, so it rides here rather than on
// model.DuplicateCandidate.
type scoredDuplicate struct {
	model.DuplicateCandidate
	Score float64
}

// duplicateGateResult is what the near-copy gate answered for one create.
type duplicateGateResult struct {
	// Unanswered is why the gate's read did not answer, in the words the create tells the filer
	// (model.Suggestions.NearCopyCheck); empty exactly when it answered. The create goes in
	// either way and is told this, forced or not, a forced one also recording that it was forced
	// with no issues named, because refusing a create whose check was slow or broken would turn
	// one bad read into a project that cannot be written to.
	Unanswered string
	// Candidates are the project's open non-ancestor issues whose score reaches
	// duplicateNudgeScore, best score first, at most duplicateCandidateLimit.
	Candidates []scoredDuplicate
}

// answered reports whether the gate's read answered at all. Candidates is empty both when it
// answered and found nothing and when it did not answer, and the two are recorded apart.
func (result duplicateGateResult) answered() bool { return result.Unanswered == "" }

// refusals are the candidates at or above duplicateRefuseScore: what a 409 names, and what a
// forced create records that it forced past. Never nil, so it marshals as [].
func (result duplicateGateResult) refusals() []model.DuplicateCandidate {
	refusals := []model.DuplicateCandidate{}
	for _, candidate := range result.Candidates {
		if candidate.Score >= duplicateRefuseScore {
			refusals = append(refusals, candidate.DuplicateCandidate)
		}
	}
	return refusals
}

// nudge is the closest candidate when its score is at or above duplicateNudgeScore and below
// duplicateRefuseScore, and nil otherwise: a create the gate would have refused is refused or
// forced, and is never also nudged.
func (result duplicateGateResult) nudge() *model.DuplicateCandidate {
	if len(result.Candidates) == 0 {
		return nil
	}
	closest := result.Candidates[0]
	if closest.Score < duplicateNudgeScore || closest.Score >= duplicateRefuseScore {
		return nil
	}
	candidate := closest.DuplicateCandidate
	return &candidate
}

// duplicateGateBound is how long the gate's read may take, both when it refuses a create and when
// it only names what a forced create went past: whatever the server was built with, else
// duplicateGateTimeout. Only a test sets the field, so a case's outcome is its own choice
// rather than its machine's speed.
func (s *server) duplicateGateBound() time.Duration {
	return cmp.Or(s.duplicateBound, duplicateGateTimeout)
}

// duplicateGateText is what the gate reads for a new issue: its title and its spec, joined as
// createIssue joins them for the related-item check.
func duplicateGateText(title, spec string) string {
	if strings.TrimSpace(spec) == "" {
		return title
	}
	return title + "\n" + spec
}

// duplicateGate scores the project's open issues, other than the new issue's ancestors, against
// its title and spec. Both paths of a create run it, so the record of what a forced create went
// past is the refusal an unforced one would have met.
//
// It runs in a read transaction of its own, bounded by duplicateGateBound on the context and by
// statement_timeout inside it, because the create waits on it. One bound for both paths: with
// two, a read of 1.2 seconds would refuse an unforced create while recording for a forced one
// that the check did not answer.
//
// A read that does not answer refuses nothing: the create goes in and the filer is told, in the
// words Unanswered carries, that no near-copy check was made. The bound and a failure of the
// read itself are told apart there and in the log, because the second is the server's own fault
// - before this gate it was a visible 500 - and happens whatever the bound is.
func (s *server) duplicateGate(
	ctx context.Context, project, title, spec string, ancestors []string,
) duplicateGateResult {
	bound := s.duplicateGateBound()
	gateCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	candidates, err := s.readDuplicateCandidates(gateCtx, project, title, spec, ancestors, bound, duplicateNudgeScore)
	switch {
	case err == nil:
		return duplicateGateResult{Candidates: candidates}
	case nearCopyBoundHit(err) || errors.Is(err, context.Canceled):
		// The read ran out of its bound, or the create's own request went away under it, which
		// answers nobody anything.
		slog.Warn("dispatch: the near-copy check did not answer within its bound",
			"project", project, "bound", bound, "error", err)
		return duplicateGateResult{Unanswered: nearCopyCheckNotMade(bound)}
	}
	slog.Error("dispatch: the near-copy check failed", "project", project, "error", err)
	return duplicateGateResult{Unanswered: nearCopyCheckFailed}
}

// readDuplicateCandidates picks the new text's rarest words and scores the project's open issues
// against them, in one read transaction, answering those at or above floor, best score first.
// The gate's floor is duplicateNudgeScore, the lowest score it ever names; the replay that sets
// the two bars reads from zero, so the whole distribution is visible to it.
func (s *server) readDuplicateCandidates(
	ctx context.Context, project, title, spec string, ancestors []string, bound time.Duration, floor float64,
) ([]scoredDuplicate, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	// The rollback runs on a context of its own, not the gate's: the gate's is expired exactly
	// when the rollback matters, and pgx answers a rollback on an expired context by destroying
	// the connection instead of returning it to the pool, which leaves a backend on the database
	// for as long as the server lives. A read held past the bound is the ordinary case here, not
	// the rare one, since the bound is what ends a slow one.
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), duplicateGateRollback)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	if _, err := tx.Exec(ctx, fmt.Sprintf("set local statement_timeout = '%dms'", bound.Milliseconds())); err != nil {
		return nil, err
	}
	// The new issue does not exist yet, so nothing of it is left out of the word statistics.
	picked, err := s.pickRelatedTerms(ctx, tx, project, duplicateGateText(title, spec), title, relatedExclusion{}, priceUnknownTerms)
	if err != nil {
		return nil, err
	}
	keyLexemes, err := duplicateKeyLexemes(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	lexemes, weights := nearCopyScoreTerms(picked, keyLexemes)
	if ancestors == nil {
		ancestors = []string{}
	}
	if len(lexemes) < duplicateMinTerms || picked.TextLexemes <= duplicateMinTerms {
		// Too little to score a share of, so the only near copy is one holding the same words:
		// duplicateEqualLexemesQuery. Either the pick came out short, or the text itself holds
		// no more words than duplicateMinTerms, which is the case a share reads wrongly: a
		// three-word title scores 1.0 against any wordy issue that happens to hold its three
		// words, so "Held by a stopped agent" is refused as a copy of "Held by a running agent"
		// whose spec mentions a stopped one. Its answer is an equality rather than a score, so
		// floor does not apply to it - a match is 1 and nothing else is answered.
		rows, err := tx.Query(ctx, duplicateEqualLexemesQuery,
			project, title, duplicateStoredSpec(spec), ancestors, duplicateHeadlineOptions)
		if err != nil {
			return nil, err
		}
		return scanDuplicateCandidates(rows)
	}
	rows, err := tx.Query(ctx, duplicateScoreQuery,
		project, lexemes, weights, ancestors, floor, duplicateHeadlineOptions)
	if err != nil {
		return nil, err
	}
	return scanDuplicateCandidates(rows)
}

// scanDuplicateCandidates reads the near-copy check's rows, (key, title, status, shared, score,
// headline), and closes them.
func scanDuplicateCandidates(rows pgx.Rows) ([]scoredDuplicate, error) {
	defer rows.Close()
	candidates := []scoredDuplicate{}
	for rows.Next() {
		var candidate scoredDuplicate
		var headline string
		if err := rows.Scan(
			&candidate.Key, &candidate.Title, &candidate.Status, &candidate.SharedTerms, &candidate.Score, &headline,
		); err != nil {
			return nil, err
		}
		candidate.Snippet = markSnippet(headline)
		candidate.Href = "/issues/" + candidate.Key
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// nearCopyScoreTerms is duplicateScoreQuery's $2 and $3: the picked lexemes and their idf, with
// every lexeme an issue key contributes left out (duplicateKeyLexemes says why). The gate and
// the replay that times it both build the statement's parameters here, so what the replay
// measures is the statement the gate runs.
func nearCopyScoreTerms(picked relatedTerms, keyLexemes map[string]bool) ([]string, []float64) {
	lexemes := make([]string, 0, len(picked.Terms))
	weights := make([]float64, 0, len(picked.Terms))
	for _, term := range picked.Terms {
		if keyLexemes[term.Lexeme] || duplicateIssueKeyLexeme.MatchString(term.Lexeme) {
			continue
		}
		lexemes = append(lexemes, term.Lexeme)
		weights = append(weights, term.IDF)
	}
	return lexemes, weights
}

// nearCopyBoundHit says whether the gate's read ended at its bound. The bound is on the context
// and on statement_timeout inside the read transaction, so Postgres cancels the statement
// (SQLSTATE 57014) or the context expires first; either way no check was made, and the read
// itself is at no fault.
func nearCopyBoundHit(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "57014" // query_canceled
}

// nearCopyCheckNotMade is model.Suggestions.NearCopyCheck when the gate's read ran out of its
// bound: the create went in without the near-copy check an unforced create otherwise meets. The
// time is the bound the read was given (duplicateGateBound), never a literal, so the note cannot
// name a bound the server does not run under.
func nearCopyCheckNotMade(bound time.Duration) string {
	return fmt.Sprintf("the near-copy check before this create did not answer in %s, so the "+
		"create was not checked against the project's open issues", bound)
}

// nearCopyCheckFailed is model.Suggestions.NearCopyCheck when the gate's read failed outright: a
// SQL error, a connection lost, anything that is not the bound. It is worded apart from a slow
// read because it is deterministic - every create of every project meets it until it is fixed -
// and a filer told its create "was not answered in 500ms" would wait for a bound that was never
// the cause.
const nearCopyCheckFailed = "the near-copy check before this create failed, so the create was " +
	"not checked against the project's open issues"
