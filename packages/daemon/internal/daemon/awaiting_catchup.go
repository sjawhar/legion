package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The boot catch-up of the roots that run or wait on a person (admitted, awaiting_person): what a
// root may have missed while this daemon was down, read from Dispatch and applied through the live
// handlers, so the record — the asks' mirror, the claim, the gate, the answers — stands as the live
// events would have left it, and admission's next pass decides from it.

type awaitingRoot struct {
	issue record.Issue
	gate  *record.DesignGate
}

// catchUpAttempts is how many times the boot's catch-up of one root reads Dispatch again when
// Dispatch answers it with an error it may stop giving (a 5xx, 408 or 429) before it skips that
// root; catchUpRetryDelay is the wait between two attempts.
const (
	catchUpAttempts   = 3
	catchUpRetryDelay = 2 * time.Second
)

// catchUpAwaiting reads the person-owned state a root may have missed while this daemon was down.
// It runs after Reconcile committed its durable-consumer holds, so a synthetic answer observes the
// same admission fences as a live event. Each fact's id names what it read (a review's or an ask
// event's place in the issue's Dispatch event log), so a second catch-up of the same state is dropped
// as processed; what keeps one act from answering twice, beside a later JetStream redelivery of the
// original event whose id differs, is the record: a root already answered is skipped, a changes
// request the gate records as told answers nothing, and one Dispatch recorded before the wait began
// answers nothing either (workflow's answerAwaitedGate).
//
// Each root gets a deadline of its own (bootTimeout), so the catch-up of many roots never has to
// fit one boot attempt, and a root it finished — caught up, or skipped — is remembered across the
// boot's retries (caughtUp), so a retry resumes after it. A root whose own deadline passes, or one
// Dispatch does not answer for at all (dispatch.TransientError), makes the attempt transient: the
// boot's readiness retry waits it out and starts at that root again. A root Dispatch answers with an
// error, or one the catch-up cannot read for another reason, is logged and skipped, after
// catchUpAttempts reads for an error Dispatch may stop giving (a 5xx, 408, 429) and at once for a
// credential refusal (401, 403) or anything else: it waits for its live events, as before this
// daemon booted, and blocks no other root's catch-up nor the boot.
func (w *workflowRuntime) catchUpAwaiting(ctx context.Context) error {
	if w.dispatch == nil {
		return nil
	}
	var roots []awaitingRoot
	listing, cancel := context.WithTimeout(ctx, bootTimeout)
	err := pgx.BeginFunc(listing, w.pool, func(tx pgx.Tx) error {
		issues, err := w.records.Issues(listing, tx)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if issue.Project != w.dispatchProject || issue.Key != issue.Tree || issue.Lingers() ||
				issue.Phase != phase.Admitted && !issue.AwaitingPerson() {
				continue
			}
			gate, err := w.records.Gate(listing, tx, issue.Key)
			if err != nil {
				return err
			}
			roots = append(roots, awaitingRoot{issue: issue, gate: gate})
		}
		return nil
	})
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return &dispatch.TransientError{Err: fmt.Errorf("list awaiting roots: %w", err)}
		}
		return fmt.Errorf("list awaiting roots: %w", err)
	}
	if w.caughtUp == nil {
		w.caughtUp = map[string]struct{}{}
	}
	for _, root := range roots {
		done := root.issue.Key + "/generation-" + strconv.FormatUint(uint64(root.issue.Generation), 10)
		if _, ok := w.caughtUp[done]; ok {
			continue
		}
		if err := w.catchUpWithRetries(ctx, root); err != nil {
			return err
		}
		w.caughtUp[done] = struct{}{}
	}
	return nil
}

// catchUpWithRetries catches up one root, reading Dispatch again for an error it may stop giving,
// and returns only what makes the boot's attempt fail: the boot's own end, or Dispatch not
// answering at all. Every other error is logged and the root skipped.
func (w *workflowRuntime) catchUpWithRetries(ctx context.Context, root awaitingRoot) error {
	bound := w.catchUpRootTimeout
	if bound == 0 {
		bound = bootTimeout
	}
	delay := w.catchUpRetryDelay
	if delay == 0 {
		delay = catchUpRetryDelay
	}
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, bound)
		err := w.catchUpAwaitingRoot(attemptCtx, root)
		cancel()
		refusal, answered := dispatchAnswered(err)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			return nil
		case answered && retriable(refusal) && attempt < catchUpAttempts:
			w.log.Warn("boot catch-up reads a root again after Dispatch refused it", "issue", root.issue.Key, "attempt", attempt, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		case answered:
			w.log.Error("boot catch-up skipped a root Dispatch keeps refusing; its live events still reach it", "issue", root.issue.Key,
				"status", refusal.Status, "attempts", attempt, "error", err)
			return nil
		case dispatch.Unreachable(err):
			return err
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			return &dispatch.TransientError{Err: fmt.Errorf("catch up %s: %w", root.issue.Key, err)}
		}
		w.log.Warn("boot catch-up skipped a root it could not read; its live events still reach it", "issue", root.issue.Key, "error", err)
		return nil
	}
}

// dispatchAnswered is the refusal Dispatch answered err with, when Dispatch answered at all: an
// *Error that no transport failure (TransientError) carries.
func dispatchAnswered(err error) (*dispatch.Error, bool) {
	var transient *dispatch.TransientError
	var refusal *dispatch.Error
	if err == nil || errors.As(err, &transient) || !errors.As(err, &refusal) {
		return nil, false
	}
	return refusal, true
}

// retriable says whether Dispatch may stop giving refusal on its own: a 5xx, 408 or 429. A
// credential refusal (401, 403) waits on an operator, and a coded 4xx on nothing at all.
func retriable(refusal *dispatch.Error) bool {
	return refusal.Status >= 500 || refusal.Status == http.StatusRequestTimeout || refusal.Status == http.StatusTooManyRequests
}

// catchUpAwaitingRoot catches up one root: its Dispatch claim, then, unless it is answered, its
// document's approval (for a root waiting on a person) and its event log. The two reads are
// independent: one failing still runs the other, and both errors are returned together.
func (w *workflowRuntime) catchUpAwaitingRoot(ctx context.Context, root awaitingRoot) error {
	current, err := w.dispatch.GetIssue(ctx, root.issue.Key)
	if err != nil {
		return fmt.Errorf("read Dispatch claim of %s: %w", root.issue.Key, err)
	}
	if current.LastSeq > 0 {
		if current.Claim != nil && (current.Claim.Kind == "" || current.Claim.ID == "") {
			return fmt.Errorf("read Dispatch claim of %s: holder has no kind or id", root.issue.Key)
		}
		if err := w.applyAwaitingCatchUp(ctx, fmt.Sprintf("claim:%s:%d", root.issue.Key, current.LastSeq),
			intake.DispatchClaim{Key: root.issue.Key, Seq: current.LastSeq, ClaimedBy: current.Claim.Holder()}); err != nil {
			return err
		}
	}
	if root.issue.Answered() {
		return nil
	}
	var gateErr error
	if root.issue.AwaitingPerson() && (root.issue.Awaiting.GateVersion != 0 || root.gate != nil) {
		gateErr = w.catchUpAwaitingGate(ctx, root)
	}
	return errors.Join(gateErr, w.catchUpAwaitingAsks(ctx, root))
}

// catchUpAwaitingGate reads the approval of a root waiting on its design gate, or on a question
// while it has a gate: a newer version, whoever wrote it, moves the approval request to the agent
// and answers a gate wait, and an approval or changes request of the version waited on (for a
// question, the gate's latest) answers either. When the same read shows the person approved a
// newer version, the approval alone is applied, as the gate seed does: it records that version and
// opens the gate in one fact, so admission resumes the architect with its gate open, never with a
// task telling it to revise a spec the person has already approved. A review carries its place in
// the issue's Dispatch event log (dispatch.Approval's Seq and At), which identifies it in the
// gate's told record, places it against the wait's start, and names it in the fact's id.
func (w *workflowRuntime) catchUpAwaitingGate(ctx context.Context, root awaitingRoot) error {
	if root.gate == nil {
		return fmt.Errorf("awaiting root %s has no design gate", root.issue.Key)
	}
	approval, err := w.dispatch.Approval(ctx, root.gate.ArtifactID)
	if err != nil {
		return fmt.Errorf("read approval of %s for %s: %w", root.gate.ArtifactID, root.issue.Key, err)
	}
	review := record.EventMark{Seq: approval.Seq, At: approval.At}
	approved := approval.ApprovedLatest()
	// The awaited version, not the gate's, is what a review must reach. A question waits on no
	// version, and is answered by a review of the gate's latest. A newer version is applied from its
	// own event in the event log (catchUpAwaitingAsks), whose sequence says whether it came after
	// the wait's start; the version this read shows carries none.
	waited := root.issue.Awaiting.GateVersion
	if waited == 0 {
		waited = root.gate.LatestVersion
	}
	if approved != 0 && approved >= waited {
		return w.applyAwaitingCatchUp(ctx, fmt.Sprintf("gate-approved:%s:%d:%d:%d", root.issue.Key, root.issue.Generation, approved, review.ID()),
			intake.DispatchArtifact{Key: root.issue.Key, ArtifactID: root.gate.ArtifactID, Kind: intake.DispatchArtifactApproved, Version: approved, Mark: review})
	}
	if changes, reason := approval.ChangesRequestedLatest(); changes != 0 && changes >= waited &&
		!classify.DesignGateChangesTold(*root.gate, changes, reason, review) {
		// The id names the review by its place in the event log, so a person who repeats an earlier
		// review's version and wording in a later wait of the same generation has made another
		// review, with another id, and that new act is not dropped as processed.
		return w.applyAwaitingCatchUp(ctx, fmt.Sprintf("gate-changes:%s:%d:%d:%d", root.issue.Key, root.issue.Generation, changes, review.ID()),
			intake.DispatchArtifact{Key: root.issue.Key, ArtifactID: root.gate.ArtifactID, Kind: intake.DispatchArtifactChangesRequested, Version: changes, Reason: reason, Mark: review})
	}
	return nil
}

// catchUpAwaitingAsks applies every ask event, review and document version Dispatch recorded on
// the root since the daemon last handed its architect work, or since its wait began when that is
// earlier (catchUpFrom), while the daemon was down: it reads the root's Dispatch event log past
// that point and applies each through the live decoder and the live handlers, with its own
// sequence, so the asks' mirror and the architect's hand-backs stand as the live events would have
// left them. The approval read alone would lose a review a newer version followed, which the log
// still holds. Each fact's id names the event's sequence and the root's generation, so a second
// catch-up of the same event is dropped as processed. An event of a type this release does not
// read is passed over before it is decoded, and one the decoder refuses is logged and passed over
// too, so one unreadable event never stops the rest of a root's catch-up at every boot.
func (w *workflowRuntime) catchUpAwaitingAsks(ctx context.Context, root awaitingRoot) error {
	from := catchUpFrom(root.issue)
	events, err := w.dispatch.EventsAfter(ctx, root.issue.Key, from)
	if err != nil {
		return fmt.Errorf("read the events of %s after %d: %w", root.issue.Key, from, err)
	}
	for _, event := range events {
		var header struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event, &header); err != nil {
			return fmt.Errorf("read the events of %s: %w", root.issue.Key, err)
		}
		switch header.Type {
		case "ask.opened", "ask.answered", "ask.resolved", "ask.handed_back", "comment.created", "comment.answered",
			"artifact.version", "artifact.approved", "artifact.changes_requested":
		default:
			continue
		}
		fact, err := intake.DecodeDispatchEvent(w.dispatchProject, root.issue.Key, event)
		if err != nil {
			w.log.Warn("boot catch-up passed over an event it could not read", "issue", root.issue.Key, "seq", header.Seq, "type", header.Type, "error", err)
			continue
		}
		if fact == nil {
			continue
		}
		if err := w.applyAwaitingCatchUp(ctx, fmt.Sprintf("event:%s:%d:%d", root.issue.Key, root.issue.Generation, header.Seq), fact); err != nil {
			return err
		}
	}
	return nil
}

// catchUpFrom is the sequence a root's event log is read after: its last handout, or its wait's
// start when that is earlier.
func catchUpFrom(root record.Issue) int64 {
	from := root.HandoutSeq
	if root.AwaitingPerson() {
		from = min(from, root.Awaiting.Since.Seq)
	}
	return from
}

func (w *workflowRuntime) applyAwaitingCatchUp(ctx context.Context, eventID string, fact intake.Fact) error {
	if _, err := intake.ApplyFact(ctx, w.pool, "daemon", "awaiting-catch-up:"+eventID, fact, w.handlers...); err != nil {
		return fmt.Errorf("apply %s: %w", eventID, err)
	}
	return nil
}
