package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) resolveAnchor(ctx context.Context, tx pgx.Tx, owner owner, input *model.AnchorInput, kind docs.MarkKind, rowID string, actor model.Actor) (*model.Anchor, string, *model.Version, error) {
	if input == nil {
		return nil, "", nil, nil
	}
	artifactRef := strings.TrimSpace(input.Artifact)
	if artifactRef == "" {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor artifact is required")
	}
	quotePath := input.Quote != nil && input.MarkID == nil
	markPath := input.Quote == nil && input.MarkID != nil && input.Occurrence == nil
	if !quotePath && !markPath {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor must provide quote with optional occurrence or mark_id")
	}
	if input.Quote != nil && *input.Quote == "" {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor quote must not be empty")
	}
	if input.MarkID != nil && strings.TrimSpace(*input.MarkID) == "" {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor mark_id must not be empty")
	}
	artifact, err := s.lockAnchorArtifact(ctx, tx, owner, artifactRef)
	if err != nil {
		return nil, "", nil, err
	}
	if artifact.Kind != "doc" {
		return nil, "", nil, errorf(http.StatusBadRequest, "NOT_DOCUMENT", "anchors require a document artifact")
	}

	anchor := model.Anchor{ArtifactID: artifact.ID}
	if input.Quote != nil {
		anchor.MarkID = rowID
		quote, err := s.deps.Docs.MarkQuote(docs.WithTx(ctx, tx), artifact.ID, docs.MarkSpec{
			Kind: kind,
			ID:   rowID,
			By:   actor,
		}, *input.Quote, input.Occurrence)
		if err != nil {
			return &anchor, artifact.Name, nil, err
		}
		anchor.Quote = quote
	} else {
		anchor.MarkID = *input.MarkID
		quote, err := s.deps.Docs.VerifyMark(ctx, artifact.ID, kind, anchor.MarkID)
		if err != nil {
			return nil, "", nil, err
		}
		anchor.Quote = quote
	}
	version, wrote, err := s.deps.Docs.SnapshotVersion(ctx, tx, artifact.ID, actor)
	if err != nil {
		return &anchor, artifact.Name, nil, err
	}
	anchor.Version = version.Number
	if wrote {
		return &anchor, artifact.Name, &version, nil
	}
	return &anchor, artifact.Name, nil, nil
}

func (s *server) lockAnchorArtifact(ctx context.Context, tx pgx.Tx, owner owner, artifactRef string) (model.Artifact, error) {
	if owner.IssueKey != nil {
		return scanArtifact(tx.QueryRow(ctx, `
			select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
			from artifacts
			where issue_key = $1 and (id::text = $2 or slug = $2)
			for key share
		`, *owner.IssueKey, artifactRef))
	}
	if owner.ArtifactID == nil {
		return model.Artifact{}, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	artifact, err := scanArtifact(tx.QueryRow(ctx, `
		select id::text, issue_key, project_key, ref_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts
		where id = $1 and issue_key is null
		for key share
	`, *owner.ArtifactID))
	if err != nil {
		return model.Artifact{}, err
	}
	if artifactRef != artifact.ID && artifactRef != artifact.Slug {
		return model.Artifact{}, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor must target this document")
	}
	return artifact, nil
}
