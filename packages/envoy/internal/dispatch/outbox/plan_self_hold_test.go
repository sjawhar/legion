package outbox

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/session"
)

// A copy that waits holds the owner's later events back and holds no copy of its own event: the
// order the outbox keeps is the one between an owner's successive events, and the copies of one
// event have no order among themselves (planChains, blocker).
//
// TestAWaitingRoleForwardHoldsNoCopyOfItsOwnEvent holds this where the forward waits for its
// holder's own live mention, and there the event has no copy of that session at all: a session
// whose mention of the comment is live is left to that mention on every copy path
// (destinations.leftToMention). This is the shape where it does. The comment carries two
// unanswered @role: mentions, both live, both pinned to no session, and neither role's holder is
// one this read can name, so no one key stands for either mention and the event's role route
// waits for them (mentionHolds.waitsForAnUnnamedMention, unpinnedRoleMentions.keyFor). The role
// the issue is routed to is held by the session that wrote the thread root, so the same event
// carries that session's author copy, which reads those attempts as the mentions of no session
// it can name and goes in this very pass (destinations.addSession,
// resolveUnpinnedRoleMentions): a copy of the waiting forward's own event, on the key that
// forward holds.
//
// Held by a blocker carrying its own event, that copy settles on an outcome settleEvent has not
// written, whose zero state holdBack reads as released: no delay, no mentionPendingDelay clamp,
// the batch logging that NATS is unreachable and stopping, and the event read again at once
// until the attempts run out their lease. So the hold is read for the owner's later events alone
// (by.event != event, planChains).
func TestAWaitingCopyHoldsNoCopyOfItsOwnEvent(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	const role = "routed-to-the-session-that-wrote-the-thread-root"
	const holder = "ses_holds_the_routed_role_and_wrote_the_root"
	const first = "role:unanswered-first-when-the-comment-was-written"
	const second = "role:unanswered-second-when-the-comment-was-written"
	roleTopic := contracts.RoleTopicPrefix + role
	holderSubject := contracts.AgentSubject(holder)

	rig := newRoleRig(t)
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	route := "role:" + role
	seedIssue(t, rig.database, "T-1", &route)
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder}, "Who is on this?", nil)
	body := "@" + first + " @" + second + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: first, Delivery: "steer"}, {Target: second, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, first, "", "pending")
	seedDelivery(t, rig.database, commentID, second, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	// The defect's own signature: an event held by a blocker carrying itself reads an outcome
	// settleEvent has not written, whose zero state holdBack takes for a released one.
	released := 0
	var firstReleased string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "NATS is unreachable") {
			released++
			if firstReleased == "" {
				firstReleased = line
			}
		}
	}
	if released > 0 {
		t.Fatalf("the batch logged %d released outcomes with NATS up, the first %q: the author copy of this very event was held by the wait its own role forward to %s wrote, a blocker carrying the event itself (planChains, blocker.event), and holdBack read that event's unwritten outcome as released",
			released, firstReleased, holder)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if !slices.Contains(reached, holderSubject) {
		t.Fatalf("the comment reached %v, want %s among them: no attempt of this comment names that session, and the copy it gets as the thread root's author goes in the pass the role forward waits in",
			reached, holderSubject)
	}
	if slices.Contains(reached, roleTopic) {
		t.Fatalf("the comment reached %v, want the route %s not among them: its forward waits for two unpinned mentions no one key stands for",
			reached, route)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on the comment's unpinned mentions, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while the role forward waits on the comment's unpinned mentions", at)
	}

	// Both senders resolve, find no session and fail their attempts, as they do within a round
	// trip: the forward goes then, under the event's own key prefixed.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("fail the comment's mentions: %v", err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once both mentions settled: attempt_count %d, publish_error %s, reached %v",
			attempts, publishFailure(failure), reached)
	}
	waitFor(t, 10*time.Second, "the route's forward", func() bool { return len(recv.received()) > 1 })
}
