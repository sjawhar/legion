package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// Project documents have no issue status and return nil.
func (s *server) requireOpenOwnerStatus(ctx context.Context, tx pgx.Tx, owner owner) (*string, error) {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	if owner.IssueKey != nil {
		status, err := s.requireOpenIssue(ctx, tx, *owner.IssueKey)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		select true from artifacts where id = $1 and issue_key is null for update
	`, *owner.ArtifactID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return nil, err
	}
	return nil, nil
}
