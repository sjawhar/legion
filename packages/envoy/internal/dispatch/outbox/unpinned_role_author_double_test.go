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
)

// The session that wrote the thread a comment replies to is sent that comment in the pass that
// reads the event, under the event's own key, while the comment's one unanswered @role: mention
// is live, names no session, and this read can name no holder for the role it targets. The
// mention's sender can pin that very session a round trip later, and then that session is shown
// the comment twice: this copy under dispatch-<event id>, and the mention's own steer under
// agent.<session>.<comment>:role:<role>:steer, which are two keys and so two messages at one
// host.
//
// That double is a trade this head accepts, not a defect of it, and this test holds the shape
// that produces it so that no later change alters it without saying so.
//
// Neither way out of it is open. Keying this copy by the mention is what
// TestR17UnrelatedAuthorCopyOrder forbids: the key it would carry is the mention's, not the
// event's, and an owner's successive events must reach a destination under their own keys and in
// id order, so comment N under a mention's key and comment N+1 under dispatch-<event id> is the
// ordering that test reads as broken. Holding the copy back instead is what the wider rule did
// before this head, and it cost every session an unanswered mention does not name - an author, a
// follower, a document's author - the 2 s re-read the event waits out, for a mention that
// reaches one session. So the copies this head holds back are only the ones bound for the
// sessions the owner's own work names, its route and its claimant, which stay in the event's
// destination list marked waiting (destinations.addWorkingSession,
// unpinnedRoleMentions.unnamedState); every other copy goes in the pass that reads the event,
// under the event's own key (destinations.addSession).
//
// The window the double is possible in is the attempt's own lease and no longer,
// model.DeliveryClaimLapse: past it the attempt reads resumed or abandoned rather than live
// (model.MentionResumedSQL, model.MentionAbandonedSQL), the copies that wait go, and the
// mention's send is the delivery sweep's rather than a send the outbox is waiting out.
//
// Measured on the real binaries at this head, 20 comments in this very staging: 2 of the 20
// reached this session twice, the author copy first both times (+0.275 s and +0.401 s) and the
// mention after it (+7.221 s and +11.765 s), while the other 18 reached it once. With the same
// session also the issue's claimant it was 0 of 20, because then its copy is one this head holds
// back until the attempt settles (TestACommentAnUnansweredRoleMentionWhoseHolderTheOutboxCannotNameReachesThatSessionOnce).
func TestAnUnnamedRoleMentionPinnedToTheThreadAuthorShowsItTheCommentTwice(t *testing.T) {
	const role = "mentioned-and-held-by-nobody-this-read-can-name"
	const author = "ses_wrote_the_thread_root"
	const claimant = "ses_claims_the_issue"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role
	route := target

	rig := newRoleRig(t)
	// No claim stands for the role when the outbox reads it, so this read can name no holder
	// for the role the comment mentions and the issue is routed to; the listener names one to
	// the mention's sender a round trip later, and the sender pins it on the attempt row.
	rig.silent(t, contracts.RoleTopicPrefix+role)
	wrote := rig.receiver(t, author, true)
	claims := rig.receiver(t, claimant, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", &route)
	// The claim is another session's, so the copy the owner's own work names is not the thread
	// author's: that is the difference between 2 of 20 and 0 of 20.
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: claimant})
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "Who is on this?", nil)
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			// What the creating transaction records when the lookup of the route's own target
			// went unanswered: the route string alone, no session.
			SuppressRoute: true, SuppressedRoute: route, MentionsDecideRoutes: true,
		},
	})
	seedCreatedAttempt(t, rig.database, commentID, target, "steer")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	eventKey := fmt.Sprintf("dispatch-%d", comment.ID)
	mentionKey := contracts.AgentSendDedupeKey(author, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	_, _, reached := eventState(t, rig.database, comment.ID)
	authorSubject := contracts.AgentSubject(author)
	if !slices.Contains(reached, authorSubject) {
		t.Fatalf("the comment reached %v, want %s among them: no mention of it names the thread root's author, so its copy goes in the pass that reads the event; held back for the unnamed mention it would wait out that attempt's lease (model.DeliveryClaimLapse = %v), which is what every unrelated session paid before this head",
			reached, authorSubject, model.DeliveryClaimLapse)
	}
	waitFor(t, 10*time.Second, "the thread author's copy", func() bool { return len(wrote.received()) > 0 })
	copies := wrote.received()
	if len(copies) != 1 {
		t.Fatalf("%s was sent %+v in the pass that read event %d, want its author copy alone", author, copies, comment.ID)
	}
	if copies[0].DedupeKey == mentionKey {
		t.Fatalf("the thread author's copy is keyed %q, the key the mention of %s carries at that session, want the event's own key %q: a copy so keyed makes comment N reach this destination under a mention's key and comment N+1 under its own, which TestR17UnrelatedAuthorCopyOrder reads as an owner's events out of order",
			copies[0].DedupeKey, target, eventKey)
	}
	if copies[0].DedupeKey != eventKey {
		t.Fatalf("the thread author's copy is keyed %q, want the event's own key %q: a session no mention of the comment names is published to as if it had not been mentioned",
			copies[0].DedupeKey, eventKey)
	}

	// The other arm of the same read: the copies bound for the sessions the owner's own work
	// names wait for that attempt, so nothing but this author copy went out this pass.
	claimantSubject := contracts.AgentSubject(claimant)
	if slices.Contains(reached, claimantSubject) {
		t.Fatalf("the comment reached %v, want %s not among them: that session claims the issue, so its copy waits for an attempt that names no session yet and whose role this read can name no holder for",
			reached, claimantSubject)
	}
	if got := claims.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v while the mention of %s was live and named no session, want nothing: the sender can pin that mention to the issue's claimant", claimant, got, target)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while a live attempt of %s names no session and the copies the owner's own work names wait for it", at, target)
	}
	attempts, _ := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while those copies wait on an unpinned mention, want 0: a wait is no failure", attempts)
	}

	// The sender's own resolution lands on the thread's author, as it may: it pins that session
	// on the attempt row and settles the send, whose own key is the mention's.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2)
		where comment_id = $1 and target = $3
	`, commentID, author, target); err != nil {
		t.Fatalf("settle the mention of %s on %s: %v", target, author, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once the mention of %s settled: attempt_count %d, publish_error %v, reached %v",
			target, attempts, failure, reached)
	}
	time.Sleep(time.Second)
	// The outbox sends that session nothing further - its copy went, and the settled mention
	// leaves it nothing to publish - so the comment stands at its host under two keys: this copy
	// under the event's own, and the mention's steer under the mention's. That is the double.
	copies = wrote.received()
	if len(copies) != 1 || copies[0].DedupeKey != eventKey {
		t.Fatalf("%s was sent %+v once its mention settled, want the one author copy under %q: the mention's own send carries %q, which this copy does not claim, so that host is shown the comment twice",
			author, copies, eventKey, mentionKey)
	}
}
