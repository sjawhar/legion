package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

// Every title is read through search_vector (migration 0068), as the issues trigger reads it, so a
// title whose whole vector would pass Postgres's limit on one tsvector, the new title or a stored
// one, is compared by the words that open it rather than failing the creation.
//
// A candidate's headline marks the words its title shares with the new one, so its query is those
// words, at most duplicateHeadlineWords of them: the words the candidate's title holds of the new
// title's are exactly the shared ones, and the query is one OR node per word, which Postgres reads
// recursively. A query of 20,000 such words overflows the default 2 MB max_stack_depth (SQLSTATE
// 54001) where 10,000 does not, on Postgres 16.15, and a title of distinct words shares that many.
const duplicateQuery = `
with parent as (select coalesce((select title from issues where key = $3), '') as title),
new_title as (
  select array(select unnest(tsvector_to_array(search_vector('', search_text($2))))
               except select unnest(tsvector_to_array(search_vector('', search_text(p.title))))) as lex
    from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array(select unnest(tsvector_to_array(search_vector('', search_text(i.title))))
               except select unnest(tsvector_to_array(search_vector('', search_text(p.title))))) as lex
    from issues i, parent p
   where i.project_key = $1 and i.key <> $3),
scored as (
  select c.key, c.title, c.status, c.updated_at,
         array(select unnest(c.lex) intersect select unnest(n.lex)) as shared_lex,
         least(cardinality(c.lex), cardinality(n.lex)) as shorter
    from cand c, new_title n
   where c.lex && n.lex)
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
