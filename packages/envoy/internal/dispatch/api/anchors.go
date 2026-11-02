package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// resolveAnchor returns the anchor, its document's name, and the version the anchor snapshot
// wrote (nil when the live document already matched the newest version). The result carries
// what that write moved in the reference graph, which the caller names on its event.
func (s *server) resolveAnchor(ctx context.Context, tx pgx.Tx, owner owner, input *model.AnchorInput, kind docs.MarkKind, rowID string, actor model.Actor) (*model.Anchor, string, *docs.VersionResult, error) {
	if input == nil {
		return nil, "", nil, nil
	}
	artifactRef := strings.TrimSpace(input.Artifact)
	if artifactRef == "" {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "anchor artifact is required")
	}
	if input.OnFile() {
		anchor, name, err := s.resolveFileAnchor(ctx, tx, owner, input, kind, rowID, artifactRef)
		return anchor, name, nil, err
	}
	if input.Version != nil || input.Box != nil {
		return nil, "", nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "version and box go only with a comment on an uploaded file, which takes no quote or mark_id")
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
	var anchored docs.Anchored
	// ctx is joined to this handler's transaction (Docs.Join), so both document calls run
	// inside it and read through it.
	if input.Quote != nil {
		anchor.MarkID = rowID
		anchored, err = s.deps.Docs.MarkQuote(ctx, artifact.ID, docs.MarkSpec{
			Kind: kind,
			ID:   rowID,
			By:   actor,
		}, *input.Quote, input.Occurrence)
		if err != nil {
			return &anchor, artifact.Name, nil, err
		}
	} else {
		anchor.MarkID = *input.MarkID
		anchored, err = s.deps.Docs.VerifyMark(ctx, artifact.ID, kind, anchor.MarkID)
		if err != nil {
			return nil, "", nil, err
		}
	}
	anchor.Quote = anchored.Quote
	if anchored.BlockID != "" {
		anchor.BlockID = &anchored.BlockID
	}

	snapshot, err := s.deps.Docs.SnapshotVersion(ctx, artifact.ID, actor)
	if err != nil {
		return &anchor, artifact.Name, nil, err
	}
	anchor.Version = snapshot.Version.Number
	if snapshot.Wrote {
		return &anchor, artifact.Name, &snapshot, nil
	}
	return &anchor, artifact.Name, nil, nil
}

// boxTolerance is how far past the picture's right edge or bottom a box's far side may reach: a
// browser's fractions of a box drawn to the very edge can sum a rounding error above 1.
const boxTolerance = 1e-9

// resolveFileAnchor resolves a comment's anchor on an uploaded file that is not a Markdown document:
// the file, the version the comment is about (the file's latest when the input names none), and on
// a picture the box. It marks nothing in any document, so it writes no version and its anchor is
// never refreshed or orphaned; its mark id is the comment's own, as a quote anchor's is.
func (s *server) resolveFileAnchor(ctx context.Context, tx pgx.Tx, owner owner, input *model.AnchorInput, kind docs.MarkKind, rowID, artifactRef string) (*model.Anchor, string, error) {
	switch kind {
	case docs.MarkAsk:
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "an ask names a passage of a document: give quote or mark_id. To ask about an uploaded file, comment on it or ask on the issue")
	case docs.MarkSuggestion:
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "a suggestion replaces quoted text of a document: give quote or mark_id. Comment on an uploaded file instead")
	}
	box, err := anchorBox(input.Box)
	if err != nil {
		return nil, "", err
	}
	artifact, err := s.lockAnchorArtifact(ctx, tx, owner, artifactRef)
	if err != nil {
		return nil, "", err
	}
	if artifact.Kind == "doc" {
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "a comment on a document names a passage: give quote or mark_id. A comment on a whole file, or on part of a picture, is for an uploaded file that is not a document")
	}
	if box != nil && artifact.Kind != "image" {
		return nil, "", errorf(http.StatusBadRequest, "NOT_PICTURE", "a box marks part of a picture, and %s is an uploaded file that is not one: comment on the whole file without a box", artifact.Name)
	}
	// The artifact's key-share lock keeps its row; a version written meanwhile only adds a later
	// number, so the latest read here is the newest the comment could have been written on.
	var latest int
	if err := tx.QueryRow(ctx, `select coalesce(max(number), 0) from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&latest); err != nil {
		return nil, "", err
	}
	version := latest
	if input.Version != nil {
		version = *input.Version
		var exists bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from artifact_versions where artifact_id = $1 and number = $2)`, artifact.ID, version).Scan(&exists); err != nil {
			return nil, "", err
		}
		if !exists {
			return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "version %d is not a version of %s, whose latest is version %d", version, artifact.Name, latest)
		}
	}
	if version == 0 {
		return nil, "", errorf(http.StatusBadRequest, "INVALID_ANCHOR", "%s has no version to comment on", artifact.Name)
	}
	return &model.Anchor{
		Kind:       model.AnchorKindFile,
		ArtifactID: artifact.ID,
		MarkID:     rowID,
		Version:    version,
		Box:        box,
	}, artifact.Name, nil
}

// anchorBox checks a request's box: four fractions of the picture, inside it and covering some of
// it. JSON carries no NaN or infinity, so every side it decodes is finite.
func anchorBox(input *model.AnchorBoxInput) (*model.AnchorBox, error) {
	if input == nil {
		return nil, nil
	}
	if input.Left == nil || input.Top == nil || input.Width == nil || input.Height == nil {
		return nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "box needs left, top, width and height, each a fraction of the picture's width or height")
	}
	box := model.AnchorBox{Left: *input.Left, Top: *input.Top, Width: *input.Width, Height: *input.Height}
	switch {
	case box.Left < 0 || box.Top < 0:
		return nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "box left and top must be at least 0: they are fractions of the picture from its top-left corner")
	case box.Width <= 0 || box.Height <= 0:
		return nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "box width and height must be greater than 0")
	case box.Left+box.Width > 1+boxTolerance:
		return nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "box left + width must be at most 1: the box runs past the picture's right edge")
	case box.Top+box.Height > 1+boxTolerance:
		return nil, errorf(http.StatusBadRequest, "INVALID_ANCHOR", "box top + height must be at most 1: the box runs past the picture's bottom")
	}
	return &box, nil
}

func (s *server) lockAnchorArtifact(ctx context.Context, tx pgx.Tx, owner owner, artifactRef string) (model.Artifact, error) {
	if owner.IssueKey != nil {
		return scanArtifact(tx.QueryRow(ctx, `
			select `+artifactColumns+`
			from artifacts
			where issue_key = $1 and (id::text = $2 or slug = $2)
			for key share
		`, *owner.IssueKey, artifactRef))
	}
	if owner.ArtifactID == nil {
		return model.Artifact{}, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	artifact, err := scanArtifact(tx.QueryRow(ctx, `
		select `+artifactColumns+`
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

// anchorBlock is where anchor's block stands in its document, for the single-record comment and
// ask reads: no position for no anchor, an anchor naming no block, or a block the live document no
// longer holds (a deleted row's cell). The position is one derived field of the read, so a
// document it cannot read leaves the read standing: no position, and an AnchorBlockError naming
// the error as writeHandlerError names a document error (documentErrorCode, in its order), or
// INTERNAL, as it answers an unclassified failure, logged at WARN. Nor does it wait for a failed
// room's recovery (docs.WithoutRecoveryWait), and a failed room is DOC_SERVICE_UNAVAILABLE whatever
// failed it, ahead of a missing block: the room's cause is another operation's, and this read
// never renders, so it says nothing of whether the block can be placed once the room is evicted.
// Only a request that has gone away fails the read, and the request's own context says so: a
// failed room's cause can carry another request's cancellation (a writer whose client went away
// during its commit), which says nothing of this one.
func (s *server) anchorBlock(ctx context.Context, anchor *model.Anchor) (model.AnchorPosition, error) {
	if anchor == nil || anchor.BlockID == nil || *anchor.BlockID == "" {
		return model.AnchorPosition{}, nil
	}
	path, err := s.deps.Docs.BlockPath(docs.WithoutRecoveryWait(ctx), anchor.ArtifactID, *anchor.BlockID)
	if err == nil {
		return model.AnchorPosition{AnchorBlock: &path}, nil
	}
	code := documentErrorCode(err)
	switch {
	case ctx.Err() != nil:
		return model.AnchorPosition{}, ctx.Err()
	case code == codeDocServiceUnavailable:
	case errors.Is(err, pmdoc.ErrTargetNotFound):
		return model.AnchorPosition{}, nil
	case code == codeDocSchema, code == codeDocumentUnloadable:
	default:
		code = codeInternal
	}
	slog.Warn("dispatch: answer a comment or ask read without its anchor's block position",
		"artifact", anchor.ArtifactID, "block", *anchor.BlockID, "anchor_block_error", code, "error", err)
	return model.AnchorPosition{AnchorBlockError: code}, nil
}
