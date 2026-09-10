package api

import (
	"context"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const duplicateHeadlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", HighlightAll=true"

const duplicateQuery = `
with parent as (select coalesce((select title from issues where key = $3), '') as title),
new_title as (
  select array(select unnest(tsvector_to_array(to_tsvector('english', $2)))
               except select unnest(tsvector_to_array(to_tsvector('english', p.title)))) as lex
    from parent p),
cand as (
  select i.key, i.title, i.status, i.updated_at,
         array(select unnest(tsvector_to_array(to_tsvector('english', i.title)))
               except select unnest(tsvector_to_array(to_tsvector('english', p.title)))) as lex
    from issues i, parent p
   where i.project_key = $1 and i.key <> $3),
scored as (
  select c.key, c.title, c.status, c.updated_at, n.lex as new_lex,
         (select count(*) from (select unnest(c.lex) intersect select unnest(n.lex)) s)::int as shared,
         least(cardinality(c.lex), cardinality(n.lex)) as shorter
    from cand c, new_title n
   where c.lex && n.lex)
select key, title, status, shared,
       ts_headline('english', title,
         to_tsquery('simple', (select string_agg(quote_literal(x), ' | ') from unnest(new_lex) x)), $4) as headline
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
