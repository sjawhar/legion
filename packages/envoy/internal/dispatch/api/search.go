package api

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	// searchFusionK is reciprocal rank fusion's constant: a row at position p of its kind's list
	// scores 1/(searchFusionK+p). 60 is the value of the paper that introduced the method (Cormack,
	// Clarke and Buettcher, 2009) and Elasticsearch's and OpenSearch's default; nothing tunes it.
	searchFusionK = 60
	// searchEmbedTimeout bounds the one Bedrock call search makes (the query's own embedding).
	// Past it, or any other embedder failure, search answers keyword-only and says so (LEGION-549)
	// rather than making every search wait indefinitely on a degraded embedder.
	searchEmbedTimeout = 3 * time.Second
	// searchMeaningFloor is the lowest cosine similarity a meaning-leg row may score to reach legs
	// at all: below it, a row is noise, not a weak match, and letting it through would rank it by
	// position exactly as the strongest match in an empty kind, worth 1/(searchFusionK+1) - as
	// much as a real hit, zero floor (before this) gives nothing. Chosen from embed-v4 pairs on
	// this corpus: true paraphrase matches scored 0.45-0.50, same-domain-different-issue
	// near-misses 0.34-0.40, and unrelated content topped out at 0.18 (one query, eight unrelated
	// documents) - 0.25 sits with a wide margin on both sides of that gap. It is not re-derived
	// per request or per corpus; a corpus whose true matches cluster lower would need a new floor
	// chosen the same way.
	searchMeaningFloor = 0.25
	markStart          = "\uE000" // private-use sentinels: ts_headline writes them, markSnippet turns them into <mark>
	markEnd            = "\uE001"
	headlineOptions    = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=24, MinWords=12, MaxFragments=1"
)

// searchQuery ranks each kind of content on its own list, then merges the lists by reciprocal
// rank fusion, so a page takes each kind's best in turn: its top holds the best issue, document,
// ask, comment and message. kinds unions the five kinds' matches with no per-kind limit or order
// of its own; one window over it, partitioned by kind, orders each kind's matches by ts_rank_cd,
// then recency, then id, and numbers them (pos), and the same partition's count(*), taken before
// any cut, is that kind's every match (matches). Filtering to pos <= $7 (contracts.SearchKindDepth)
// then keeps each kind's best $7 as legs, the one place the kind's total order is written. An
// issue whose key is the whole query heads the issue list, since ts_rank_cd scores its own key no
// higher than another issue's title citing it. Rows that score alike (every kind's first row
// scores 1/61) are ordered issue, document, ask, comment, message (the thing before what is
// inside it), then recency, then id, which makes the order total, so consecutive offsets cover
// the reachable rows once while the corpus holds still. legs and fused carry no row's text, so
// the union, the window and the fused score cost nothing per kind's full body; only the page's
// own rows pay for one lateral fetch of their text and, after that, a snippet. totals is joined
// outside the page, so a page past the end still answers how many rows the query matches. This is
// the keyword-only fallback (LEGION-549 degraded mode) and the query runFusedSearch runs for
// LEGION-550's write-time suggestions, which never calls the embedder at all.
//
// Parameters: $1 q, $2 project (empty for every project), $3 limit, $4 firstTerm,
// $5 headlineOptions, $6 offset, $7 contracts.SearchKindDepth, $8 searchFusionK.
const searchQuery = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, upper(btrim($1)) as own_key),
kinds as (
  select 'issue' as kind, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = q.own_key as own, ts_rank_cd(i.search, q.tsq) as r
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document' as kind, a.issue_key, case when a.issue_key is null then a.id else null::uuid end as owner_artifact_id, a.id as artifact_id, a.id::text as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         false as own, ts_rank_cd(v.search, q.tsq) as r
    from artifacts a
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.search, v.created_at from artifact_versions v
                  where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
  union all
  select 'comment' as kind, c.issue_key, c.artifact_id as owner_artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid) as artifact_id, c.id::text as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         false as own, ts_rank_cd(c.search, q.tsq) as r
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'ask' as kind, k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end as owner_artifact_id,
         coalesce(k.artifact_id, k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) as artifact_id, k.id::text as id, k.block_id,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         false as own, ts_rank_cd(k.search, q.tsq) as r
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'message' as kind, m.issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, m.id::text as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         false as own, ts_rank_cd(m.search, q.tsq) as r
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
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
page as (
  select l.*, f.score, array_position(array['issue', 'document', 'ask', 'comment', 'message'], l.kind) as kind_order
    from legs l join fused f using (kind, id)
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
  -- Mirrors the kind arms in kinds above (issue/document/comment/ask/message -> table and
  -- text column); the two must stay in sync. issue reuses r.issue_title, already carried from
  -- the same issues row by kinds, rather than re-reading it.
  left join lateral (
    select case r.kind
      when 'issue' then r.issue_title
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1) -- re-resolves the latest version kinds already found once; accepted, bounded by the page size
      when 'comment' then (select c.body from comments c where c.id = r.id::uuid)
      when 'ask' then (select k.question || ' ' || coalesce(k.options::text, '') || ' ' || coalesce(k.answer->>'text', '') from asks k where k.id = r.id::uuid)
      when 'message' then (select m.body from messages m where m.id = r.id::uuid)
    end as text
  ) txt on true
 order by r.score desc, r.kind_order, r.updated_at desc, r.id
`

// searchQueryMeaning extends searchQuery with a second list per kind, ranked by cosine distance
// to the query's own embedding ($9, a pgvector literal) against embeddings.embedding
// (0071_embeddings_core.up.sql), rather than ts_rank_cd. Each kind's meaning candidates are their
// own CTE (meaning_issue_candidates and so on) doing plainly `order by embedding <=> $9 limit $7`
// - pgvector's HNSW index serves that shape directly (confirmed by
// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan) - rather than joining meaning rows into
// one shared window spanning every kind, which would force a sequential scan of embeddings once
// per leg. Each candidate set is then floor-filtered (searchMeaningFloor) before kinds ever sees
// it: a candidate below the floor is noise, not a weak match, and reaching legs at all would rank
// it by position exactly as the strongest real match in an empty kind.
//
// Every leg now carries which list it belongs to (list), and ranked's window partitions by
// (kind, list): each list keeps its own top $7 and its own position numbering, so a row that
// matches both lists for the same (kind, id) occupies two rows of legs - one per list - each
// contributing its own 1/($8+pos) term. fused sums those terms grouped by (kind, id), which is
// reciprocal rank fusion of the two lists exactly as it fuses the five kinds: an item only the
// keyword list reached, only the meaning list reached, or both, is ranked once, at the sum of
// whichever lists found it. legs_unique picks one representative row's owner/text-location
// columns per (kind, id) - both lists' rows carry the same ones, read from the same underlying
// issue/document/comment/ask/message - before the page is cut and joined back to fused's score.
// total is every distinct (kind, id) either list matched, not a sum of list sizes, so an id both
// lists reach is counted once.
//
// Parameters: $1 q, $2 project, $3 limit, $4 firstTerm, $5 headlineOptions, $6 offset,
// $7 contracts.SearchKindDepth, $8 searchFusionK, $9 the query's embedding (a vector literal),
// $10 searchMeaningFloor.
const searchQueryMeaning = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, upper(btrim($1)) as own_key, $9::vector as qvec),
meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> q.qvec) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> q.qvec
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
         1 - (e.embedding <=> q.qvec) as r
    from embeddings e
    join artifacts a on a.id::text = e.id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document' and a.kind = 'doc' and e.embedding is not null and ($2 = '' or p.key = $2)
   order by e.embedding <=> q.qvec
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
         1 - (e.embedding <=> q.qvec) as r
    from embeddings e
    join comments c on c.id::text = e.id
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment' and e.embedding is not null and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> q.qvec
   limit $7
),
meaning_comment as (
  select 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, k.block_id, i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> q.qvec) as r
    from embeddings e
    join asks k on k.id::text = e.id
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask' and e.embedding is not null and ($2 = '' or coalesce(i.project_key, p.key) = $2)
   order by e.embedding <=> q.qvec
   limit $7
),
meaning_ask as (
  select 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, (anchor->>'artifact_id')::uuid), id::text, block_id,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
),
meaning_message_candidates as (
  select m.issue_key, m.id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> q.qvec) as r
    from embeddings e
    join messages m on m.id::text = e.id
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message' and e.embedding is not null and ($2 = '' or i.project_key = $2)
   order by e.embedding <=> q.qvec
   limit $7
),
meaning_message as (
  select 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
),
kinds as (
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = q.own_key as own, ts_rank_cd(i.search, q.tsq) as r
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ts_rank_cd(v.search, q.tsq)
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
         false, ts_rank_cd(c.search, q.tsq)
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, (k.anchor->>'artifact_id')::uuid), k.id::text, k.block_id,
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ts_rank_cd(k.search, q.tsq)
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ts_rank_cd(m.search, q.tsq)
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
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
page as (
  select l.*, f.score, array_position(array['issue', 'document', 'ask', 'comment', 'message'], l.kind) as kind_order
    from legs_unique l join fused f using (kind, id)
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
  -- Mirrors the kind arms above (issue/document/comment/ask/message -> table and text column),
  -- exactly as searchQuery's own tail does; the two must stay in sync.
  left join lateral (
    select case r.kind
      when 'issue' then r.issue_title
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = r.artifact_id order by v.number desc limit 1)
      when 'comment' then (select c.body from comments c where c.id = r.id::uuid)
      when 'ask' then (select k.question || ' ' || coalesce(k.options::text, '') || ' ' || coalesce(k.answer->>'text', '') from asks k where k.id = r.id::uuid)
      when 'message' then (select m.body from messages m where m.id = r.id::uuid)
    end as text
  ) txt on true
 order by r.score desc, r.kind_order, r.updated_at desc, r.id
`

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}

	query := r.URL.Query()
	searchText := strings.TrimSpace(query.Get("q"))
	if utf8.RuneCountInString(searchText) < 2 {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "q must be at least 2 characters")
		return
	}
	// q is capped as dispatch_search caps it, counted after trimming so a query the tool sends is
	// never refused here, and a project must be a key (an empty one searches every project). Both
	// ride in the URL; packages/contracts/AGENTS.md "Search limits" says what keeps it short.
	if length := len16(searchText); length > contracts.SearchQueryMax {
		tooLong := capExceededError("q", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; "+contracts.SearchQueryHint)
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "project must be a project key such as CORE")
		return
	}

	limit := searchDefaultLimit
	if parsed, present, valid := parseQueryInt(query, "limit", 1, searchMaxLimit); present {
		if !valid {
			writeError(w, "INVALID_LIMIT", http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = parsed
	}
	offset := 0
	if parsed, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			writeError(w, "INVALID_OFFSET", http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}

	response, err := s.runSearch(r.Context(), searchText, project, limit, offset)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}

// runSearch is the search handler's own entry point (unlike runFusedSearch, LEGION-550's
// write-time suggestions never call this - suggestions never try meaning search, so they never
// pay for an embedder call or its timeout). The numnode check runs here, before anything else -
// including before the one Bedrock call this function ever makes - so a stopword-only or
// operator-only query (the same shape runFusedSearch's and runMeaningSearch's own checks catch)
// never pays for an embedding it would throw away; runFusedSearch repeats the same check for its
// other caller, LEGION-550's write-time suggestions, which never reaches this function at all.
// It tries meaning search first when an embedder is configured, falling back to keyword-only
// (runFusedSearch) only when the query's own embedding could not be had - never when the meaning
// query itself then fails, which is a real error the caller should see as a 500, not something to
// paper over as a degraded answer.
func (s *server) runSearch(ctx context.Context, q, project string, limit, offset int) (model.SearchResponse, error) {
	var nodes int
	if err := s.deps.Store.Pool.QueryRow(ctx, "select numnode(websearch_to_tsquery('english', $1))", q).Scan(&nodes); err != nil {
		return model.SearchResponse{}, err
	}
	if nodes == 0 {
		return model.SearchResponse{Results: []model.SearchResult{}, Limit: limit, Offset: offset}, nil
	}
	if s.deps.Embedder == nil {
		return s.runKeywordOnlyDegraded(ctx, q, project, limit, offset)
	}
	vector, err := s.embedQuery(ctx, q)
	if err != nil {
		slog.Warn("dispatch search: query embedding unavailable, answering keyword-only", "error", err)
		return s.runKeywordOnlyDegraded(ctx, q, project, limit, offset)
	}
	return s.runMeaningSearch(ctx, q, project, limit, offset, vector)
}

// runKeywordOnlyDegraded runs the keyword-only fallback and stamps the response Degraded on
// success: the one thing both of runSearch's fallback branches (no embedder configured, or the
// query's own embedding failing) do identically.
func (s *server) runKeywordOnlyDegraded(ctx context.Context, q, project string, limit, offset int) (model.SearchResponse, error) {
	response, err := s.runFusedSearch(ctx, q, project, limit, offset)
	if err == nil {
		response.Degraded = contracts.SearchDegradedEmbedderUnavailable
	}
	return response, err
}

// runFusedSearch is the search handler's query, SQL and row-scanning shared with LEGION-550's
// write-time suggestions (suggestions.go): both read the same fused ranking
// (sjawhar/legion#1764), the handler bound by its request context and the suggestions call bound
// by writeSuggestionTimeout instead. q is assumed already validated (length, non-empty after
// trimming); project empty searches every project.
func (s *server) runFusedSearch(ctx context.Context, q, project string, limit, offset int) (model.SearchResponse, error) {
	var nodes int
	if err := s.deps.Store.Pool.QueryRow(ctx, "select numnode(websearch_to_tsquery('english', $1))", q).Scan(&nodes); err != nil {
		return model.SearchResponse{}, err
	}
	if nodes == 0 {
		return model.SearchResponse{Results: []model.SearchResult{}, Limit: limit, Offset: offset}, nil
	}

	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(ctx, searchQuery, q, project, limit, firstTerm(q), headlineOptions,
		offset, contracts.SearchKindDepth, searchFusionK)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, q, limit, offset)
	if err != nil {
		return model.SearchResponse{}, err
	}
	response.TookMS = time.Since(started).Milliseconds()
	return response, nil
}

// runMeaningSearch is runFusedSearch's counterpart once a query embedding exists: same row
// shape, searchQueryMeaning in place of searchQuery. Its only caller, runSearch, has already run
// the numnode check before ever reaching here (so it never pays for an embedding it would throw
// away); this function assumes nodes > 0, unlike runFusedSearch, which repeats that check for
// its other caller, LEGION-550's write-time suggestions.
func (s *server) runMeaningSearch(ctx context.Context, q, project string, limit, offset int, vector []float32) (model.SearchResponse, error) {

	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(ctx, searchQueryMeaning, q, project, limit, firstTerm(q), headlineOptions,
		offset, contracts.SearchKindDepth, searchFusionK, embed.Literal(vector), searchMeaningFloor)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, q, limit, offset)
	if err != nil {
		return model.SearchResponse{}, err
	}
	response.TookMS = time.Since(started).Milliseconds()
	return response, nil
}

// scanSearchRows reads searchQuery's and searchQueryMeaning's shared column shape - every column
// through the headline - into a SearchResponse. q is the original query text, for each result's
// href (searchHref's own query-string echo).
func scanSearchRows(rows pgx.Rows, q string, limit, offset int) (model.SearchResponse, error) {
	response := model.SearchResponse{Results: []model.SearchResult{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var result model.SearchResult
		var kind, ownerKind, id, headline *string
		var ownerKey, ownerTitle, ownerStatus, ownerProject, ownerSlug, ownerArtifactID, ownerName, slug, name *string
		var primary bool
		var blockID *string
		var rank *float64
		if err := rows.Scan(
			&response.Total,
			&response.Reachable,
			&kind,
			&ownerKind,
			&ownerKey,
			&ownerTitle,
			&ownerStatus,
			&ownerProject,
			&ownerSlug,
			&ownerArtifactID,
			&ownerName,
			&slug,
			&name,
			&primary,
			&id,
			&blockID,
			&rank,
			&headline,
		); err != nil {
			return model.SearchResponse{}, err
		}
		// A page past the last reachable row is one row of totals and no hit.
		if kind == nil {
			continue
		}
		result.Kind, result.ID, result.Rank = *kind, *id, *rank
		if *ownerKind == "issue" {
			result.Owner = model.SearchOwner{Kind: *ownerKind, Key: *ownerKey, Title: *ownerTitle, Status: *ownerStatus}
			result.Issue = &model.SearchIssue{Key: *ownerKey, Title: *ownerTitle, Status: *ownerStatus}
		} else {
			result.Owner = model.SearchOwner{
				Kind:       *ownerKind,
				Project:    *ownerProject,
				Slug:       *ownerSlug,
				ArtifactID: *ownerArtifactID,
				Name:       *ownerName,
			}
		}
		if slug != nil {
			result.Artifact = &model.SearchArtifact{Slug: *slug, Name: *name}
		}
		result.Snippet = markSnippet(*headline)
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, q)
		response.Results = append(response.Results, result)
	}
	if err := rows.Err(); err != nil {
		return model.SearchResponse{}, err
	}
	return response, nil
}

// embedQuery embeds q for meaning search (embed.InputQuery - Cohere's embed-v4 asymmetric mode
// embeds a query differently from a stored document), bounded by searchEmbedTimeout so one
// degraded request never holds the whole search handler open on a slow or wedged Bedrock call.
func (s *server) embedQuery(ctx context.Context, q string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, searchEmbedTimeout)
	defer cancel()
	vectors, err := s.deps.Embedder.Embed(ctx, []string{q}, embed.InputQuery)
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 {
		// Not a timeout: the Embedder interface promises one vector per input text, and this is
		// a contract violation by whatever implementation answered, never something an actual
		// deadline produces. runSearch logs this exactly like a real timeout and falls back to
		// keyword-only, which is still the right behavior for callers - the distinct message
		// matters only to whoever reads the log.
		return nil, fmt.Errorf("embed: embedder returned %d vectors for 1 query text", len(vectors))
	}
	return vectors[0], nil
}

func firstTerm(query string) string {
	for _, token := range strings.Fields(query) {
		if strings.HasPrefix(token, "-") || strings.EqualFold(token, "or") {
			continue
		}
		if token = strings.Trim(token, `"`); token != "" {
			return token
		}
	}
	return ""
}

func markSnippet(headline string) string {
	return strings.NewReplacer(markStart, "<mark>", markEnd, "</mark>").Replace(html.EscapeString(headline))
}

// issueDocumentHref is the SPA route for one of an issue's documents: the primary document is
// the issue's Spec tab, every other artifact its own Artifacts route.
func issueDocumentHref(issueHref string, artifact *model.SearchArtifact, primary bool) string {
	if primary {
		return issueHref + "/spec"
	}
	return issueHref + "/artifacts/" + url.PathEscape(artifact.Slug)
}

// searchHref is where a hit's reader should land. A comment or ask that carries a document —
// the document its anchor quotes, or the one holding its typed block — belongs beside that
// document, so its href opens the document itself and names the item in the query (or the
// block in the fragment), exactly as the SPA's own thread links do. An item with no document
// is a Conversation turn, named by its own route so the turn is focused.
func searchHref(kind string, owner model.SearchOwner, artifact *model.SearchArtifact, primary bool, id string, blockID *string, query string) string {
	issueHref := "/issues/" + owner.Key
	// The document a hit belongs beside: a project document's own route, or the issue document
	// its anchor names. An issue item with no document has none, and keeps its own route.
	documentHref := ""
	if owner.Kind == "document" {
		documentHref = "/projects/" + url.PathEscape(owner.Project) + "/documents/" + url.PathEscape(owner.Slug)
	} else if artifact != nil {
		documentHref = issueDocumentHref(issueHref, artifact, primary)
	}

	switch kind {
	case "issue":
		return issueHref
	case "document":
		if owner.Kind == "document" {
			return documentHref + "?q=" + url.QueryEscape(query)
		}
		// A document hit always has its artifact; reading it here says so.
		return issueDocumentHref(issueHref, artifact, primary) + "?q=" + url.QueryEscape(query)
	case "comment":
		if documentHref == "" {
			return issueHref + "/comments/" + id
		}
		return documentHref + "?comment=" + url.QueryEscape(id)
	case "ask":
		if documentHref == "" {
			return issueHref + "/asks/" + id
		}
		if blockID != nil {
			return documentHref + "#b-" + url.PathEscape(*blockID)
		}
		return documentHref + "?ask=" + url.QueryEscape(id)
	case "message":
		return issueHref + "/messages/" + id
	default:
		return ""
	}
}
