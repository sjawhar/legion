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

// The second road to one host, which TestACommentAnUnansweredRoleMentionNotYetPinnedReachesThatSessionOnce
// does not reach: the role forward of the event's own route. There the comment's only mention is
// of the route's own target, so suppressesCurrentRoute leaves the route out and the forward is
// never made; here the issue is routed to another role, whose holder is the very session the
// mention of the first one is about to be pinned to. Nothing suppresses that route, so the batch
// resolves it, forwards the comment to that holder's agent subject
// (resolveRoleHolders, roleForward.forward), and the host is shown the comment twice: once under
// the event's key with contracts.RoleForwardDedupePrefix in front, a key it never claimed, and
// once as the mention lands under agent.<session>.<comment>:role:<role>:steer.
//
// The forward has a wait of its own for exactly this (roleCopyWaitsForAMention), and it had the
// same blind spot: it asked the state of the holder's own attempts
// (mentionDeliveries.sessions), and an attempt the sender has not pinned a session on yet counts
// under no session at all (model.MentionSessionSQL), so the holder it is on its way to reads
// mentionFailed and the forward goes. What names that holder is the attempt's own role, resolved
// as the route's is (unpinnedRoleMentions, resolveUnpinnedRoleMentions), and both roads ask one
// question of it (mentionHolds.live, destinations.leftToMention).
//
// So the forward waits, for that attempt's own lease and no longer, and once the sender has
// settled the attempt and pinned its session the copy goes under that mention's own key, which
// the host drops (roleFallbackKey).
func TestARoleForwardWaitsForAnUnpinnedRoleMentionOfItsHolder(t *testing.T) {
	const routed = "routed-to-the-holder-of-the-mentioned-role"
	const mentioned = "mentioned-with-no-session-pinned-yet"
	const holder = "ses_holds_the_routed_role_and_the_mentioned_one"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + mentioned
	route := "role:" + routed

	rig := newRoleRig(t)
	// One session holds both roles: the issue's route, which reaches it by a forward, and the
	// role the comment mentions, which reaches it by that mention's own send.
	rig.claim(t, routed, holder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, mentioned, holder, session.SessionEntry{SelfSubscribed: true})
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
			// No suppression: the route is another role than the mention's target, so the
			// route's copy stands and the batch forwards it to the role's holder.
		},
	})
	// The attempt as the creating transaction leaves it: claimed, live, and naming no session,
	// because only the sender's own resolution learns which session holds the role.
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if forwards := recv.received(); len(forwards) > 0 {
		t.Fatalf("the batch forwarded %s %d copies of the comment, the first under %q, while the mention of %s was live and pinned to no session yet, want none: that mention is on its way to the same host, so this copy shows it the comment twice",
			holder, len(forwards), forwards[0].DedupeKey, target)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if slices.Contains(reached, contracts.RoleTopicPrefix+routed) {
		t.Fatalf("the comment reached %v, want the route %s not among them: its forward waits for the mention of %s, which the role the route names resolves to the same session",
			reached, route, target)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while the route's forward waits on the unpinned mention of %s", at, target)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on a mention, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}

	// The sender's own resolution lands: it pins the session on the row and settles the attempt
	// sent, as one does within the lease (coalesce(session_id, $6) in
	// internal/dispatch/api/comment_delivery.go).
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = $2 where comment_id = $1 and target = $3
	`, commentID, holder, target); err != nil {
		t.Fatalf("settle the mention of %s: %v", target, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once the mention of %s settled: attempt_count %d, publish_error %v, reached %v",
			target, attempts, failure, reached)
	}
	forwards := recv.received()
	if len(forwards) != 1 {
		t.Fatalf("%s was sent %+v once its mention settled, want the route's one forward", holder, forwards)
	}
	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	if forwards[0].DedupeKey != want {
		t.Fatalf("the copy forwarded to %s is keyed %q, want the key of the mention its sender pinned to that session %q, which its host drops",
			holder, forwards[0].DedupeKey, want)
	}
}

// The hold is the role's holder and nobody else: a session the comment reaches for a reason of
// its own, whom no mention of it names, is sent its copy in the pass that holds the holder's
// back. An unpinned attempt whose role this read resolves says nothing about that session - the
// role it names resolves to one session, which is the one session its send can reach - so
// holding every copy of the comment would delay an author, a follower or a claimant the mention
// will never deliver anything to.
//
// A role this read can name no holder for is the other arm, and holds back only the copies bound
// for the sessions the owner's own work names - its route and its claimant - every other copy
// going in the pass that reads it under the event's own key
// (resolveUnpinnedRoleMentions, destinations.addWorkingSession,
// TestR17UnrelatedAuthorCopyFirstPass).
//
// Here both sessions are on one comment: the issue's claimant holds the role the comment
// mentions, with the mention's attempt live and pinned to no session yet, and the thread root's
// author is a session nothing of the comment names. One scan leaves the claimant's copy to the
// mention and sends the author's.
func TestAnUnpinnedRoleMentionHoldsBackNoOtherSessionsCopy(t *testing.T) {
	const mentioned = "mentioned-while-another-session-is-owed-its-copy"
	const holder = "ses_claims_the_issue_and_holds_the_mentioned_role"
	const author = "ses_wrote_the_thread_root"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + mentioned

	rig := newRoleRig(t)
	rig.claim(t, mentioned, holder, session.SessionEntry{SelfSubscribed: true})
	claimant := rig.receiver(t, holder, true)
	wrote := rig.receiver(t, author, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "Who is on this?", nil)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	_, _, reached := eventState(t, rig.database, comment.ID)
	authorSubject := contracts.AgentSubject(author)
	if !slices.Contains(reached, authorSubject) {
		t.Fatalf("the comment reached %v, want %s among them: no mention of this comment names the thread root's author, and the hold an unpinned mention puts on the role's holder delays no other session's copy",
			reached, authorSubject)
	}
	waitFor(t, 10*time.Second, "the author's copy", func() bool { return len(wrote.received()) > 0 })
	copies := wrote.received()
	if len(copies) != 1 || copies[0].DedupeKey != fmt.Sprintf("dispatch-%d", comment.ID) {
		t.Fatalf("%s was sent %+v, want the author copy of event %d alone, under the event's own key",
			author, copies, comment.ID)
	}
	if slices.Contains(reached, contracts.AgentSubject(holder)) {
		t.Fatalf("the comment reached %v, want %s not among them: that session holds the role the comment mentions, and its mention is on its way to the same host",
			reached, contracts.AgentSubject(holder))
	}
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v while its own mention of %s was live and unpinned, want nothing", holder, got, target)
	}
}
