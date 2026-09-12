// Package docs defines Dispatch's live document service. A room has the
// authoritative ProseMirror Y.XmlFragment and a server-owned marks Y.Map
// projecting Postgres rows. Browsers edit the tree and originate browser marks;
// API calls write rows and the marks projection, while agent quote anchors are
// marked by Go. Markdown is canonically rendered from the tree for reads and
// versions. At boot the service migrates legacy Y.Text rooms and offset anchors
// once; check-documents inspects that conversion without modifying the database.
package docs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// API is the live document operation surface used by Dispatch's HTTP handlers.
type API interface {
	SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) (string, error)
	ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error)
	Text(ctx context.Context, artifactID string) (string, error)
	Blocks(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error)
	SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error)
	CommitVersion(artifactID string, version model.Version)
	SetIssueClosed(issueKey string, closed bool)
	ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error)
	MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, occurrence *int) (string, error)
	VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (string, error)
	SuggestionKind(ctx context.Context, artifactID, id string) (string, error)
	AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error
	RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error
	ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord) error
	Evict(ctx context.Context, artifactID string) error
	NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error)
	CompactAll(ctx context.Context, keep int) error
}

type txContextKey struct{}

// WithTx allows a document operation to join the transaction that created its
// containing artifact or event. Callers commit or roll back tx themselves.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	return tx, ok
}
