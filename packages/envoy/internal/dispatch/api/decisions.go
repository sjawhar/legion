package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type issueDecisions struct {
	Document      *string          `json:"document"`
	OpenQuestions []issueAskRow    `json:"open_questions"`
	Decisions     []model.Decision `json:"decisions"`
}

func (s *server) getIssueDecisions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	answer, err := s.issueDecisionNodes(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version := 0
	if raw := r.URL.Query().Get("version"); raw != "" {
		version, err = strconv.Atoi(raw)
		if err != nil || version < 1 {
			writeError(w, "INVALID_VERSION", http.StatusBadRequest, "version must be positive")
			return
		}
	}
	artifactID := r.URL.Query().Get("artifact")
	if version > 0 {
		document, loadErr := s.loadArtifact(r.Context(), s.deps.Store.Pool, artifactID)
		if loadErr != nil {
			s.writeHandlerError(w, loadErr)
			return
		}
		if document.IssueKey == nil || *document.IssueKey != r.PathValue("key") {
			writeError(w, "TARGET_NOT_FOUND", http.StatusNotFound, "the document is not on this issue")
			return
		}
	}
	if err = s.pinDecisionPassages(r.Context(), answer.Decisions, artifactID, version); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// artifactReference is a document's own dispatch:// address.
func artifactReference(document model.Artifact) string {
	return refs.ArtifactRef(document.IssueKey, document.Project, document.SessionID, document.Slug,
		document.Kind, document.Primary)
}

// issueSpecDocument is the issue's primary document, the spec a decision's links point into. An
// issue with none, and an issue that does not exist, answer found false: the view is a read, and
// an issue with no spec simply holds no decision link.
func (s *server) issueSpecDocument(ctx context.Context, key string) (model.Artifact, bool, error) {
	artifact, err := s.loadArtifactRow(ctx, s.deps.Store.Pool, s.deps.Store.Pool.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where issue_key = $1 and is_primary and kind = 'doc'
	`, key))
	if err != nil {
		if artifactMissing(err) {
			return model.Artifact{}, false, nil
		}
		return model.Artifact{}, false, err
	}
	return artifact, true, nil
}

func artifactMissing(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.code == "ARTIFACT_NOT_FOUND" || apiErr.code == "ARTIFACT_REFERENCE_AMBIGUOUS"
}

func (s *server) issueDecisionQuestions(ctx context.Context, key string) ([]issueAskRow, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+askReadColumns+` `+askReadFrom+`
    where a.issue_key = $1 and a.state = 'open' and exists (
      select 1 from artifacts d join document_blocks b on b.artifact_id=d.id
      where d.issue_key=$1 and d.kind='doc' and d.id=a.block_artifact_id
        and b.block_id=a.block_id and b.type='ask'
    )
		order by a.created_at, a.id
  `, key)
	if err != nil {
		return nil, err
	}
	questions := []issueAskRow{}
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		questions = append(questions, issueAskRow{Ask: ask, LastReply: reply})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	asks := make([]*model.Ask, len(questions))
	for index := range questions {
		asks[index] = &questions[index].Ask
	}
	if err := s.attachOpenedEventIDs(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	if err := attachAskBacklinkCounts(ctx, s.deps.Store.Pool, asks); err != nil {
		return nil, err
	}
	return questions, nil
}

func decisionTextSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// sliceMarkdown is markdown[from:to] held to the string's own bounds: a block range the document
// read gives lies inside it, and a range that does not answers "" rather than panicking.
func sliceMarkdown(markdown string, from, to int) string {
	if from < 0 || to > len(markdown) || from > to {
		return ""
	}
	return markdown[from:to]
}

// decisionKeeps is every decision of the document a person has kept, by heading block.
func decisionKeeps(ctx context.Context, q queryer, artifactID string) (map[string]model.DecisionKeep, error) {
	rows, err := q.Query(ctx, `
		select block_id, text_sha256, changed_version, kept_by, kept_at
		from decision_keeps where artifact_id = $1::uuid
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keeps := map[string]model.DecisionKeep{}
	for rows.Next() {
		keep := model.DecisionKeep{ArtifactID: artifactID}
		if err := rows.Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion,
			&keep.KeptBy, &keep.KeptAt); err != nil {
			return nil, err
		}
		keeps[keep.BlockID] = keep
	}
	return keeps, rows.Err()
}

// keepDecision is POST /api/v1/artifacts/{id}/blocks/{block_id}/keep {"text_sha256": "<sum>"}:
// the caller agrees with the agent's decision node, as the text they read it at.
// The sum is the `text_sha256` the view served them, and a decision whose text has changed
// since is 409 DECISION_CHANGED with nothing written: a Keep over text nobody read is the one
// thing Keep exists to prevent, as `409 ASK_EDITED` holds an answer to the wording it was given
// on. A block that is not a current, unreplaced agent decision is 404 TARGET_NOT_FOUND.
func (s *server) keepDecision(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input struct {
		TextSHA256 string `json:"text_sha256"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.TextSHA256 == "" {
		writeError(w, "INVALID_KEEP", http.StatusBadRequest,
			"text_sha256 is the text_sha256 the decision was read at")
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	blockID := r.PathValue("block_id")
	decision, err := s.keptDecision(r.Context(), document, blockID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if decision.TextSHA256 != input.TextSHA256 {
		writeError(w, "DECISION_CHANGED", http.StatusConflict,
			"the decision changed since it was read; read it again and keep what it says now")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	keep := model.DecisionKeep{ArtifactID: document.ID}
	if err := tx.QueryRow(r.Context(), `
		insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
		values ($1::uuid, $2, $3, $4, $5)
		on conflict (artifact_id, block_id) do update
			set text_sha256 = excluded.text_sha256, changed_version = excluded.changed_version,
				kept_by = excluded.kept_by, kept_at = now()
		returning block_id, text_sha256, changed_version, kept_by, kept_at
	`, document.ID, blockID, decisionTextSum(decision.Text), decision.ChangedVersion,
		model.CanonicalLogin(actor.ID)).
		Scan(&keep.BlockID, &keep.TextSHA256, &keep.ChangedVersion, &keep.KeptBy, &keep.KeptAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, issueOwner(*document.IssueKey).event("decision.kept", actor, map[string]any{
		"artifact_id":     document.ID,
		"block_id":        blockID,
		"changed_version": keep.ChangedVersion,
	}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, keep)
}

// keptDecision resolves the actual decision node in any document of its issue. An ask is
// answered through its own answer route; Keep is only for an agent's current choice.
func (s *server) keptDecision(ctx context.Context, document model.Artifact, blockID string) (model.Decision, error) {
	notFound := errorf(http.StatusNotFound, "TARGET_NOT_FOUND", "no decision %q in %s", blockID, document.Name)
	if document.IssueKey == nil {
		return model.Decision{}, notFound
	}
	decisions, err := s.readDecisions(ctx, document, artifactReference(document), "")
	if err != nil {
		return model.Decision{}, err
	}
	for _, decision := range decisions {
		if decision.BlockID == blockID && decision.Kind == "agent" && !decision.Replaced {
			return decision, nil
		}
	}
	return model.Decision{}, notFound
}

// getDocumentRead is GET /api/v1/me/documents/{id}/read: the caller's last read version of the
// document, null when they have read none. It is per-person state, as GET /api/v1/me/state is.
func (s *server) getDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version *int
	var at *time.Time
	err = s.deps.Store.Pool.QueryRow(r.Context(), `
		select version, read_at from user_document_reads where login = $1 and artifact_id = $2::uuid
	`, model.CanonicalLogin(actor.ID), document.ID).Scan(&version, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = version, at
	WriteJSON(w, http.StatusOK, read)
}

// putDocumentRead is PUT /api/v1/me/documents/{id}/read {"version": n}: the caller has read the
// document through that version. It is the caller's own view state, written as PUT
// /api/v1/me/issues/{key}/state is, and announces nothing.
func (s *server) putDocumentRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input struct {
		Version *int `json:"version"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Version == nil || *input.Version < 0 {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "version must be a whole number that is not negative")
		return
	}
	document, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read := model.DocumentRead{ArtifactID: document.ID}
	var version int
	var at time.Time
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		insert into user_document_reads (login, artifact_id, version) values ($1, $2::uuid, $3)
		on conflict (login, artifact_id) do update set version = excluded.version, read_at = now()
		returning version, read_at
	`, model.CanonicalLogin(actor.ID), document.ID, *input.Version).Scan(&version, &at); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	read.Version, read.ReadAt = &version, &at
	WriteJSON(w, http.StatusOK, read)
}
