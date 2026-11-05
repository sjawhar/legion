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

// The read of a session's message deliveries (S2) is an index-only scan of
// message_deliveries_session_created (0147), never a read through message_deliveries_session
// (0051) of every delivery the session ever received. It is explained twice: as the custom plan a
// statement's first runs get, and as the generic plan Postgres caches for it afterwards, which
// knows neither the session nor the window's start.
func TestTheReadOfASessionsMessageDeliveriesReadsItsWindowsRange(t *testing.T) {
	t.Parallel()
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
	if _, err := database.Pool.Exec(ctx, `analyze message_deliveries`); err != nil {
		t.Fatalf("analyze deliveries: %v", err)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	// Both plans are read over a table whose every page is all-visible, which is what a routine
	// vacuum of a live table leaves behind and what makes an index-only scan worth choosing: the
	// planner reads relallvisible/relpages as the fraction of rows it can answer from the index
	// alone, and charges a heap fetch for the rest.
	//
	// The vacuum this replaces could not put it there. A page is marked all-visible only once
	// every transaction on the whole server is past the rows on it, and this module's packages
	// run eight at a time against one Postgres, so one open transaction in another package's
	// database left relallvisible at zero and both plans a bitmap heap scan - a reading of the
	// server's other tests, not of the statement. The claim here is about what the planner is
	// told, so the test tells it (PostgreSQL 18's pg_restore_relation_stats, the setter beside
	// the pg_clear_relation_stats internal/dispatch/refs states the other half of this kind of
	// claim with). The write is transactional, so the rollback above leaves the database the way
	// the harness hands it on.
	if _, err := tx.Exec(ctx, `
		select pg_restore_relation_stats(
			'schemaname', n.nspname::text, 'relname', c.relname::text,
			'version', current_setting('server_version_num')::integer,
			'relpages', c.relpages, 'reltuples', c.reltuples, 'relallvisible', c.relpages)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relname = 'message_deliveries'
	`); err != nil {
		t.Fatalf("state that every page of message_deliveries is all-visible: %v", err)
	}
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
// message_deliveries, and every such read is an index-only scan of
// message_deliveries_session_created.
func readsWindow(t *testing.T, plan json.RawMessage) bool {
	t.Helper()
	reads, other := deliveryReads(t, plan)
	return reads > 0 && !other
}

// deliveryReads counts the nodes under plan that read message_deliveries by an index-only scan of
// message_deliveries_session_created, and reports whether any node reads it another way.
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
