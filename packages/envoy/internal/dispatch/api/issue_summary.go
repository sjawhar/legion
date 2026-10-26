package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// The summary reads serve what a rendered dispatch:// reference shows, an issue's title and status
// and a document's name, kind and latest version, at a cost a page linking a hundred items can pay
// on every event that touches them: the full reads (GET /api/v1/issues/{key}, GET
// /api/v1/projects/{key}/artifacts/{slug}) read every version, approval, child and open ask, and
// an issue's route roster. They are open to every signed-in caller, as those reads are.

// issueSummary is GET /api/v1/issues/{key}/summary: the issue's key, project, title and status,
// and its documents and files in the order the full read lists its artifacts, so an issue link and
// its documents' links share one read.
type issueSummary struct {
	Key       string                 `json:"key"`
	Project   string                 `json:"project"`
	Title     string                 `json:"title"`
	Status    string                 `json:"status"`
	Documents []issueSummaryDocument `json:"documents"`
}

// issueSummaryDocument is one artifact of an issue summary. LatestVersion is its greatest version
// number, null only for an artifact with no version, which no committed write leaves.
type issueSummaryDocument struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	LatestVersion *int   `json:"latest_version"`
}

// documentSummary is GET /api/v1/projects/{key}/artifacts/{slug}/summary: the project document
// the full read resolves that reference to, with its greatest version number.
type documentSummary struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	LatestVersion *int   `json:"latest_version"`
}

// summaryLatestVersion is the select-list term an artifact summary reads its latest version
// through, over artifacts aliased a: one index probe of artifact_versions' (artifact_id, number)
// key per artifact, inside the summary's one query.
const summaryLatestVersion = `(select max(v.number) from artifact_versions v where v.artifact_id = a.id)`

func (s *server) getIssueSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	var summary issueSummary
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select key, project_key, title, status from issues where key = $1
	`, r.PathValue("key")).Scan(&summary.Key, &summary.Project, &summary.Title, &summary.Status); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.id::text, a.slug, a.name, a.kind, `+summaryLatestVersion+`
		from artifacts a where a.issue_key = $1 order by a.created_at, a.id
	`, summary.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	summary.Documents, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (issueSummaryDocument, error) {
		var document issueSummaryDocument
		err := row.Scan(&document.ID, &document.Slug, &document.Name, &document.Kind, &document.LatestVersion)
		return document, err
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if summary.Documents == nil {
		summary.Documents = []issueSummaryDocument{}
	}
	WriteJSON(w, http.StatusOK, summary)
}

func (s *server) getProjectDocumentSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	q := s.deps.Store.Pool
	read := func(predicate string, value string) (documentSummary, error) {
		var summary documentSummary
		err := q.QueryRow(ctx, `
			select a.id::text, coalesce(a.project_key, ''), a.slug, a.name, a.kind, `+summaryLatestVersion+`
			from artifacts a where `+predicate, value).Scan(
			&summary.ID, &summary.Project, &summary.Slug, &summary.Name, &summary.Kind, &summary.LatestVersion,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return documentSummary{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return summary, err
	}
	summary, err := resolveDocumentReference(ctx, s, q, artifactTarget{Project: r.PathValue("key")}, r.PathValue("slug"),
		func(refKey string) (documentSummary, error) { return read(`a.ref_key = $1`, refKey) },
		func(id string) (documentSummary, error) { return read(`a.id = $1::uuid`, id) },
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, summary)
}
