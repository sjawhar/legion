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

// R17 timing: the architect's staging shape. role:reviewer is unheld at the batch read, the
// comment's role mention attempt is live and unpinned, ses-u (thread root author, named by no
// mention) is owed an author-route copy.
func TestR17UnrelatedAuthorCopyFirstPass(t *testing.T) {
	const role = "reviewer"
	const holder = "ses-r"
	const author = "ses-u"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	r := rig.receiver(t, holder, true)
	u := rig.receiver(t, author, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	route := target
	seedIssue(t, rig.database, "T-1", &route)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "root", nil)
	body := "@" + target + " please look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			SuppressRoute: true, SuppressedRoute: route, MentionsDecideRoutes: true,
		},
	})
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	start := time.Now()
	holding(t, rig.deps()).scan(ctx, unfenced{})
	firstPass := time.Since(start)
	_, _, reached := eventState(t, rig.database, comment.ID)
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	sentFirst := slices.Contains(reached, contracts.AgentSubject(author))
	var reread time.Duration
	if due != nil {
		reread = time.Until(*due)
	}
	t.Logf("R17 FIRST PASS (%v): ses-u copy sent=%v reached=%v published=%v attempts=%d next re-read in %v",
		firstPass, sentFirst, reached, publishedAt(t, rig.database, comment.ID) != nil, attempts, reread.Round(10*time.Millisecond))

	// The sender's resolution lands: ses-r claimed the role, the attempt is pinned to it and sent.
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	if _, err := rig.database.Pool.Exec(ctx, `update comment_deliveries set state = 'sent', session_id = coalesce(session_id, $2) where comment_id = $1 and target = $3`,
		commentID, holder, target); err != nil {
		t.Fatal(err)
	}
	dueNow(t, rig.database, comment.ID)
	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)
	_, _, reached = eventState(t, rig.database, comment.ID)
	t.Logf("R17 SECOND PASS: reached=%v published=%v ses-u copies=%d ses-r copies=%d",
		reached, publishedAt(t, rig.database, comment.ID) != nil, len(u.received()), len(r.received()))
	if !sentFirst {
		t.Errorf("ses-u's author copy was not sent in the first pass: it waited for the re-read (%v)", reread.Round(10*time.Millisecond))
	}
}

// R17 order: comment N carries the unpinned, unresolved role mention; comment N+1 of the same
// owner names no mention. ses-u, the thread root's author, is owed both.
func TestR17UnrelatedAuthorCopyOrder(t *testing.T) {
	const role = "reviewer"
	const author = "ses-u"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	u := rig.receiver(t, author, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "root", nil)
	body := "@" + target + " please look"
	firstID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	first := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: firstID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
			MentionsDecideRoutes: true,
		},
	})
	seedDelivery(t, rig.database, firstID, target, "", "pending")
	secondID := seedComment(t, rig.database, "T-1", alice, "and one more thing", &rootID)
	second := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: secondID, IssueKey: new("T-1"), Body: "and one more thing", ReplyTo: &rootID},
		},
	})

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(500 * time.Millisecond)
	// The sender finds no holder and fails the attempt, as it does within a round trip.
	if _, err := rig.database.Pool.Exec(ctx, `update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1 and target = $2`,
		firstID, target); err != nil {
		t.Fatal(err)
	}
	dueNow(t, rig.database, first.ID)
	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)
	var order []string
	for _, item := range u.received() {
		order = append(order, item.DedupeKey)
	}
	t.Logf("R17 ORDER: event N=%d N+1=%d; ses-u received dedupe keys in order %v", first.ID, second.ID, order)
	want := []string{fmt.Sprintf("dispatch-%d", first.ID), fmt.Sprintf("dispatch-%d", second.ID)}
	if !slices.Equal(order, want) {
		t.Errorf("ses-u received %v, want %v: an owner's events reach a destination in id order", order, want)
	}
}
