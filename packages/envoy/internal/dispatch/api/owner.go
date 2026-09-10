package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// owner identifies the issue or unlinked artifact that owns collaboration.
type owner struct {
	IssueKey   *string
	ArtifactID *string
}

func issueOwner(key string) owner {
	return owner{IssueKey: new(key)}
}

func documentOwner(artifactID string) owner {
	return owner{ArtifactID: new(artifactID)}
}

func ownerOf(issueKey, artifactID *string) owner {
	return owner{IssueKey: issueKey, ArtifactID: artifactID}
}

func (o owner) event(eventType string, actor model.Actor, payload any) model.Event {
	return model.Event{IssueKey: o.IssueKey, ArtifactID: o.ArtifactID, Type: eventType, Actor: actor, Payload: payload}
}

// requireOpenOwner locks the owner before its collaboration rows are changed.
func (s *server) requireOpenOwner(ctx context.Context, tx pgx.Tx, owner owner) error {
	if (owner.IssueKey == nil) == (owner.ArtifactID == nil) {
		return errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	if owner.IssueKey != nil {
		return s.requireOpenIssue(ctx, tx, *owner.IssueKey)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		select true from artifacts where id = $1 and issue_key is null for update
	`, *owner.ArtifactID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
		}
		return err
	}
	return nil
}
