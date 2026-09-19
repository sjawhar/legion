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
	SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error)
	CommitVersion(artifactID string, version model.Version)
	SetIssueClosed(issueKey string, closed bool)
	ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (int, error)
	SetBlockAttributes(ctx context.Context, artifactID, blockID string, attributes map[string]any, actor model.Actor) error
	ScheduleSettlement(artifactID string)
	MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, occurrence *int) (Anchored, error)
	VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (Anchored, error)
	BlockForQuote(ctx context.Context, artifactID, quote string) (string, error)
	SuggestionKind(ctx context.Context, artifactID, id string) (string, error)
	AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error
	RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error
	ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord, actor model.Actor) error
	Evict(ctx context.Context, artifactID string) error
	NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error)
	CompactAll(ctx context.Context, keep int) error
}

type eventCollectorContextKey struct{}

// EventCollector retains document-generated events until the caller's enclosing
// transaction commits. API handlers publish the collected events beside their own
// events; document settlement publishes them after its own transaction commits.
type EventCollector struct {
	events []model.Event
}

// NewEventCollector constructs a transaction-local document event collector.
func NewEventCollector() *EventCollector {
	return &EventCollector{}
}

// WithEventCollector lets document mutations register events for the caller to
// publish after the shared transaction commits.
func WithEventCollector(ctx context.Context, collector *EventCollector) context.Context {
	return context.WithValue(ctx, eventCollectorContextKey{}, collector)
}

func eventCollector(ctx context.Context) *EventCollector {
	collector, _ := ctx.Value(eventCollectorContextKey{}).(*EventCollector)
	return collector
}

func collectEvent(ctx context.Context, event model.Event) {
	collector := eventCollector(ctx)
	if collector != nil {
		collector.events = append(collector.events, event)
	}
}

// Events returns the events appended during this document transaction.
func (c *EventCollector) Events() []model.Event {
	if c == nil {
		return nil
	}
	return c.events
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
