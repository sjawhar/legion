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

// R18 review: the claimant's copy waited on an unnamed role mention; the attempt's lease lapses
// while still unpinned and the role still unnamed, so the batch writes a fallback row for the
// claimant. The fallback pass must not publish that row while the attempt is still owed a resend
// (Resending), since the resend may pin the claimant.
func TestR18UnnamedFallbackRowWaitsForResend(t *testing.T) {
	const role = "unnamed-by-the-outbox"
	const holder = "ses_claimant"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	claimant := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + target + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, target, "", "pending")

	holding(t, rig.deps()).scan(ctx, unfenced{})
	if got := claimant.received(); len(got) != 0 {
		t.Fatalf("pass 1 sent claimant %+v", got)
	}

	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set created_at = now() - $2::interval, claimed_at = now() - $2::interval
		where comment_id = $1 and target = $3
	`, commentID, model.DeliveryClaimLapse+time.Minute, target); err != nil {
		t.Fatal(err)
	}
	dueNow(t, rig.database, comment.ID)
	holding(t, rig.deps()).scan(ctx, unfenced{})
	row := readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder))
	t.Logf("after lease: row settled=%v published=%v attempts=%d; event published=%v; claimant got %d",
		row.settled, row.published, row.attempts, publishedAt(t, rig.database, comment.ID) != nil, len(claimant.received()))

	// The fallback pass, with the attempt still unpinned and owed a resend, and the role still
	// unnamed by the outbox.
	publishFallbackCopies(ctx, rig.deps(), unfenced{})
	time.Sleep(time.Second)
	row = readHeldCopy(t, rig.database, commentID, contracts.AgentSubject(holder))
	got := claimant.received()
	t.Logf("after fallback pass: row settled=%v published=%v; claimant got %+v", row.settled, row.published, got)
	if row.published || len(got) != 0 {
		t.Errorf("the fallback pass published the claimant's copy (keys %v) while the unnamed mention it waited on is still owed a resend, which may pin the claimant and land under %s beside it",
			keysOf(got), contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer")))
	}
}

func keysOf(items []contracts.Envelope) []string {
	var keys []string
	for _, item := range items {
		keys = append(keys, item.DedupeKey)
	}
	return keys
}

// R18 review: the claimant's copy waits on an unnamed role mention; the owner's next event bound
// for the claimant must not overtake it.
func TestR18ClaimantWaitingCopyKeepsOrder(t *testing.T) {
	const role = "unnamed-by-the-outbox"
	const holder = "ses_claimant"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	claimant := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + target + " please look"
	firstID := seedComment(t, rig.database, "T-1", alice, body, nil)
	first := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: firstID, IssueKey: new("T-1"), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, firstID, target, "", "pending")
	secondID := seedComment(t, rig.database, "T-1", alice, "and one more thing", nil)
	second := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: secondID, IssueKey: new("T-1"), Body: "and one more thing"},
		},
	})

	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(500 * time.Millisecond)
	if _, err := rig.database.Pool.Exec(ctx, `update comment_deliveries set state = 'failed', error = 'no live session' where comment_id = $1 and target = $2`,
		firstID, target); err != nil {
		t.Fatal(err)
	}
	dueNow(t, rig.database, first.ID)
	dueNow(t, rig.database, second.ID)
	holding(t, rig.deps()).scan(ctx, unfenced{})
	time.Sleep(time.Second)
	order := keysOf(claimant.received())
	want := []string{fmt.Sprintf("dispatch-%d", first.ID), fmt.Sprintf("dispatch-%d", second.ID)}
	t.Logf("claimant order %v", order)
	if !slices.Equal(order, want) {
		t.Errorf("claimant received %v, want %v", order, want)
	}
}

// R18 review: the outbox first reads the comment after the unnamed attempt's lease lapsed with the
// send still owed (Resending). The thread author's copy: published now, or held for the resend?
func TestR18AuthorPastLeaseUnnamed(t *testing.T) {
	const role = "unnamed-by-the-outbox"
	const author = "ses_author"
	ctx := store.WithTransactionTracking(context.Background())
	target := "role:" + role

	rig := newRoleRig(t)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	wrote := rig.receiver(t, author, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	seedIssue(t, rig.database, "T-1", nil)
	rootID := seedComment(t, rig.database, "T-1", model.Actor{Kind: "session", ID: author}, "root", nil)
	body := "@" + target + " please look"
	commentID := seedComment(t, rig.database, "T-1", alice, body, &rootID)
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body, ReplyTo: &rootID,
				Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
		},
	})
	seedDelivery(t, rig.database, commentID, target, "", "pending")
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set created_at = now() - $2::interval, claimed_at = now() - $2::interval
		where comment_id = $1 and target = $3
	`, commentID, model.DeliveryClaimLapse+time.Minute, target); err != nil {
		t.Fatal(err)
	}
	holding(t, rig.deps()).scan(ctx, unfenced{})
	publishFallbackCopies(ctx, rig.deps(), unfenced{})
	time.Sleep(time.Second)
	t.Logf("author past lease: event published=%v author got %v", publishedAt(t, rig.database, comment.ID) != nil, keysOf(wrote.received()))
}
