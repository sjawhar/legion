package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

// duplicateCandidateLimit is how many candidates the gate names: the five a 409 has always named.
const duplicateCandidateLimit = 5

// The two bars the near-copy score is read against. A score is the share of the new text's picked
// rarest-word weight that a candidate holds, so both are fractions of one.
//
// TestNearCopyRefusalReplay sets them: it replays every create of INFRA from 2026-10-20 to
// 2026-11-04 against the project as it stood at each create, over the corpus
// `near-copy-corpus.jsonl` attached to dispatch://INFRA-1160, and reports the false-refusal and
// nudge rates at each bar. The test asserts that these constants hold the rates the report
// names, so a later drift fails it. A change to the term picker, to the project's term
// statistics or to the scoring statement invalidates the calibration: run the replay again.
const (
	// duplicateRefuseScore is the score at or above which a create is refused 409
	// POSSIBLE_DUPLICATE. At most 5% of the creates the replay judges not duplicates may reach it
	// (dispatch://INFRA-1160/spec#b-r2). Measured on 2026-11-05 over 1,159 INFRA creates: 64
	// refused, 49 of them not duplicates, 4.28% of the 1,144 creates not marked. It is the
	// lowest bar under that ceiling, so it stops the most near copies the ceiling allows: the
	// next bar down, 0.70, refuses 75 that are not duplicates, 6.55%.
	duplicateRefuseScore = 0.75
	// duplicateNudgeScore is the score at or above which the closest open issue is named to the
	// filer as a likely duplicate, without refusing the create. At most 15% of the replay's
	// creates may reach it (dispatch://INFRA-1160/spec#b-r4). Measured on the same replay: 114
	// creates nudged, 9.8%. The next bar down, 0.55, nudges 163, 14.1%, which is under the
	// ceiling by less than one create in a hundred and so is not where a bar belongs.
	duplicateNudgeScore = 0.60
	// duplicateMinTerms is how many terms the new text must carry before the gate judges it at
	// all. The score is a share of the picked terms, and pickRelatedTerms keeps only a term some
	// other item of the project holds (a term no other item holds has df 0 and is dropped). So a
	// text that yields one or two terms scores 1.0 against any issue that holds them, and a
	// two-word title would be refused as a near copy of an issue it shares one word with. Three
	// is what the title rule this replaces asked for as well (`shared >= 3`), and it is as high
	// as it can go: one near-copy pair the replay must refuse, INFRA-432 against INFRA-431, is
	// two issues with one title and no spec at all, and it carries three terms.
	duplicateMinTerms = 3
)

// duplicateIssueKeyLexeme matches a lexeme shaped like a Dispatch issue key, lower-cased as a
// vector holds it.
var duplicateIssueKeyLexeme = regexp.MustCompile(`^[a-z][a-z0-9_]*-[0-9]+$`)

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

// scoredDuplicate is one candidate with the near-copy score that found it. The score is the
// gate's own reading and is not part of what a client is told, so it rides here rather than on
// model.DuplicateCandidate.
type scoredDuplicate struct {
	model.DuplicateCandidate
	Score float64
}

// duplicateGateResult is what the near-copy gate answered for one create.
type duplicateGateResult struct {
	// Answered is false when the gate's read failed or ran out of its bound. The create goes in
	// either way: a forced one records that it was forced with no issues named, an unforced one
	// is told that no near-copy check was made. Refusing a create because the gate was slow
	// would turn one slow read into a project that cannot be written to.
	Answered bool
	// Candidates are the project's open non-ancestor issues whose score reaches
	// duplicateNudgeScore, best score first, at most duplicateCandidateLimit.
	Candidates []scoredDuplicate
}

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
// The error is the read's own. An unforced create is refused with it, as createIssue has always
// answered a failed check; a forced one goes on with Answered false.
func (s *server) duplicateGate(
	ctx context.Context, project, title, spec string, ancestors []string,
) duplicateGateResult {
	bound := s.duplicateGateBound()
	gateCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	candidates, err := s.readDuplicateCandidates(gateCtx, project, title, spec, ancestors, bound, duplicateNudgeScore)
	if err != nil {
		slog.Warn("dispatch: the near-copy check did not answer", "project", project, "error", err)
		return duplicateGateResult{}
	}
	return duplicateGateResult{Answered: true, Candidates: candidates}
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
	defer tx.Rollback(ctx)
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
	lexemes := make([]string, 0, len(picked.Terms))
	weights := make([]float64, 0, len(picked.Terms))
	for _, term := range picked.Terms {
		if keyLexemes[term.Lexeme] || duplicateIssueKeyLexeme.MatchString(term.Lexeme) {
			continue
		}
		lexemes = append(lexemes, term.Lexeme)
		weights = append(weights, term.IDF)
	}
	if len(lexemes) < duplicateMinTerms {
		return nil, nil
	}
	if ancestors == nil {
		ancestors = []string{}
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

// nearCopyCheckNotMade is model.Suggestions.NearCopyCheck when the gate's read did not answer:
// the create went in without the near-copy check an unforced create otherwise meets.
const nearCopyCheckNotMade = "the near-copy check before this create did not answer in " +
	"500ms, so the create was not checked against the project's open issues"
