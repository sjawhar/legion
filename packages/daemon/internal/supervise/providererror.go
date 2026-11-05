package supervise

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A turn that ends in a provider error does not retire its task (dispatch://INFRA-956 R2). The
// work is still owed: the claim used to go idle holding nothing, and the only way to give the
// task back was an edit of the daemon's store. The task is kept here, and the same session is
// prompted with it again after a wait that grows to a limit, with no attempt limit and no new
// session.
//
// Three waits and one count make that up, and this file holds all four.
const (
	// promptRetryFirst and promptRetryMax bound the ladder: 30 seconds before the first re-send,
	// doubling to at most 10 minutes (promptRetryDelay). internal/backoff is not it — it draws
	// from [step/2, step] and caps at 32 times its base, which this ladder's exactness forbids.
	promptRetryFirst = 30 * time.Second
	promptRetryMax   = 10 * time.Minute
	// turnOutcomeWait is how long an error neither the error's own fields nor a report settles
	// waits for that report before it is taken as one that can clear. Silence is not a provider
	// saying "this cannot clear", and a Claude Code worker, whose bridge sends no Retryable and
	// often no status, would otherwise hold its issue at every failed turn.
	turnOutcomeWait = 2 * time.Minute
	// providerErrorRunTold is the run of provider-error turns at which the project's controller
	// is told, once (Deps.ProviderErrorRunReached). There is no hold and no attempt limit behind
	// it: a chain's bottom refusing deterministically costs one request every promptRetryMax.
	providerErrorRunTold = 6
)

// promptRetryDelay is the wait before the attempt-th send of a task a provider error took back,
// from attempt 1: 30 s, 60 s, 120 s, and never more than 10 minutes. There is no jitter — the
// ladder is one claim's own, not a herd's — and no attempt limit, so it flattens at the top
// rather than ending.
func promptRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Over this many doublings the shift overflows, and every one of them is past the cap.
	if attempt > 32 {
		return promptRetryMax
	}
	delay := promptRetryFirst << (attempt - 1)
	if delay > promptRetryMax {
		return promptRetryMax
	}
	return delay
}

// providerError is what the machine knows about the failed turn it is still deciding on: the
// delivery that turn worked, as takeBackPending left it, and the fields Oh My Pi reported with
// the error (StreamPromptResult). The provider's own message is not among them and never reaches
// this package.
//
// It is memory only, as the turn-outcome report it waits for is (turnoutcome.go): the decision
// takes at most turnOutcomeWait, and a daemon restarted inside that window reads a task sitting
// unconfirmed on its claim and sends it again, which is the same answer the wait would have
// reached.
type providerError struct {
	delivery  string
	retryable bool
	status    int
	model     string
}

// providerErrored is a turn that ran and ended in a provider error. The task is kept whatever the
// decision: it goes back unconfirmed under a new id, keeping its read mark, because the turn ran
// and the agent read it. No prompt budget is charged — chargePrompt retires and then fails a
// claim past Limits.PromptFailures, which would fail every claim at once in a provider outage,
// and R2 forbids an attempt limit — so the claim counts its own run of such turns instead, and a
// turn that ends with no error puts that run back to zero (promptResulted).
func (m *Machine) providerErrored(ctx context.Context, r StreamPromptResult) error {
	e := providerError{retryable: r.Retryable, status: r.HTTPStatus, model: r.Model}
	m.claim.ProviderErrorRun++
	m.log.Warn("supervise: the turn this task's prompt ran ended in a provider error; the task is kept and sent again",
		"delivery", r.DeliveryID, "retryable", r.Retryable, "httpStatus", r.HTTPStatus,
		"provider", r.Provider, "model", r.Model, "providerErrorRun", m.claim.ProviderErrorRun)
	m.tellControllerOfProviderErrors(e)
	if err := m.takeBackPending(ctx, taskRead); err != nil {
		return err
	}
	e.delivery = m.claim.Pending.ID
	m.providerError = &e
	return m.decideProviderError(ctx)
}

// decideProviderError says whether the error the machine is holding can clear, in this order:
//
//   - it can when Oh My Pi marked it retryable, or its HTTP status is 429 or any 5xx, or the
//     claim's newest turn-outcome report says the turn was refused. A content filter clears
//     because the plugin has already moved the session down its model chain, so the next prompt
//     goes to another model.
//   - it cannot, and the claim fails so the workflow holds the issue for its architect, only when
//     the error is marked not retryable and that report says provider-error. A malformed request
//     is the case this exists for.
//   - neither settles it: the report is waited for, once, for turnOutcomeWait, and the wait's
//     expiry takes the error as one that can clear (turnOutcomeWaitOver).
//
// A report counts only when it is the claim's newest (turnoutcome.go keeps that one) and of this
// claim's generation: a report of a generation the claim has left is another process's turn. One
// that arrives while the wait is armed decides at once, from turnOutcome.
func (m *Machine) decideProviderError(ctx context.Context) error {
	e := m.providerError
	if e == nil || pendingID(m.claim.Pending) != e.delivery {
		return nil
	}
	outcome := m.reportedOutcome()
	switch {
	case e.retryable, e.status == 429, e.status >= 500 && e.status <= 599, outcome == claim.TurnRefused:
		return m.providerErrorClears(ctx)
	case outcome == claim.TurnProviderError:
		cause := m.providerErrorCause(*e)
		m.providerErrorOver()
		return m.fail(ctx, "a provider error that cannot clear ended this claim's turn: "+cause)
	}
	if _, waiting := m.timers[TimerTurnOutcome]; waiting {
		return nil
	}
	m.log.Info("supervise: neither the error nor a report says whether this provider error can clear; waiting for the report",
		"delivery", e.delivery, "wait", turnOutcomeWait)
	m.arm(TimerTurnOutcome, turnOutcomeWait, e.delivery)
	return m.persist(ctx)
}

// providerErrorClears keeps the task on the claim and arms the next step of the ladder for it:
// the delivery counts one more send, carries the sentence the re-sent prompt is given
// (Delivery.ProviderErrorNote), and is written with the claim's run of provider-error turns in
// one transaction. Its timer's firing is what sends it (promptRetryDue); until then sendPending
// holds it, and a newer task queued meanwhile is not held at all.
func (m *Machine) providerErrorClears(ctx context.Context) error {
	e := *m.providerError
	cause := m.providerErrorCause(e)
	m.providerErrorOver()
	p := m.claim.Pending
	p.ProviderErrorRetries++
	p.ProviderErrorNote = providerErrorTask(cause)
	if err := m.saved(m.deps.Store.PutClaimAndDelivery(ctx, m.stored(), *p)); err != nil {
		return err
	}
	after := promptRetryDelay(p.ProviderErrorRetries)
	m.arm(TimerPromptRetry, after, p.ID)
	m.log.Info("supervise: this provider error can clear; the task goes to the same session again after a wait",
		"delivery", p.ID, "attempt", p.ProviderErrorRetries, "wait", after, "cause", cause)
	return nil
}

// promptRetryDue is the ladder's wait over: the task goes to the same session again. A claim
// still working the turn whose error armed this is handed nothing here, and the end of that turn
// sends it (turnEnded).
func promptRetryDue(m *Machine, ctx context.Context, _ Event) error {
	m.log.Info("supervise: the wait after a provider error is over; the task goes again",
		"delivery", pendingID(m.claim.Pending))
	return m.sendPending(ctx)
}

// turnOutcomeWaitOver is the wait for the turn-outcome report running out: the error is taken as
// one that can clear, and the task goes again on the ladder.
func turnOutcomeWaitOver(m *Machine, ctx context.Context, _ Event) error {
	if m.providerError == nil {
		return nil
	}
	m.log.Info("supervise: no turn-outcome report came within the wait; the provider error is taken as one that can clear",
		"delivery", m.providerError.delivery, "wait", turnOutcomeWait)
	return m.providerErrorClears(ctx)
}

// thisGenerationsReport is the claim's newest turn-outcome report when it is one of this claim's
// generation, and nil otherwise: a report of a generation the claim has left is another
// process's turn, and says nothing about this one.
func (m *Machine) thisGenerationsReport() *TurnOutcome {
	if r := m.lastTurnOutcome; r != nil && r.Generation == m.claim.Generation {
		return r
	}
	return nil
}

// reportedOutcome is the class that report gives, "" when there is no such report.
func (m *Machine) reportedOutcome() string {
	if r := m.thisGenerationsReport(); r != nil {
		return r.Outcome
	}
	return ""
}

// providerErrorCause names what ended the turn, in words built from structured fields alone: the
// category of a refusal a report named, else the HTTP status, else nothing more than that a
// provider failed. A refusal's own explanation is itself refused when it is sent back
// (dispatch://INFRA-956/artifact/cause-evidence-md), so the provider's text reaches neither the
// prompt this is written into nor the controller's notice.
func (m *Machine) providerErrorCause(e providerError) string {
	if r := m.thisGenerationsReport(); r != nil && r.Outcome == claim.TurnRefused && r.Category != "" {
		return "the provider's " + r.Category + " content filter"
	}
	if e.status != 0 {
		return "a provider error (HTTP " + strconv.Itoa(e.status) + ")"
	}
	return "a provider error"
}

// providerErrorTask is the sentence a task taken back by a provider error is sent behind, as
// interruptedTask is the sentence a task taken back by a death is sent behind. Another agent
// reads it, so it follows ASD-STE100: one instruction per sentence, and active voice.
func providerErrorTask(cause string) string {
	return "Your previous turn on this task stopped because of " + cause + ". Your work did not cause it. " +
		"The daemon sends you the task again. Read what that turn already did in your workspace and on the " +
		"issue's branch. Then continue the task."
}

// tellControllerOfProviderErrors tells the project's controller, once, when the claim's run of
// provider-error turns reaches providerErrorRunTold; a turn with no error puts the run back to
// zero, and only then can another notice go. It is the one exit from a run of such turns, and it
// is a notice rather than a hold: R2 forbids an attempt limit.
func (m *Machine) tellControllerOfProviderErrors(e providerError) {
	if m.claim.ProviderErrorRun != providerErrorRunTold || m.deps.ProviderErrorRunReached == nil {
		return
	}
	on := ""
	if e.model != "" {
		on = " on " + e.model
	}
	reason := fmt.Sprintf("%d turns in a row%s ended in %s; the daemon keeps the task and sends it again every %s at most",
		m.claim.ProviderErrorRun, on, m.providerErrorCause(e), promptRetryMax)
	m.log.Warn("supervise: a run of turns that ended in a provider error; the controller is told",
		"providerErrorRun", m.claim.ProviderErrorRun, "model", e.model, "reason", reason)
	m.deps.ProviderErrorRunReached(m.current(), reason)
}

// waitsForProviderError is whether either wait a provider error arms is armed for the delivery of
// this id. sendPending holds such a task; a newer task queued meanwhile is one no wait names, and
// it goes at once.
func (m *Machine) waitsForProviderError(delivery string) bool {
	for _, kind := range []TimerKind{TimerPromptRetry, TimerTurnOutcome} {
		if a, armed := m.timers[kind]; armed && a.deliveryID == delivery {
			return true
		}
	}
	return false
}

// providerErrorOver drops both waits and the error they were armed for: the decision is made, or
// the task they are for is gone (retirePending), or the process whose turn failed is gone
// (letGo).
func (m *Machine) providerErrorOver() {
	m.providerError = nil
	m.disarm(TimerPromptRetry)
	m.disarm(TimerTurnOutcome)
}
