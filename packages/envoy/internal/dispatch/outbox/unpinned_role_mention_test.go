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

// A session an unanswered @role: mention names is shown the comment once even while the sender
// has not pinned it on the attempt row yet, which is the state every such comment is in when the
// outbox first reads it.
//
// TestACommentAnUnansweredRoleMentionPinnedReachesThatSessionOnce holds this for an attempt whose
// session_id the sender has already written. That is the state after a round trip to the
// listener, and the comment's own lookup of that target went unanswered precisely because the
// listener did not answer within 250 ms, so the sender's resolution - another round trip to the
// same listener - is slower still, while the outbox publishes the comment as soon as it commits.
// Until that write lands the row names only the target (session_id null), model.MentionSessionSQL
// counts it under no session, and nothing of the mention rule knows which session the copy
// belongs to.
//
// Measured on the real binaries at 86760ef3 with the listener behind a 600 ms round trip, which
// is what makes the comment's own lookup time out while the sender's send still lands: the
// session that claims the issue and holds its role was shown each of 4 comments twice, under
// agent.<session>.<comment>:role:<role>:steer from its own mention and dispatch-<event id> from
// the claimant copy. Main at the merge base shows it once there, because main resolves the
// mention inside the comment's request and the payload names the session.
//
// What the copy must do instead is what a live mention does everywhere else: wait. An attempt of
// this comment that is live and names no session yet can still turn out to be this session's, so
// the copies bound for a session's own topic wait for it to settle, bounded by that attempt's own
// lease, exactly as destinations.leftTo holds a copy back for a live attempt it can name.
func TestACommentAnUnansweredRoleMentionNotYetPinnedReachesThatSessionOnce(t *testing.T) {
	const role = "routed-to-the-session-its-mention-will-pin"
	const holder = "ses_claims_the_issue_and_holds_the_role"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role
	roleTopic := contracts.RoleTopicPrefix + role

	rig := newRoleRig(t)
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, roleTopic)
	claimant := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	route := target
	seedIssue(t, rig.database, "T-1", &route)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			// What the creating transaction records when the lookup of the route's own target
			// went unanswered: the route string alone, no session.
			SuppressRoute: true, SuppressedRoute: route,
		},
	})
	// The attempt as the creating transaction leaves it: claimed, live, and naming no session,
	// because only the sender's own resolution learns which session holds the role.
	if _, err := rig.database.Pool.Exec(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, created_at, claimed_at)
		values ($1, $2, 1, 'steer', null, 'pending', now(), now())
	`, commentID, target); err != nil {
		t.Fatalf("create the unpinned attempt of %s: %v", target, err)
	}

	holding(t, rig.deps()).scan(ctx, unfenced{})

	// Whatever the batch does with the event itself, the session the mention is about to reach
	// must not have been sent a copy of this comment on its own topic.
	_, _, reached := eventState(t, rig.database, comment.ID)
	subject := contracts.AgentSubject(holder)
	if slices.Contains(reached, subject) {
		t.Fatalf("the comment reached %s while its only mention's attempt was live and not yet pinned to that session; the mention is on its way to the same host, so this copy shows the comment twice (reached %v)",
			subject, reached)
	}
	time.Sleep(2 * time.Second)
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v while its own mention of %s was still live and unpinned, want nothing: that mention is on its way to the same host, so this copy shows it the comment twice",
			holder, got, fmt.Sprintf("dispatch-%d", comment.ID))
	}
}
