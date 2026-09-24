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
	CommitVersion(artifactID string, version model.Version)
	SetIssueClosed(ctx context.Context, issueKey string, closed bool)
	AcquireConditionalEdit(ctx context.Context, artifactID string) (func(), error)
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
	CreditLiveWrites(collector *EventCollector)
	PublishLiveWrites(collector *EventCollector)
	DiscardLiveWrites(collector *EventCollector)
	FailLiveWrites(collector *EventCollector, cause error)
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

type eventCollectorContextKey struct{}

// EventCollector retains what document operations joined to a transaction produce until that
// transaction ends: the events they generate and their writes to live documents. Once the
// transaction commits, API handlers credit the writes (CreditLiveWrites) before CommitVersion,
// then apply them with PublishLiveWrites and publish the collected events beside their own;
// when it does not commit they drop the writes with DiscardLiveWrites. Document settlement
// publishes its events after its own transaction commits.
type EventCollector struct {
	events []model.Event
	// live holds, per document, the writes this transaction made to it. A live room sees
	// them only through PublishLiveWrites, so a transaction that does not commit never
	// reaches a room or a browser.
	live  map[string]*liveWrite
	order []string
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
