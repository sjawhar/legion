package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

// duplicateQuery finds the issues of project $1, other than the parent $3, whose title
// near-duplicates the new title $2, comparing their lexemes: the lexemes of the search_vector of a
// title's search_text, with an empty head. The new title's are built here, and every stored title's
// are read from issues.title_lexemes, which the issues trigger writes with that expression
// (migration 0069), so a creation parses one title, not every title in the project. search_vector
// (0068) bounds a vector to what Postgres holds in one tsvector, so a title past that limit, the new
// one or a stored one, is compared by the words that open it rather than failing the creation.
//
// The parent's lexemes count on neither side. terms holds the new title's lexemes less the
// parent's, marked new, and the parent's, marked not, each once, so one join of every stored lexeme
// in the project against it counts both what a title shares with the new one and how many of its
// own lexemes are the parent's.
//
// A candidate's headline marks the words its title shares with the new one, so its query is those
// words, at most duplicateHeadlineWords of them: the words the candidate's title holds of the new
// title's are exactly the shared ones, and the query is one OR node per word, which Postgres reads
// recursively. A query of 20,000 such words overflows the default 2 MB max_stack_depth (SQLSTATE
// 54001) where 10,000 does not, on Postgres 16.15, and a title of distinct words shares that many.
const duplicateQuery = `
with parent as (select coalesce((select title_lexemes from issues where key = $3), '{}') as lexemes),
new_title as (
  select array(select unnest(tsvector_to_array(search_vector('', search_text($2))))
               except select unnest(p.lexemes)) as lexemes
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
         least(c.own, cardinality(n.lexemes)) as shorter
    from cand c, new_title n)
select key, title, status, cardinality(shared_lex) as shared,
       ts_headline('english', search_text(title),
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(shared_lex[1:$5]) x)), $4) as headline
  from scored
 where (cardinality(shared_lex) >= 3 and 2 * cardinality(shared_lex) >= shorter)
    or (cardinality(shared_lex) >= 1 and cardinality(shared_lex) = shorter)
 order by shared desc, updated_at desc
 limit 5
`

// duplicateHeadlineWords bounds a candidate's headline query (duplicateQuery), a tenth of the
// 10,000 words Postgres's default stack reads.
const duplicateHeadlineWords = 1000

// duplicateCandidates returns issues in project whose title near-duplicates title,
// excluding parentKey and ignoring the parent title's terms. parentKey may be "".
func (s *server) duplicateCandidates(ctx context.Context, q queryer, project, title, parentKey string) ([]model.DuplicateCandidate, error) {
	rows, err := q.Query(ctx, duplicateQuery, project, title, parentKey, duplicateHeadlineOptions, duplicateHeadlineWords)
	if err != nil {
		return nil, err
	}
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
