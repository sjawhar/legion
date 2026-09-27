package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Before an agent's question becomes an ask, Dispatch looks for human-authored content in the
// ask's project that may already answer it: answered asks (the answer and the question it
// answered), human comments, human issue messages, and issues filed in the last day whose title
// or spec matches. An issue counts whoever filed it, because an issue filed from a human's
// answer carries that answer; every other kind counts only when a human wrote it.
//
// A candidate is scored against the question (its text and option labels) over the same
// english tsvectors search reads, weighting each shared term by how rare it is across Dispatch:
// idf(t) = ln((N+1)/(df(t)+1)) over every ask, comment, message and issue title, where df comes
// from the search columns' GIN indexes. The score is
//
//	sum(idf(t)^2 over shared terms) * sqrt(|Q|) / (sum(idf(t)^2 over Q) * sqrt(|C|))
//
// for question terms Q and candidate terms C: a cosine whose candidate norm is approximated from
// its term count and the question's own mean weight, so a candidate that repeats the question
// scores 1, and no candidate's terms need their own document frequencies. A candidate needs at
// least three shared terms and its kind's threshold. The thresholds and the one-day issue window
// come from replaying production's 505 agent questions asked through these routes from
// 2026-09-20 to 2026-09-27, which refuses 37 of them; the issue threshold is the highest that
// still offers AGENTC-1010 for the question it answered.
const (
	priorAnswerMinShared = 3
	priorAnswerLimit     = 5
	priorAskThreshold    = 0.35
	priorWordsThreshold  = 0.25
	priorIssueThreshold  = 0.24
	priorIssueWindow     = 24 * time.Hour
)

const (
	priorAnswerHeadline = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=30, MinWords=12, MaxFragments=1"
	priorTitleHeadline  = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"
)

// priorAnswerQuery takes $1 the ask's issue key or null, $2 its project document's id or null,
// $3 the question text, $4 the moment it is asked (nothing written at or after it is prior),
// $5 the issue window's start, $6..$8 the ask, comment-or-message and issue thresholds, $9 the
// minimum shared terms, $10 the limit, and $11 and $12 the headline options for text and for an
// issue title.
const priorAnswerQuery = `
with target as (
  select coalesce((select project_key from issues where key = $1),
                  (select project_key from artifacts where id = $2::uuid)) as project
),
terms as (
  select distinct lexeme,
         ('''' || replace(replace(lexeme, '\', '\\'), '''', '''''') || '''')::tsquery as query
    from unnest(tsvector_to_array(to_tsvector('english', $3))) lexeme
),
corpus as (
  select (select count(*) from asks where created_at < $4) + (select count(*) from comments where created_at < $4)
       + (select count(*) from messages where created_at < $4) + (select count(*) from issues where created_at < $4) as n
),
weights as (
  select t.query,
         power(ln((c.n + 1.0) / (1
           + (select count(*) from asks where search @@ t.query and created_at < $4)
           + (select count(*) from comments where search @@ t.query and created_at < $4)
           + (select count(*) from messages where search @@ t.query and created_at < $4)
           + (select count(*) from issues where search @@ t.query and created_at < $4))), 2) as w
    from terms t, corpus c
),
question as (
  select sum(w) as weight, count(*) as size,
         (select string_agg(query::text, ' | ') from terms)::tsquery as any_term
    from weights
),
candidates as (
  select 'ask' as kind, k.id::text as id, k.issue_key, i.project_key as issue_project, a.project_key as doc_project, a.slug as doc_slug,
         (k.answer->>'at')::timestamptz as at, jsonb_build_object('kind', 'user', 'id', k.answer->>'user') as author, k.search as vec
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
   where k.state = 'answered' and (k.answer->>'at')::timestamptz < $4
  union all
  select 'comment', c.id::text, c.issue_key, i.project_key, a.project_key, a.slug, c.created_at, c.author, c.search
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
   where c.author->>'kind' = 'user' and c.created_at < $4
  union all
  select 'message', m.id::text, m.issue_key, i.project_key, null, null, m.created_at, m.author, m.search
    from messages m join issues i on i.key = m.issue_key
   where m.author->>'kind' = 'user' and m.created_at < $4
),
matched as (
  select c.kind, c.id, c.issue_key, c.doc_slug, c.at, c.author, c.vec
    from candidates c, target t, question q
   where coalesce(c.issue_project, c.doc_project) = t.project and c.vec @@ q.any_term
  union all
  select 'issue', i.key, i.key, null, i.created_at, i.created_by, i.search || coalesce(v.search, ''::tsvector)
    from issues i
    cross join target t
    left join artifacts pa on pa.issue_key = i.key and pa.is_primary
    left join lateral (select search from artifact_versions
                        where artifact_id = pa.id and created_at < $4 order by number desc limit 1) v on true
   where i.project_key = t.project and i.key is distinct from $1 and i.created_at < $4 and i.created_at >= $5
),
scored as (
  select m.*, s.shared,
         s.weight * sqrt(q.size) / (q.weight * sqrt(greatest(length(m.vec), 1))) as score
    from matched m
    cross join question q
    cross join lateral (select count(*) as shared, coalesce(sum(w.w), 0) as weight
                          from weights w where m.vec @@ w.query) s
   where q.weight > 0
),
top as (
  select * from scored
   where shared >= $9
     and score >= case kind when 'ask' then $6::float8 when 'issue' then $8::float8 else $7::float8 end
   order by score desc, at desc
   limit $10
)
select top.kind, top.id, top.issue_key, coalesce(t.project, ''), top.doc_slug, top.at, top.author, top.shared, top.score,
       case top.kind
         when 'ask' then (select ts_headline('english', k.question, q.any_term, $11) || ' → ' ||
                                 ts_headline('english', concat_ws(': ',
                                   nullif((select string_agg(s, ', ') from jsonb_array_elements_text(
                                     case when jsonb_typeof(k.answer->'selected') = 'array' then k.answer->'selected' else '[]'::jsonb end) s), ''),
                                   nullif(k.answer->>'text', '')), q.any_term, $11)
                            from asks k where k.id = top.id::uuid)
         when 'comment' then (select ts_headline('english', c.body, q.any_term, $11) from comments c where c.id = top.id::uuid)
         when 'message' then (select ts_headline('english', m.body, q.any_term, $11) from messages m where m.id = top.id::uuid)
         else (select ts_headline('english', i.title, q.any_term, $12) || ' — ' ||
                      ts_headline('english', coalesce((select v.markdown from artifacts pa join artifact_versions v on v.artifact_id = pa.id
                                                        where pa.issue_key = i.key and pa.is_primary and v.created_at < $4
                                                        order by v.number desc limit 1), ''), q.any_term, $11)
                 from issues i where i.key = top.id)
       end
  from top, target t, question q
 order by top.score desc, top.at desc
`

// priorAnswerCandidates returns the human-authored content in the owner's project that may
// already answer text, best first, at most priorAnswerLimit. asOf is the moment of the ask:
// only what was written before it counts.
func (s *server) priorAnswerCandidates(ctx context.Context, q queryer, owner owner, text string, asOf time.Time) ([]model.PriorAnswerCandidate, error) {
	rows, err := q.Query(ctx, priorAnswerQuery,
		owner.IssueKey, owner.ArtifactID, text, asOf, asOf.Add(-priorIssueWindow),
		priorAskThreshold, priorWordsThreshold, priorIssueThreshold, priorAnswerMinShared, priorAnswerLimit,
		priorAnswerHeadline, priorTitleHeadline)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []model.PriorAnswerCandidate{}
	for rows.Next() {
		var candidate model.PriorAnswerCandidate
		var id, project, headline string
		var issueKey, slug *string
		var author []byte
		if err := rows.Scan(&candidate.Kind, &id, &issueKey, &project, &slug, &candidate.At, &author,
			&candidate.SharedTerms, &candidate.Score, &headline); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(author, &candidate.Author); err != nil {
			return nil, fmt.Errorf("decode prior answer author: %w", err)
		}
		if candidate.Kind == "issue" {
			candidate.Ref = "dispatch://" + id
		} else {
			docSlug := ""
			if slug != nil {
				docSlug = *slug
			}
			candidate.Ref = refs.ItemRef(candidate.Kind, issueKey, project, docSlug, id)
		}
		candidate.Snippet = markSnippet(headline)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// priorAnswerText is what a question is matched on: its text and its option labels.
func priorAnswerText(question string, options []model.AskOption) string {
	parts := []string{question}
	for _, option := range options {
		parts = append(parts, option.Label)
	}
	return strings.Join(parts, "\n")
}

// priorAnswerRefusal is the 409 message: the best candidate, and the two ways forward.
func priorAnswerRefusal(candidates []model.PriorAnswerCandidate) string {
	best := candidates[0]
	return fmt.Sprintf(
		"a human may already have answered this: %s (%s by %s, %s). Read the candidates; cite the answer instead of asking, or ask again with force: true if none of them answers this question",
		best.Ref, best.Kind, best.Author.ID, best.At.UTC().Format("2006-01-02 15:04Z"),
	)
}
