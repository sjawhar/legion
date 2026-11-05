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
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	// searchFusionK is the fusion constant: a row's legs contribute their own weighted scores,
	// and their sum is divided by searchFusionK + 1 - the score reciprocal rank fusion gave the
	// first row of a list, 1/61 - so the keyword leg's rank and the meaning leg's cosine, each on
	// a scale of its own and weighted below, are softened onto that one scale, the scale every
	// result's rank has been published on since LEGION-386: a row one leg found at full strength
	// still scores about 1/61, as the best row of a list did. 60 is the value of the paper that
	// introduced the method (Cormack, Clarke and Buettcher, 2009) and Elasticsearch's and
	// OpenSearch's default; nothing tunes it.
	searchFusionK = 60
	// searchKeywordWeight is what a keyword hit's own rank is worth in the fused score, and the
	// unit searchMeaningWeight is written in: 1, so a keyword-only search ranks by the row's
	// length-normalised ts_rank_cd (searchKeywordLegsSQL) and nothing else.
	searchKeywordWeight = 1.0
	// searchMeaningWeight is what a meaning hit's cosine similarity is worth beside it. The two
	// legs score on different scales, and 0.4 is what lines them up on the corpus
	// searchMeaningFloor was derived from: ts_rank_cd with length normalisation scores a title
	// hit 0.5, an issue matched by its spec alone 0.167 and a one-word comment or message body
	// 0.091 (measured on this corpus's own lexeme weights, A 1.0, C 0.2, D 0.1), while a meaning
	// hit's cosine runs 0.45-0.50 for a true paraphrase and 0.9 for a near-copy. At 0.4 a
	// near-copy (0.36) still sits below a title hit, a true paraphrase (0.20) above an issue
	// found by its spec alone, and a hit at the floor (0.10) beside the weakest keyword hit
	// there is, so meaning fills what the words miss without displacing an exact match.
	searchMeaningWeight = 0.4
	// searchOwnKeyLead is what q.own_key is worth: an issue whose key is the whole query leads
	// the page, since ts_rank_cd scores its own key no higher than the title of another issue
	// citing it (TestSearchListsAnIssueFirstForItsOwnKey). 1 is above every score the legs can
	// reach together, (searchKeywordWeight + searchMeaningWeight) / (searchFusionK + 1), about
	// 0.023, so the lead is absolute rather than one more term to be outweighed.
	searchOwnKeyLead = 1.0
	// searchEmbedTimeout bounds the one model-service call search makes (the query's own embedding).
	// Past it, or any other embedder failure, search answers keyword-only and says so (LEGION-549)
	// rather than making every search wait indefinitely on a degraded embedder.
	searchEmbedTimeout = 3 * time.Second
	// searchMeaningFloor is the lowest cosine similarity a meaning-leg row may score to reach legs
	// at all: below it, a row is noise, not a weak match, and letting it through would put a row
	// the query means nothing to on the page, where every row is read as an answer, and give it
	// searchMeaningWeight's share of a score besides. Chosen from embed-v4 pairs on
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

// searchKeywordLegs is the five-kind keyword-ranked union - issue, document, comment, ask,
// message, each matched against q.tsq and ranked by rank of its own search column - shared
// verbatim by searchQueryBody's "kinds" CTE and searchQueryMeaningBody's "kinds" CTE, where it is
// unioned with the five meaning_* CTEs below it - this, searchFusedSQL and searchPageAndTailSQL
// are the whole of the duplication between the two queries. rank is search's ts_rank_cd
// (searchKeywordLegsSQL) or the related-item check's coordination rank
// (relatedCoordinationRank). Every arm carries 'keyword' as its own list column, which the shared
// fused CTE reads to weigh this leg's score against the meaning leg's; in the keyword-only query
// it is the only value the column ever takes.
//
// Column shape after kind, list: issue_key, owner_artifact_id, artifact_id, id, block_id,
// issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at, own, r.
// block_id is an ask's typed block only while it is a node of the reference graph
// (askBlockNodeSQL). The ask arm's anchor cast is store.CastableUUID's, which answers the document
// every spelling of an artifact_id Postgres casts names.
//
// The document arm is a named piece of its own (searchDocumentKeywordLeg), so that it can be
// ranked, read and measured apart from the four arms beside it.
func searchKeywordLegs(rank func(searchColumn string) string) string {
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = q.own_key as own, ` + rank("i.search") + ` as r
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
` + searchDocumentKeywordLeg(rank) + `
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
         coalesce(k.artifact_id, k.block_artifact_id, ` + store.CastableUUID(`k.anchor->>'artifact_id'`) + `), k.id::text, ` + askBlockNodeSQL + `,
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

// searchDocumentKeywordLeg is searchKeywordLegs' document arm, written apart from the four arms
// beside it so that it can be ranked, read and measured on its own: it is a small part of the
// cost of every statement that unions it, and a cost that small is legible only when the arm is
// read apart from theirs.
//
// It reads document_search (migration 20261101182447), one row per document holding its latest
// version's vector under a GIN index, rather than a lateral over artifact_versions: that table's
// search column has no index and can have none (0010 left it out, and its 5.2 GiB is above what
// the pre-deploy census lets a migration touch), so an arm reading it tests every document's
// latest version in turn. The rows are the same ones - 20261101182454's trigger holds
// document_search at each document's latest version with markdown - and the join to artifacts
// decides the arm's project, kind and issue.
//
// The text carries no newline of its own at either end: searchKeywordLegs joins it to its
// neighbours with the union the arms are separated by, and a caller running the arm by itself
// wraps it in a select of its own.
func searchDocumentKeywordLeg(rank func(searchColumn string) string) string {
	return `  select 'document' as kind, 'keyword', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + rank("v.search") + `
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)`
}

// searchKeywordLegsSQL is search's own keyword legs, each arm ranked by ts_rank_cd with length
// normalisation.
var searchKeywordLegsSQL = searchKeywordLegs(func(searchColumn string) string {
	// 32 is ts_rank_cd's "divide the rank by itself plus one" normalisation flag: the raw rank
	// grows with every further matching lexeme a text holds, and dividing it by itself plus one
	// maps it onto [0, 1), so a long document no longer scores above a short issue merely by
	// holding more of the query's lexemes, and the one score every kind is now ranked on
	// (searchFusedSQL) can weigh a rank against a cosine similarity. The related-item check's own
	// rank already passes it (relatedCoordinationRank).
	return "ts_rank_cd(" + searchColumn + ", q.tsq, 32)"
})

// askBlockNodeSQL is the block id of ask k's typed block while that block is a node of the
// reference graph (a document_blocks row of the document holding it), and null otherwise: an id
// past text.MaxBlockIDBytes is stored but never a node, and a later edit can remove the block.
// searchHref links an ask's block only when this is set, since a reader of the `#b-` link reads
// the block's node, and links the ask itself (`?ask=`) when it is not. Both ask arms - the
// keyword one above and meaning_ask_candidates - select it.
const askBlockNodeSQL = `case when exists (select 1 from document_blocks d where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id) then k.block_id end`

// searchFusedSQL is the one score every match is ranked by, shared verbatim by searchQueryBody's
// and searchQueryMeaningBody's "fused" CTE: one scored list across every kind, rather than five
// lists of their own merged by the position a row holds in its kind. Each leg contributes its own
// score - the keyword leg's length-normalised ts_rank_cd (searchKeywordLegsSQL), the meaning
// leg's cosine similarity - weighted onto the one scale the two can be added on
// (searchKeywordWeight, searchMeaningWeight), the sum divided by searchFusionK + 1. Grouping by
// (kind, id) is what makes a row both legs found one row scoring the sum of both, which is
// strictly above what either leg alone gives it, since every leg's own term is positive (a
// matched tsquery ranks above zero, and a meaning row below searchMeaningFloor never reaches
// legs). No kind is ranked ahead of another here or in the page, which orders by this score
// alone, then recency, then id (searchPageAndTailSQL); what position still decides is depth, how
// many of a leg's rows reach legs at all ($7, contracts.SearchKindDepth), and nothing else. An
// issue whose key is the whole query leads the page by searchOwnKeyLead, read from the own column
// the issue arm carries (q.own_key).
//
// The keyword-only body has one list, so its 'meaning' arm never fires there; the text is shared
// whole rather than kept as two near-identical copies, as the keyword legs themselves are.
var searchFusedSQL = fmt.Sprintf(`fused as (
  select kind, id,
         sum(case list when 'keyword' then %[1]g * r else %[2]g * r end) / ($8 + 1)
           + case when bool_or(own) then %[3]g else 0 end as score
    from legs group by kind, id
),
`, searchKeywordWeight, searchMeaningWeight, searchOwnKeyLead)

// searchPageAndTailSQL is the "page the fused ranking, then stitch back the snippet" tail shared
// by searchQuery and searchQueryMeaning - the other half of the LEGION-549 round-5 dedup above:
// identical but for legsSource, the name of the CTE each query's own ranked/fused machinery hands
// it - searchQuery's single-list legs, or searchQueryMeaning's legs_unique (one representative
// row per (kind, id) after scoring the keyword and the meaning list together, searchFusedSQL).
// legsSource is always one of those two Go identifiers below, never caller input, so there is no
// SQL injection surface in formatting it directly into the query text.
//
// The order is the fused score, then recency, then id, and no kind is named in it: a kind leads
// only by scoring highest. id breaks what is left, so the order is total and consecutive offsets
// walk the reachable rows once while the corpus holds still.
func searchPageAndTailSQL(legsSource string) string {
	return fmt.Sprintf(`page as (
  select l.*, f.score
    from %s l join fused f using (kind, id)
   order by f.score desc, l.updated_at desc, l.id
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
 order by r.score desc, r.updated_at desc, r.id
`, legsSource)
}

// searchQuery scores every kind of content on one list, so a page holds the query's best matches
// whatever kind they are: kinds unions the five kinds' matches (searchKeywordLegsSQL) with no
// per-kind limit or order of its own; one window over it, partitioned by kind, orders each
// kind's matches by own, then ts_rank_cd, then recency, then id, and numbers them (pos), and the
// same partition's count(*), taken before any cut, is that kind's every match (matches).
// Filtering to pos <= $7 (contracts.SearchKindDepth) then keeps each kind's best $7 as legs,
// which is how deep a kind is read and no longer where it is ranked: fused (searchFusedSQL)
// scores what survives by each row's own rank, and the page orders by that score, then recency,
// then id, which makes the order total, so consecutive offsets cover the reachable rows once
// while the corpus holds still. A kind's one weak match therefore no longer outranks the second
// of fifty strong ones in another kind, and no kind leads a tie by being named first. An issue
// whose key is the whole query leads the page (searchOwnKeyLead), since ts_rank_cd scores its own
// key no higher than the title of another issue citing it. legs and fused carry no row's text, so
// the union, the window and the fused score cost nothing per kind's full body; only the page's
// own rows pay for one lateral fetch of their text and, after that, a snippet. totals is joined
// outside the page, so a page past the end still answers how many rows the query matches. This is
// the keyword-only fallback (LEGION-549 degraded mode); the related-item check runs its body,
// with its keyword arms ranked by relatedCoordinationRank, over its own q.
//
// Parameters: $1 q, $2 project (empty for every project), $3 limit, $4 firstTerm,
// $5 headlineOptions, $6 offset, $7 contracts.SearchKindDepth, $8 searchFusionK.
var searchQuery = searchQOpener + searchQueryBody(searchKeywordLegsSQL)

// searchQOpener opens searchQuery and searchQueryMeaning: their q, the query text read as
// websearch_to_tsquery, its first term ($4) for the snippet window, and the issue key the whole
// text would be (own_key). The related-item check prefixes the same two bodies with its own q
// (relatedQOpener), their keyword legs ranked by its idf-weighted coordination rank
// (relatedCoordinationRank); the rest of each body, the fused score and the page, is search's own.
const searchQOpener = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, upper(btrim($1)) as own_key),
`

// searchQueryBody is searchQuery after its q, over keywordLegs (searchKeywordLegs): every CTE it
// reads q from and the page.
func searchQueryBody(keywordLegs string) string {
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
` + searchFusedSQL + `totals as (
  select (select coalesce(sum(matches), 0) from (select max(matches) as matches from legs group by kind) each_kind) as total,
         (select count(*) from fused) as reachable
),
` + searchPageAndTailSQL("legs")
}

// searchQueryMeaning extends searchQuery with a second list per kind, ranked by cosine distance
// to the query's own embedding ($9, a pgvector literal) against embeddings.embedding
// (0072_embeddings_core.up.sql), rather than ts_rank_cd. Each kind's meaning candidates are their
// own CTE (meaning_issue_candidates and so on) doing plainly `order by embedding <=> $9 limit $7`
// - pgvector's HNSW index serves that shape directly (confirmed by
// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan) - rather than joining meaning rows into
// one shared window spanning every kind, which would force a sequential scan of embeddings once
// per leg. The vector is the bound parameter itself ($9::vector), never a column of q: the index
// orders by distance only to a value fixed for the whole scan, and a column joined in from q is
// not one, so ordering by it sorts every candidate in memory instead. Each candidate CTE reads
// only vectors whose embeddings.model is $11, the query embedder's own identity
// (embed.Embedder.Identity, INFRA-393): a vector another model or place made lives in a different
// space, and its cosine distance to this query means nothing. The coverage pass (embedqueue.Cover)
// drops such vectors once the configured embedder has answered, so the filter has to catch what a
// differently configured process writes during a roll, and every vector of the identity before
// it while a newly configured embedder has not embedded anything yet. Each candidate set is then
// floor-filtered (searchMeaningFloor) before kinds ever sees it: a candidate below the floor is
// noise, not a weak match, and reaching legs at all would put it on the page as an answer and
// score it as one.
//
// kinds unions searchKeywordLegs - the same five-arm keyword text searchQuery's own kinds CTE
// uses verbatim, each arm already carrying 'keyword' as list - with the five meaning_* CTEs
// above. Every leg now carries which list it belongs to (list), and ranked's window partitions by
// (kind, list): each list keeps its own top $7, so a row that matches both lists for the same
// (kind, id) occupies two rows of legs - one per list - each carrying its own score, a
// ts_rank_cd rank on one list and a cosine similarity on the other. fused weighs those two scales
// against each other and sums them grouped by (kind, id) (searchFusedSQL): an item only the
// keyword list reached, only the meaning list reached, or both, is ranked once, and an item both
// lists found scores above what either list alone would have given it. legs_unique picks one
// representative row's owner/text-location columns per (kind, id) - both lists' rows carry the
// same ones, read from the same underlying issue/document/comment/ask/message - before the page
// is cut and joined back to fused's score. total is every distinct (kind, id) either list
// matched, not a sum of list sizes, so an id both lists reach is counted once.
//
// Parameters: $1 q, $2 project, $3 limit, $4 firstTerm, $5 headlineOptions, $6 offset,
// $7 contracts.SearchKindDepth, $8 searchFusionK, $9 the query's embedding (a vector literal),
// $10 searchMeaningFloor, $11 the query embedder's identity.
var searchQueryMeaning = searchQOpener + searchQueryMeaningBody(searchKeywordLegsSQL)

// searchQueryMeaningBody is searchQueryMeaning after its q, over keywordLegs (searchKeywordLegs).
func searchQueryMeaningBody(keywordLegs string) string {
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
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, ` + askBlockNodeSQL + ` as block_id, i.title as issue_title, i.status as issue_status,
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
         coalesce(artifact_id, block_artifact_id, ` + store.CastableUUID(`anchor->>'artifact_id'`) + `), id::text, block_id,
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
` + searchFusedSQL + `legs_unique as (
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
` + searchPageAndTailSQL("legs_unique")
}

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
	// q is capped as `dispatch search` caps it, counted after trimming so a query the command
	// sends is never refused here, and a project must be a key (an empty one searches every
	// project). Both ride in the URL; packages/contracts/AGENTS.md "Search limits" says what keeps
	// it short.
	if length := len16(searchText); length > contracts.SearchQueryMax {
		tooLong := capExceededError("q", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; "+contracts.SearchQueryHint)
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
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
	// Where each ask hit stands is read for this route alone: the write-time suggestions share
	// the ranking (runFusedSearch) but never show it. TookMS counts the read.
	started := time.Now()
	if err := s.attachAskStates(r.Context(), s.deps.Store.Pool, response.Results); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response.TookMS += time.Since(started).Milliseconds()
	WriteJSON(w, http.StatusOK, response)
}

// runSearch is the search handler's own entry point. The numnode check runs here, before anything
// else - including before the one model-service call this function ever makes - so a
// stopword-only or operator-only query never pays for an embedding it would throw away. It tries
// meaning search first when an embedder is configured, falling back to keyword-only
// (runFusedSearchQuery) only when the query's own embedding could not be had - never when the
// meaning query itself then fails, which is a real error the caller should see as a 500, not
// something to paper over as a degraded answer.
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
// query's own embedding failing) do identically. runSearch has already run the numnode check
// before ever reaching here.
func (s *server) runKeywordOnlyDegraded(ctx context.Context, q, project string, limit, offset int) (model.SearchResponse, error) {
	response, err := s.runFusedSearchQuery(ctx, q, project, limit, offset)
	if err == nil {
		response.Degraded = contracts.SearchDegradedEmbedderUnavailable
	}
	return response, err
}

// runFusedSearchQuery runs searchQuery, assuming nodes > 0 already, which its caller
// (runKeywordOnlyDegraded, after runSearch's check) must have established first. q is assumed
// already validated (length, non-empty after trimming); project empty searches every project.
func (s *server) runFusedSearchQuery(ctx context.Context, q, project string, limit, offset int) (model.SearchResponse, error) {
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

// runMeaningSearch is runFusedSearchQuery's counterpart once a query embedding exists: same row
// shape, searchQueryMeaning in place of searchQuery. Its only caller, runSearch, has already run
// the numnode check before ever reaching here (so it never pays for an embedding it would throw
// away); this function assumes nodes > 0.
func (s *server) runMeaningSearch(ctx context.Context, q, project string, limit, offset int, vector []float32) (model.SearchResponse, error) {

	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(ctx, searchQueryMeaning, q, project, limit, firstTerm(q), headlineOptions,
		offset, contracts.SearchKindDepth, searchFusionK, embed.Literal(vector), searchMeaningFloor, s.deps.Embedder.Identity())
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
				Project:    deref(ownerProject),
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
// degraded request never holds the whole search handler open on a slow or wedged model service.
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
// document, so its href opens the document itself and names the item in the query, exactly as
// the SPA's own thread links do. An ask whose typed block is a node of the reference graph
// (blockID, askBlockNodeSQL) names that block in the fragment instead, which a reader of the
// href reads as the block's node; one whose block is no node names the ask. An item with no
// document is a Conversation turn, named by its own route so the turn is focused.
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
			return documentHref + "#b-" + text.EncodeBlockID(*blockID)
		}
		return documentHref + "?ask=" + url.QueryEscape(id)
	case "message":
		return issueHref + "/messages/" + id
	default:
		return ""
	}
}
