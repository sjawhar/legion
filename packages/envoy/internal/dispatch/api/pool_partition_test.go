package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// partitionAcquireBound is the acquire bound these tests give the pools they fill, so a read that
// reached a held pool fails in a fraction of a second rather than after the pool's own bound.
const partitionAcquireBound = 300 * time.Millisecond

// holdEveryConnection takes every connection of pool and holds it until the test ends: the pool as
// a burst of work that has it all.
func holdEveryConnection(t *testing.T, pool *store.Pool) {
	t.Helper()
	for range pool.Stat().MaxConns() {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold a connection: %v", err)
		}
		t.Cleanup(conn.Release)
	}
	if stat := pool.Stat(); stat.AcquiredConns() != stat.MaxConns() {
		t.Fatalf("the pool holds %d of %d connections, want all", stat.AcquiredConns(), stat.MaxConns())
	}
}

// appendEventOn commits one event on issueKey through a transaction of pool, as another writer's
// commit, and answers its id.
func appendEventOn(t *testing.T, pool *store.Pool, issueKey string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	event, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: &issueKey,
		Type:     "test.seeded",
		Actor:    model.Actor{Kind: "session", ID: "seed-session-0123456789"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	return event.ID
}

// readFeedThrough reads the events subscription's frames until one carries id, failing on a frame
// past it.
func readFeedThrough(t *testing.T, client *socketClient, sub string, id int64) {
	t.Helper()
	for got := int64(0); got != id; {
		got = nextEventID(t, client, sub)
		if got > id {
			t.Fatalf("the feed sent id %d before the event %d", got, id)
		}
	}
}

// The event feed reads through a pool of its own (store.Pool.Live): with every connection of the
// request pool held - the requests of a burst - an open live socket's events subscription still
// receives an event another writer commits, read back out of the log by the relay. The
// subscription's own catch-up runs on the live pool too, and the socket is opened, and its first
// frame seen, before the request pool is taken.
func TestAnOpenEventFeedReceivesACommitWhileTheRequestPoolIsFullyAcquired(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{
		requestPoolSize: 4, acquireBound: partitionAcquireBound, streamHeartbeat: time.Hour,
	}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	client := rig.dial(t, asUser("alice"))
	subscribeEvents(t, client, "events", nil)

	// A frame is sent only after the subscription's catch-up has read its last page, so once this
	// one arrives the feed reads nothing more until the next commit.
	readFeedThrough(t, client, "events", appendEventOn(t, rig.database.Pool, issue.Key))

	holdEveryConnection(t, rig.database.Pool)
	background, err := rig.database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	readFeedThrough(t, client, "events", appendEventOn(t, background, issue.Key))
}

// The suggestion sweep runs on the pool cmd/dispatch hands it, the background pool: with every
// connection of the request pool held, one pass still runs to its end and records the outcome it
// finds.
func TestASuggestionSweepPassCompletesWhileTheRequestPoolIsFullyAcquired(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{requestPoolSize: 4, acquireBound: partitionAcquireBound})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OUT5")
	target := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT5", "title": "Target issue",
	}, "alice"))
	source := decodeBody[testIssueWithSuggestions](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "OUT5", "title": "Source issue", "force": true,
	}, "alice"))
	seedWriteSuggestion(t, srv, "issue", source.Key, "", "alice", "issue", target.Key, target.Key)
	time.Sleep(10 * time.Millisecond) // the further activity must postdate the suggestion row
	if continued := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source.Key+"/messages", map[string]any{
		"body": "This is in fact a separate problem; continuing here.",
	}, "alice"); continued.Code != http.StatusCreated {
		t.Fatalf("continue the source issue: status=%d body=%s", continued.Code, continued.Body.String())
	}
	leases := sweepLeases(t, database)
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}

	holdEveryConnection(t, database.Pool)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); RunSuggestionOutcomeSweep(ctx, background, sweepLeaseTick, leases, nil) }()
	defer func() { stop(); <-done }()
	var outcome string
	for deadline := time.Now().Add(30 * time.Second); outcome != "overridden"; {
		if time.Now().After(deadline) {
			t.Fatalf("no sweep pass recorded the outcome with the request pool held: it is %q, want overridden", outcome)
		}
		time.Sleep(sweepLeaseTick)
		if err := background.QueryRow(t.Context(), `
			select outcome from write_suggestions where source_issue_key = $1 and suggested_id = $2
		`, source.Key, target.Key).Scan(&outcome); err != nil {
			t.Fatalf("read the suggestion's outcome: %v", err)
		}
	}
}

// The requests read through a pool the background loops never touch: with every connection of the
// background pool held - its loops all mid-pass - an issue read still answers.
func TestAnIssueReadAnswersWhileTheBackgroundPoolIsFullyAcquired(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	database.Pool.SetAcquireBoundForTest(store.BackgroundPool, partitionAcquireBound)
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	holdEveryConnection(t, background)

	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("read the issue with the background pool held: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}
