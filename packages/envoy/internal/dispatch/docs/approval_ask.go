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
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// OpenApprovalAsk returns the open approval ask on artifactID, or nil. The caller holds the document
// owner's lock, which every writer of an approval ask takes first (a request, a review, a version
// move or carry), so it observes the same version the review and approval routes use and the row
// holds still until the caller's transaction ends; the read takes no row lock.
func OpenApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	row := tx.QueryRow(ctx, `
		select `+AskColumns+`
		from asks a
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = $1
		order by a.created_at desc, a.id desc
		limit 1
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

// openApprovalAsk is OpenApprovalAsk as the transaction's queued writes leave the request: the
// request a version write of this transaction moved (moveApprovalAsk), nil once a queued answer
// closed it (carryApproval), else the stored one.
func openApprovalAsk(ctx context.Context, tx pgx.Tx, artifactID string) (*model.Ask, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		if pending := ledger.approvals[artifactID]; pending != nil {
			if pending.closed {
				return nil, nil
			}
			return pending.ask, nil
		}
	}
	return OpenApprovalAsk(ctx, tx, artifactID)
}

// MoveApprovalAsk advances the document's one open approval ask to a new settled version. mover is
// the actor whose edit moved the request: the caller knows it even when the version credits more
// than one author. The row and its thread stay open; requested_version records that the agent must
// hand it back before a human sees it in Waiting on you again. Only the first move since the
// request was opened or handed back wakes anyone, even when a thread reply already left it waiting
// on its agent; a later one, while requested_version is already below the version it named, is
// quiet (RewriteApprovalAsk). It runs in the transaction ctx joined (errUnjoined otherwise), as
// every other document operation does, so its writes reach that transaction's write queue and the
// event it appends reaches its ledger (eventSink), which publishes it after its Commit.
func MoveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, mover model.Actor, serverURL string) error {
	if _, joined := txFromContext(ctx); !joined {
		return errUnjoined
	}
	return moveApprovalAsk(ctx, tx, broker, artifactID, version, mover, serverURL)
}

// moveApprovalAsk is MoveApprovalAsk without that refusal, for the version write every document
// version goes through (writeVersionTx): it runs in a transaction a caller joined and in the one a
// settlement or the block-id backfill owns, which no caller joins (Ledger). Where its writes are
// queued, the ledger records the request as moved (pendingApproval), so the carry that follows in
// the same version write reads the request as this move leaves it.
func moveApprovalAsk(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifactID string, version model.Version, mover model.Actor, serverURL string) error {
	ask, err := openApprovalAsk(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	if ask == nil || ask.Approval.Version == version.Number {
		return nil
	}
	summary, err := ApprovalAskSummary(*ask)
	if err != nil {
		return err
	}
	if err := RewriteApprovalAsk(ctx, tx, broker, ask, mover, version.Number, summary, serverURL); err != nil {
		return err
	}
	recordPendingApproval(ctx, tx, artifactID, pendingApproval{ask: ask})
	return nil
}

// RewriteApprovalAsk rewords one open approval row to name version with summary, indexes its new
// question, stamps edited_at, and records its ask.edited event. A document version move and a
// request with a new summary both reword; neither hands the request back, so requested_version
// stays as it was. A move of a request that an earlier version already moved since it was opened
// or handed back, requested_version below the version it named, is recorded quiet
// (model.AskEditEventPayload.Quiet): it changes only that version, so like a human's unnamed version
// it wakes nobody. The event it appends goes to the ledger of the transaction it runs in, as every
// document operation's does (eventSink), which publishes it after its Commit; no caller reads it
// back, since where the writes are queued the append has not run when this returns.
//
// The new question, the approval it encodes, the references it cites and its edited_at are worked
// out here; the update, the references' writes, the append and the stamp are one step (write).
// edited_at is clock_timestamp(), read here rather than in the update, so the event's copy of the
// row carries it before the write phase runs, and so does ask, which a later step of the same
// transaction queues from (carryApproval's answer).
func RewriteApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	broker *events.Broker,
	ask *model.Ask,
	actor model.Actor,
	version int,
	summary, serverURL string,
) error {
	sink := eventSink(ctx)
	previous := model.NewAskEditPrevious(*ask)
	quiet := version != ask.Approval.Version && ask.Approval.RequestedVersion < ask.Approval.Version
	ask.Question = ApprovalQuestion(ask.Approval.Name, version, summary)
	// A fresh approval rather than the old one changed in place: the event below keeps its own
	// copy of the row, which a later rewording of the same request in this transaction must not
	// reach before the event is appended.
	moved := *ask.Approval
	moved.Version = version
	ask.Approval = &moved
	approval, err := EncodeApproval(ask.Approval)
	if err != nil {
		return err
	}
	changes, index, err := planEditedAskReferences(ctx, tx, ask.ID, previous.Question, ask.Question, serverURL)
	if err != nil {
		return fmt.Errorf("index approval ask: %w", err)
	}
	var editedAt time.Time
	if err := tx.QueryRow(ctx, `select clock_timestamp()`).Scan(&editedAt); err != nil {
		return fmt.Errorf("read approval ask edit time: %w", err)
	}
	ask.EditedAt = askTimestamp(&editedAt)
	row := *ask
	payload := model.NewAskEditEventPayload(row, previous, actor, changes)
	payload.Quiet = quiet
	return write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("rewrite approval ask", `
			update asks
			set question = $2, approval = $3, edited_at = $4
			where id = $1
		`, ask.ID, row.Question, approval, editedAt)
		if err := index(b); err != nil {
			return fmt.Errorf("index approval ask: %w", err)
		}
		if err := broker.QueueAppend(b, model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type: "ask.edited", Actor: actor, Payload: payload,
		}, sink); err != nil {
			return fmt.Errorf("append approval ask edit: %w", err)
		}
		refs.QueueStamp(b, "ask", ask.ID)
		return nil
	})
}

// EncodeApproval is an approval ask's approval as asks.approval stores it.
func EncodeApproval(approval *model.AskApproval) ([]byte, error) {
	encoded, err := json.Marshal(approval)
	if err != nil {
		return nil, fmt.Errorf("encode approval ask: %w", err)
	}
	return encoded, nil
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
