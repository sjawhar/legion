package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/session"
)

// A copy the comment's one unanswered @role: mention may yet be pinned to goes in the pass that
// reads it, under the key that mention's own send carries at that very session: the session the
// sender pins is shown the comment once, because whichever of the copy and the mention reaches
// its host second repeats a key that host has claimed already
// (unpinnedRoleMentions.keyFor, roleFallbackKey, contracts.RoleForwardDedupeKey).
//
// The staging is the second road to one host, the one
// TestARoleForwardWaitsForAnUnpinnedRoleMentionOfItsHolder holds for a role the outbox can
// resolve: the issue is routed to a role this session holds, so the batch forwards the comment
// to its agent subject, and the comment mentions another role, whose attempt is live, names no
// session, and whose holder the outbox's own read cannot name - no claim stands for it here. The
// sender names one a round trip later and can name this very session.
//
// Waiting for that attempt is what the wider rule did, and it costs every such forward the 2 s
// re-read for a mention that reaches one session (resolveUnpinnedRoleMentions). The key costs
// that one session the mention's mode - the comment arrives as this copy rather than as a steer -
// and nothing else, which is the trade the round's ruling made.
func TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey(t *testing.T) {
	const routed = "routed-to-a-session-the-unanswered-mention-may-name"
	const mentioned = "mentioned-and-held-by-nobody-the-outbox-can-name"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + mentioned
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	rig.silent(t, contracts.RoleTopicPrefix+mentioned)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			// No suppression: the route is another role than the mention's target.
		},
	})
	// The attempt as the creating transaction leaves it: claimed, live, and naming no session.
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	// The copy is the one send this pass makes to that session, so a pass that made none is read
	// for what it is rather than timed out: the wait it took instead is the finding.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(recv.received()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	forwards := recv.received()
	if len(forwards) != 1 || forwards[0].DedupeKey != want {
		t.Fatalf("%s was sent %+v in the pass that read the comment, want one copy under %q, the key the mention of %s carries wherever its sender pins it: a copy under the event's own key %s waits for the attempt or shows that host the comment twice",
			holder, forwards, want, target, fmt.Sprintf("dispatch-%d", comment.ID))
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published in the pass that read it: attempt_count %d, publish_error %s, reached %v. Nothing of it waits: the one session its forward reaches takes the copy under the mention's own key",
			attempts, publishFailure(failure), reached)
	}

	// The sender's own resolution lands on this very session, as it may: it pins the session on
	// the row and the send lands under that same key, which the host drops
	// (coalesce(session_id, $6) in internal/dispatch/api/comment_delivery.go).
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2) where comment_id = $1 and target = $3
	`, commentID, holder, target); err != nil {
		t.Fatalf("settle the mention of %s: %v", target, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)

	if copies := recv.received(); len(copies) != 1 {
		t.Fatalf("%s was sent %+v once its mention was pinned and sent, want the one copy it already has: the copy carries that mention's own key, so the session is shown the comment once",
			holder, copies)
	}
}

// Two live unpinned attempts of one comment hold their copies as they did before the key: one key
// serves one mention, each carries a target of its own, and a copy under either would stand
// beside the other (unpinnedRoleMentions.keyFor).
//
// So the forward to a session either of them may be pinned to waits for them, and so does the
// copy bound for the session the issue's claim names, and the comment is published once both
// have settled.
func TestTwoUnpinnedRoleMentionsHoldTheirCopiesAsBefore(t *testing.T) {
	const routed = "routed-to-a-session-either-mention-may-name"
	const first = "mentioned-first-and-held-by-nobody-the-outbox-can-name"
	const second = "mentioned-second-and-held-by-nobody-the-outbox-can-name"
	const holder = "ses_holds_the_routed_role_and_claims_the_issue"
	ctx := store.WithTransactionTracking(context.Background())
	firstTarget, secondTarget := "role:"+first, "role:"+second
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + firstTarget + " @" + secondTarget + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: firstTarget, Delivery: "steer"}, {Target: secondTarget, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, firstTarget, "", "pending")
	seedDelivery(t, rig.database, commentID, secondTarget, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if forwards := recv.received(); len(forwards) != 0 {
		t.Fatalf("%s was sent %+v while two mentions of the comment were live and pinned to no session, want nothing: one key serves one mention, so neither copy can carry the key the mention that reaches this host will repeat",
			holder, forwards)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if slices.Contains(reached, contracts.AgentSubject(holder)) || slices.Contains(reached, contracts.RoleTopicPrefix+routed) {
		t.Fatalf("the comment reached %v, want neither the route %s nor %s among them: both copies wait for the comment's two unpinned mentions",
			reached, route, contracts.AgentSubject(holder))
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while two unpinned mentions of the comment are live", at)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the copies wait on two unpinned mentions, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}

	// Both senders resolve, find no session, and fail their attempts, as they do within a round
	// trip. Nothing is pinned, so the copies are the outbox's and go under the event's own key.
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
	waitFor(t, 10*time.Second, "the copies both mentions' failure owes", func() bool { return len(recv.received()) > 0 })
}

// A comment with two unpinned mentions, one past the lease the outbox waits a send out for and
// one live, holds the forward bound for a session either may be pinned to, as two live ones do:
// no key stands for either mention, one key serving one mention, and the live one may still be
// pinned to this very holder (unpinnedRoleMentions.keyFor,
// mentionHolds.waitsForAnUnnamedMention).
//
// That wait is what round 18 changed. It used to read the first state the comment's unnamed
// attempts came to, which is the lapsed one's here, and that is no state of the live mention:
// the forward went in this pass under the prefixed event key, a key that host never claimed,
// and the mention the sender pinned to it landed beside it.
func TestAForwardWaitsForALiveUnnamedMentionBesideALapsedOne(t *testing.T) {
	const routed = "routed-to-a-session-either-mention-may-name"
	const lapsed = "mentioned-first-whose-attempt-is-past-its-lease"
	const live = "mentioned-second-whose-attempt-is-live"
	const holder = "ses_holds_the_routed_role"
	ctx := store.WithTransactionTracking(context.Background())
	lapsedTarget, liveTarget := "role:"+lapsed, "role:"+live
	route := "role:" + routed

	rig := newRoleRig(t)
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+routed)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	body := "@" + lapsedTarget + " @" + liveTarget + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, Mentions: []model.Mention{
				{Target: lapsedTarget, Delivery: "steer"}, {Target: liveTarget, Delivery: "steer"}}},
		},
	})
	// The first attempt's sender died with the send in flight, so the delivery sweep owes it a
	// resend; the second is a send a sender claimed just now.
	seedDelivery(t, rig.database, commentID, lapsedTarget, "", "resumed")
	seedDelivery(t, rig.database, commentID, liveTarget, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if forwards := recv.received(); len(forwards) != 0 {
		t.Fatalf("%s was sent %+v while a live unpinned mention of the comment named no session and no key stood for it, want nothing: the sender can pin that mention to this very holder, and this copy would carry a key its host never claimed",
			holder, forwards)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while a live unpinned mention of the comment is still being sent", at)
	}
	attempts, _ := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on an unpinned mention, want 0: a wait is no failure", attempts)
	}

	// Both senders resolve, find no session and fail their attempts: nothing is pinned, so the
	// copy is the outbox's and goes under the event's own key.
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
	waitFor(t, 10*time.Second, "the copy both mentions' failure owes", func() bool { return len(recv.received()) > 0 })
}
