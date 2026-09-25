// Package docs defines Dispatch's live document service. A room has the
// authoritative ProseMirror Y.XmlFragment and a server-owned marks Y.Map
// projecting Postgres rows. Browsers edit the tree and originate browser marks;
// API calls write rows and the marks projection, while agent quote anchors are
// marked by Go. Markdown is canonically rendered from the tree for reads and
// versions.
package docs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// API is the live document operation surface used by Dispatch's HTTP handlers.
type API interface {
	SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string, actor model.Actor) (string, error)
	ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error)
	Text(ctx context.Context, artifactID string) (string, error)
	TextWithToken(ctx context.Context, artifactID string) (string, string, error)
	Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error)
	TextWithBlocks(ctx context.Context, artifactID string) (string, []model.ArtifactBlock, error)
	SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (VersionResult, error)
	SetIssueClosed(ctx context.Context, issueKey string, closed bool)
	AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error)
	ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (EditOutcome, error)
	SetBlockAttributes(ctx context.Context, artifactID, blockID string, attributes map[string]any, actor model.Actor) error
	SetAskBlockText(ctx context.Context, artifactID, blockID string, edit AskBlockEdit, actor model.Actor) (AskBlockText, error)
	ScheduleSettlement(artifactID string)
	MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, occurrence *int) (Anchored, error)
	VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (Anchored, error)
	BlockForQuote(ctx context.Context, artifactID, quote string) (string, error)
	SuggestionKind(ctx context.Context, artifactID, id string) (string, error)
	AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error
	RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error
	ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord, actor model.Actor) error
	Join(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger)
	NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (VersionResult, error)
	CompactAll(ctx context.Context, keep int) error
}

// VersionResult is a written document version together with what its markdown moved in the
// reference graph. Every caller that appends an `artifact.*` event for the version passes
// Changes to the payload builder, so no producer can emit one that stays silent about the
// batched backlink counts it changed. Wrote is false when SnapshotVersion found the live
// document already matched the newest version and recorded nothing.
type VersionResult struct {
	Version model.Version
	Wrote   bool
	Changes model.ReferenceChanges
}

// EditOutcome is what one batch of document edit operations did. Changed compares the document's
// semantic identity — the nodeToken over the whole tree, inline marks included, which is what a
// precondition compares — before and after the whole batch, not against the newest version, whose
// markdown differs while unsettled browser text is pending. A batch that leaves the document as
// it was mints no version, named or not. Unchanged indexes, in order, each operation that left
// the tree exactly as it found it.
type EditOutcome struct {
	Applied   int
	Changed   bool
	Unchanged []int
}
