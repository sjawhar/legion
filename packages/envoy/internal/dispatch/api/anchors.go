package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type targetAmbiguousError struct {
	candidates []text.Candidate
}

func (e *targetAmbiguousError) Error() string { return "target is ambiguous" }

func (s *server) resolveAnchor(ctx context.Context, tx pgx.Tx, issueKey string, input *model.AnchorInput, actor model.Actor) (*model.Anchor, string, error) {
	if input == nil {
		return nil, "", nil
	}
	artifactRef := strings.TrimSpace(input.Artifact)
	if artifactRef == "" {
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor artifact is required")
	}
	artifact, err := s.lockAnchorArtifact(ctx, tx, issueKey, artifactRef)
	if err != nil {
		return nil, "", err
	}
	if artifact.Kind != "doc" {
		return nil, "", errorf(http.StatusBadRequest, "NOT_DOCUMENT", "anchors require a document artifact")
	}
	live, err := s.deps.Docs.Text(ctx, artifact.ID)
	if err != nil {
		return nil, "", err
	}

	anchor := model.Anchor{ArtifactID: artifact.ID}
	switch {
	case input.Quote != nil && input.From == nil && input.To == nil:
		anchor.Quote = *input.Quote
		anchor.From, anchor.To, err = text.Resolve(live, anchor.Quote, input.Occurrence)
		if err != nil {
			return nil, "", anchorResolveError(err)
		}
	case input.Quote == nil && input.From != nil && input.To != nil && input.Occurrence == nil:
		if *input.From < 0 || *input.From >= *input.To || *input.To > text.Len16(live) {
			return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor range must satisfy 0 <= from < to <= text length")
		}
		anchor.From = *input.From
		anchor.To = *input.To
		anchor.Quote = text.Slice16(live, anchor.From, anchor.To)
	default:
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor must provide artifact and either quote or from/to")
	}
	version, err := s.anchorVersion(ctx, tx, artifact.ID, live, actor)
	if err != nil {
		return nil, "", err
	}
	anchor.Version = version
	return &anchor, artifact.Name, nil
}

func (s *server) lockAnchorArtifact(ctx context.Context, tx pgx.Tx, issueKey, artifactRef string) (model.Artifact, error) {
	var artifact model.Artifact
	var createdBy []byte
	if err := tx.QueryRow(ctx, `
		select id::text, issue_key, slug, name, kind, is_primary, created_by, created_at
		from artifacts
		where issue_key = $1 and (id::text = $2 or slug = $2)
		for update
	`, issueKey, artifactRef).Scan(
		&artifact.ID, &artifact.IssueKey, &artifact.Slug, &artifact.Name, &artifact.Kind, &artifact.Primary, &createdBy, &artifact.CreatedAt,
	); err != nil {
		return model.Artifact{}, err
	}
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode anchor artifact author: %w", err)
	}
	return artifact, nil
}

func (s *server) anchorVersion(ctx context.Context, tx pgx.Tx, artifactID, live string, actor model.Actor) (int, error) {
	var number int
	var markdown *string
	if err := tx.QueryRow(ctx, `
		select number, markdown
		from artifact_versions
		where artifact_id = $1
		order by number desc
		limit 1
	`, artifactID).Scan(&number, &markdown); err != nil {
		return 0, err
	}
	if markdown != nil && *markdown == live {
		return number, nil
	}
	authors, err := json.Marshal([]model.Actor{actor})
	if err != nil {
		return 0, fmt.Errorf("encode anchor version authors: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, $3
		from artifact_versions
		where artifact_id = $1
		returning number
	`, artifactID, live, authors).Scan(&number); err != nil {
		return 0, fmt.Errorf("write anchor document version: %w", err)
	}
	return number, nil
}

func anchorResolveError(err error) error {
	var ambiguous *text.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		return &targetAmbiguousError{candidates: ambiguous.Candidates}
	}
	if errors.Is(err, text.ErrTargetNotFound) {
		return errorf(http.StatusUnprocessableEntity, "TARGET_NOT_FOUND", "anchor quote was not found")
	}
	return err
}
