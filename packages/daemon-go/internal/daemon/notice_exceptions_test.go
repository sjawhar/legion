package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// exceptionEnvelope is the listener's report of a failed role-lane forward, as
// packages/envoy/cmd/listener/delivery.go publishes it: an envoy envelope whose payload names the
// original topic, summary, payload and dedupe key, and the reason.
func exceptionEnvelope(t *testing.T, eventID, reason, topic, summary string, payload any, key string) []byte {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	exception, err := json.Marshal(roleLaneException{OriginalTopic: topic, EventID: "evt-original", Reason: reason, PayloadSummary: summary, Payload: string(encoded), DedupeKey: key})
	if err != nil {
		t.Fatalf("encode exception: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": eventID, "source": "envoy", "topic": "notifications.envoy.exceptions." + topic, "payload": string(exception)})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return envelope
}

// queuedNotices is every queued notice row, as "<kind> on <issue>" with its payload.
func queuedNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select issue, payload::text from outbox where kind = 'notice' order by id")
	if err != nil {
		t.Fatalf("read notice rows: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var issue, payload string
		if err := rows.Scan(&issue, &payload); err != nil {
			t.Fatalf("scan notice row: %v", err)
		}
		got = append(got, issue+" "+payload)
	}
	return got
}

// A notice the listener accepted but could not forward to the owning architect's session is
// queued again as a row of its issue, due after the re-hold delay: the forward failed
// (delivery_failed), the registration lapsed between the publish and the forward (no_holder), or
// the receipt never came while the architect's claim has no live session (receipt_timeout from a
// session that stopped running but is still registered). Once it is due and a session holds the
// role, the executor delivers it to the owner.
func TestANoticeTheListenerCouldNotForwardIsQueuedAgain(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		architect supervise.ClaimState
	}{
		{"delivery_failed", supervise.StateWorking},
		{"no_holder", supervise.StateWorking},
		{"receipt_timeout", supervise.StateLaunching},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.architect)
			clock := time.Now()
			publisher := &holderPublisher{}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }}
			notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}

			if err := runner.rehold(context.Background(), exceptionEnvelope(t, "evt-exception", tc.reason, architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7")); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			var nextAt time.Time
			if err := pool.QueryRow(context.Background(), "select next_at from outbox where kind = 'notice'").Scan(&nextAt); err != nil {
				t.Fatalf("read the re-held row: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-2 {"kind": "pr-blocked", "role": "architect", "reason": "max_fix_attempts"}` ||
				!nextAt.Equal(clock.Add(noticeReholdDelay).Truncate(time.Microsecond)) {
				t.Fatalf("queued %v due %s; want the one notice of LEGION-2, due %s", got, nextAt, clock.Add(noticeReholdDelay))
			}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run before it is due: %v", err)
			}
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered %+v before the re-hold delay passed", delivered)
			}
			clock = clock.Add(noticeReholdDelay)
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once due: %v", err)
			}
			if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %v with %d rows left, want the re-held notice at the root architect", deliveredKinds(t, delivered), outboxRows(t, pool))
			}
		})
	}
}

// Only a failed forward of this daemon's notice to an architect of its project is queued again. A
// late receipt while the architect's session is live is a slow holder that has the notice. Any
// other report — another project's role, a
// phase worker's role, the merge queue's READY, another publisher's key, a summary that is not the
// executor's own, a payload that is not a notice — is someone else's, and changes nothing.
func TestAReportThatIsNotAFailedNoticeForwardChangesNothing(t *testing.T) {
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	root := "notifications.role.legion-legion-legion-1-architect"
	for _, tc := range []struct {
		name, reason, topic, summary string
		payload                      any
		key                          string
	}{
		{"a late receipt from a live session", "receipt_timeout", root, "pr-blocked on LEGION-2", notice, "legion-outbox:7"},
		{"another project's architect", "delivery_failed", "notifications.role.legion-other-legion-1-architect", "pr-blocked on LEGION-2", notice, "legion-outbox:7"},
		{"a phase worker's role", "delivery_failed", "notifications.role.legion-legion-legion-2-planner", "pr-blocked on LEGION-2", notice, "legion-outbox:7"},
		{"the merge queue's READY", "delivery_failed", "notifications.role.merge-queue", "READY #42 at abc123", "READY #42 at abc123", "legion-outbox:7"},
		{"another publisher's key", "delivery_failed", root, "pr-blocked on LEGION-2", notice, "envoy.agent.42"},
		{"a summary of another kind", "delivery_failed", root, "held on LEGION-2", notice, "legion-outbox:7"},
		{"a summary naming no issue", "delivery_failed", root, "pr-blocked on everything", notice, "legion-outbox:7"},
		{"a payload that is not a notice", "delivery_failed", root, "pr-blocked on LEGION-2", map[string]string{"kind": "pr-blocked", "packet": "READY"}, "legion-outbox:7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			runner := &outbox{log: quietLogger(), pool: pool, records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
			if err := runner.rehold(context.Background(), exceptionEnvelope(t, "evt-exception", tc.reason, tc.topic, tc.summary, tc.payload, tc.key)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != 0 {
				t.Fatalf("queued %v, want nothing", got)
			}
		})
	}
}

// The listener sends each report once, but a report handled twice queues its notice once: the
// exception's event id is recorded with the row. A report that cannot be read is refused.
func TestAReportIsQueuedOnceAndAnUnreadableOneIsRefused(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
	report := exceptionEnvelope(t, "evt-exception", "delivery_failed", "notifications.role.legion-legion-legion-1-architect", "pr-blocked on LEGION-2",
		record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, "legion-outbox:7")
	for range 2 {
		if err := runner.rehold(context.Background(), report); err != nil {
			t.Fatalf("rehold: %v", err)
		}
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v after the same report twice, want one row", got)
	}
	for _, malformed := range [][]byte{[]byte("not json"), []byte(`{"event_id":"evt-2","source":"agent","payload":"{}"}`), []byte(`{"event_id":"evt-3","source":"envoy","payload":"not json"}`)} {
		if err := runner.rehold(context.Background(), malformed); err == nil {
			t.Fatalf("rehold(%s) = nil, want a refusal", malformed)
		}
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v after the malformed reports, want still one row", got)
	}
}

// The report arrives over core NATS on the original role topic's exceptions subject; the
// subscription hears it there and queues the notice again.
func TestARoleLaneExceptionOnNATSQueuesTheNoticeAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, records: records, supervisor: sup, project: "legion", now: time.Now}
	conn, err := nats.Connect(testnats.URL(t), nats.Timeout(time.Second))
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(conn.Close)
	failed := make(chan error, 1)
	sub, err := subscribeNoticeExceptions(conn, func(data []byte) {
		if err := runner.rehold(context.Background(), data); err != nil {
			failed <- err
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	topic := "notifications.role.legion-legion-legion-1-architect"
	report := exceptionEnvelope(t, "evt-exception", "delivery_failed", topic, "design-approved on LEGION-1", record.Notice{Kind: "design-approved", Version: 3}, "legion-outbox:9")
	if err := conn.Publish("notifications.envoy.exceptions."+topic, report); err != nil {
		t.Fatalf("publish the report: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(queuedNotices(t, pool)) == 0 && time.Now().Before(deadline) {
		select {
		case err := <-failed:
			t.Fatalf("rehold: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-1 {"kind": "design-approved", "version": 3}` {
		t.Fatalf("queued %v, want the design-approved notice of LEGION-1", got)
	}
}
