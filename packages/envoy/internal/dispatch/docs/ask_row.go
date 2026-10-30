package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AskColumns selects one ask row, aliased a, in the order ScanAsk reads it.
const AskColumns = `a.id::text, a.issue_key, a.artifact_id::text, a.block_id, a.block_artifact_id::text, a.author, a.question, a.options, a.multiple, a.urgency,
		a.anchor, a.state, a.answer, a.resolution, a.created_at, a.edited_at, a.kind, a.approval`

// ScanAsk decodes one AskColumns row. extra receives, in order, any columns the query
// selects after AskColumns.
func ScanAsk(row pgx.Row, extra ...any) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution, approval []byte
	var editedAt *time.Time
	targets := append([]any{
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &ask.BlockID, &ask.BlockArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
		&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt, &ask.Kind, &approval,
	}, extra...)
	if err := row.Scan(targets...); err != nil {
		return model.Ask{}, err
	}
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask options: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := model.DecodeAnchor(anchor, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask anchor: %w", err)
		}
		ask.Anchor = &value
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask resolution: %w", err)
		}
		ask.Resolution = &value
	}
	if len(approval) > 0 {
		var value model.AskApproval
		if err := json.Unmarshal(approval, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask approval: %w", err)
		}
		ask.Approval = &value
	}
	ask.EditedAt = askTimestamp(editedAt)
	return ask, nil
}

// WriteAskResolution closes an open ask the caller has locked as retracted or resolved: it
// records the kind, the reason and who closed it on the row, and returns the ask as it now
// stands. It is the one place that says what resolving an ask stores; each caller writes a block
// ask's own state and appends the ask.resolved event its path owns. The resolution is decided
// here and its update is a write (write): queued on ctx's ledger when the caller joined one, so it
// runs in the transaction's write phase in the order the caller made it.
func WriteAskResolution(ctx context.Context, tx pgx.Tx, ask model.Ask, kind, reason string, actor model.Actor) (model.Ask, error) {
	resolved, encoded, err := askResolution(ask, kind, reason, actor)
	if err != nil {
		return model.Ask{}, err
	}
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		queueAskResolution(b, "", ask.ID, encoded)
		return nil
	}); err != nil {
		return model.Ask{}, err
	}
	return resolved, nil
}

// askResolution is ask as WriteAskResolution leaves it, with the resolution as the row stores it,
// decided before anything is written so a writer that queues the update knows the row it leaves.
func askResolution(ask model.Ask, kind, reason string, actor model.Actor) (model.Ask, []byte, error) {
	resolution := model.AskResolution{Kind: kind, Reason: reason, Actor: actor, At: time.Now().UTC()}
	encoded, err := json.Marshal(resolution)
	if err != nil {
		return model.Ask{}, nil, fmt.Errorf("encode ask resolution: %w", err)
	}
	ask.State = "resolved"
	ask.Resolution = &resolution
	return ask, encoded, nil
}

// queueAskResolution queues the write of a resolution askResolution encoded onto ask id's row, its
// failure wrapped as within failed when within is not empty.
func queueAskResolution(b *store.WriteBatch, within, id string, encoded []byte) {
	what := "write ask resolution"
	if within != "" {
		what = within + ": " + what
	}
	b.Queue(what, `update asks set state = 'resolved', resolution = $2 where id = $1`, id, encoded)
}

// WriteRestoredAsks writes back open each of restored, asks an issue's close withdrew whose rows
// its reopen restores: each row gets state open with no answer and no resolution and keeps
// everything else. The update is a write (write), queued on ctx's ledger when the caller joined
// one; each block ask's row is recorded as it will stand (recordAskBlock), so a read of the block
// asks or of a copy's source later in the transaction - the reopen's write of the asks' blocks and
// copies (WriteIssueAskStates), which classifies them by the rows - reads it open, as it would
// once the update had run.
func WriteRestoredAsks(ctx context.Context, tx pgx.Tx, restored []model.Ask) error {
	if len(restored) == 0 {
		return nil
	}
	ids := make([]string, len(restored))
	for index, ask := range restored {
		ids[index] = ask.ID
	}
	if err := writeExec(ctx, tx, "restore withdrawn ask",
		`update asks set state = 'open', answer = null, resolution = null where id = any($1::uuid[])`, ids); err != nil {
		return err
	}
	for _, ask := range restored {
		if ask.BlockArtifactID != nil {
			recordAskBlock(ctx, tx, *ask.BlockArtifactID, ask)
		}
	}
	return nil
}

// askTimestamp formats a nullable ask timestamp as the wire's RFC 3339 UTC text, nil for nil.
func askTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return new(value.UTC().Format(time.RFC3339Nano))
}
