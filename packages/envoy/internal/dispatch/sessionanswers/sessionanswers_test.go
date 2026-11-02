package sessionanswers

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// A message the listener stored for a session that is not running (state 'queued') is a person's
// words the session owes an answer for exactly as a sent one is: it reaches that session when it
// runs again, so both delivery arms read the two states alike (INFRA-626). An attempt that
// failed, and one still pending, reach nobody and are left out.
func TestAQueuedDeliveryIsListedAsASentOneIs(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for state, want := range map[string]int{"sent": 1, "queued": 1, "failed": 0, "pending": 0} {
		t.Run(state, func(t *testing.T) {
			var messageID string
			if err := database.Pool.QueryRow(ctx, `
				insert into messages (author, body, target)
				values ('{"kind":"user","id":"alice"}', $1, 'session:reader-' || $2::text)
				returning id::text
			`, "A person wrote this ("+state+").", state).Scan(&messageID); err != nil {
				t.Fatalf("seed the message: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state)
				values ($1::uuid, 1, 'steer', 'reader-' || $2::text, $2)
			`, messageID, state); err != nil {
				t.Fatalf("seed the %s delivery: %v", state, err)
			}
			items, err := List(ctx, database.Pool, "reader-"+state, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatalf("list what reached the session: %v", err)
			}
			if len(items) != want {
				t.Fatalf("a %s delivery listed %d items, want %d: %#v", state, len(items), want, items)
			}
			if want == 1 && (items[0].Kind != "message" || items[0].ID != messageID) {
				t.Fatalf("a %s delivery listed %#v, want message %s", state, items[0], messageID)
			}
		})
	}
}

// The read of a session's message deliveries (S2) is an index-only scan of
// message_deliveries_session_created (0147), never a read through message_deliveries_session
// (0051) of every delivery the session ever received. It is explained twice: as the custom plan a
// statement's first runs get, and as the generic plan Postgres caches for it afterwards, which
// knows neither the session nor the window's start.
func TestTheReadOfASessionsMessageDeliveriesReadsItsWindowsRange(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var messageID string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Broadcast.', 'session:session-0')
		returning id
	`).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	// 4,000 deliveries over 20 sessions, one an hour back from now, as long-lived sessions'
	// histories of broadcast copies.
	if _, err := database.Pool.Exec(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, created_at)
		select $1, n, 'btw', 'session-' || (n % 20), 'sent', now() - n * interval '1 hour'
		from generate_series(1, 4000) n
	`, messageID); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `vacuum analyze message_deliveries`); err != nil {
		t.Fatalf("vacuum deliveries: %v", err)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	explain := func(statement string, args ...any) json.RawMessage {
		t.Helper()
		var plans []struct {
			Plan json.RawMessage `json:"Plan"`
		}
		if err := tx.QueryRow(ctx, "explain (format json) "+statement, args...).Scan(&plans); err != nil || len(plans) != 1 {
			t.Fatalf("explain %s: %v, want one plan", statement, err)
		}
		return plans[0].Plan
	}

	since := time.Now().Add(-72 * time.Hour)
	if custom := explain(query, "session-1", since, "notifications.agent.session-1"); !readsWindow(t, custom) {
		t.Errorf("the custom plan reads message deliveries other than by an index-only scan of message_deliveries_session_created:\n%s", custom)
	}
	if _, err := tx.Exec(ctx, "prepare session_answers (text, timestamptz, text) as "+query); err != nil {
		t.Fatalf("prepare the read: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plans: %v", err)
	}
	if generic := explain(`execute session_answers('session-1', now() - interval '72 hours', 'notifications.agent.session-1')`); !readsWindow(t, generic) {
		t.Errorf("the generic plan reads message deliveries other than by an index-only scan of message_deliveries_session_created:\n%s", generic)
	}
}

// readsWindow reports whether an EXPLAIN (FORMAT JSON) plan node, or a node under it, reads
// message_deliveries exactly once, by an index-only scan of message_deliveries_session_created,
// the window's own index. That index carries state, so the arm's `state in ('sent', 'queued')`
// is answered from the index and the read touches no heap page.
func readsWindow(t *testing.T, plan json.RawMessage) bool {
	t.Helper()
	reads, other := deliveryReads(t, plan)
	return reads == 1 && !other
}

// deliveryReads counts the nodes under plan that read message_deliveries by an index-only scan of
// the window's index, and reports whether any node reads it another way.
func deliveryReads(t *testing.T, plan json.RawMessage) (int, bool) {
	t.Helper()
	var node struct {
		NodeType  string            `json:"Node Type"`
		Relation  string            `json:"Relation Name"`
		IndexName string            `json:"Index Name"`
		Plans     []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.Relation == "message_deliveries" {
		if node.NodeType == "Index Only Scan" && node.IndexName == "message_deliveries_session_created" {
			return 1, false
		}
		return 0, true
	}
	reads, other := 0, false
	for _, child := range node.Plans {
		childReads, childOther := deliveryReads(t, child)
		reads += childReads
		other = other || childOther
	}
	return reads, other
}
