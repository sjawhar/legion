package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

func (s *server) resolveAnchor(ctx context.Context, tx pgx.Tx, issueKey string, input *model.AnchorInput, actor model.Actor) (*model.Anchor, string, *model.Version, error) {
	if input == nil {
		return nil, "", nil, nil
	}
	artifactRef := strings.TrimSpace(input.Artifact)
	if artifactRef == "" {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor artifact is required")
	}
	artifact, err := s.lockAnchorArtifact(ctx, tx, issueKey, artifactRef)
	if err != nil {
		return nil, "", nil, err
	}
	if artifact.Kind != "doc" {
		return nil, "", nil, errorf(http.StatusBadRequest, "NOT_DOCUMENT", "anchors require a document artifact")
	}
	live, err := s.deps.Docs.Text(ctx, artifact.ID)
	if err != nil {
		return nil, "", nil, err
	}

	anchor := model.Anchor{ArtifactID: artifact.ID}
	switch {
	case input.Quote != nil && input.From == nil && input.To == nil:
		if *input.Quote == "" {
			return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor quote must not be empty")
		}
		anchor.Quote = *input.Quote
		anchor.From, anchor.To, err = text.Resolve(live, anchor.Quote, input.Occurrence)
		if err != nil {
			return nil, "", nil, anchorResolveError(err)
		}
	case input.Quote == nil && input.From != nil && input.To != nil:
		if *input.From < 0 || *input.From >= *input.To || *input.To > text.Len16(live) {
			return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor range must satisfy 0 <= from < to <= text length")
		}
		anchor.From = *input.From
		anchor.To = *input.To
		anchor.Quote = text.Slice16(live, anchor.From, anchor.To)
	case input.Quote != nil && input.From != nil && input.To != nil:
		if *input.From < 0 || *input.From >= *input.To || *input.Quote == "" {
			return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor range must satisfy 0 <= from < to and quote must not be empty")
		}
		anchor.Quote = *input.Quote
		if *input.To <= text.Len16(live) && text.Slice16(live, *input.From, *input.To) == anchor.Quote {
			anchor.From = *input.From
			anchor.To = *input.To
			break
		}
		anchor.From, anchor.To, err = text.Resolve(live, anchor.Quote, input.Occurrence)
		if errors.Is(err, text.ErrTargetNotFound) {
			return nil, "", nil, errorf(http.StatusConflict, "ANCHOR_STALE", "anchor quote %q is stale", anchor.Quote)
		}
		var ambiguous *text.ErrTargetAmbiguous
		if errors.As(err, &ambiguous) {
			return nil, "", nil, errorf(http.StatusConflict, "ANCHOR_STALE", "anchor quote %q occurs %d times", anchor.Quote, len(ambiguous.Candidates))
		}
		if err != nil {
			return nil, "", nil, err
		}
	default:
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor must provide artifact and either quote or from/to")
	}
	version, wrote, err := s.deps.Docs.SnapshotVersion(ctx, tx, artifact.ID, actor)
	if err != nil {
		return nil, "", nil, err
	}
	anchor.Version = version.Number
	if wrote {
		return &anchor, artifact.Name, &version, nil
	}
	return &anchor, artifact.Name, nil, nil
}

func (s *server) lockAnchorArtifact(ctx context.Context, tx pgx.Tx, issueKey, artifactRef string) (model.Artifact, error) {
	return scanArtifact(tx.QueryRow(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts
		where issue_key = $1 and (id::text = $2 or slug = $2)
		for update
	`, issueKey, artifactRef))
}

// anchorResolveError maps a missing quote to 422 (the caller's input is at
// fault); an ambiguous quote already renders as 409 TARGET_AMBIGUOUS.
func anchorResolveError(err error) error {
	if errors.Is(err, text.ErrTargetNotFound) {
		return errorf(http.StatusUnprocessableEntity, "TARGET_NOT_FOUND", "anchor quote was not found")
	}
	return err
}
