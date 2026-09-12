package api

import (
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	markStart          = "\uE000" // private-use sentinels: ts_headline writes them, markSnippet turns them into <mark>
	markEnd            = "\uE001"
	headlineOptions    = "StartSel=" + markStart + ", StopSel=" + markEnd + ", MaxWords=24, MinWords=12, MaxFragments=1"
)

const searchQuery = `
with q as (select websearch_to_tsquery('english', $1) as tsq, $4::text as term),
hits as (
  select 'issue' as kind, i.key as issue_key, null::uuid as owner_artifact_id, null::uuid as artifact_id, i.key as id, null::text as block_id,
         ts_rank_cd(i.search, q.tsq) as rank, i.title as text, i.title as issue_title, i.status as issue_status,
         null::text as owner_project, null::text as owner_slug, null::text as owner_name, i.updated_at
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document', a.issue_key, case when a.issue_key is null then a.id else null::uuid end, a.id, a.id::text, null::text,
         ts_rank_cd(v.search, q.tsq), v.markdown, i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, v.created_at)
    from artifacts a
    left join issues i on i.key = a.issue_key
    join projects p on p.key = a.project_key
    join lateral (select v.search, v.markdown, v.created_at from artifact_versions v
                  where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or p.key = $2)
  union all
  select 'comment', c.issue_key, c.artifact_id, coalesce(c.artifact_id, (c.anchor->>'artifact_id')::uuid), c.id::text, null::text,
         ts_rank_cd(c.search, q.tsq), c.body, i.title, i.status, p.key, a.slug, a.name,
         coalesce(i.updated_at, c.created_at)
    from comments c
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
    left join projects p on p.key = a.project_key, q
   where c.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'ask', k.issue_key, case when k.issue_key is null then coalesce(k.artifact_id, k.block_artifact_id) else null::uuid end, coalesce(k.artifact_id, k.block_artifact_id, (k.anchor->>'artifact_id')::uuid), k.id::text, k.block_id,
         ts_rank_cd(k.search, q.tsq), k.question || ' ' || coalesce(k.options::text, '') || ' ' || coalesce(k.answer->>'text', ''), i.title, i.status,
         p.key, a.slug, a.name, coalesce(i.updated_at, k.created_at)
    from asks k
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
    left join projects p on p.key = a.project_key, q
   where k.search @@ q.tsq and ($2 = '' or coalesce(i.project_key, p.key) = $2)
  union all
  select 'message', m.issue_key, null::uuid, null::uuid, m.id::text, null::text, ts_rank_cd(m.search, q.tsq), m.body,
         i.title, i.status, null::text, null::text, null::text, i.updated_at
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
),
ranked as (
  select * from hits
   order by rank desc, updated_at desc, kind, id
   limit $3
)
select r.kind, coalesce(r.issue_key, r.owner_project), coalesce(r.issue_title, r.owner_name),
       coalesce(r.issue_status, 'document'),
       case when r.owner_artifact_id is null then 'issue' else 'document' end, r.issue_key,
       r.owner_project, r.owner_slug, r.owner_artifact_id::text, r.owner_name,
       ar.slug, ar.name, coalesce(ar.is_primary, false), r.id, r.block_id, r.rank,
       ts_headline('english',
         case when q.term <> '' and strpos(lower(r.text), lower(q.term)) > 0
              then substr(r.text, greatest(1, strpos(lower(r.text), lower(q.term)) - 1500), 4000)
              else left(r.text, 4000) end,
         q.tsq, $5) as headline
  from ranked r left join artifacts ar on ar.id = r.artifact_id, q
 order by r.rank desc, r.updated_at desc, r.kind, r.id
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

	limit := searchDefaultLimit
	if query.Has("limit") {
		parsed, err := strconv.Atoi(query.Get("limit"))
		if err != nil || parsed < 1 || parsed > searchMaxLimit {
			writeError(w, "INVALID_LIMIT", http.StatusBadRequest, "limit must be an integer from 1 to 50")
			return
		}
		limit = parsed
	}

	var nodes int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select numnode(websearch_to_tsquery('english', $1))", searchText).Scan(&nodes); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if nodes == 0 {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "query has no searchable terms")
		return
	}

	started := time.Now()
	rows, err := s.deps.Store.Pool.Query(r.Context(), searchQuery, searchText, strings.TrimSpace(query.Get("project")), limit, firstTerm(searchText), headlineOptions)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	results := []model.SearchResult{}
	for rows.Next() {
		var result model.SearchResult
		var ownerKind string
		var ownerKey, ownerProject, ownerSlug, ownerArtifactID, ownerName, slug, name *string
		var primary bool
		var headline string
		var blockID *string
		if err := rows.Scan(
			&result.Kind,
			&result.Issue.Key,
			&result.Issue.Title,
			&result.Issue.Status,
			&ownerKind,
			&ownerKey,
			&ownerProject,
			&ownerSlug,
			&ownerArtifactID,
			&ownerName,
			&slug,
			&name,
			&primary,
			&result.ID,
			&blockID,
			&result.Rank,
			&headline,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if ownerKind == "issue" {
			result.Owner = model.SearchOwner{Kind: ownerKind, Key: *ownerKey}
		} else {
			result.Owner = model.SearchOwner{
				Kind:       ownerKind,
				Project:    *ownerProject,
				Slug:       *ownerSlug,
				ArtifactID: *ownerArtifactID,
				Name:       *ownerName,
			}
		}
		if slug != nil {
			result.Artifact = &model.SearchArtifact{Slug: *slug, Name: *name}
		}
		result.Snippet = markSnippet(headline)
		result.Href = searchHref(result.Kind, result.Owner, result.Artifact, primary, result.ID, blockID, searchText)
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, model.SearchResponse{Results: results, TookMS: time.Since(started).Milliseconds()})
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

func searchHref(kind string, owner model.SearchOwner, artifact *model.SearchArtifact, primary bool, id string, blockID *string, query string) string {
	if owner.Kind == "document" {
		documentHref := "/projects/" + url.PathEscape(owner.Project) + "/documents/" + url.PathEscape(owner.Slug)
		switch kind {
		case "document":
			return documentHref + "?q=" + url.QueryEscape(query)
		case "comment":
			return documentHref + "?comment=" + url.QueryEscape(id)
		case "ask":
			if blockID != nil {
				return documentHref + "#b-" + url.PathEscape(*blockID)
			}
			return documentHref + "?ask=" + url.QueryEscape(id)
		}
	}

	issueHref := "/issues/" + owner.Key
	switch kind {
	case "issue":
		return issueHref
	case "document":
		if primary {
			return issueHref + "/spec?q=" + url.QueryEscape(query)
		}
		return issueHref + "/artifacts/" + url.PathEscape(artifact.Slug) + "?q=" + url.QueryEscape(query)
	case "comment":
		return issueHref + "/comments/" + id
	case "ask":
		if blockID != nil {
			return issueHref + "/spec#b-" + url.PathEscape(*blockID)
		}
		return issueHref + "/asks/" + id
	case "message":
		return issueHref + "/log"
	default:
		return ""
	}
}
