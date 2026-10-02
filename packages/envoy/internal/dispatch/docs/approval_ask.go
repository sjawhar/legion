package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// OpenApprovalAsk returns the open approval ask on artifactID, locked, or nil. The caller holds the
// document owner's row, so it observes the same version the review and approval routes use.
func OpenApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	row := tx.QueryRow(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = $1
		order by a.created_at desc, a.id desc
		limit 1
		for no key update
	`, artifactID)
	ask, err := ScanAsk(row)
	if err == nil {
		return &ask, nil
	}
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return nil, fmt.Errorf("read open approval ask: %w", err)
}

// MoveApprovalAsk advances the document's one open approval ask to a new settled version. The
// row and its thread stay open; requested_version records that the agent must hand it back before
// a human sees it in Waiting on you again.
func MoveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, serverURL string) ([]model.Event, error) {
	ask, err := OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	if ask == nil || ask.Approval.Version == version.Number {
		return nil, nil
	}
	actor := SettlementActor
	if len(version.Authors) == 1 {
		actor = version.Authors[0]
	}
	summary, err := ApprovalAskSummary(*ask)
	if err != nil {
		return nil, err
	}
	event, err := RewriteApprovalAsk(ctx, tx, broker, ask, actor, version.Number, summary, serverURL)
	if err != nil {
		return nil, err
	}
	return []model.Event{event}, nil
}

// RewriteApprovalAsk rewords one open approval row to name version with summary, indexes its new
// question, stamps edited_at, and records its ask.edited event. A document version move and a
// request with a new summary both reword; neither hands the request back, so requested_version
// stays as it was.
func RewriteApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	broker *events.Broker,
	ask *model.Ask,
	actor model.Actor,
	version int,
	summary, serverURL string,
) (model.Event, error) {
	previous := model.AskEditPrevious{
		Question: ask.Question,
		Options:  ask.Options,
		Multiple: ask.Multiple,
		Urgency:  ask.Urgency,
	}
	ask.Question = ApprovalQuestion(ask.Approval.Name, version, summary)
	ask.Approval.Version = version
	approval, err := json.Marshal(ask.Approval)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode approval ask: %w", err)
	}
	var editedAt time.Time
	if err := tx.QueryRow(ctx, `
		update asks
		set question = $2, approval = $3, edited_at = clock_timestamp()
		where id = $1
		returning edited_at
	`, ask.ID, ask.Question, approval).Scan(&editedAt); err != nil {
		return model.Event{}, fmt.Errorf("rewrite approval ask: %w", err)
	}
	ask.EditedAt = askTimestamp(&editedAt)
	changes, err := refs.ReplaceCounted(ctx, tx, "ask", ask.ID, ask.Question, serverURL)
	if err != nil {
		return model.Event{}, fmt.Errorf("index approval ask: %w", err)
	}
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
		Type: "ask.edited", Actor: actor,
		Payload: model.NewAskEditEventPayload(*ask, previous, actor, changes),
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("append approval ask edit: %w", err)
	}
	if err := refs.Stamp(ctx, tx, "ask", ask.ID, event.ID); err != nil {
		return model.Event{}, fmt.Errorf("stamp approval ask references: %w", err)
	}
	return event, nil
}

// ApprovalQuestion renders the server-owned question prefix with its optional summary.
func ApprovalQuestion(name string, version int, summary string) string {
	question := fmt.Sprintf("Approve %s (version %d)?", name, version)
	if summary == "" {
		return question
	}
	return question + " " + summary
}

// ApprovalAskSummary parses the summary after the server-owned approval question prefix.
func ApprovalAskSummary(ask model.Ask) (string, error) {
	prefix := ApprovalQuestion(ask.Approval.Name, ask.Approval.Version, "")
	if ask.Question == prefix {
		return "", nil
	}
	if summary, ok := strings.CutPrefix(ask.Question, prefix+" "); ok && summary != "" {
		return summary, nil
	}
	return "", fmt.Errorf("approval ask %s has a question not written by the approval route", ask.ID)
}
