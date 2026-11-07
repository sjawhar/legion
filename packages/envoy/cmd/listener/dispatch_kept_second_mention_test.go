package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
)

// outboxFailedRoleComment writes a comment on the issue key names that mentions two sessions,
// each mention's send having landed and then been recorded failed - a receipt timeout, which is
// what a listener that answers past Dispatch's send window leaves - so each session's host holds
// the comment under that mention's own key already. The issue's route resolved to the first of
// them when the comment was written, so the comment names that route suppressed by it, and a
// failed mention suppresses nothing: the event's own publish carries the role route. It returns
// the comment's id, the event, and the dedupe key each mention's send went under.
func outboxFailedRoleComment(
	t *testing.T, database *dispatchstore.Store, broker *events.Broker, key, role, first, second string,
) (string, model.Event, string, string) {
	t.Helper()
	ctx := context.Background()
	body := "@" + first + " @" + second + " are you there?"
	var commentID string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, key, body).Scan(&commentID); err != nil {
		t.Fatalf("create the comment: %v", err)
	}
	var mentions []model.Mention
	for _, sessionID := range []string{first, second} {
		target := "session:" + sessionID
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_mentions (comment_id, target, delivery) values ($1, $2, 'steer')
		`, commentID, target); err != nil {
			t.Fatalf("create the mention of %s: %v", target, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, error, created_at)
			values ($1, $2, 1, 'steer', $3, 'failed',
			        'the listener did not answer within the send window', now() - interval '2 minutes')
		`, commentID, target, sessionID); err != nil {
			t.Fatalf("create the failed attempt of %s: %v", target, err)
		}
		mentions = append(mentions, model.Mention{Target: target, Delivery: "steer", SessionID: new(sessionID)})
	}
	event := outboxEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new(key), Body: body, Mentions: mentions,
			},
			SuppressRoute: true, SuppressedRoute: "role:" + role, SuppressedRouteSessionID: new(first),
		},
	})
	sendKey := func(sessionID string) string {
		return contracts.AgentSendDedupeKey(
			sessionID, model.CommentMentionIdempotencyKey(commentID, "session:"+sessionID, "steer"))
	}
	return commentID, event, sendKey(first), sendKey(second)
}

// A comment that mentions two sessions, both of whose mentions landed and were then recorded
// failed, is kept for the role nobody holds under one key, and the session that claims the role
// next may be either of them. The kept copy carries the lowest session's failed mention
// (roleFallbackKey in internal/dispatch/outbox/role.go), so the drain forwards it to any other
// holder with contracts.RoleForwardDedupePrefix in front of a key that host never claimed, and
// the second mentioned session - whose own mention's send landed - is shown the comment twice.
//
// That is the defect TestAKeptCopyOfAFailedMentionReachesThatSessionUnderTheMentionsKey holds
// shut for one mentioned session, one door further out: what the holder's host has already seen
// is decided by which session claims the role, which no writer of the lane knows when the copy
// is kept. The copy a holder is handed must therefore carry the key of that holder's own failed
// mention, whichever of the comment's mentioned sessions claims the role.
//
// Measured on the real binaries at 5684c712 (two-mention kept-copy scenario, the comment's
// mentions of ses-a and ses-r both failed on a receipt timeout and ses-r claiming the role
// afterwards): ses-r was shown each of 4 comments twice, under
// agent.ses-r.<comment>:session:ses-r:steer and
// envoy.role.forward.agent.ses-a.<comment>:session:ses-a:steer.
func TestAKeptCopyReachingASecondMentionedHolderCarriesItsOwnMentionsKey(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-for-two-mentioned-sessions"
		first  = "ses_a_mentioned_first"
		second = "ses_b_mentioned_second"
		key    = "T-1"
	)
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })
	database := migratedDispatchDatabase(t)
	broker := events.NewBroker()
	outboxRoleIssue(t, database, key, role)
	// Each mentioned session's host answers on its own agent subject, as a self-subscribed
	// session does, so the outbox's copy of the comment to each of them is taken and the event
	// publishes; the role itself stays unclaimed, so its route's copy is kept for its next
	// holder.
	answeredForwards(t, harness, first)
	secondsFrames := answeredForwards(t, harness, second)
	_, event, firstKey, secondKey := outboxFailedRoleComment(t, database, broker, key, role, first, second)
	runOutbox(t, harness, database, broker)

	waitFor(t, 30*time.Second, "Dispatch's outbox to publish the comment of an unheld role", func() bool {
		return publishedEventAt(t, database, event.ID) != nil
	})
	kept := waitForKeptRoleMessage(t, harness.client, role, fmt.Sprintf("dispatch-%d", event.ID))
	if kept.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the kept copy's topic is %q, want the role topic Dispatch published it on", kept.Topic)
	}

	// The second mentioned session, not the first, is the one that claims the role.
	lane.register(t, second)
	lane.claim(t, second, role)
	handed := lane.await(t, 1, 30*time.Second)

	if handed[0].holder != second {
		t.Fatalf("the copy reached %s, want the role's next holder %s", handed[0].holder, second)
	}
	if handed[0].key != secondKey {
		t.Fatalf("the copy forwarded to %s, whose own mention's send landed before its attempt failed, is keyed %q, want that mention's own key %q, which its host drops; the kept copy carries %q, the first mentioned session's",
			second, handed[0].key, secondKey, kept.DedupeKey)
	}
	if strings.HasPrefix(handed[0].key, roleForwardDedupePrefix) {
		t.Fatalf("the copy forwarded to the session whose own mention it repeats carries %q, want no role forward's prefix: a key no host has claimed shows the comment a second time",
			handed[0].key)
	}
	if want := bus.ForwardMsgID(secondKey, contracts.AgentSubject(second)); handed[0].msgID != want {
		t.Fatalf("the forward was made under %q, want %q, so that the outbox's own handover of this copy and the drain's name one message",
			handed[0].msgID, want)
	}
	// The whole of what that session's host is handed: its own mention's copy and this forward
	// under one key, so the comment is shown once.
	keys := map[string]struct{}{}
	for reading := true; reading; {
		select {
		case frame := <-secondsFrames:
			keys[frame.DedupeKey] = struct{}{}
		case <-time.After(2 * time.Second):
			reading = false
		}
	}
	if _, ok := keys[firstKey]; ok || len(keys) != 1 {
		t.Fatalf("%s's host was handed the comment under %v, want its own mention's key %q alone", second, keys, secondKey)
	}
}
