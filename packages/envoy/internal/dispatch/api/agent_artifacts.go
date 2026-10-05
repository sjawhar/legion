package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// An agent's conversation owns the files and images sent in it (LEGION-541): the third artifact
// owner beside an issue and a project, addressed dispatch://agent/<session id>/artifact/<slug>.
// It holds no document, appends no event and takes no lock but its own. This file is what the
// artifact routes (artifacts.go) do differently for it: its owner key, its upload's lock and
// owner lookups, its upload's missing event, and its read by slug.

// agentOwnsFilesOnly is the refusal of a markdown document uploaded to an agent's conversation.
const agentOwnsFilesOnly = "an agent's conversation holds files and images; a document belongs to an issue or a project"

// agentArtifactByNameSQL and agentArtifactSlugTakenSQL are an upload's two lookups by owner for an
// agent's conversation, $1 the session id and $2 the name or the slug. They compare session_id,
// which artifactOwnerKey's session arm is built from, rather than that expression, which no index
// holds, so the partial index artifacts_session_id (0071) serves both lookups every upload makes
// under the conversation's lock instead of a scan of every owner's artifacts.
const (
	agentArtifactByNameSQL    = `select ` + artifactColumns + ` from artifacts where session_id = $1 and name = $2`
	agentArtifactSlugTakenSQL = `select exists(select 1 from artifacts where session_id = $1 and slug = $2)`
)

// agentOwnerKey is an agent's conversation's owner key, what its artifacts' ref_key holds before
// `/<slug>`: artifactOwnerKey's session arm.
func agentOwnerKey(session string) string {
	return "agent/" + session
}

// requestSessionID is the path's session_id, held to the rule every reference to an agent's
// artifact is (text.IsSessionID), so every owner an upload accepts is one a reference can name.
func requestSessionID(r *http.Request) (string, error) {
	session := r.PathValue("session_id")
	if !text.IsSessionID(session) {
		return "", errorf(http.StatusBadRequest, "INVALID_SESSION_ID",
			"session_id %q is not a session id: it must be non-empty and hold no /, ?, #, whitespace, control character or <>\"'`[]|", session)
	}
	return session, nil
}

// ownerLookup is the query a lookup of the target's artifacts runs, and its $1: the owners'
// ownerQuery with the owner key, or for an agent's conversation agentQuery with the session id.
func (target artifactTarget) ownerLookup(ownerQuery, agentQuery string) (query, owner string) {
	if target.Session != "" {
		return agentQuery, target.Session
	}
	return ownerQuery, target.refPrefix()
}

// lockAgentArtifacts serialises one conversation's uploads for the rest of tx. A conversation has
// no row to lock, so its uploads take their slugs and version numbers one after another under this
// transaction-scoped advisory lock instead, as an issue's do under the issue's row.
func lockAgentArtifacts(ctx context.Context, tx pgx.Tx, session string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('agent-artifacts:' || $1))`, session); err != nil {
		return fmt.Errorf("lock the agent's artifacts: %w", err)
	}
	return nil
}

// ownsEvents reports whether an upload to the target appends an event. An agent's conversation
// owns none: the event log, the outbox's topics and the event stream each address an issue, a
// document or a project, so a session's upload appends none, and the message that carries the
// file is the conversation's record of it.
func (target artifactTarget) ownsEvents() bool {
	return target.Session == ""
}

// loadAgentArtifact reads the artifact the request's session_id and slug name. A conversation
// holds no document, so its artifacts are read by slug alone, with no filename falling back to a
// document as an issue's or a project's reference does (loadArtifactByDocumentReference).
func (s *server) loadAgentArtifact(ctx context.Context, q queryer, r *http.Request) (model.Artifact, error) {
	session, err := requestSessionID(r)
	if err != nil {
		return model.Artifact{}, err
	}
	return s.loadArtifactByRefKey(ctx, q, agentOwnerKey(session)+"/"+r.PathValue("slug"))
}

// POST /api/v1/agents/{session_id}/artifacts
func (s *server) uploadAgentArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.uploadArtifactFor(w, r, artifactTarget{Session: session})
}
