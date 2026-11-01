package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// insertEvent writes one event of T-1 straight into the log with seq, payload, created_at as old
// as age and, when published, published_at set, and returns its id.
func insertEvent(t *testing.T, database *store.Store, seq int, payload string, age time.Duration, published bool) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at, published_at)
		values ('T-1', $1, 'message.created', '{"kind":"session","id":"test"}', $2::jsonb, false,
		        now() - $3::interval, case when $4 then now() end)
		returning id
	`, seq, payload, age, published).Scan(&id); err != nil {
		t.Fatalf("insert event %d: %v", seq, err)
	}
	return id
}

func lag(t *testing.T, database *store.Store) time.Duration {
	t.Helper()
	lag, err := Lag(context.Background(), database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return lag
}

// The outbox's lag is zero while nothing it owes is unpublished - a published event and a pending
// subscription command, which it never publishes, are not owed however old - and is the age of the
// oldest event it does owe once there is one, growing while that event waits.
func TestLagIsZeroWithNothingOwedAndGrowsWithAnUnpublishedEvent(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	if got := lag(t, database); got != 0 {
		t.Fatalf("lag with no events = %v, want 0", got)
	}

	insertEvent(t, database, 1, `{"body":"published"}`, time.Hour, true)
	insertEvent(t, database, 2, `{"session_id":"s","pending":true}`, time.Hour, false)
	if got := lag(t, database); got != 0 {
		t.Fatalf("lag with only a published event and a pending subscription command = %v, want 0", got)
	}

	insertEvent(t, database, 3, `{"body":"owed"}`, 30*time.Second, false)
	first := lag(t, database)
	if first < 30*time.Second || first >= time.Hour {
		t.Fatalf("lag with an event owed for 30s = %v, want at least 30s and under the pending command's hour", first)
	}
	time.Sleep(50 * time.Millisecond)
	if second := lag(t, database); second <= first {
		t.Fatalf("lag read again 50ms later = %v, want more than %v", second, first)
	}
}

// Published counts the events a batch's write marks published, and only those: an event the batch
// backed off is not published, and one another write already marked is not marked again.
func TestPublishedCountsTheEventsTheBatchMarksPublished(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	first := insertEvent(t, database, 1, `{"body":"one"}`, 0, false)
	second := insertEvent(t, database, 2, `{"body":"two"}`, 0, false)
	failed := insertEvent(t, database, 3, `{"body":"three"}`, 0, false)
	already := insertEvent(t, database, 4, `{"body":"four"}`, 0, true)

	before := Published()
	if err := recordBatch(context.Background(), Deps{Store: database}, []outcome{
		{id: first, state: published},
		{id: second, state: published},
		{id: failed, state: backedOff, attempts: 1, retryAt: time.Now().Add(time.Minute)},
		{id: already, state: published},
	}); err != nil {
		t.Fatal(err)
	}
	if got := Published() - before; got != 2 {
		t.Fatalf("Published rose by %d, want 2: the two events this batch marked published", got)
	}
	if publishedAt(t, database, failed) != nil {
		t.Fatal("the backed-off event was marked published")
	}
}
