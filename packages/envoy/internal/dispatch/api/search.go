package api

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
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
	// searchBroadenWords is how many words a query may hold before search adds the any-word leg
	// beside websearch_to_tsquery's AND of them (searchQOpener). Five is where the two readings
	// part: a person naming a thing writes two to five words ("astrolabe", "search ranking
	// fusion"), every one of them meant, and the AND of them is what makes the answer precise;
	// from six words on they are writing a sentence ("why does the controller restart after every
	// deploy"), whose auxiliary words no one item repeats, so the AND answers nothing at all
	// while the words themselves rank the rows holding most of them first. Only words count: a
	// quoted phrase, an OR and a -word are operators a person wrote, and websearch_to_tsquery
	// keeps reading them (readSearchQuery).
	searchBroadenWords = 5
	// searchAuthorMax bounds the author filter's value, in UTF-16 units as every other cap on
	// this route counts: 320 is the longest address RFC 5321 allows, and an author is a person's
	// login (an address) or a session's id, which is shorter.
	searchAuthorMax = 320
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
	// searchWindowCandidates is how many nearest windows one window arm takes before its
	// `distinct on (id)` keeps the nearest window of each item. Every arm is held to
	// contracts.SearchKindDepth items afterwards (ranked), but a window is not an item: one
	// long document holds hundreds of them, and at a candidate depth of SearchKindDepth its
	// windows alone fill the arm, which then yields that one document and every other document
	// the query matches is lost. Ten times the depth leaves room for a thousand windows, which
	// is the one document's hundreds and the rest of the page's items beside them, and the arm
	// still reads them through embeddings_cosine, nearest first.
	searchWindowCandidates = 10 * contracts.SearchKindDepth
)

// searchKeywordLegs is the five-kind keyword-ranked union - issue, document, comment, ask,
// message, each matched against q.tsq and ranked by rank of its own search column - shared
// verbatim by searchQueryBody's "kinds" CTE and searchQueryMeaningBody's "kinds" CTE, where it is
// unioned with the five meaning_* CTEs below it - this, searchFusedSQL and searchPageAndTailSQL
// are the whole of the duplication between the two queries. rank is search's ts_rank_cd
// (searchKeywordLegsSQL) or the related-item check's coordination rank
// (relatedCoordinationRank). Every arm carries 'keyword' as its own list column, which the shared
// fused CTE reads to weigh this leg's score against the meaning leg's; in the keyword-only query
// it is the only value the column ever takes. Every arm carries q's three filters
// (searchRowFilters) in the arm itself, so a filtered search reads its own matches as deep as
// any other search reads, rather than a page of unfiltered ones with the rest struck out.
//
// The issue arm matches an issue the query names by key as well as one q.tsq finds
// (q.own_keys, searchOwnIssueKeys): a key is indexed with its project ('infra' <-> '-1042'), so
// the number alone, or a space where the hyphen is, matches no issue's vector, and the issue a
// person asked for would be absent from the answer rather than first in it.
//
// Column shape after kind, list: issue_key, owner_artifact_id, artifact_id, id, block_id,
// window_id, covers, issue_title, issue_status, owner_project, owner_slug, owner_name,
// updated_at, own, r. block_id is an ask's typed block only while it is a node of the reference
// graph (askBlockNodeSQL). window_id and covers are the embeddings window a meaning arm matched
// and the blocks its vector covers; a keyword arm matched no window and carries null for both,
// as it carries 'keyword' as list, so the two unions have one column shape. The ask arm's anchor
// cast is store.CastableUUID's, which answers the document every spelling of an artifact_id
// Postgres casts names.
//
// The document arm is a named piece of its own (searchDocumentKeywordLeg), so that it can be
// ranked, read and measured apart from the four arms beside it.
func searchKeywordLegs(rank func(searchColumn string) string) string {
	return `
  select 'issue' as kind, 'keyword' as list, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
         i.key = any(q.own_keys) as own, ` + rank("i.search") + ` as r
    from issues i, q
   where (i.search @@ q.tsq or i.key = any(q.own_keys)) and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
  union all
` + searchDocumentKeywordLeg(rank) + `
  union all
  select 'comment' as kind, 'keyword', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at),
         false, ` + rank("c.search") + `
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "c.author", "c.created_at") + `
  union all
  select 'ask' as kind, 'keyword', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end,
         coalesce(k.artifact_id, k.block_artifact_id, ` + store.CastableUUID(`k.anchor->>'artifact_id'`) + `), k.id::text, ` + askBlockNodeSQL + `,
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at),
         false, ` + rank("k.search") + `
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "k.author", "k.created_at") + `
  union all
  select 'message' as kind, 'keyword', m.issue_key, null::uuid, null::uuid, m.id::text, null::text,
         null::text, null::text[],
         i.title, i.status, null::text, null::text, null::text, i.updated_at,
         false, ` + rank("m.search") + `
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "m.author", "m.created_at")
}

// searchRowFilters is the status, author and date filter every leg of every search narrows by,
// read from q (searchQOpener) rather than bound into each arm, so the related-item check's own q
// names them once as no filter at all. Each is a no-op while its own value is empty.
//
// status is the issue status the row belongs to: a row whose kind has no issue - a project
// document, and a comment or ask on one - matches no status at all, since the filter asks for
// the rows of the issues in that status and such a row belongs to none. author is the jsonb
// actor that wrote the item the row names, matched on its id, which is a person's login or a
// session's id; every kind has one, an issue and a document their creator. written is when the
// item itself was last written, which the date filter reads in place of the recency the row is
// ordered by: an issue's updated_at moves whenever anything on it moves, and a comment written
// last year on an issue touched today is not a comment from today.
func searchRowFilters(status, author, written string) string {
	return `
     and (q.status_filter = '' or ` + status + ` = q.status_filter)
     and (q.author_filter = '' or ` + author + `->>'id' = q.author_filter)
     and (q.updated_since is null or ` + written + ` >= q.updated_since)`
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
         null::text, null::text[],
         i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at),
         false, ` + rank("v.search") + `
    from document_search v
    join artifacts a on a.id = v.artifact_id
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)` +
		searchRowFilters("i.status", "a.created_by", "v.created_at")
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
// issue the query names by key leads the page by searchOwnKeyLead, read from the own column the
// issue arm carries (q.own_keys), and own_lead carries that fact out of the score so the page can
// order such issues among themselves by their key, which for one number is their project's order
// (searchPageAndTailSQL).
//
// The keyword-only body has one list, so its 'meaning' arm never fires there; the text is shared
// whole rather than kept as two near-identical copies, as the keyword legs themselves are.
var searchFusedSQL = fmt.Sprintf(`fused as (
  select kind, id, bool_or(own) as own_lead,
         sum(case list when 'keyword' then %[1]g * r else %[2]g * r end) / ($8 + 1)
           + case when bool_or(own) then %[3]g else 0 end as score
    from legs group by kind, id
),
`, searchKeywordWeight, searchMeaningWeight, searchOwnKeyLead)

// windowOwnerUUID and windowItemUUID read the item a window belongs to out of the window's own
// id, "<owner key>#<the first block it covers>": the owner key is the id up to the first #, and
// it is the artifact id for a document's window and "<kind>/<uuid>" for a comment's, a message's
// or an ask's (refs.ItemKind.WindowOwnerKey). Each meaning arm joins its item on that item's
// primary key through one of these, so the planner reaches the row through the key rather than
// reading the whole table to compare a text it computes per row. The cast is guarded
// (store.CanonicalUUID) so a window id in any other shape resolves to no item instead of
// answering SQLSTATE 22P02 for the whole search.
var (
	windowOwnerUUID = store.CanonicalUUID(`split_part(e.id, '#', 1)`)
	windowItemUUID  = store.CanonicalUUID(`split_part(split_part(e.id, '#', 1), '/', 2)`)
)

// searchPageAndTailSQL is the "page the fused ranking, then stitch back the snippet" tail shared
// by searchQuery and searchQueryMeaning - the other half of the LEGION-549 round-5 dedup above:
// identical but for legsSource, the name of the CTE each query's own ranked/fused machinery hands
// it - searchQuery's single-list legs, or searchQueryMeaning's legs_unique (one representative
// row per (kind, id) after scoring the keyword and the meaning list together, searchFusedSQL).
// legsSource is always one of those two Go identifiers below, never caller input, so there is no
// SQL injection surface in formatting it directly into the query text.
//
// The order is the fused score, then the key of an issue the query named (own_lead), then
// recency, then id, and no kind is named in it: a kind leads only by scoring highest. id breaks
// what is left, so the order is total and consecutive offsets walk the reachable rows once while
// the corpus holds still. The own_lead term orders nothing but those issues: for every other row
// it is null, and every issue it does order already stands above them by searchOwnKeyLead. A
// number alone names one issue of each project (searchOwnIssueKeys), and their ids are their
// keys, so ordering them by id is ordering them by project key.
//
// The page also names the block a meaning hit matched (hit.block_id, INFRA-370). A meaning leg
// matched one window of an owner's blocks, and that window covers a run of them (r.covers); the
// block named is the covered block whose own text ranks highest for the query by ts_rank_cd over
// the configuration the keyword legs are built with ('english' over search_text), ties broken by
// the block's place in the window, and so the window's first block when no covered block shares a
// term with the query. A search for the text of a one-line ask option therefore names the option,
// not the question that gives the option its context and stands in the same window. The rank runs
// in the page's own lateral join, over the rows of one page and the few blocks each window covers,
// never over every candidate; a keyword row and an issue carry no covers, so the join answers
// nothing for them.
func searchPageAndTailSQL(legsSource string) string {
	return fmt.Sprintf(`page as (
  select l.*, f.score, f.own_lead
    from %s l join fused f using (kind, id)
   order by f.score desc, case when f.own_lead then l.id end, l.updated_at desc, l.id
   limit $3 offset $6
)
select t.total, t.reachable, r.kind, case when r.owner_artifact_id is null then 'issue' else 'document' end,
       r.issue_key, r.issue_title, r.issue_status,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, hit.block_id, r.score::float8,
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
  -- The block of r.covers that matched, if the row carries a window. Each covered block's own
  -- text comes from the table its indexer writes it to: a document's, a comment's and an ask's
  -- block texts are kept per version or revision, the latest being the current text. A message
  -- is never edited and keeps no text table (refs/item_blocks.go's table map), so its block is
  -- ranked on the excerpt its row holds, the block's first refs.BlockExcerptRunes runes.
  left join lateral (
    select covered.block_id
      from unnest(r.covers) with ordinality as covered(block_id, ord)
      cross join lateral (
        select case r.kind
          when 'document' then (select x.text from document_block_texts x
                                 where x.artifact_id = r.artifact_id and x.block_id = covered.block_id
                                 order by x.version desc limit 1)
          when 'comment' then (select x.text from comment_block_texts x
                                 where x.comment_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'ask' then (select x.text from ask_block_texts x
                                 where x.ask_id = r.id::uuid and x.block_id = covered.block_id
                                 order by x.revision desc limit 1)
          when 'message' then (select x.excerpt from message_blocks x
                                 where x.message_id = r.id::uuid and x.block_id = covered.block_id)
        end as text
      ) block
     order by ts_rank_cd(to_tsvector('english', search_text(coalesce(block.text, ''))), q.tsq) desc, covered.ord
     limit 1
  ) hit on true
 order by r.score desc, case when r.own_lead then r.id end, r.updated_at desc, r.id
`, legsSource)
}

// searchQuery scores every kind of content on one list, so a page holds the query's best matches
// whatever kind they are: kinds unions the five kinds' matches (searchKeywordLegsSQL) with no
// per-kind limit or order of its own; one window over it, partitioned by kind, orders each
// kind's matches by own, then ts_rank_cd, then recency, then id, and numbers them (pos), and the
// same partition's count(*), taken before any cut, is that kind's every match (matches).
// Filtering to pos <= $7 (contracts.SearchKindDepth) then keeps each kind's best $7 as legs,
// which is how deep a kind is read and no longer where it is ranked: fused (searchFusedSQL)
// scores what survives by each row's own rank, and the page orders by that score, then the key of
// an issue the query named (own_lead, which orders nothing else), then recency, then id, which
// makes the order total, so consecutive offsets cover the reachable rows once
// while the corpus holds still. A kind's one weak match therefore no longer outranks the second
// of fifty strong ones in another kind, and no kind leads a tie by being named first. An issue
// the query names by key leads the page (searchOwnKeyLead), since ts_rank_cd scores its own key
// no higher than the title of another issue citing it. legs and fused carry no row's text, so
// the union, the window and the fused score cost nothing per kind's full body; only the page's
// own rows pay for one lateral fetch of their text and, after that, a snippet. totals is joined
// outside the page, so a page past the end still answers how many rows the query matches. This is
// the keyword-only fallback (LEGION-549 degraded mode); the related-item check runs its body,
// with its keyword arms ranked by relatedCoordinationRank, over its own q.
//
// Parameters: $1 q, $2 project (empty for every project), $3 limit, $4 firstTerm,
// $5 headlineOptions, $6 offset, $7 contracts.SearchKindDepth, $8 searchFusionK, then
// searchQOpener's own six from $9: $9 the any-word leg's words, $10 the words it excludes,
// $11 the issues the query names by key, $12 status, $13 author, $14 updated_since
// (searchQueryArguments).
var searchQuery = searchQOpener(9) + searchQueryBody(searchKeywordLegsSQL)

// searchQOpener opens searchQuery and searchQueryMeaning: their q, the query text read as
// websearch_to_tsquery, its first term ($4) for the snippet window, the issues the query names by
// key (own_keys, searchOwnIssueKeys) and the three filters every leg narrows by
// (searchRowFilters). base is the first parameter index of the six values the opener itself
// binds, which differs between the two statements because the keyword body's own parameters end
// at $8 and the meaning body's at $11. The related-item check prefixes the same two bodies with
// its own q (relatedQOpener), which names the same columns with no key and no filter, their
// keyword legs ranked by its idf-weighted coordination rank (relatedCoordinationRank); the rest
// of each body, the fused score and the page, is search's own.
//
// tsq is websearch_to_tsquery's own reading of the text - the phrase, OR and -word forms a person
// already has - with the any-word leg OR-ed onto it for a query past searchBroadenWords words
// ($base, readSearchQuery), so a long question answers at least what its strongest words answer,
// ranked by how many of them a row holds, where the AND of every word answers nothing as soon as
// one of them is absent everywhere. The words the query excludes are ANDed onto that leg
// ($base+1), since OR-ing the words back in without them would return the rows a -word left out;
// a query that excludes nothing binds the empty string and takes the shorter form.
func searchQOpener(base int) string {
	return fmt.Sprintf(`
with q as (select
         case when $%[1]d = '' then websearch_to_tsquery('english', $1)
              when $%[2]d = '' then websearch_to_tsquery('english', $1) || websearch_to_tsquery('english', $%[1]d)
              else websearch_to_tsquery('english', $1)
                   || (websearch_to_tsquery('english', $%[1]d) && websearch_to_tsquery('english', $%[2]d))
         end as tsq,
         $4::text as term, $%[3]d::text[] as own_keys,
         $%[4]d::text as status_filter, $%[5]d::text as author_filter, $%[6]d::timestamptz as updated_since),
`, base, base+1, base+2, base+3, base+4, base+5)
}

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

// searchWindowLimit is the LIMIT a window arm's candidate CTE takes, searchWindowCandidates as
// SQL. It is written into the statement rather than bound, so that the two queries built from
// searchQueryMeaningBody keep the parameters they have: the number is this build's own, never a
// caller's.
var searchWindowLimit = "limit " + strconv.Itoa(searchWindowCandidates)

// searchMeaningScan is what a meaning query's transaction sets before it runs, both statements
// local to that transaction and no other:
//
//   - hnsw.ef_search: how many nearest vectors the HNSW index walks before the kind, project
//     and model filters run. pgvector's default, 40, is spent on whichever kind holds the most
//     vectors - the document windows, tens of thousands of them against one row per issue - so
//     the issue and message arms are left with few candidates or none, and the project filter
//     can take the rest. 400 keeps them, at a wider walk of the index paid once per query.
//   - hnsw.iterative_scan: with it off, an arm whose kind filter rejects every one of those 400
//     answers nothing at all; relaxed_order keeps scanning until the arm's own LIMIT is met or
//     hnsw.max_scan_tuples (20,000) is reached. Relaxed rather than strict: every arm re-ranks
//     its rows by r (distinct on, then ranked), so nothing downstream reads the index's order.
//
// Both meaning queries run in a transaction beginMeaningScan opens, and set them there.
var searchMeaningScan = []string{
	`set local hnsw.ef_search = 400`,
	`set local hnsw.iterative_scan = relaxed_order`,
}

// beginMeaningScan opens the read transaction a meaning query runs in, searchMeaningScan's two
// settings applied to it: `set local` holds them to this transaction, so a pooled connection
// carries neither of them back to the next request. The caller ends the transaction, a rollback
// deferred, since neither query writes; a transaction a setting failed on is rolled back here
// and never handed back. Search's own query (runMeaningSearch) and the related-item check's
// (relatedMeaningSearch) both start here, so neither can read the index on other settings than
// the other.
func (s *server) beginMeaningScan(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	for _, setting := range searchMeaningScan {
		if _, err := tx.Exec(ctx, setting); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return tx, nil
}

// searchQueryMeaning extends searchQuery with a second list per kind, ranked by cosine distance
// to the query's own embedding ($9, a pgvector literal) against embeddings.embedding
// (0072_embeddings_core.up.sql), rather than ts_rank_cd. Each kind's meaning candidates are their
// own CTE (meaning_issue_candidates and so on) doing plainly `order by embedding <=> $9 limit n`
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
// Four of the five arms read windows of neighbouring blocks (INFRA-370: kinds document_block,
// comment_block, message_block and ask_part), where an issue's arm still reads the issue's one
// whole-text row. A window's id is "<owner key>#<suffix>", so the item it belongs to is
// split_part(id, '#', 1) - the artifact id for a document, "<kind>/<uuid>" for a comment, a
// message or an ask - and each arm projects the item's own kind and id, so fusion still ranks
// one row per item however many of its windows the index hands back. A window arm takes
// searchWindowCandidates of them, ten times the items any arm keeps, because one long
// document's windows would otherwise fill an arm of contracts.SearchKindDepth and leave every
// other document of the page out. Each arm then keeps the item's
// nearest window alone (distinct on (id), the highest cosine first) and carries that window's
// id and covers, the blocks its vector holds, on to legs_unique and the page, which names the
// one of them that matched (searchPageAndTailSQL).
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
// same ones, read from the same underlying issue/document/comment/ask/message - preferring the
// row that names a window, since only that row knows which blocks the hit covers, before the
// page is cut and joined back to fused's score. total is every distinct (kind, id) either list
// matched, not a sum of list sizes, so an id both lists reach is counted once.
//
// Parameters: $1 q, $2 project, $3 limit, $4 firstTerm, $5 headlineOptions, $6 offset,
// $7 contracts.SearchKindDepth, $8 searchFusionK, $9 the query's embedding (a vector literal),
// $10 searchMeaningFloor, $11 the query embedder's identity, then searchQOpener's own six from
// $12: $12 the any-word leg's words, $13 the words it excludes, $14 the issues the query names
// by key, $15 status, $16 author, $17 updated_since (searchMeaningQueryArguments).
var searchQueryMeaning = searchQOpener(12) + searchQueryMeaningBody(searchKeywordLegsSQL)

// searchQueryMeaningBody is searchQueryMeaning after its q, over keywordLegs (searchKeywordLegs).
func searchQueryMeaningBody(keywordLegs string) string {
	return `meaning_issue_candidates as (
  select i.key as issue_key, i.key as id, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join issues i on i.key = e.id, q
   where e.kind = 'issue' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "i.created_by", "i.updated_at") + `
   order by e.embedding <=> $9::vector
   limit $7
),
meaning_issue as (
  select 'issue' as kind, 'meaning' as list, issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, id, null::text as block_id,
         null::text as window_id, null::text[] as covers,
         issue_title, issue_status, null::text as owner_project, null::text as owner_slug, null::text as owner_name, updated_at,
         false as own, r
    from meaning_issue_candidates where r >= $10
),
meaning_document_candidates as (
  select a.issue_key, a.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, v.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join artifacts a on a.id = ` + windowOwnerUUID + `
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where e.kind = 'document_block' and a.kind = 'doc' and e.embedding is not null and e.model = $11 and ($2 = '' or p.key = $2)` +
		searchRowFilters("i.status", "a.created_by", "v.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_document as (
  select distinct on (meaning_document_candidates.id) 'document' as kind, 'meaning' as list, issue_key, case when issue_key is null then id else null::uuid end, id, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_document_candidates where r >= $10
   order by meaning_document_candidates.id, r desc, window_id
),
meaning_comment_candidates as (
  select c.issue_key, c.artifact_id, c.anchor, c.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
         coalesce(i.updated_at, c.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join comments c on c.id = ` + windowItemUUID + `
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where e.kind = 'comment_block' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "c.author", "c.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_comment as (
  select distinct on (meaning_comment_candidates.id) 'comment' as kind, 'meaning' as list, issue_key, artifact_id, coalesce(artifact_id, (anchor->>'artifact_id')::uuid), id::text, null::text,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_comment_candidates where r >= $10
   order by meaning_comment_candidates.id, r desc, window_id
),
meaning_ask_candidates as (
  select k.issue_key, k.artifact_id, k.block_artifact_id, k.anchor, k.id, ` + askBlockNodeSQL + ` as block_id, e.id as window_id, e.covers,
         i.title as issue_title, i.status as issue_status,
         p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join asks k on k.id = ` + windowItemUUID + `
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where e.kind = 'ask_part' and e.embedding is not null and e.model = $11 and ($2 = '' or coalesce(i.project_key, p.key) = $2)` +
		searchRowFilters("i.status", "k.author", "k.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_ask as (
  select distinct on (meaning_ask_candidates.id) 'ask' as kind, 'meaning' as list, issue_key, case when issue_key is null then coalesce(artifact_id, block_artifact_id) else null::uuid end,
         coalesce(artifact_id, block_artifact_id, ` + store.CastableUUID(`anchor->>'artifact_id'`) + `), id::text, block_id,
         window_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at,
         false, r
    from meaning_ask_candidates where r >= $10
   order by meaning_ask_candidates.id, r desc, window_id
),
meaning_message_candidates as (
  select m.issue_key, m.id, e.id as window_id, e.covers, i.title as issue_title, i.status as issue_status, i.updated_at,
         1 - (e.embedding <=> $9::vector) as r
    from embeddings e
    join messages m on m.id = ` + windowItemUUID + `
    join issues i on i.key = m.issue_key, q
   where e.kind = 'message_block' and e.embedding is not null and e.model = $11 and ($2 = '' or i.project_key = $2)` +
		searchRowFilters("i.status", "m.author", "m.created_at") + `
   order by e.embedding <=> $9::vector
   ` + searchWindowLimit + `
),
meaning_message as (
  select distinct on (meaning_message_candidates.id) 'message' as kind, 'meaning' as list, issue_key, null::uuid, null::uuid, id::text, null::text,
         window_id, covers,
         issue_title, issue_status, null::text, null::text, null::text, updated_at,
         false, r
    from meaning_message_candidates where r >= $10
   order by meaning_message_candidates.id, r desc, window_id
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
  select distinct on (kind, id) kind, issue_key, owner_artifact_id, artifact_id, id, block_id, covers,
         issue_title, issue_status, owner_project, owner_slug, owner_name, updated_at
    from legs
   order by kind, id, (window_id is null), r desc
),
totals as (
  select (select count(*) from (select distinct kind, id from kinds) u) as total,
         (select count(*) from fused) as reachable
),
` + searchPageAndTailSQL("legs_unique")
}

// searchParameters is one validated search: the words, the filters every leg of it is narrowed
// by, and the page. ownKeys is what runSearch resolves the words to when they name an issue
// (searchOwnIssueKeys) and empty otherwise.
type searchParameters struct {
	text    string
	project string
	status  string
	author  string
	since   *time.Time
	limit   int
	offset  int
	ownKeys []string
}

// searchReading is a query read as the words a person wrote and the operators they wrote around
// them: words are the plain words websearch_to_tsquery ANDs, excluded the -word terms it negates,
// phrase whether the text quotes a phrase and or whether it holds the OR operator. A quoted
// phrase's words are words of their own here, since the any-word leg searches for each of them;
// the phrase itself stays in the query text, which the statement reads with websearch_to_tsquery
// whatever this reading says.
type searchReading struct {
	words    []string
	excluded []string
	phrase   bool
	or       bool
}

// readSearchQuery reads q as searchReading describes: one pass over its whitespace-separated
// tokens, the same tokens firstTerm walks. A token holding no letter or digit is no word of
// anyone's and is left out of both lists, so a stray dash or ampersand never becomes a term of
// the any-word leg.
func readSearchQuery(q string) searchReading {
	reading := searchReading{phrase: strings.Count(q, `"`) >= 2}
	for _, token := range strings.Fields(q) {
		switch {
		case strings.EqualFold(token, "or"):
			reading.or = true
		case strings.HasPrefix(token, "-"):
			if word := strings.Trim(token[1:], `"`); searchHasWordRune(word) {
				reading.excluded = append(reading.excluded, word)
			}
		default:
			if word := strings.Trim(token, `"`); searchHasWordRune(word) {
				reading.words = append(reading.words, word)
			}
		}
	}
	return reading
}

func searchHasWordRune(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// broadened is whether this query is long enough for the any-word leg (searchBroadenWords).
func (r searchReading) broadened() bool { return len(r.words) > searchBroadenWords }

// anyWords is the any-word leg's own text: every word the query holds joined by the OR a person
// writes in the bar, which websearch_to_tsquery stems exactly as it stems the query itself, so
// the two legs hold the same lexemes. Empty for a query of searchBroadenWords words or fewer,
// which leaves websearch_to_tsquery's AND the whole of the query.
func (r searchReading) anyWords() string {
	if !r.broadened() {
		return ""
	}
	return strings.Join(r.words, " or ")
}

// excludedWords is the -word terms as the same text, which searchQOpener ANDs onto the any-word
// leg. Empty when the query excludes nothing, or when it is too short to be broadened at all:
// the leg it narrows does not exist then, and the words the person excluded are already
// websearch_to_tsquery's own business.
func (r searchReading) excludedWords() string {
	if !r.broadened() || len(r.excluded) == 0 {
		return ""
	}
	return "-" + strings.Join(r.excluded, " -")
}

// operators names the operator forms this query was read as using, in the order the bar shows
// them, each the contract's own word for it so the string Dispatch writes and the string the bar
// compares against cannot drift apart.
func (r searchReading) operators() []string {
	operators := []string{}
	if r.phrase {
		operators = append(operators, contracts.SearchOperatorPhrase)
	}
	if r.or {
		operators = append(operators, contracts.SearchOperatorOr)
	}
	if len(r.excluded) > 0 {
		operators = append(operators, contracts.SearchOperatorExclude)
	}
	return operators
}

var (
	// searchIssueKeyPattern is an issue key as a person writes it in the bar: the key itself
	// (INFRA-1042), the same in any case (infra-1042), or the key with a space where the hyphen
	// is (INFRA 1042), which is what a person typing fast writes.
	searchIssueKeyPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{1,9})[-\s]+([0-9]{1,9})$`)
	// searchIssueNumberPattern is an issue number alone (1042), which names the issue of that
	// number in the project a filter names, or in every project at once.
	searchIssueNumberPattern = regexp.MustCompile(`^([0-9]{1,9})$`)
	// searchAuthorPattern is the shape of the author filter's value: one actor id, which holds no
	// whitespace and is never blank.
	searchAuthorPattern = regexp.MustCompile(`^\S+$`)
)

// searchKeyForms reads the whole query as one issue key. The key form - in any case, with a
// hyphen or a space - answers that key, spelled as the project spells it; a bare number answers
// the number alone, whose issues searchOwnIssueKeys reads; anything else answers neither. The
// forms are read here, in Go, rather than by a regular expression inside the statement, because
// the statement then takes one list of keys and matches it against the issues primary key, which
// an index answers, where a key or number pattern tested over every row of issues is a scan of
// that table on every search.
func searchKeyForms(q string) (key string, number int) {
	text := strings.TrimSpace(q)
	if match := searchIssueKeyPattern.FindStringSubmatch(text); match != nil {
		parsed, err := strconv.Atoi(match[2])
		if err != nil {
			return "", 0
		}
		return fmt.Sprintf("%s-%d", strings.ToUpper(match[1]), parsed), 0
	}
	if match := searchIssueNumberPattern.FindStringSubmatch(text); match != nil {
		parsed, err := strconv.Atoi(match[1])
		if err != nil {
			return "", 0
		}
		return "", parsed
	}
	return "", 0
}

// searchOwnIssueKeys is the issues the query names by key, which lead the page
// (searchOwnKeyLead). A key form names one issue and needs no read of its own: the issue arm
// matches the key it is given, and matches nothing when no issue holds it. A number alone names
// the issue of that number in each project, which only this one read can answer, and only a
// query that is a bare number ever pays for it; with a project named it names that project's
// issue alone, and without one every matching issue, in project order.
func (s *server) searchOwnIssueKeys(ctx context.Context, q, project string) ([]string, error) {
	key, number := searchKeyForms(q)
	if key != "" {
		return []string{key}, nil
	}
	keys := []string{}
	if number == 0 {
		return keys, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx,
		`select key from issues where number = $1 and ($2 = '' or project_key = $2) order by project_key`, number, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var found string
		if err := rows.Scan(&found); err != nil {
			return nil, err
		}
		keys = append(keys, found)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// searchQueryArguments binds searchQuery's parameters, in the order its own comment names them:
// the query, the project, the page and the two constants, then searchQOpener's own six from $9.
// searchQueryMeaning takes the same values with the meaning leg's three between them
// (searchMeaningQueryArguments), so this pair is the one place that knows what a search binds.
func searchQueryArguments(parameters searchParameters) []any {
	reading := readSearchQuery(parameters.text)
	keys := parameters.ownKeys
	if keys == nil {
		keys = []string{}
	}
	return []any{parameters.text, parameters.project, parameters.limit, firstTerm(parameters.text), headlineOptions,
		parameters.offset, contracts.SearchKindDepth, searchFusionK,
		reading.anyWords(), reading.excludedWords(), keys, parameters.status, parameters.author, parameters.since}
}

// searchMeaningQueryArguments binds searchQueryMeaning's: searchQueryArguments' first eight, the
// meaning leg's three at $9-$11, then the opener's six from $12.
func searchMeaningQueryArguments(parameters searchParameters, vector []float32, identity string) []any {
	arguments := searchQueryArguments(parameters)
	meaning := []any{embed.Literal(vector), searchMeaningFloor, identity}
	return slices.Concat(arguments[:8], meaning, arguments[8:])
}

// searchQueryReading is what the answer tells a reader about the words it was given: the operator
// forms search read them as using, and whether the query was long enough for search to add the
// any-word leg beside websearch_to_tsquery's AND (searchBroadenWords). A bar that shows a person
// what it did with their sentence reads it from here rather than parsing the sentence again.
func searchQueryReading(text string) model.SearchQueryReading {
	reading := readSearchQuery(text)
	return model.SearchQueryReading{Operators: reading.operators(), Broadened: reading.broadened()}
}

// searchAppliedFilters is the filters this search narrowed every leg by, which a bar shows as its
// chips: each is absent from the answer when the request named none.
func searchAppliedFilters(parameters searchParameters) model.SearchFilters {
	filters := model.SearchFilters{Project: parameters.project, Status: parameters.status, Author: parameters.author}
	if parameters.since != nil {
		filters.UpdatedSince = parameters.since.Format(time.RFC3339Nano)
	}
	return filters
}

// parseSearchFilters reads the three filters a request may narrow every leg by - the issue status
// a row belongs to, who wrote the item it names, and when that item was last written - and writes
// the refusal itself when one of them is repeated, blank or unreadable, each under a code of its
// own, exactly as the project and the page parameters beside them are refused. A filter the
// request leaves out narrows nothing.
func parseSearchFilters(w http.ResponseWriter, query url.Values) (status, author string, since *time.Time, ok bool) {
	if values, present := query["status"]; present {
		status = strings.TrimSpace(values[0])
		if len(values) != 1 || !model.IsIssueStatus(status) {
			writeError(w, "INVALID_STATUS", http.StatusBadRequest,
				"status must be one of "+strings.Join(model.IssueStatuses, ", "))
			return "", "", nil, false
		}
	}
	if values, present := query["author"]; present {
		author = strings.TrimSpace(values[0])
		if len(values) != 1 || !searchAuthorPattern.MatchString(author) || itemblocks.Units16(author) > searchAuthorMax {
			writeError(w, "INVALID_AUTHOR", http.StatusBadRequest, fmt.Sprintf(
				"author must be one actor id - a person's login or a session id, with no whitespace - of at most %d characters",
				searchAuthorMax))
			return "", "", nil, false
		}
	}
	if values, present := query["updated_since"]; present {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(values[0]))
		if len(values) != 1 || err != nil {
			writeError(w, "INVALID_UPDATED_SINCE", http.StatusBadRequest, "updated_since must be an RFC3339 timestamp")
			return "", "", nil, false
		}
		since = &parsed
	}
	return status, author, since, true
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
	if length := itemblocks.Units16(searchText); length > contracts.SearchQueryMax {
		tooLong := capExceededError("q", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; "+contracts.SearchQueryHint)
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	if project != "" && !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	status, author, since, ok := parseSearchFilters(w, query)
	if !ok {
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

	parameters := searchParameters{text: searchText, project: project, status: status, author: author,
		since: since, limit: limit, offset: offset}
	response, err := s.runSearch(r.Context(), parameters)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// What search made of the words, and which filters it narrowed every leg by, for a caller
	// that shows a person what it did with them; answered on every path, the empty answer of a
	// query with no lexemes included.
	response.Query = searchQueryReading(searchText)
	response.Filters = searchAppliedFilters(parameters)
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
// stopword-only or operator-only query never pays for an embedding it would throw away; a query
// that names an issue by key answers that issue even when it holds no lexeme any item matches,
// which is why the keys are resolved beside that check rather than after it. It tries meaning
// search first when an embedder is configured, falling back to keyword-only (runFusedSearchQuery)
// only when the query's own embedding could not be had - never when the meaning query itself then
// fails, which is a real error the caller should see as a 500, not something to paper over as a
// degraded answer.
func (s *server) runSearch(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	var nodes int
	if err := s.deps.Store.Pool.QueryRow(ctx,
		"select numnode(websearch_to_tsquery('english', $1))", parameters.text).Scan(&nodes); err != nil {
		return model.SearchResponse{}, err
	}
	keys, err := s.searchOwnIssueKeys(ctx, parameters.text, parameters.project)
	if err != nil {
		return model.SearchResponse{}, err
	}
	parameters.ownKeys = keys
	if nodes == 0 && len(keys) == 0 {
		return model.SearchResponse{Results: []model.SearchResult{}, Limit: parameters.limit, Offset: parameters.offset}, nil
	}
	if s.deps.Embedder == nil {
		return s.runKeywordOnlyDegraded(ctx, parameters)
	}
	vector, err := s.embedQuery(ctx, parameters.text)
	if err != nil {
		slog.Warn("dispatch search: query embedding unavailable, answering keyword-only", "error", err)
		return s.runKeywordOnlyDegraded(ctx, parameters)
	}
	return s.runMeaningSearch(ctx, parameters, vector)
}

// runKeywordOnlyDegraded runs the keyword-only fallback and stamps the response Degraded on
// success: the one thing both of runSearch's fallback branches (no embedder configured, or the
// query's own embedding failing) do identically. runSearch has already run the numnode check
// before ever reaching here.
func (s *server) runKeywordOnlyDegraded(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	response, err := s.runFusedSearchQuery(ctx, parameters)
	if err == nil {
		response.Degraded = contracts.SearchDegradedEmbedderUnavailable
	}
	return response, err
}

// runFusedSearchQuery runs searchQuery, assuming nodes > 0 or an issue named by key already,
// which its caller (runKeywordOnlyDegraded, after runSearch's checks) must have established
// first. The text is assumed already validated (length, non-empty after trimming); an empty
// project searches every project, and an empty filter narrows nothing.
func (s *server) runFusedSearchQuery(ctx context.Context, parameters searchParameters) (model.SearchResponse, error) {
	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(ctx, searchQuery, searchQueryArguments(parameters)...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, parameters.text, parameters.limit, parameters.offset)
	if err != nil {
		return model.SearchResponse{}, err
	}
	response.TookMS = time.Since(started).Milliseconds()
	return response, nil
}

// runMeaningSearch is runFusedSearchQuery's counterpart once a query embedding exists: same row
// shape, searchQueryMeaning in place of searchQuery. Its only caller, runSearch, has already run
// the numnode check before ever reaching here (so it never pays for an embedding it would throw
// away).
//
// It runs in a read transaction of its own, the one place searchMeaningScan's settings apply
// (beginMeaningScan).
func (s *server) runMeaningSearch(ctx context.Context, parameters searchParameters, vector []float32) (model.SearchResponse, error) {
	started := time.Now()
	tx, err := s.beginMeaningScan(ctx)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, searchQueryMeaning,
		searchMeaningQueryArguments(parameters, vector, s.deps.Embedder.Identity())...)
	if err != nil {
		return model.SearchResponse{}, err
	}
	defer rows.Close()

	response, err := scanSearchRows(rows, parameters.text, parameters.limit, parameters.offset)
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
		var matchBlockID *string
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
			&matchBlockID,
			&rank,
			&headline,
		); err != nil {
			return model.SearchResponse{}, err
		}
		// A page past the last reachable row is one row of totals and no hit.
		if kind == nil {
			continue
		}
		result.Kind, result.ID, result.Rank, result.BlockID = *kind, *id, *rank, matchBlockID
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
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, matchBlockID, q)
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
//
// matchBlock is the block a meaning hit matched (INFRA-370), which a document hit names in its
// fragment so the reader lands on the passage rather than at the top of the document. A comment,
// a message and an ask keep the href they had: no item page reads a fragment, and the ask's own
// fragment names the document block it is typed on, which is another block than the part of the
// ask that matched.
func searchHref(kind string, owner model.SearchOwner, artifact *model.SearchArtifact, primary bool, id string, blockID, matchBlock *string, query string) string {
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
		// A document hit always has its artifact, so documentHref above is its document's.
		href := documentHref
		href += "?q=" + url.QueryEscape(query)
		if matchBlock != nil {
			href += "#b-" + text.EncodeBlockID(*matchBlock)
		}
		return href
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
