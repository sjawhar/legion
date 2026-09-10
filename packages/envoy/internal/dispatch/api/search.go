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
  select 'issue' as kind, i.key as issue_key, null::uuid as artifact_id, i.key as id,
         ts_rank_cd(i.search, q.tsq) as rank, i.title as text
    from issues i, q where i.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'document', a.issue_key, a.id, a.id::text, ts_rank_cd(v.search, q.tsq), v.markdown
    from artifacts a
    join issues i on i.key = a.issue_key
    join lateral (select v.search, v.markdown from artifact_versions v
                   where v.artifact_id = a.id order by v.number desc limit 1) v on true, q
   where a.kind = 'doc' and v.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'comment', c.issue_key, (c.anchor->>'artifact_id')::uuid, c.id::text, ts_rank_cd(c.search, q.tsq), c.body
    from comments c join issues i on i.key = c.issue_key, q
   where c.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'ask', k.issue_key, (k.anchor->>'artifact_id')::uuid, k.id::text, ts_rank_cd(k.search, q.tsq),
         k.question || ' ' || coalesce(k.answer->>'text', '')
    from asks k join issues i on i.key = k.issue_key, q
   where k.search @@ q.tsq and ($2 = '' or i.project_key = $2)
  union all
  select 'message', m.issue_key, null, m.id::text, ts_rank_cd(m.search, q.tsq), m.body
    from messages m join issues i on i.key = m.issue_key, q
   where m.search @@ q.tsq and ($2 = '' or i.project_key = $2)
),
ranked as (
  select h.kind, h.issue_key, h.artifact_id, h.id, h.rank, h.text, i.title as issue_title, i.status, i.updated_at
    from hits h join issues i on i.key = h.issue_key
   order by h.rank desc, i.updated_at desc, h.kind, h.id
   limit $3
)
select r.kind, r.issue_key, r.issue_title, r.status, ar.slug, ar.name, coalesce(ar.is_primary, false) as is_primary, r.id, r.rank,
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
		var slug, name *string
		var primary bool
		var headline string
		if err := rows.Scan(
			&result.Kind,
			&result.Issue.Key,
			&result.Issue.Title,
			&result.Issue.Status,
			&slug,
			&name,
			&primary,
			&result.ID,
			&result.Rank,
			&headline,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if slug != nil {
			result.Artifact = &model.SearchArtifact{Slug: *slug, Name: *name}
		}
		result.Snippet = markSnippet(headline)
		result.Href = searchHref(result.Kind, result.Issue.Key, result.Artifact, primary, result.ID, searchText)
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

func searchHref(kind, issueKey string, artifact *model.SearchArtifact, primary bool, id, query string) string {
	issueHref := "/issues/" + issueKey
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
		return issueHref + "/asks/" + id
	case "message":
		return issueHref + "/log"
	default:
		return ""
	}
}
