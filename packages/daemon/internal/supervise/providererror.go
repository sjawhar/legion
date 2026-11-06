package supervise

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A turn that ends in a provider error does not retire its task (dispatch://INFRA-956 R2). The
// work is still owed: the claim used to go idle holding nothing, and the only way to give the
// task back was an edit of the daemon's store. The task is kept here, and the same session is
// prompted with it again after a wait that grows to a limit, with no attempt limit and no new
// session.
//
// Two bounds and one count make that up, and this file holds all three.
const (
	// promptRetryFirst and promptRetryMax bound the ladder: 30 seconds before the first re-send,
	// doubling to at most 10 minutes (promptRetryDelay). internal/backoff is not it — it draws
	// from [step/2, step] and caps at 32 times its base, which this ladder's exactness forbids.
	promptRetryFirst = 30 * time.Second
	promptRetryMax   = 10 * time.Minute
	// providerErrorRunTold is the run of provider-error turns at which the project's controller
	// is told, once (Deps.ProviderErrorRunReached). There is no hold and no attempt limit behind
	// it: a chain's bottom refusing deterministically costs one request every promptRetryMax.
	providerErrorRunTold = 6
)

// promptRetryDelay is the wait before the attempt-th send of a task a provider error took back,
// from attempt 1: 30 s, 60 s, 120 s, 240 s, 480 s, and 10 minutes from the sixth send on. There
// is no jitter — the ladder is one claim's own, not a herd's — and no attempt limit, so it
// flattens at the top rather than ending.
//
// The shift is clamped before it is taken, as internal/backoff clamps its own: five doublings of
// 30 seconds is already 16 minutes, past the cap, and a shift any wider than that leaves int64
// and comes back a negative wait, which arms the timer to fire at once and sends the task with no
// wait at all.
func promptRetryDelay(attempt int) time.Duration {
	return min(promptRetryFirst<<min(max(attempt-1, 0), 5), promptRetryMax)
}

// providerError is what the machine knows about the failed turn it is deciding on: the fields
// Oh My Pi reported with the error (StreamPromptResult). The provider's own message is not
// among them and never reaches this package.
type providerError struct {
	status int
	model  string
}

// providerErrored is a turn that ran and ended in a provider error. The task is kept whatever the
// decision: it goes back unconfirmed under a new id, keeping its read mark, because the turn ran
// and the agent read it. No prompt budget is charged — chargePrompt retires and then fails a
// claim past Limits.PromptFailures, which would fail every claim at once in a provider outage,
// and R2 forbids an attempt limit — so the claim counts its own run of such turns instead, and a
// turn that ends with no error puts that run back to zero (promptResulted).
//
// The decision is made here, from this prompt's own result and nothing else
// (dispatch://INFRA-956/comment/4fa88c00-3313-4008-8eea-d7f95a04e24d): the status the provider
// answered either holds the issue (holdsTheIssue) or the task goes to the same session again on
// the ladder. Oh My Pi's Retryable flag is logged and read by nothing: the status alone decides.
func (m *Machine) providerErrored(ctx context.Context, r StreamPromptResult) error {
	e := providerError{status: r.HTTPStatus, model: r.Model}
	m.claim.ProviderErrorRun++
	m.log.Warn("supervise: the turn this task's prompt ran ended in a provider error; the task is kept and sent again",
		"delivery", r.DeliveryID, "retryable", r.Retryable, "httpStatus", r.HTTPStatus,
		"provider", r.Provider, "model", r.Model, "providerErrorRun", m.claim.ProviderErrorRun)
	m.tellControllerOfProviderErrors(e)
	if err := m.takeBackPending(ctx, taskRead); err != nil {
		return err
	}
	if holdsTheIssue(e.status) {
		return m.fail(ctx, "a provider error that cannot clear ended this claim's turn: "+m.providerErrorCause(e))
	}
	return m.providerErrorClears(ctx, e)
}

// holdsTheIssue is whether an error of this HTTP status fails the claim, so that the workflow
// holds the issue for its architect: a client error, 400 to 499, that is neither a timeout (408)
// nor a rate limit (429). A malformed request is the case this exists for. Every other error
// sends the task again:
//
//   - 5xx, 408 and 429 are the provider saying "not now", which the ladder waits out;
//   - no status at all is every content-filter refusal, which ends a turn with nothing but a
//     message. It clears because the plugin has already moved the session down its model chain,
//     so the next prompt goes to another model. A Claude Code worker's bridge names a status
//     only for a result Claude Code marked api_error (internal/claudebridge's result), so its
//     statusless turns go again here too.
func holdsTheIssue(status int) bool {
	return status >= 400 && status <= 499 &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// providerErrorClears keeps the task on the claim and arms the next step of the ladder for it:
// the delivery counts one more send, carries the sentence the re-sent prompt is given
// (Delivery.ProviderErrorNote), and is written with the claim's run of provider-error turns in
// one transaction. Its timer's firing is what sends it (promptRetryDue); until then sendPending
// holds it, and a newer task queued meanwhile replaces it rather than waiting behind it (queue).
func (m *Machine) providerErrorClears(ctx context.Context, e providerError) error {
	cause := m.providerErrorCause(e)
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

// providerErrorCause names what ended the turn, in words built from structured fields alone: the
// HTTP status the provider answered, else nothing more than that a provider failed. A refusal's
// own explanation is itself refused when it is sent back
// (dispatch://INFRA-956/artifact/cause-evidence-md), so the provider's text reaches neither the
// prompt this is written into nor the controller's notice. A content filter's refusal carries no
// status, so it is the second of the two.
func (m *Machine) providerErrorCause(e providerError) string {
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

// waitsForProviderError is whether the ladder's wait is armed for the delivery of this id.
// sendPending holds such a task, and queue replaces it with a newer one; a task that replaced it
// is one no wait names, and it goes at once.
func (m *Machine) waitsForProviderError(delivery string) bool {
	a, armed := m.timers[TimerPromptRetry]
	return armed && a.deliveryID == delivery
}
