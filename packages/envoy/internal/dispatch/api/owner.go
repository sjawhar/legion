package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// owner identifies the issue, unlinked artifact, or project that owns collaboration.
type owner struct {
	IssueKey   *string
	ArtifactID *string
	ProjectKey *string
}

func issueOwner(key string) owner {
	return owner{IssueKey: new(key)}
}

func documentOwner(artifactID string) owner {
	return owner{ArtifactID: new(artifactID)}
}

func projectOwner(key string) owner {
	return owner{ProjectKey: new(key)}
}

func ownerOf(issueKey, artifactID *string) owner {
	return owner{IssueKey: issueKey, ArtifactID: artifactID}
}

func (o owner) event(eventType string, actor model.Actor, payload any) model.Event {
	return model.Event{
		IssueKey: o.IssueKey, ArtifactID: o.ArtifactID, ProjectKey: o.ProjectKey,
		Type: eventType, Actor: actor, Payload: payload,
	}
}

func ownerForArtifact(artifact model.Artifact) owner {
	if artifact.IssueKey != nil {
		return issueOwner(*artifact.IssueKey)
	}
	return documentOwner(artifact.ID)
}

func (s *server) documentOwnerFromRequest(ctx context.Context, q queryer, r *http.Request) (model.Artifact, owner, error) {
	artifact, err := s.loadArtifact(ctx, q, r.PathValue("id"))
	if err != nil {
		return model.Artifact{}, owner{}, err
	}
	if artifact.IssueKey != nil {
		return model.Artifact{}, owner{}, errorf(
			http.StatusBadRequest,
			"ARTIFACT_LINKED",
			"artifact belongs to issue %s; use /api/v1/issues/%s/...",
			*artifact.IssueKey,
			*artifact.IssueKey,
		)
	}
	if artifact.SessionID != nil {
		return model.Artifact{}, owner{}, errorf(
			http.StatusBadRequest,
			"ARTIFACT_AGENT_OWNED",
			"artifact belongs to agent %s's conversation, which holds files and images only: it has no comments, asks, events or subscribers, which belong to an issue's or a project's documents",
			*artifact.SessionID,
		)
	}
	return artifact, documentOwner(artifact.ID), nil
}

func (s *server) loadProject(ctx context.Context, q queryer, key string) (model.Project, error) {
	var project model.Project
	if err := q.QueryRow(ctx, `select key, name, created_at from projects where key = $1`, key).Scan(
		&project.Key,
		&project.Name,
		&project.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Project{}, errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "project not found")
		}
		return model.Project{}, err
	}
	return project, nil
}

// requireOpenOwner locks the owner before its collaboration rows are changed.
func (s *server) requireOpenOwner(ctx context.Context, tx pgx.Tx, owner owner) error {
	_, err := s.requireOpenOwnerStatus(ctx, tx, owner)
	return err
}

// requireOpenOwnerStatus also returns the lifecycle status for an issue owner.
// Project documents have no issue status and return nil. It takes the owner's lock first and
// reads the owner in the next statement, never in the same one: a statement's snapshot is taken
// before it waits, so a read combined with the lock would miss a close committed while it waited
// (store.LockOwner), and the write it guards would land on a closed owner. The two go in one round
// trip (store.LockOwnerAndRead).
func (s *server) requireOpenOwnerStatus(ctx context.Context, tx pgx.Tx, owner owner) (*string, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, ownerInvalid()
	}
	if owner.IssueKey != nil {
		status, err := s.requireOpenIssue(ctx, tx, *owner.IssueKey)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}
	err := store.LockOwnerAndRead(ctx, tx, store.ArtifactOwner(*owner.ArtifactID), unlinkedArtifactRead, []any{*owner.ArtifactID}, new(bool))
	return nil, refuseMissingArtifact(err)
}

func ownerInvalid() error {
	return errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
}

// ownerOpenStatus is the refusal table an owner gets, spelled once for both the check that
// decides and the one that only spares work: an owner naming neither an issue nor a document,
// an issue that is closed, a document that does not exist. q is the pool, for a read made before
// any transaction opens; requireOpenOwnerStatus makes the same reads, with the same refusals,
// under the owner's lock. The read itself locks nothing.
func ownerOpenStatus(ctx context.Context, q queryer, owner owner) (*string, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, ownerInvalid()
	}
	if owner.IssueKey != nil {
		status, err := issueOpenStatus(ctx, q, *owner.IssueKey)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}
	return nil, refuseMissingArtifact(q.QueryRow(ctx, unlinkedArtifactRead, *owner.ArtifactID).Scan(new(bool)))
}

// unlinkedArtifactRead reads a row for a document no issue owns.
const unlinkedArtifactRead = `select true from artifacts where id = $1 and issue_key is null`

// refuseMissingArtifact is the refusal of unlinkedArtifactRead's answer err: a document that does
// not exist is ARTIFACT_NOT_FOUND, any other failure err itself.
func refuseMissingArtifact(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	return err
}

// refuseClosedOwnerBeforeResolving answers, from the pool and locking nothing, the refusal
// requireOpenOwnerStatus will make under the owner's lock. A comment that mentions anyone
// resolves every mention against the Envoy listener before its transaction opens, and an owner
// that does not exist or is already closed refuses the comment whatever the listener says, so
// a request that cannot be written costs no listener call at all; an upload to an issue is refused
// so before its body is spooled (refuseUploadToAClosedIssue). The locked check still decides; this
// one only spares the work, and takes no lock of its own so a write on a busy owner waits for that
// lock once rather than twice.
func (s *server) refuseClosedOwnerBeforeResolving(ctx context.Context, owner owner) error {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	_, err := ownerOpenStatus(ctx, s.deps.Store.Pool, owner)
	return err
}
