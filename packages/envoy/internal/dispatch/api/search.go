package api

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	// searchFusionK is reciprocal rank fusion's constant: a row at position p of its kind's list
	// scores 1/(searchFusionK+p). 60 is the value of the paper that introduced the method (Cormack,
	// Clarke and Buettcher, 2009) and Elasticsearch's and OpenSearch's default; nothing tunes it.
	searchFusionK   = 60
	markStart       = "\uE000" // private-use sentinels: ts_headline writes them, markSnippet turns them into <mark>
	markEnd         = "\uE001"
	headlineOptions = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=24, MinWords=12, MaxFragments=1"
)

// searchQuery ranks each kind of content on its own list, then merges the lists by reciprocal
// rank fusion, so a page takes each kind's best in turn: its top holds the best issue, document,
// ask, comment and message. Each list orders its matches by
// ts_rank_cd, then recency, then id, and keeps its best $7 (contracts.SearchKindDepth): the
// count(*) over () beside it is every match, taken before that cut, and the deeper matches are
// never returned at any offset. An issue whose key is the whole query heads the issue list, since
// ts_rank_cd scores its own key no higher than another issue's title citing it. Rows that score
// alike (every kind's first row scores 1/61) are ordered issue, document, ask, comment, message
// (the thing before what is inside it), then recency, then id, which makes the order total, so
// consecutive offsets cover the reachable rows once while the corpus holds still. The page is cut
// before ts_headline runs, so only the rows it returns pay for a snippet, and totals is joined
// outside the page, so a page past the end still answers how many rows the query matches.
//
// Parameters: $1 q, $2 project (empty for every project), $3 limit, $4 firstTerm,
// $5 headlineOptions, $6 offset, $7 contracts.SearchKindDepth, $8 searchFusionK.
const searchQuery = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term, upper(btrim($1)) as own_key),
issue_leg as (
  select leg.*, row_number() over (order by own desc, r desc, updated_at desc, id) as pos from (
    select 'issue' as kind, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
           i.title as text, i.title as issue_title, i.status as issue_status,
           null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
           i.key = q.own_key as own, ts_rank_cd(i.search, q.tsq) as r, count(*) over () as matches
      from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
     order by own desc, r desc, updated_at desc, id
     limit $7) leg
),
document_leg as (
  select leg.*, row_number() over (order by own desc, r desc, updated_at desc, id) as pos from (
    select 'document' as kind, a.issue_key, case when a.issue_key is null then a.id else null::uuid end as owner_artifact_id, a.id as artifact_id, a.id::text as id, null::text as block_id,
           v.markdown as text, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
           coalesce(i.updated_at, v.created_at) as updated_at,
           false as own, ts_rank_cd(v.search, q.tsq) as r, count(*) over () as matches
      from artifacts a
      left join issues i on i.key = a.issue_key
      join projects p on p.key = a.project_key
      join lateral (select v.search, v.markdown, v.created_at from artifact_versions v
                    where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
     where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
     order by own desc, r desc, updated_at desc, id
     limit $7) leg
),
comment_leg as (
  select leg.*, row_number() over (order by own desc, r desc, updated_at desc, id) as pos from (
    select 'comment' as kind, c.issue_key, c.artifact_id as owner_artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid) as artifact_id, c.id::text as id, null::text as block_id,
           c.body as text, i.title as issue_title, i.status as issue_status, p.key as owner_project, a.slug as owner_slug, a.name as owner_name,
           coalesce(i.updated_at, c.created_at) as updated_at,
           false as own, ts_rank_cd(c.search, q.tsq) as r, count(*) over () as matches
      from comments c
      left join issues i on i.key = c.issue_key
      left join artifacts a on a.id = c.artifact_id
      left join projects p on p.key = a.project_key, q
     where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     order by own desc, r desc, updated_at desc, id
     limit $7) leg
),
ask_leg as (
  select leg.*, row_number() over (order by own desc, r desc, updated_at desc, id) as pos from (
    select 'ask' as kind, k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end as owner_artifact_id,
           coalesce(k.artifact_id, k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) as artifact_id, k.id::text as id, k.block_id,
           k.question || ' ' || coalesce(k.options::text, '') || ' ' || coalesce(k.answer->>'text', '') as text, i.title as issue_title, i.status as issue_status,
           p.key as owner_project, a.slug as owner_slug, a.name as owner_name, coalesce(i.updated_at, k.created_at) as updated_at,
           false as own, ts_rank_cd(k.search, q.tsq) as r, count(*) over () as matches
      from asks k
      left join issues i on i.key = k.issue_key
      left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
      left join projects p on p.key = a.project_key, q
     where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
     order by own desc, r desc, updated_at desc, id
     limit $7) leg
),
message_leg as (
  select leg.*, row_number() over (order by own desc, r desc, updated_at desc, id) as pos from (
    select 'message' as kind, m.issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, m.id::text as id, null::text as block_id,
           m.body as text, i.title as issue_title, i.status as issue_status,
           null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at,
           false as own, ts_rank_cd(m.search, q.tsq) as r, count(*) over () as matches
      from messages m join issues i on i.key = m.issue_key, q
     where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
     order by own desc, r desc, updated_at desc, id
     limit $7) leg
),
legs as (
  select * from issue_leg union all select * from document_leg union all select * from comment_leg
  union all select * from ask_leg union all select * from message_leg
),
fused as (select kind, id, sum(1.0 / ($8 + pos)) as score from legs group by kind, id),
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
         search_text(case when q.term <> '' and strpos(lower(r.text), lower(q.term)) > 0
              then substr(r.text, greatest(1, strpos(lower(r.text), lower(q.term)) - 1500), 4000)
              else left(r.text, 4000) end),
         q.tsq, $5) as headline
  from totals t cross join q
  left join (page r left join artifacts ar on ar.id = r.artifact_id) on true
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
	if query.Has("limit") {
		parsed, err := strconv.Atoi(query.Get("limit"))
		if err != nil || parsed < 1 || parsed > searchMaxLimit {
			writeError(w, "INVALID_LIMIT", http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = parsed
	}
	offset := 0
	if query.Has("offset") {
		parsed, err := strconv.Atoi(query.Get("offset"))
		if err != nil || parsed < 0 {
			writeError(w, "INVALID_OFFSET", http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = parsed
	}

	var nodes int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select numnode(websearch_to_tsquery('english', $1))", searchText).Scan(&nodes); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if nodes == 0 {
		WriteJSON(w, http.StatusOK, model.SearchResponse{Results: []model.SearchResult{}, Limit: limit, Offset: offset})
		return
	}

	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(r.Context(), searchQuery, searchText, project, limit, firstTerm(searchText), headlineOptions,
		offset, contracts.SearchKindDepth, searchFusionK)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

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
			s.writeHandlerError(w, err)
			return
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
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, searchText)
		response.Results = append(response.Results, result)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	response.TookMS = time.Since(started).Milliseconds()
	WriteJSON(w, http.StatusOK, response)
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
