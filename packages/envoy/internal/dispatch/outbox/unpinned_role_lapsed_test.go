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

// lapsedMention is the staging unpinnedRoleMentionPastItsLease builds, for the arms that read what
// becomes of the copy it held.
type lapsedMention struct {
	rig       *roleRig
	commentID string
	comment   model.Event
	target    string
	holder    string
	claimant  *receiver
}

// unpinnedRoleMentionPastItsLease builds the window this package's measurement found and runs the
// comment past it: a comment whose only mention is an unanswered @role:, whose attempt is live
// and pinned to no session when the outbox first reads it, and whose lease then lapses with the
// send still to be made - the sender's lane has not reached it, so the delivery sweep owes it
// (model.OwedMentionAttemptSQL) and the outbox waits on it no longer.
//
// The role's holder is the issue's claimant, so the comment has a copy bound for that session's
// own topic (destinations.addClaimantRoute, destinations.addSession); the thread root's author is
// a session no mention of the comment names, which is published to under the event's own key, as
// it was before this rule and after it.
//
// What the batch must not do once that lease lapses is publish the claimant's copy under the
// event's own key. The attempt names no session while the sender has pinned none, so that is the
// only key the batch's own reading of the attempts can give it - a key that host never claimed,
// published to a host the mention reaches a moment later under its own. The copy is kept as a
// comment_fallback_copies row instead, keyed by the session the attempt's role resolves to, and
// the pass that publishes it reads the comment's attempts as they stand then
// (destinations.leftTo, publishFallbackCopies). Nothing waits longer for it than the event did:
// the event publishes at the lease, as it always has.
func unpinnedRoleMentionPastItsLease(ctx context.Context, t *testing.T) lapsedMention {
	t.Helper()
	const mentioned = "mentioned-and-pinned-only-once-its-lease-has-lapsed"
	const holder = "ses_claims_the_issue_and_holds_the_mentioned_role"
	const author = "ses_wrote_the_thread_root"
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
	// The attempt as the creating transaction leaves it: claimed, live, and naming no session.
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v while its own mention of %s was live and unpinned, want nothing",
			holder, got, target)
	}
	waitFor(t, 10*time.Second, "the author's copy", func() bool { return len(wrote.received()) > 0 })
	copies := wrote.received()
	if len(copies) != 1 || copies[0].DedupeKey != fmt.Sprintf("dispatch-%d", comment.ID) {
		t.Fatalf("%s was sent %+v, want the author copy of event %d alone, under the event's own key: no attempt of this comment names that session",
			author, copies, comment.ID)
	}

	// The lease lapses with the send still to be made: the attempt was opened a lease ago and
	// the claim on it is that old too, which is model.MentionResumedSQL - the sender holds it
	// queued behind a lane, or died with it, and the delivery sweep owes it either way. The
	// outbox waits on it no longer.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set created_at = now() - $2::interval, claimed_at = now() - $2::interval
		where comment_id = $1 and target = $3
	`, commentID, model.DeliveryClaimLapse+time.Minute, target); err != nil {
		t.Fatalf("age the attempt of %s past its lease: %v", target, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v once the lease of its own mention of %s lapsed, want nothing: no attempt names that session while the sender has pinned none, so this copy carries the event's own key %s - one that host never claimed - and the mention lands beside it. The copy is the fallback pass's to key, from the attempts as they stand when it publishes them",
			holder, got, target, fmt.Sprintf("dispatch-%d", comment.ID))
	}
	row := readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder))
	if row.settled || row.published || row.attempts != 0 {
		t.Fatalf("the copy held for %s reads settled=%v published=%v attempt_count=%d, want an unsettled row no pass has decided yet",
			holder, row.settled, row.published, row.attempts)
	}
	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once its mention's lease lapsed: attempt_count %d, publish_error %v, reached %v. The row holds the copy; the event waits for nothing",
			attempts, failure, reached)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if slices.Contains(reached, contracts.AgentSubject(holder)) {
		t.Fatalf("the comment reached %v, want %s not among them: the copy bound for it is the fallback row's",
			reached, contracts.AgentSubject(holder))
	}
	return lapsedMention{rig: rig, commentID: commentID, comment: comment, target: target, holder: holder, claimant: claimant}
}

// A copy the outbox kept while an unanswered @role: mention's attempt was live and unpinned is
// published past that attempt's lease by the fallback pass, which reads the comment's attempts as
// they stand then rather than the keys the batch read when it kept the copy. By then the sender
// has pinned a session on the attempt, so the copy carries that mention's own dedupe key and the
// host that took the mention drops it.
//
// Measured on the real binaries at e4a1a3ab, in the staging latechaim.sh builds - a comment
// mentioning @role:reviewer whose listener lookup goes unanswered, the role claimed while the
// mentions are in flight, and a mention sender whose lane reaches each attempt a round trip at a
// time - 7 of 20 comments were shown to the role's holder twice: the outbox's copy under
// dispatch-<event id> at about +60.5 s, as the attempt's lease lapsed with its send still queued,
// and the mention itself under agent.<session>.<comment>:role:reviewer:steer 3 to 27 s later, as
// the lane reached it. The other 13 were the comments whose mentions landed inside the lease,
// where the copy is skipped outright.
//
// The two arms are what the pass finds when it publishes: a send that landed, where the copy is
// settled with nothing sent, and one that failed, where the copy goes under the mention's own
// key. Both leave that host one copy of the comment.
func TestACopyKeptPastAnUnpinnedRoleMentionsLeaseIsKeyedByThatMention(t *testing.T) {
	t.Run("a send that landed settles the copy with nothing sent", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		staged := unpinnedRoleMentionPastItsLease(ctx, t)

		// The sender's lane reaches the attempt at last: it resolves the role, pins the
		// session on the row and the send lands (coalesce(session_id, $6) in
		// internal/dispatch/api/comment_delivery.go).
		if _, err := staged.rig.database.Pool.Exec(ctx, `
			update comment_deliveries set state = 'sent', session_id = $2 where comment_id = $1 and target = $3
		`, staged.commentID, staged.holder, staged.target); err != nil {
			t.Fatalf("settle the mention of %s sent: %v", staged.target, err)
		}

		publishFallbackCopies(ctx, staged.rig.deps(), unfenced{})

		row := readHeldCopy(t, staged.rig.database, staged.commentID, contracts.AgentSubject(staged.holder))
		if !row.settled || row.published {
			t.Fatalf("the copy held for %s reads settled=%v published=%v, want it settled with nothing published: the mention delivered the comment",
				staged.holder, row.settled, row.published)
		}
		if got := staged.claimant.received(); len(got) != 0 {
			t.Fatalf("%s was sent %+v though its own mention of %s delivered the comment, want nothing",
				staged.holder, got, staged.target)
		}
	})

	t.Run("a send that failed publishes the copy under that mention's key", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		staged := unpinnedRoleMentionPastItsLease(ctx, t)

		// The same lane, the same resolution, and a send that fails: the session is pinned on
		// the row all the same, so the attempt names it and the key it went under is known.
		if _, err := staged.rig.database.Pool.Exec(ctx, `
			update comment_deliveries set state = 'failed', session_id = $2 where comment_id = $1 and target = $3
		`, staged.commentID, staged.holder, staged.target); err != nil {
			t.Fatalf("settle the mention of %s failed: %v", staged.target, err)
		}

		publishFallbackCopies(ctx, staged.rig.deps(), unfenced{})

		row := readHeldCopy(t, staged.rig.database, staged.commentID, contracts.AgentSubject(staged.holder))
		if !row.settled || !row.published {
			t.Fatalf("the copy held for %s reads settled=%v published=%v, want it published: its mention failed, so the outbox owes that session the comment",
				staged.holder, row.settled, row.published)
		}
		waitFor(t, 10*time.Second, "the copy the mention's failure owes", func() bool {
			return len(staged.claimant.received()) > 0
		})
		copies := staged.claimant.received()
		want := contracts.AgentSendDedupeKey(
			staged.holder, model.CommentMentionIdempotencyKey(staged.commentID, staged.target, "steer"))
		if len(copies) != 1 || copies[0].DedupeKey != want {
			t.Fatalf("%s was sent %+v, want one copy under %q, the key its mention's send went under: a send that landed before its attempt failed gave that host the comment already, and the event's own key %s is one it never claimed",
				staged.holder, copies, want, fmt.Sprintf("dispatch-%d", staged.comment.ID))
		}
	})
}
