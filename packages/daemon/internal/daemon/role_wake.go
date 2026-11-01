package daemon

import (
	"context"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// projectClaimRole is the role token's claim is on, and whether token is a claim of this project
// at all. A role topic carries a claim token (notify.RoleTopicPrefix), and the token ends in its
// role, so the role is read from the name rather than from a store the hot path would have to
// wait on.
func (r *outbox) projectClaimRole(token string) (claim.Role, bool) {
	if !strings.HasPrefix(token, "legion-"+r.project+"-") {
		return "", false
	}
	for _, role := range claim.Roles {
		if strings.HasSuffix(token, "-"+string(role)) {
			return role, true
		}
	}
	return "", false
}

// roleTopicClaim is the claim a role topic names, and whether it is one of this project's.
func (r *outbox) roleTopicClaim(topic string) (claim.Token, claim.Role, bool) {
	token, isRole := strings.CutPrefix(topic, notify.RoleTopicPrefix)
	if !isRole {
		return "", "", false
	}
	role, ok := r.projectClaimRole(token)
	if !ok {
		return "", "", false
	}
	return claim.Token(token), role, true
}

// wakeIdleSuspendedRole brings back the worker a message could not reach, when the idle suspend is
// why it could not. why names the message, for the line it logs and the resume's rule.
//
// It is a dispatch on the claim as this daemon holds it rather than one condition, because the
// states a role topic can be unreachable in differ in what would end it:
//
//   - suspended by the idle suspend, and resumable by rule: the process was taken from a worker
//     whose session is kept, so a resume brings that same agent back and its plugin takes the
//     Envoy role again. This is the one branch that acts.
//   - running and holding no role: a pane whose session lost its role claim. The fix there is to
//     have that pane claim its role again, which is a separate diagnosis and deliberately not
//     built here; the branch belongs beside this one when it is.
//   - anything else — an operator's suspension (RuleMayResume refuses it, and only the operator
//     ends it), a claim that failed, retired or this daemon does not supervise, a worker suspended
//     with its lingering tree, or a role the idle suspend is not for at all: nothing here may act.
//
// A resume that cannot be issued is logged and nothing else: the message the wake was for is held
// or refused by its own caller, which tries again.
func (r *outbox) wakeIdleSuspendedRole(ctx context.Context, token claim.Token, role claim.Role, why string) {
	if !supervise.IdleSuspendable(role) {
		return
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		return
	}
	held := machine.View().Claim
	switch {
	case held.SuspendedBy == supervise.SuspendedByIdle && held.RuleMayResume():
		wakeable, err := r.issueWakeable(ctx, held.Issue)
		if err != nil {
			r.log.Warn("outbox could not read the issue of an idle-suspended worker a message names",
				"claim", token, "issue", held.Issue, "why", why, "error", err)
			return
		}
		if !wakeable {
			return
		}
		rule := "a message found no holder on its role topic: " + why
		if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: token, Reason: rule, Only: supervise.SuspendedByIdle}); err != nil {
			r.log.Warn("outbox could not resume the idle-suspended worker a message names",
				"claim", token, "issue", held.Issue, "why", why, "error", err)
			return
		}
		r.log.Info("outbox resumed the idle-suspended worker a message names",
			"claim", token, "issue", held.Issue, "why", why)
	default:
		// Every other state is somebody else's to end, the running-but-unheld pane included.
	}
}

// wakeNoticedRole resumes the worker a notice tells the architect to message. A notice goes to the
// architect's own role topic, and one that carries a Topic names another role's — a review-stuck
// notice names its reviewer's, which the architect answers with its own `envoy_publish`
// (internal/prompts/go/architect-common.md). That publish is refused outright while nobody holds
// the role (the listener's 404, notify.ErrNoHolder). The listener's `role_unheld` report of the
// refusal resumes the worker too (wakeExceptionRole), but only after the architect's publish has
// failed, so a worker the idle suspend took the process from is resumed here, as the notice goes
// out, and holds its role again by the time the architect answers.
//
// A stuck review round is the one path the daemon cannot end on its own — the reviewer must submit
// a review — so a reviewer nobody can reach freezes the round until a person acts.
func (r *outbox) wakeNoticedRole(ctx context.Context, topic string) {
	token, role, ok := r.roleTopicClaim(topic)
	if !ok {
		return
	}
	r.wakeIdleSuspendedRole(ctx, token, role, "a notice names its role topic")
}

// wakeExceptionRole answers a role-lane exception that is not a notice of this daemon's to an
// architect: the listener could not deliver a message on a worker's role topic — a peer agent's
// `envoy_publish` to a role that finished its phase, or a message of the daemon's own. Either it
// accepted the publish and could not forward it, because the holder it resolved is a session that
// is no longer running (`delivery_failed`, `no_holder`, `receipt_timeout`), or its confirmed route
// refused the publish with its 404 because nobody holds the role (`role_unheld`): the role was
// never claimed, or its holder's registration has lapsed, which is how a worker the idle suspend
// stopped looks once its stopped session's registration has gone. When the idle suspend is what
// stopped that session, the worker is resumed here, so the sender's next attempt reaches it.
//
// Every `role_unheld` report reaches this wake. The listener copies its dedupe key, source and
// source session from the publish request, so none can prove that this daemon published it: a peer
// can choose the idempotency key `legion-outbox:<id>` and make the same-looking report. This can
// relaunch an idle-suspended phase worker for the daemon's own phase-ended message, which its
// executor already finishes undelivered because the message is not useful later. That cost is one
// resume of a worker the idle suspend took, confined by wakeIdleSuspendedRole's phase-role and
// suspension checks; suppressing a peer's valid wake instead leaves the worker unreachable. A
// report of any other shape changes nothing.
func (r *outbox) wakeExceptionRole(ctx context.Context, exception roleLaneException) error {
	switch exception.Reason {
	case "delivery_failed", "no_holder", "receipt_timeout", "role_unheld":
	default:
		return nil
	}
	token, role, ok := r.roleTopicClaim(exception.OriginalTopic)
	if !ok {
		return nil
	}
	r.wakeIdleSuspendedRole(ctx, token, role, "the listener could not deliver a message to its role ("+exception.Reason+")")
	return nil
}
