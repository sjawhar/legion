package api

import "fmt"

// This file holds the five statements of R1 as main writes them, copied from the text this change
// replaces: their document arm reads each document's latest version through a lateral join over
// artifact_versions and tests `v.search @@ q.tsq` there. They are the oracle of the equality
// tests in document_search_test.go, which run each statement beside the package's own and
// compare the rows.
//
// Every piece is a literal copy, including the pieces this change does not touch
// (searchPageAndTailSQL, the meaning CTEs, relatedCoordinationRank) and the two expressions the
// package builds in Go (store.CastableUUID's cast and askBlockNodeSQL), so that nothing of the
// oracle is built from the package's own helpers: an oracle sharing a helper with the statement
// under test changes with it, and the comparison then holds nothing. The cost is that an edit to
// a part of these statements this change leaves alone has to be copied here too, which is what
// the equality tests report when it is not.

// mainSearchKeywordLegs is main's searchKeywordLegs: the five-kind keyword union whose document
// arm this change moves to document_search.
func mainSearchKeywordLegs(rank func(searchColumn string) string) string {
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = q.own_key as own, ` + rank("i.search") + ` as r
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + rank("v.search") + `
    from artifacts a
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.search, v.created_at from artifact_versions v
                  where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
  union all
  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, ` + rank("c.search") + `
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, (case when pg_input_is_valid(k.anchor->>'artifact_id', 'uuid') then (k.anchor->>'artifact_id')::uuid end)), k.id::text, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end,
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ` + rank("k.search") + `
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ` + rank("m.search") + `
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)`
}

// mainSearchKeywordLegsSQL is main's searchKeywordLegsSQL, those legs ranked by ts_rank_cd.
var mainSearchKeywordLegsSQL = mainSearchKeywordLegs(func(searchColumn string) string {
	return "ts_rank_cd(" + searchColumn + ", q.tsq)"
})

// mainSearchPageAndTailSQL is main's searchPageAndTailSQL, which this change leaves as it is: its
// own lateral over artifact_versions reads the page's documents' markdown for the snippet, and
// tests no tsquery.
func mainSearchPageAndTailSQL(legsSource string) string {
	return fmt.Sprintf(`page as (
  select l.*, f.score, array_position(array['issue', 'document', 'ask', 'comment', 'message'], l.kind) as kind_order
    from %s l join fused f using (kind, id)
   order by f.score desc, kind_order, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, r.score::float8,
       ts_headline('english',
         search_text(case when q.term <> '' and strpos(lower(txt.text), lower(q.term)) > 0
              then substr(txt.text, greatest(1, strpos(lower(txt.text), lower(q.term)) - 1500), 4000)
              else left(txt.text, 4000) end),
         q.tsq, $5) as headline
  from totals t cross join q
  left join (page r left join artifacts ar on ar.id = r.artifact_id) on true
  -- Mirrors the kind arms in kinds above (issue/document/comment/ask/message -> table and text
  -- column); the two must stay in sync. issue reuses r.issue_title, already carried from the
  -- same issues row by kinds, rather than re-reading it. ask reads the text its search vector is
  -- built from (asks_search: the question, then ask_search_suffix's option labels and
  -- descriptions, answer text and selected labels), so its snippet shows what the query matched.
  left join lateral (
    select case r.kind
      when 'issue' then r.issue_title
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1) -- re-resolves the latest version kinds already found once; accepted, bounded by the page size
      when 'comment' then (select c.body from comments c where c.id = r.id::uuid)
      when 'ask' then (select k.question || ' ' || ask_search_suffix(k.options, k.answer) from asks k where k.id = r.id::uuid)
      when 'message' then (select m.body from messages m where m.id = r.id::uuid)
    end as text
  ) txt on true
 order by r.score desc, r.kind_order, r.updated_at desc, r.id
`, legsSource)
}

// mainSearchQOpener is main's searchQOpener.
const mainSearchQOpener = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, upper(btrim($1)) as own_key),
`

// mainSearchQueryBody is main's searchQueryBody.
func mainSearchQueryBody(keywordLegs string) string {
	return `kinds as (` + keywordLegs + `
),
ranked as (
  select *, row_number() over (partition by kind order by own desc, r desc, updated_at desc, id) as pos,
         count(*) over (partition by kind) as matches
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (select kind, id, 1.0 / ($8 + pos) as score from legs), -- (kind, id) is unique per row today (each kind's own PK or key); a list that stops being unique here would silently multiply rows instead of summing their score.
totals as (
  select (select coalesce(sum(matches), 0) from (select max(matches) as matches from legs group by kind) each_kind) as total,
         (select count(*) from fused) as reachable
),
` + mainSearchPageAndTailSQL("legs")
}

// mainSearchQueryMeaningBody is main's searchQueryMeaningBody.
func mainSearchQueryMeaningBody(keywordLegs string) string {
	return `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_issue as (
  select 'issue' as kind, 'meaning' as list, issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, id, null::text as block_id,
         issue_title, issue_status, null::text as owner_project, null::text as owner_slug, null::text as owner_name, updated_at,
         false as own, r
    from meaning_issue_candidates where r >= $10
),
meaning_document_candidates as (
  select a.issue_key, a.id, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id::text = e.id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
   where e.kind = 'document' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_document as (
  select 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id::text = e.id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key
   where e.kind = 'comment' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_comment as (
  select 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end as block_id, i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id::text = e.id
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key
   where e.kind = 'ask' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_ask as (
  select 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (case when pg_input_is_valid(anchor->>'artifact_id', 'uuid') then (anchor->>'artifact_id')::uuid end)), id::text, block_id,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
),
meaning_message_candidates as (
  select m.issue_key, m.id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id::text = e.id
    join issues i on i.key = m.issue_key
   where e.kind = 'message' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_message as (
  select 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
),
kinds as (` + keywordLegs + `
  union all select * from meaning_issue
  union all select * from meaning_document
  union all select * from meaning_comment
  union all select * from meaning_ask
  union all select * from meaning_message
),
ranked as (
  select *, row_number() over (partition by kind, list order by own desc, r desc, updated_at desc, id) as pos
    from kinds
),
legs as (
  select * from ranked where pos <= $7
),
fused as (select kind, id, sum(1.0 / ($8 + pos)) as score from legs group by kind, id),
legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
` + mainSearchPageAndTailSQL("legs_unique")
}

// mainRelatedQOpener is main's relatedQOpener.
const mainRelatedQOpener = `
with q as (select $1::tsquery as tsq, $4::text as term, ''::text as own_key),
`

// mainRelatedTermWeights is main's relatedTermWeights.
func mainRelatedTermWeights(lexemes int) string {
	return fmt.Sprintf(`term_weights as (
  select quote_literal(w.lexeme)::tsquery as tq, w.idf from unnest($%d::text[], $%d::float8[]) w(lexeme, idf)
),
`, lexemes, lexemes+1)
}

// mainRelatedCoordinationRank is main's relatedCoordinationRank.
func mainRelatedCoordinationRank(searchColumn string) string {
	return "((select sum(w.idf) from term_weights w where " + searchColumn + " @@ w.tq) + 0.001 * ts_rank_cd(" + searchColumn + ", q.tsq, 32))"
}

// The four whole statements of R1 that search's own two bodies make, as main makes them.
var (
	mainSearchQuery         = mainSearchQOpener + mainSearchQueryBody(mainSearchKeywordLegsSQL)
	mainSearchQueryMeaning  = mainSearchQOpener + mainSearchQueryMeaningBody(mainSearchKeywordLegsSQL)
	mainRelatedKeywordLegs  = mainSearchKeywordLegs(mainRelatedCoordinationRank)
	mainRelatedKeywordQuery = mainRelatedQOpener + mainRelatedTermWeights(9) + mainSearchQueryBody(mainRelatedKeywordLegs)
	mainRelatedMeaningQuery = mainRelatedQOpener + mainRelatedTermWeights(12) + mainSearchQueryMeaningBody(mainRelatedKeywordLegs)
)

// mainRelatedProbeSQL is main's relatedProbeSQL, the fifth statement of R1.
const mainRelatedProbeSQL = `
with probe as (
  select l.lexeme, quote_literal(l.lexeme)::tsquery as tq from unnest($2::text[]) l(lexeme)
),
any_probe as (
  select string_agg(quote_literal(lexeme), ' | ')::tsquery as tq from probe
),
document_hits as (
  select distinct u.lexeme
    from artifacts a
    join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
    cross join any_probe
    cross join lateral unnest(v.search) u
   where a.kind = 'doc' and a.project_key = $1
     and a.issue_key is distinct from $3 and a.id is distinct from $4::uuid
     and v.search @@ any_probe.tq and u.lexeme = any($2::text[])
)
select p.lexeme from probe p
 where exists (select 1 from issues i where i.search @@ p.tq and i.project_key = $1 and i.key <> $3)
    or exists (select 1 from document_hits d where d.lexeme = p.lexeme)
    or exists (select 1 from comments c
                 left join issues i on i.key = c.issue_key
                 left join artifacts a on a.id = c.artifact_id
                where c.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and c.issue_key is distinct from $3 and ($4::uuid is null or c.artifact_id is distinct from $4::uuid))
    or exists (select 1 from asks k
                 left join issues i on i.key = k.issue_key
                 left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                where k.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and k.issue_key is distinct from $3
                  and ($4::uuid is null or coalesce(k.artifact_id, k.block_artifact_id) is distinct from $4::uuid))
    or exists (select 1 from messages m join issues i on i.key = m.issue_key
                where m.search @@ p.tq and i.project_key = $1 and m.issue_key <> $3)`
