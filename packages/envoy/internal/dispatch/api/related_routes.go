package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// relatedRequest is the no-write related route's question, from GET's query or POST's body.
type relatedRequest struct {
	Title        string `json:"title"`
	Text         string `json:"text"`
	ExcludeIssue string `json:"exclude_issue"`
	KeywordsOnly bool   `json:"keywords_only"`
}

// checkText is what the check reads: the title and the text, each alone when the other is blank,
// on lines of their own as a new issue's title and spec are.
func (request relatedRequest) checkText() string {
	title, text := strings.TrimSpace(request.Title), strings.TrimSpace(request.Text)
	if title == "" || text == "" {
		return title + text
	}
	return title + "\n" + text
}

// getRelated answers GET /api/v1/projects/{key}/related: the related-item check over the query's
// title and text. They ride in the URL, so they are capped together as `dispatch search` caps q;
// a longer text goes through POST.
func (s *server) getRelated(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	request := relatedRequest{
		Title: query.Get("title"), Text: query.Get("text"), ExcludeIssue: strings.TrimSpace(query.Get("exclude_issue")),
	}
	switch query.Get("keywords_only") {
	case "", "false":
	case "true":
		request.KeywordsOnly = true
	default:
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "keywords_only must be true or false")
		return
	}
	if length := len16(request.checkText()); length > contracts.SearchQueryMax {
		tooLong := capExceededError("title and text", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; send a longer text with POST")
		return
	}
	s.answerRelated(w, r, request)
}

// postRelated answers POST /api/v1/projects/{key}/related: the check over the body's title and
// text. The title is capped as an issue's is, and the text, like a new issue's spec, by the body's
// own bound (decodeJSON's maxJSONRequestBytes).
func (s *server) postRelated(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var request relatedRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if length := len16(request.Title); length > contracts.IssueTitleMax {
		capExceeded(w, "title", length, contracts.IssueTitleMax)
		return
	}
	request.ExcludeIssue = strings.TrimSpace(request.ExcludeIssue)
	s.answerRelated(w, r, request)
}

// answerRelated runs the check both forms share against the path's project, recording nothing:
// it has no source to record, and a reader trying words out must not count as a write's check.
func (s *server) answerRelated(w http.ResponseWriter, r *http.Request, request relatedRequest) {
	project := r.PathValue("key")
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("key", project))
		return
	}
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if request.ExcludeIssue != "" {
		if err := s.requireProjectIssue(r, project, request.ExcludeIssue); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, s.relatedCheck(r.Context(), relatedInput{
		project:      project,
		text:         request.checkText(),
		excludeIssue: request.ExcludeIssue,
		keywordsOnly: request.KeywordsOnly,
	}))
}

// requireProjectIssue refuses an exclude_issue that is not an issue of project, as a create
// refuses a parent it cannot find or that sits in another project.
func (s *server) requireProjectIssue(r *http.Request, project, key string) error {
	if !issueKeyPattern.MatchString(key) {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue %q is not an issue key", key)
	}
	var issueProject string
	err := s.deps.Store.Pool.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&issueProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue issue %s not found", key)
	}
	if err != nil {
		return err
	}
	if issueProject != project {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue %s is not an issue of project %s", key, project)
	}
	return nil
}

// relatedChecksReportQuery counts the related_checks rows the filters select ($1 project, $2
// from, $3 to, $4 source kind; each null for no filter). The percentiles are percentile_disc,
// which ignores nulls like every aggregate here, so each is a time some check took rather than
// one interpolated between two checks.
const relatedChecksReportQuery = `
with checks as (
  select source_kind, outcome, search, reason, keyword_ms, total_ms
    from related_checks
   where ($1::text is null or project_key = $1)
     and ($2::timestamptz is null or created_at >= $2)
     and ($3::timestamptz is null or created_at < $3)
     and ($4::text is null or source_kind = $4)
)
select
  (select count(*) from checks),
  (select coalesce(jsonb_object_agg(source_kind, n), '{}') from (select source_kind, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(outcome, n), '{}') from (select outcome, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(mode, n), '{}') from (select coalesce(search, 'not_made') as mode, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(reason, n), '{}') from (select reason, count(*) as n from checks where reason is not null group by 1) c),
  (select count(*) from checks where outcome in ('found', 'none')),
  (select count(*) from checks where reason = 'timeout'),
  count(keyword_ms), percentile_disc(0.5) within group (order by keyword_ms),
  percentile_disc(0.95) within group (order by keyword_ms), max(keyword_ms), count(*) filter (where keyword_ms > 300),
  count(total_ms), percentile_disc(0.5) within group (order by total_ms),
  percentile_disc(0.95) within group (order by total_ms), max(total_ms), count(*) filter (where total_ms > 300)
from checks`

// relatedSourceKinds are related_checks.source_kind's values, the records route's source_kind
// filter.
var relatedSourceKinds = map[string]bool{"issue": true, "ask": true, "decision_block": true}

// getRelatedChecks answers GET /api/v1/related-checks: model.RelatedChecksReport over the checks
// recorded within [from, to), of one project and one source kind when given.
func (s *server) getRelatedChecks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	var report model.RelatedChecksReport
	if project := strings.TrimSpace(query.Get("project")); project != "" {
		if !projectKeyPattern.MatchString(project) {
			writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
			return
		}
		report.Project = &project
	}
	if kind := strings.TrimSpace(query.Get("source_kind")); kind != "" {
		if !relatedSourceKinds[kind] {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("source_kind %q is not issue, ask or decision_block", kind))
			return
		}
		report.SourceKind = &kind
	}
	for _, bound := range []struct {
		name   string
		target **time.Time
	}{{"from", &report.From}, {"to", &report.To}} {
		if strings.TrimSpace(query.Get(bound.name)) == "" {
			continue
		}
		parsed, err := parseOptionalRFC3339(query, bound.name, time.Time{})
		if err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
			return
		}
		*bound.target = &parsed
	}
	if report.From != nil && report.To != nil && !report.From.Before(*report.To) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "from must be before to")
		return
	}

	keyword, total := &report.KeywordMS, &report.TotalMS
	if err := s.deps.Store.Pool.QueryRow(r.Context(), relatedChecksReportQuery,
		report.Project, report.From, report.To, report.SourceKind,
	).Scan(
		&report.Total, &report.BySourceKind, &report.ByOutcome, &report.BySearch, &report.ByReason,
		&report.Completed, &report.SkippedForTime,
		&keyword.Count, &keyword.P50, &keyword.P95, &keyword.Max, &keyword.Over300MS,
		&total.Count, &total.P50, &total.P95, &total.Max, &total.Over300MS,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, report)
}
