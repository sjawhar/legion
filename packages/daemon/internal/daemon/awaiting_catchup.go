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

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The boot catch-up of the roots that run or wait on a person (admitted in a slot, awaiting_person): what a
// root's durable Dispatch stream may not have delivered while this daemon was down, read from the
// root's Dispatch event log and applied through the live decoder and handlers, so the record — the
// asks' mirror, the architect's hand-backs, the gate, the answers — stands as the live events would
// have left it. Each root is held out of admission's decision whether it waits on a person from
// before the boot's own admission pass until its window is applied (admit's HoldForCatchUp), and
// one pass then decides it, so no root enters awaiting_person or is told in place halfway through
// what it missed.
//
// Each root keeps a cursor (record.Store.CaughtUpSeq): the boot reads only the events after it,
// and raises it to the issue's latest sequence it read. A root recorded before the cursor existed,
// and given no work by this release since, has none: its first catch-up replays nothing and only
// sets the cursor, since a daemon of an earlier release applied everything before it live, and a
// replay of that history through this release's handlers would read old hand-backs as new. A root
// recorded since, or handed work by this release since (an admission, a re-admission, a resume:
// record.Store.StartHandout), starts at 0 and reads from its last handout; a re-admission sets the
// cursor back to 0, so a new generation never replays the old one's tail. A gate whose document
// has no approval request in the asks' mirror — one an earlier release registered before the
// mirror existed, or one whose request the daemon never saw
// an event of — has the request Dispatch shows open on its document mirrored, which declares the
// architect's hand-back when the request waits on the person (workflow's gateRequestFound).

// catchUpRoot is one root the boot's catch-up reads: its record, its design gate (nil for none),
// its cursor (cursor, and whether it has one at all), and whether the mirror holds an approval
// request of its gate's document (mirrored).
type catchUpRoot struct {
	issue     record.Issue
	gate      *record.DesignGate
	cursor    int64
	hasCursor bool
	// mirrored says whether the asks' mirror holds an approval request of the gate's document.
	mirrored bool
}

// catchUpAttempts is how many times the boot's catch-up of one root reads Dispatch again when
// Dispatch answers it with an error it may stop giving (a 5xx, 408 or 429) before it skips that
// root; catchUpRetryDelay is the wait between two attempts.
const (
	catchUpAttempts   = 3
	catchUpRetryDelay = 2 * time.Second
)

// catchesUp says whether the boot's catch-up reads issue: a tree root of the daemon's project, not
// lingering, in awaiting_person, or at admitted holding a slot (slotted). A root waiting in line
// for its first slot never ran: its architect has handed nothing back, and its admission is a
// handout of its own.
func (w *workflowRuntime) catchesUp(issue record.Issue, slotted map[string]struct{}) bool {
	if issue.Project != w.dispatchProject || !claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Lingers() {
		return false
	}
	_, holds := slotted[issue.Key]
	return issue.Phase == phase.AwaitingPerson || issue.Phase == phase.Admitted && holds
}

// slottedIssues is the key of every issue holding an admission slot.
func (w *workflowRuntime) slottedIssues(ctx context.Context, tx pgx.Tx) (map[string]struct{}, error) {
	slots, err := w.records.Slots(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("list admission slots: %w", err)
	}
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	return slotted, nil
}

// holdForCatchUp holds every root the catch-up will read, and has not finished in an earlier
// attempt of this boot, out of admission's decision whether it waits on a person, inside the
// boot's reconcile transaction and before its admission pass. Without a Dispatch client nothing is
// caught up, and nothing is held.
func (w *workflowRuntime) holdForCatchUp(ctx context.Context, tx pgx.Tx) error {
	if w.dispatch == nil {
		return nil
	}
	issues, err := w.records.Issues(ctx, tx)
	if err != nil {
		return fmt.Errorf("list the roots to catch up: %w", err)
	}
	slotted, err := w.slottedIssues(ctx, tx)
	if err != nil {
		return err
	}
	var keys []string
	for _, issue := range issues {
		if _, done := w.caughtUp[caughtUpKey(issue)]; w.catchesUp(issue, slotted) && !done {
			keys = append(keys, issue.Key)
		}
	}
	w.admission.HoldForCatchUp(keys...)
	return nil
}

// caughtUpKey names a root's catch-up in this boot: its key and generation.
func caughtUpKey(issue record.Issue) string {
	return issue.Key + "/generation-" + strconv.FormatUint(issue.Generation, 10)
}

// catchUpAwaiting catches up every root the boot holds (holdForCatchUp), one after another. Each
// fact's id names what it read (an event's sequence, the request a read found), so a second catch-up
// of the same state is dropped as processed, and a JetStream redelivery of an event the catch-up
// already applied finds the mirror row as fresh as itself and changes nothing.
//
// Each root gets a deadline of its own (bootTimeout), so the catch-up of many roots never has to
// fit one boot attempt, and a root it finished — caught up, or skipped — is remembered across the
// boot's retries (caughtUp), so a retry resumes after it. A root whose own deadline passes, or one
// Dispatch does not answer for at all (dispatch.TransientError), makes the attempt transient: the
// boot's readiness retry waits it out and starts at that root again, still held. A root Dispatch
// answers with an error, or one the catch-up cannot read for another reason, is logged and skipped,
// after catchUpAttempts reads for an error Dispatch may stop giving (a 5xx, 408, 429) and at once
// for a credential refusal (401, 403) or anything else: its hold is released and admission decides
// it on the record as it stands, and its live events still reach it. A root the boot held that the
// catch-up no longer reads (the reconcile moved it out) is released too.
func (w *workflowRuntime) catchUpAwaiting(ctx context.Context) error {
	if w.dispatch == nil {
		return nil
	}
	var roots []catchUpRoot
	listing, cancel := context.WithTimeout(ctx, bootTimeout)
	err := pgx.BeginFunc(listing, w.pool, func(tx pgx.Tx) error {
		issues, err := w.records.Issues(listing, tx)
		if err != nil {
			return err
		}
		slotted, err := w.slottedIssues(listing, tx)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if !w.catchesUp(issue, slotted) {
				continue
			}
			root := catchUpRoot{issue: issue}
			if root.gate, err = w.records.Gate(listing, tx, issue.Key); err != nil {
				return err
			}
			if root.cursor, root.hasCursor, err = w.records.CaughtUpSeq(listing, tx, issue.Key); err != nil {
				return err
			}
			if root.gate != nil {
				asks, err := w.records.RootAsks(listing, tx, []string{issue.Key})
				if err != nil {
					return err
				}
				for _, ask := range asks[issue.Key] {
					root.mirrored = root.mirrored || ask.GateRequestOf(root.gate.ArtifactID)
				}
			}
			roots = append(roots, root)
		}
		return nil
	})
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return &dispatch.TransientError{Err: fmt.Errorf("list the roots to catch up: %w", err)}
		}
		return fmt.Errorf("list the roots to catch up: %w", err)
	}
	if w.caughtUp == nil {
		w.caughtUp = map[string]struct{}{}
	}
	reading := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		reading[root.issue.Key] = struct{}{}
	}
	for _, key := range w.admission.CatchUpHeld() {
		if _, ok := reading[key]; !ok {
			w.admission.ReleaseCatchUp(key)
		}
	}
	for _, root := range roots {
		done := caughtUpKey(root.issue)
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
// answering at all. Every other error is logged, and the root is released as it stands.
func (w *workflowRuntime) catchUpWithRetries(ctx context.Context, root catchUpRoot) error {
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
		err := w.catchUpRoot(attemptCtx, root)
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
			return w.finishCatchUp(ctx, root, 0)
		case dispatch.Unreachable(err):
			return err
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
			return &dispatch.TransientError{Err: fmt.Errorf("catch up %s: %w", root.issue.Key, err)}
		}
		w.log.Warn("boot catch-up skipped a root it could not read; its live events still reach it", "issue", root.issue.Key, "error", err)
		return w.finishCatchUp(ctx, root, 0)
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

// catchUpRoot catches up one root: it reads the issue's latest sequence on Dispatch, mirrors the
// approval request Dispatch shows open on a gate's document the mirror holds none of (as of that
// sequence), applies its
// event log's window — the events after its cursor, or after its last handout for a root with a
// cursor of 0, and none at all for a root with no cursor — and then finishes it (finishCatchUp).
// The request's read and the window are independent: one failing still runs the other, and the
// root is finished only when both succeeded, so a failed read leaves its cursor where it was.
func (w *workflowRuntime) catchUpRoot(ctx context.Context, root catchUpRoot) error {
	current, err := w.dispatch.GetIssue(ctx, root.issue.Key)
	if err != nil {
		return fmt.Errorf("read the latest sequence of %s: %w", root.issue.Key, err)
	}
	through := current.LastSeq
	bindErr := w.bindGateRequest(ctx, root, through)
	if from, replay := catchUpFrom(root); replay && through > from {
		applied, err := w.catchUpWindow(ctx, root, from)
		if err != nil {
			return errors.Join(bindErr, err)
		}
		through = max(through, applied)
	}
	if bindErr != nil {
		return bindErr
	}
	return w.finishCatchUp(ctx, root, through)
}

// catchUpFrom is the sequence a root's event log is read after, and whether it is read at all: its
// cursor; for a cursor of 0, its last handout, as the record already holds everything before it;
// and no read for a root with no cursor, whose first catch-up after the deploy replays nothing.
//
// A cursor below the root's last handout (a resume after its last catch-up) is read from the
// cursor, not from the handout: the handout is the highest sequence the record held, not proof
// that every event below it arrived, since the durable stream may deliver out of order. Replaying
// the events between the two is safe: a hand-back at or below the handout declares nothing, a
// review placed before the wait began answers nothing, and a changes request already told is not
// told again (its processed key is per generation, and a re-admission restarts the cursor).
func catchUpFrom(root catchUpRoot) (int64, bool) {
	switch {
	case !root.hasCursor:
		return 0, false
	case root.cursor > 0:
		return root.cursor, true
	}
	return root.issue.HandoutSeq, true
}

// bindGateRequest reads the approval of a root's gate whose document has no approval request in the
// mirror and, when Dispatch shows one open, applies it (intake.GateRequestFound) at since, the
// issue's sequence read before it.
func (w *workflowRuntime) bindGateRequest(ctx context.Context, root catchUpRoot, since int64) error {
	if root.gate == nil || root.mirrored || since <= 0 {
		return nil
	}
	approval, err := w.dispatch.Approval(ctx, root.gate.ArtifactID)
	if err != nil {
		return fmt.Errorf("read the approval of %s for %s: %w", root.gate.ArtifactID, root.issue.Key, err)
	}
	if approval.AskID == "" {
		return nil
	}
	fact := intake.GateRequestFound{Issue: root.issue.Key, Generation: root.issue.Generation, ArtifactID: root.gate.ArtifactID,
		RequestAsk: approval.AskID, WaitsOnAgent: approval.WaitingOn == "agent", Since: since}
	return w.applyCatchUp(ctx, fmt.Sprintf("gate-request:%s:%d:%s:%d", root.issue.Key, root.issue.Generation, approval.AskID, since), fact)
}

// catchUpWindow applies every ask event (an ask.edited one when it moved an approval request),
// review and document version Dispatch recorded on the
// root after from: it reads the root's event log past that point and applies each through the live
// decoder and the live handlers, with its own sequence, so the asks' mirror and the architect's
// hand-backs stand as the live events would have left them. It returns the highest sequence it
// read. Each fact's id names the event's sequence and the root's generation, so a second catch-up
// of the same event is dropped as processed. An event of a type the catch-up does not read is
// passed over before it is decoded (an issue's own events are the boot listing's), and one the
// decoder refuses or the handlers fail on is logged and passed over too, so one event never stops
// the rest of a root's catch-up at every boot. Passing over loses nothing: every event the window
// holds was either applied live before, or is still unacknowledged on the durable Dispatch
// consumer, which retries it once the catch-up ends. Only the boot's own end (ctx) stops it.
func (w *workflowRuntime) catchUpWindow(ctx context.Context, root catchUpRoot, from int64) (int64, error) {
	events, err := w.dispatch.EventsAfter(ctx, root.issue.Key, from)
	if err != nil {
		return 0, fmt.Errorf("read the events of %s after %d: %w", root.issue.Key, from, err)
	}
	var read int64
	for _, event := range events {
		var header struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event, &header); err != nil {
			return 0, fmt.Errorf("read the events of %s: %w", root.issue.Key, err)
		}
		read = max(read, header.Seq)
		switch header.Type {
		case "ask.opened", "ask.answered", "ask.resolved", "ask.handed_back", "ask.edited", "comment.created", "comment.answered",
			"artifact.version", "artifact.approved", "artifact.changes_requested":
		default:
			continue
		}
		fact, unread, err := intake.DecodeDispatchEvent(w.dispatchProject, root.issue.Key, event)
		if err != nil {
			w.log.Warn("boot catch-up passed over an event it could not read", "issue", root.issue.Key, "seq", header.Seq, "type", header.Type, "error", err)
			continue
		}
		if len(unread) > 0 {
			w.log.Warn("boot catch-up read an event with fields it took as unknown", "issue", root.issue.Key, "seq", header.Seq, "type", header.Type, "unread", unread)
		}
		if fact == nil {
			continue
		}
		if err := w.applyCatchUp(ctx, fmt.Sprintf("event:%s:%d:%d", root.issue.Key, root.issue.Generation, header.Seq), fact); err != nil {
			if ctx.Err() != nil {
				return 0, err
			}
			w.log.Error("boot catch-up passed over an event its handlers failed on", "issue", root.issue.Key, "seq", header.Seq, "type", header.Type, "error", err)
		}
	}
	return read, nil
}

// finishCatchUp releases root's catch-up hold and applies intake.CatchUpApplied, which raises its
// cursor to through (0 moves none) and on which admission decides it, once. Should the fact fail,
// the hold is taken again, so the boot's retry finds the root as held as before.
func (w *workflowRuntime) finishCatchUp(ctx context.Context, root catchUpRoot, through int64) error {
	w.admission.ReleaseCatchUp(root.issue.Key)
	fact := intake.CatchUpApplied{Issue: root.issue.Key, Through: through}
	if err := w.applyCatchUp(ctx, fmt.Sprintf("caught-up:%s:%d:%d:%s", root.issue.Key, root.issue.Generation, through, w.bootID), fact); err != nil {
		w.admission.HoldForCatchUp(root.issue.Key)
		return err
	}
	return nil
}

func (w *workflowRuntime) applyCatchUp(ctx context.Context, eventID string, fact intake.Fact) error {
	if _, err := intake.ApplyFact(ctx, w.pool, "daemon", "awaiting-catch-up:"+eventID, fact, w.handlers...); err != nil {
		return fmt.Errorf("apply %s: %w", eventID, err)
	}
	return nil
}
