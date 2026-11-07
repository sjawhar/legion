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
// that produces it so that no later change alters it without saying so. It is one rule and not
// three: a session that is sent a copy the hold does not cover, and that the sender then pins
// the mention to, is shown the comment twice. A follower and a primary document's author are
// the same session to this rule, each with a test of its own beside this one
// (TestAnUnnamedRoleMentionPinnedToAFollowerShowsItTheCommentTwice,
// TestAnUnnamedRoleMentionPinnedToAPrimaryDocumentAuthorShowsItTheCommentTwice).
//
// Neither way out of it is open. Keying this copy by the mention is what
// TestR17UnrelatedAuthorCopyOrder forbids: the key it would carry is the mention's, not the
// event's, and an owner's successive events must reach a destination under their own keys and in
// id order, so comment N under a mention's key and comment N+1 under dispatch-<event id> is the
// ordering that test reads as broken. Holding the copy back instead is what the wider rule did
// before this head, and it cost every session an unanswered mention does not name - an author, a
// follower, a document's author - the 2 s re-read the event waits out, for a mention that
// reaches one session. So the copies this head holds back while the attempt is live are only
// the ones bound for the sessions the owner's own work names, its route and its claimant, which
// stay in the event's destination list marked waiting (destinations.addWorkingSession,
// unpinnedRoleMentions.liveUnnamed); every other copy goes in the pass that reads the event,
// under the event's own key (destinations.addSession).
//
// What model.DeliveryClaimLapse bounds is the publishing of that copy and not the double: a
// copy goes out under the event's own key only while the attempt is live, past which the
// attempt's state is every session's and every copy of the comment is the fallback pass's
// (unpinnedRoleMentions.resendingUnnamed, TestR18UnnamedFallbackRowWaitsForResend). The mention
// it stands beside lands whenever its send does - the sweep resends an attempt past that lease,
// and a sender holding it queued sends it when its lane reaches it - which no lease bounds.
//
// Measured on the real binaries at this head, 20 comments in this very staging: 2 of the 20
// reached this session twice, the author copy first both times (+0.275 s and +0.401 s) and the
// mention after it (+7.221 s and +11.765 s, within that lease and under its own key), while the
// other 18 reached it once. With the same session also the issue's claimant it was 0 of 20,
// because then its copy is one this head holds back until the attempt settles
// (TestACommentAnUnansweredRoleMentionWhoseHolderTheOutboxCannotNameReachesThatSessionOnce).
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

// A session that follows the ask a comment replies to is the same session as the thread author
// above to the rule this head applies: nothing of the comment says the unanswered @role:
// mention is on its way to it, so its copy goes in the pass that reads the event, under the
// event's own key, and a sender that pins that mention to it shows its host the comment twice.
// This test pins the follower's half of that one rule
// (TestAnUnnamedRoleMentionPinnedToTheThreadAuthorShowsItTheCommentTwice, whose doc holds the
// trade, the measurements and what bounds it).
func TestAnUnnamedRoleMentionPinnedToAFollowerShowsItTheCommentTwice(t *testing.T) {
	const role = "mentioned-and-held-by-nobody-this-read-can-name"
	const follower = "ses_follows_the_ask"
	const claimant = "ses_claims_the_issue"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	// No claim stands for the role when the outbox reads it; the listener names one to the
	// mention's sender a round trip later, and the sender pins it on the attempt row.
	rig.silent(t, contracts.RoleTopicPrefix+role)
	follows := rig.receiver(t, follower, true)
	claims := rig.receiver(t, claimant, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	// The claim is another session's, so the copy the owner's own work names is not the
	// follower's: that copy waits for the attempt, and this one does not.
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: claimant})
	askID := seedAsk(t, rig.database, "T-1", model.Actor{Kind: "session", ID: follower}, "Who is on this?")
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, AskID: &askID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedCreatedAttempt(t, rig.database, commentID, target, "steer")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	eventKey := fmt.Sprintf("dispatch-%d", comment.ID)
	mentionKey := contracts.AgentSendDedupeKey(follower, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	_, _, reached := eventState(t, rig.database, comment.ID)
	followerSubject := contracts.AgentSubject(follower)
	if !slices.Contains(reached, followerSubject) {
		t.Fatalf("the comment reached %v, want %s among them: no mention of it names the ask's follower, so its copy goes in the pass that reads the event; held back for the unnamed mention it would wait out that attempt's lease (model.DeliveryClaimLapse = %v), which is what every unrelated session paid before this head",
			reached, followerSubject, model.DeliveryClaimLapse)
	}
	waitFor(t, 10*time.Second, "the ask follower's copy", func() bool { return len(follows.received()) > 0 })
	copies := follows.received()
	if len(copies) != 1 {
		t.Fatalf("%s was sent %+v in the pass that read event %d, want its follower copy alone", follower, copies, comment.ID)
	}
	if copies[0].DedupeKey == mentionKey {
		t.Fatalf("the follower's copy is keyed %q, the key the mention of %s carries at that session, want the event's own key %q: a copy so keyed makes comment N reach this destination under a mention's key and comment N+1 under its own, which TestR17UnrelatedAuthorCopyOrder reads as an owner's events out of order",
			copies[0].DedupeKey, target, eventKey)
	}
	if copies[0].DedupeKey != eventKey {
		t.Fatalf("the follower's copy is keyed %q, want the event's own key %q: a session no mention of the comment names is published to as if it had not been mentioned",
			copies[0].DedupeKey, eventKey)
	}

	// The other arm of the same read: the copy bound for the session the owner's own work names
	// waits for that attempt, so nothing but this follower copy went out this pass.
	claimantSubject := contracts.AgentSubject(claimant)
	if slices.Contains(reached, claimantSubject) {
		t.Fatalf("the comment reached %v, want %s not among them: that session claims the issue, so its copy waits for an attempt that names no session yet and whose role this read can name no holder for",
			reached, claimantSubject)
	}
	if got := claims.received(); len(got) != 0 {
		t.Fatalf("%s was sent %+v while the mention of %s was live and named no session, want nothing: the sender can pin that mention to the issue's claimant", claimant, got, target)
	}

	// The sender's own resolution lands on the follower, as it may: it pins that session on the
	// attempt row and settles the send, whose own key is the mention's.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2)
		where comment_id = $1 and target = $3
	`, commentID, follower, target); err != nil {
		t.Fatalf("settle the mention of %s on %s: %v", target, follower, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once the mention of %s settled: attempt_count %d, publish_error %v, reached %v",
			target, attempts, failure, reached)
	}
	time.Sleep(time.Second)
	// The outbox sends that session nothing further, so the comment stands at its host under two
	// keys: this copy under the event's own, and the mention's steer under the mention's.
	copies = follows.received()
	if len(copies) != 1 || copies[0].DedupeKey != eventKey {
		t.Fatalf("%s was sent %+v once its mention settled, want the one follower copy under %q: the mention's own send carries %q, which this copy does not claim, so that host is shown the comment twice",
			follower, copies, eventKey, mentionKey)
	}
}

// A session the issue's primary document credits is the third session of that one rule: a
// person's comment on an issue nobody routes or claims reaches it (addPrimaryDocumentAuthors),
// no mention of the comment names it, so its copy goes in the pass that reads the event under
// the event's own key, and a sender that pins the unanswered @role: mention to it shows its
// host the comment twice. Nothing of this comment is held back: with no route and no session
// claimant, the owner's own work names no session at all, and the hold covers none of the
// copies this event has (TestAnUnnamedRoleMentionPinnedToTheThreadAuthorShowsItTheCommentTwice,
// whose doc holds the trade, the measurements and what bounds it).
func TestAnUnnamedRoleMentionPinnedToAPrimaryDocumentAuthorShowsItTheCommentTwice(t *testing.T) {
	const role = "mentioned-and-held-by-nobody-this-read-can-name"
	const writer = "ses_wrote_the_spec"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	// No claim stands for the role when the outbox reads it; the listener names one to the
	// mention's sender a round trip later, and the sender pins it on the attempt row.
	rig.silent(t, contracts.RoleTopicPrefix+role)
	wrote := rig.receiver(t, writer, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	// No route and no session claimant: that is the issue whose comments reach the sessions its
	// primary document credits (takesPrimaryAuthors).
	seedIssue(t, rig.database, "T-1", nil)
	writerActor := model.Actor{Kind: "session", ID: writer}
	seedSpec(t, rig.database, "T-1", writerActor, []model.Actor{writerActor})
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedCreatedAttempt(t, rig.database, commentID, target, "steer")

	holding(t, rig.deps()).scan(ctx, unfenced{})

	eventKey := fmt.Sprintf("dispatch-%d", comment.ID)
	mentionKey := contracts.AgentSendDedupeKey(writer, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	_, _, reached := eventState(t, rig.database, comment.ID)
	writerSubject := contracts.AgentSubject(writer)
	if !slices.Contains(reached, writerSubject) {
		t.Fatalf("the comment reached %v, want %s among them: no mention of it names the session the issue's primary document credits, so its copy goes in the pass that reads the event; held back for the unnamed mention it would wait out that attempt's lease (model.DeliveryClaimLapse = %v), which is what every unrelated session paid before this head",
			reached, writerSubject, model.DeliveryClaimLapse)
	}
	waitFor(t, 10*time.Second, "the primary document author's copy", func() bool { return len(wrote.received()) > 0 })
	copies := wrote.received()
	if len(copies) != 1 {
		t.Fatalf("%s was sent %+v in the pass that read event %d, want its document author copy alone", writer, copies, comment.ID)
	}
	if copies[0].DedupeKey == mentionKey {
		t.Fatalf("the primary document author's copy is keyed %q, the key the mention of %s carries at that session, want the event's own key %q: a copy so keyed makes comment N reach this destination under a mention's key and comment N+1 under its own, which TestR17UnrelatedAuthorCopyOrder reads as an owner's events out of order",
			copies[0].DedupeKey, target, eventKey)
	}
	if copies[0].DedupeKey != eventKey {
		t.Fatalf("the primary document author's copy is keyed %q, want the event's own key %q: a session no mention of the comment names is published to as if it had not been mentioned",
			copies[0].DedupeKey, eventKey)
	}

	// The sender's own resolution lands on that session, as it may: it pins it on the attempt
	// row and settles the send, whose own key is the mention's.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2)
		where comment_id = $1 and target = $3
	`, commentID, writer, target); err != nil {
		t.Fatalf("settle the mention of %s on %s: %v", target, writer, err)
	}
	dueNow(t, rig.database, comment.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	if publishedAt(t, rig.database, comment.ID) == nil {
		attempts, failure, reached := eventState(t, rig.database, comment.ID)
		t.Fatalf("the comment was not published once the mention of %s settled: attempt_count %d, publish_error %v, reached %v",
			target, attempts, failure, reached)
	}
	time.Sleep(time.Second)
	// The outbox sends that session nothing further, so the comment stands at its host under two
	// keys: this copy under the event's own, and the mention's steer under the mention's.
	copies = wrote.received()
	if len(copies) != 1 || copies[0].DedupeKey != eventKey {
		t.Fatalf("%s was sent %+v once its mention settled, want the one document author copy under %q: the mention's own send carries %q, which this copy does not claim, so that host is shown the comment twice",
			writer, copies, eventKey, mentionKey)
	}
}
