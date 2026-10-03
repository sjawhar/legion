package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

// duplicateQuery finds the issues of project $1, other than the parent $3, whose title
// near-duplicates the new title $2, comparing their title_lexemes (migration 0069): the new title's
// are built here, and every stored title's are read from issues.title_lexemes, which the issues
// trigger writes with the same function, so a creation parses one title, not every title in the
// project. A stored title past Postgres's limit on one tsvector is compared by the words that open
// it (search_vector, 0068); a new one cannot pass it, being capped at contracts.IssueTitleMax.
//
// The parent's lexemes count on neither side. terms holds the new title's lexemes less the
// parent's, marked new, and the parent's, marked not, each once, so one join of every stored lexeme
// in the project against it counts both what a title shares with the new one and how many of its
// own lexemes are the parent's.
//
// A candidate's headline marks the words its title shares with the new one: its query is the shared
// lexemes, not the new title's. to_tsquery reads a hyphenated lexeme as a phrase of its parts as
// well (`legion-resolv` is 'legion-resolv' <-> 'legion' <-> 'resolv'), and ts_headline marks a word
// matching any of them, so a query of the new title's lexemes would mark a part the parent's lexemes
// removed.
const duplicateQuery = `
with parent as (select coalesce((select title_lexemes from issues where key = $3), '{}') as lexemes),
new_title as (
  select array(select unnest(title_lexemes($2)) except select unnest(p.lexemes)) as lexemes
    from parent p),
terms as (
  select unnest(n.lexemes) as lexeme, true as new from new_title n
  union all
  select unnest(p.lexemes), false from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array_agg(t.lexeme) filter (where t.new) as shared_lex,
         cardinality(i.title_lexemes) - count(*) filter (where not t.new) as own
    from issues i
    cross join unnest(i.title_lexemes) as l(lexeme)
    join terms t on t.lexeme = l.lexeme
   where i.project_key = $1 and i.key <> $3
   group by i.key
  having bool_or(t.new)),
scored as (
  select c.key, c.title, c.status, c.updated_at, c.shared_lex,
         cardinality(c.shared_lex) as shared,
         least(c.own, cardinality(n.lexemes)) as shorter
    from cand c, new_title n)
select key, title, status, shared,
       ts_headline('english', search_text(title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(shared_lex) x)), $4) as headline
  from scored
 where (shared >= 3 and 2 * shared >= shorter) or (shared >= 1 and shared = shorter)
 order by shared desc, updated_at desc
 limit 5
`

// duplicateCandidates returns issues in project whose title near-duplicates title,
// excluding parentKey and ignoring the parent title's terms. parentKey may be "".
func (s *server) duplicateCandidates(ctx context.Context, q queryer, project, title, parentKey string) ([]model.DuplicateCandidate, error) {
	rows, err := q.Query(ctx, duplicateQuery, project, title, parentKey, duplicateHeadlineOptions)
	if err != nil {
		return nil, err
	}
	return scanDuplicateCandidates(rows)
}

// scanDuplicateCandidates reads the duplicate check's rows, (key, title, status, shared, headline),
// and closes them.
func scanDuplicateCandidates(rows pgx.Rows) ([]model.DuplicateCandidate, error) {
	defer rows.Close()
	candidates := []model.DuplicateCandidate{}
	for rows.Next() {
		var candidate model.DuplicateCandidate
		var headline string
		if err := rows.Scan(&candidate.Key, &candidate.Title, &candidate.Status, &candidate.SharedTerms, &headline); err != nil {
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
