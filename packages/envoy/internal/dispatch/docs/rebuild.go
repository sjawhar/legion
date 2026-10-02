package docs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// RebuildReport describes the durable history a rebuild replaced. SourceVersion is the artifact
// version whose markdown became the fresh document state; Head is the new monotonic update version.
type RebuildReport struct {
	Head               int64  `json:"head"`
	RemovedCheckpoints int64  `json:"removed_checkpoints"`
	RemovedSnapshots   int64  `json:"removed_snapshots"`
	RemovedUpdates     int64  `json:"removed_updates"`
	SourceVersion      int    `json:"source_version"`
	ValidationError    string `json:"validation_error"`
}

// RebuildDocument replaces a history ygo cannot load with one fresh update built from its latest
// saved markdown, or supplied markdown. It is deliberately never a load fallback: a history that
// loads can contain unsettled edits newer than its latest version and is refused unchanged.
func (s *Service) RebuildDocument(ctx context.Context, artifactID string, markdown *string, _ model.Actor) (RebuildReport, error) {
	if _, loaded := s.rebuilding.LoadOrStore(artifactID, struct{}{}); loaded {
		return RebuildReport{}, fmt.Errorf("%w: document %s is already being rebuilt", ErrDocumentLive, artifactID)
	}
	defer s.rebuilding.Delete(artifactID)

	if s.afterRebuildMark != nil {
		s.afterRebuildMark(artifactID)
	}
	for _, room := range s.srv.Rooms() {
		if room == artifactID {
			return RebuildReport{}, fmt.Errorf("%w: document %s is live in this server; close its editors and retry, or replace it from markdown instead", ErrDocumentLive, artifactID)
		}
	}

	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return RebuildReport{}, fmt.Errorf("%w: load document before rebuild: %v", ErrServiceUnavailable, err)
	}
	validationErr := validateUpdate(loaded.Update)
	if validationErr == nil {
		return RebuildReport{}, fmt.Errorf("%w: document %s", ErrDocumentLoads, artifactID)
	}
	validationError := validationErr.Error()
	if markdown == nil {
		var (
			version        int
			latestMarkdown string
		)
		if err := s.store.Pool.QueryRow(ctx, `
			select number, markdown from artifact_versions
			where artifact_id = $1 and markdown is not null
			order by number desc limit 1
		`, artifactID).Scan(&version, &latestMarkdown); err != nil {
			return RebuildReport{}, fmt.Errorf("read latest document version: %w", err)
		}
		return s.rebuildFromMarkdown(ctx, artifactID, latestMarkdown, version, validationError)
	}
	return s.rebuildFromMarkdown(ctx, artifactID, *markdown, 0, validationError)
}

func (s *Service) rebuildFromMarkdown(ctx context.Context, artifactID, markdown string, sourceVersion int, validationError string) (RebuildReport, error) {
	tree, err := parseInput(markdown)
	if err != nil {
		return RebuildReport{}, err
	}
	if err := pmdoc.AskContentError(tree); err != nil {
		return RebuildReport{}, &ErrInvalidAskBlock{Reason: err}
	}
	if err := s.refuseDroppedAskBlocks(ctx, artifactID, tree); err != nil {
		return RebuildReport{}, &ErrInvalidAskBlock{Reason: err}
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		return RebuildReport{}, fmt.Errorf("seed rebuilt document tree: %w", err)
	}
	report, err := s.persistence.Rebuild(ctx, artifactID, seed)
	if err != nil {
		return RebuildReport{}, err
	}
	report.SourceVersion = sourceVersion
	report.ValidationError = validationError
	return report, nil
}
