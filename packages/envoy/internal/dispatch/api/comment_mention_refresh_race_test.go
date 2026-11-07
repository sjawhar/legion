package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A refresh whose statement has committed but whose result is not yet recorded loses to a keyed
// repeat that claims the same queued attempt anew in that gap: the sender must end up holding the
// repeat's claim, the one the row carries, so its next refresh writes the row again and the
// attempt does not lapse into the delivery sweep's hands.
func TestARefreshThatLosesToAKeyedRepeatKeepsTheRepeatsClaim(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second,
	})
	withMentionClaimRefresh(t, served, time.Hour)
	issue := createInteractionIssue(t, handler, "TEST", "A refresh racing a repeat", "before")
	reached, release := state.stall("s1")
	defer release()

	postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	keyed := map[string]any{"body": "second", "mentions": []map[string]any{{"target": "session:s1"}}}
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	queued := decodeMentionedComment(t,
		keyedRequest(t, handler, http.MethodPost, comments, keyed, "alice", "refresh-race")).ID
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the attempt waiting on the held lane is not held by the sender")
	}

	// The refresh's statement commits, then the keyed repeat claims the attempt anew, then the
	// refresh records what its statement returned.
	sender := served.mentions
	realRefresh := sender.refresh
	sender.refresh = func(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
		refreshed, err := realRefresh(ctx, queued)
		time.Sleep(20 * time.Millisecond)
		// What queueLocked records for a keyed repeat that claimed the attempt anew: a newer
		// claim on the row and the same claim in the sender's map.
		var newer time.Time
		if err := database.Pool.QueryRow(context.Background(), `
			update comment_deliveries set claimed_at = now()
			where comment_id = $1 and target = 'session:s1' and state = 'pending' returning claimed_at
		`, queued[0].key.comment).Scan(&newer); err != nil {
			t.Errorf("write the newer claim: %v", err)
		}
		sender.mu.Lock()
		sender.claims[queued[0].key] = newer
		sender.mu.Unlock()
		return refreshed, err
	}
	sender.refreshHeldClaims(context.Background())
	sender.refresh = realRefresh

	row := attemptClaimedAt(t, database, queued, "session:s1")
	sender.mu.Lock()
	var held time.Time
	for key, claimed := range sender.claims {
		if key.comment == queued {
			held = claimed
		}
	}
	sender.mu.Unlock()
	if !held.Equal(row) {
		t.Fatalf("the sender holds claim %v for the queued attempt, the row carries %v: the refresh recorded its "+
			"own result over the repeat's claim", held, row)
	}
	sender.refreshHeldClaims(context.Background())
	if again := attemptClaimedAt(t, database, queued, "session:s1"); !again.After(row) {
		t.Fatalf("the claim on the queued attempt is still %v after the next refresh, want it written again: "+
			"left alone it lapses a lease later and the sweep may send it out of order", again)
	}
	release()
	if sent := settledCommentDeliveries(t, database, queued); len(sent) != 1 || !allSent(sent) {
		t.Fatalf("the repeated comment's attempts = %#v, want its one attempt sent", sent)
	}
}
