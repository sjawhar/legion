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

// seedCreatedAttempt records the mention of target on commentID exactly as the comment's own
// transaction records it (createCommentFor in internal/dispatch/api/comment_create.go): the
// comment_mentions row the attempt is opened under, in the mode the comment recorded, and attempt
// 1 of it, pending, claimed by the creating process and naming neither a session nor a resolve
// error - the shape of a mention whose listener lookup went unanswered within the comment's
// 250 ms window (resolveMentionTarget), whose sender pins the session on the row a round trip
// later (coalesce(session_id, $6) in internal/dispatch/api/comment_delivery.go).
//
// It is the statement that file runs, column for column, because what this reading counts as
// live and naming no session is exactly what the production row must read as
// (loadMentionDeliveries): a fixture that guesses at those columns tells nothing about the
// comment the API wrote.
func seedCreatedAttempt(t *testing.T, database *store.Store, commentID, target, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
		values ($1, $2, $3, null)
	`, commentID, target, mode); err != nil {
		t.Fatalf("record the mention of %s: %v", target, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, resolve_error, state, claimed_at)
		values ($1, $2, 1, $3, null, null, 'pending', now())
	`, commentID, target, mode); err != nil {
		t.Fatalf("open the unanswered attempt of %s: %v", target, err)
	}
}

// A session an unanswered @role: mention names is shown the comment once even when the outbox's
// own resolution of that role names no holder at all.
//
// TestACommentAnUnansweredRoleMentionNotYetPinnedReachesThatSessionOnce holds this for a role
// whose claim the outbox reads and resolves to that very session. That resolution is a guess at
// the session the mention's sender is about to pin, and the two are made by different readers a
// round trip apart: the outbox reads the role bucket itself when it publishes the comment
// (resolveRole, roles.Buckets.RoleClaim), while the mention's sender asks the listener, which
// resolves the role through its own registry, when it sends - after the comment committed. A
// role the outbox's read names no holder for is therefore not a role nobody holds: the claim can
// be made, or become visible to the listener, between the two, and the sender pins that session
// on the attempt row all the same.
//
// Measured on the real binaries at f4fb14fc with the listener behind a 600 ms round trip: the
// session that claims the issue and holds its role was shown each of 4 comments twice, under
// agent.<session>.<comment>:role:<role>:steer from its own mention and dispatch-<event id> from
// the claimant copy, while the comment's attempt rows show that very session pinned on them by
// the time they settled (role:reviewer:ses-r:sent). The route's own copy was held back by the
// target's live attempt throughout, so the attempt was read live and naming no session, and the
// copies bound for that session's own topic went out all the same: what the reading could not
// name was the holder of the role that attempt targets.
//
// So a reading that cannot name the holder of a role one of its live unpinned attempts targets
// holds back the copies bound for the sessions the owner's own work names - its route and its
// claimant, the copy at stake here - exactly as a resolution that named the holder holds that
// holder's: the hold is the attempt's own lease and no longer, and the session the sender pins
// is then whichever of them it turns out to be (unpinnedRoleMentions.liveUnnamed,
// destinations.addWorkingSession). The copies the comment's own thread names go out in the
// pass that reads them, under the event's own key (TestR17UnrelatedAuthorCopyFirstPass), and a
// copy such a mention may yet be pinned to that the outbox does send carries the key that
// mention would carry there (TestACopyAnUnnamedRoleMentionMayBePinnedToCarriesThatMentionsKey).
// Past that lease every copy of the comment is the fallback pass's, this one and a thread
// session's alike (TestR18UnnamedFallbackRowWaitsForResend).
func TestACommentAnUnansweredRoleMentionWhoseHolderTheOutboxCannotNameReachesThatSessionOnce(t *testing.T) {
	const role = "claimed-after-the-comment-was-published"
	const holder = "ses_claims_the_issue_and_takes_the_role"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role
	roleTopic := contracts.RoleTopicPrefix + role

	rig := newRoleRig(t)
	// No claim stands for the role when the outbox reads it: the listener names its holder to
	// the mention's sender a round trip later, and the sender pins that session on the row.
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
			SuppressRoute: true, SuppressedRoute: route, MentionsDecideRoutes: true,
		},
	})
	seedCreatedAttempt(t, rig.database, commentID, target, "steer")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	_, _, reached := eventState(t, rig.database, comment.ID)
	subject := contracts.AgentSubject(holder)
	if slices.Contains(reached, subject) {
		t.Fatalf("the comment reached %s while its only mention's attempt was live, named no session, and the outbox could name no holder for %s; the sender names one a round trip later and pins it on that row, so this copy shows the comment twice (reached %v)",
			subject, target, reached)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while a live attempt of %s names no session and no holder of that role can be named", at, target)
	}
	attempts, _ := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the copies wait on an unpinned mention, want 0: a wait is no failure", attempts)
	}

	// The sender's own resolution lands: the listener names the holder, the sender pins it on
	// the row and settles the attempt sent, as one does within the lease.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2)
		where comment_id = $1 and target = $3
	`, commentID, holder, target); err != nil {
		t.Fatalf("settle the mention of %s: %v", target, err)
	}
	dueNow(t, rig.database, comment.ID)
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once the mention of %s settled: attempt_count %d, publish_error %v, reached %v",
			target, attempts, failure, reached)
	}
	time.Sleep(time.Second)
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v, want nothing: its own mention of %s delivered the comment, and a copy under %s would show it that comment twice",
			holder, got, target, fmt.Sprintf("dispatch-%d", comment.ID))
	}
}
