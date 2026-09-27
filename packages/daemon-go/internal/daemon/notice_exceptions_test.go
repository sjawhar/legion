package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// laneReport is the listener's report of a failed role-lane forward, as
// packages/envoy/cmd/listener/delivery.go publishes it: an envoy envelope whose payload names the
// original topic, summary, payload and dedupe key, the session the forward went to, and the reason.
type laneReport struct {
	eventID, reason, topic, summary string
	payload                         any
	key, recipient                  string
}

func (r laneReport) envelope(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(r.payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	exception, err := json.Marshal(map[string]string{"original_topic": r.topic, "event_id": "evt-original", "reason": r.reason, "recipient_session": r.recipient,
		"payload_summary": r.summary, "payload": string(encoded), "dedupe_key": r.key})
	if err != nil {
		t.Fatalf("encode exception: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{"event_id": r.eventID, "source": "envoy", "topic": "notifications.envoy.exceptions." + r.topic, "payload": string(exception)})
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return envelope
}

// architectClaimOn creates issue's architect claim in state, registered as session.
func architectClaimOn(t *testing.T, sup *supervisor, issue string, state supervise.ClaimState, session string) {
	t.Helper()
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: mustClaimToken(t, issue, claim.RoleArchitect), Project: "legion", Tree: "LEGION-1", Issue: issue, Role: claim.RoleArchitect, State: state, Session: session,
	}, ""); err != nil {
		t.Fatalf("create the architect claim of %s: %v", issue, err)
	}
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
// queued again as a row of its issue, due after the re-hold delay and counted as one re-send: the
// forward failed (delivery_failed), the registration lapsed between the publish and the forward
// (no_holder), or the receipt never came from a session that is not the claim's live one
// (receipt_timeout). That last covers a session that stopped running while still registered, and
// the relaunch window, where the claim's new session registered with the daemon before its plugin
// took the Envoy role back from the stopped one. Once it is due and a session holds the role, the
// executor delivers it to the owner.
func TestANoticeTheListenerCouldNotForwardIsQueuedAgain(t *testing.T) {
	for _, tc := range []struct {
		name, reason       string
		architect          supervise.ClaimState
		session, recipient string
	}{
		{"a failed forward", "delivery_failed", supervise.StateWorking, "ses_live", "ses_live"},
		{"a holder that lapsed", "no_holder", supervise.StateWorking, "ses_live", ""},
		{"a late receipt while the claim relaunches", "receipt_timeout", supervise.StateLaunching, "", "ses_stopped"},
		{"a late receipt from the stopped session after the new one registered", "receipt_timeout", supervise.StateRegistered, "ses_new", "ses_stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", tc.architect, tc.session)
			clock := time.Now()
			publisher := &holderPublisher{}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }}
			notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
			report := laneReport{"evt-exception", tc.reason, architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", notice, "legion-outbox:7", tc.recipient}

			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			var nextAt time.Time
			if err := pool.QueryRow(context.Background(), "select next_at from outbox where kind = 'notice'").Scan(&nextAt); err != nil {
				t.Fatalf("read the re-held row: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-2 {"kind": "pr-blocked", "role": "architect", "reason": "max_fix_attempts", "resends": 1}` ||
				!nextAt.Equal(clock.Add(noticeReholdDelays[0]).Truncate(time.Microsecond)) {
				t.Fatalf("queued %v due %s; want the one notice of LEGION-2, counted as one re-send, due %s", got, nextAt, clock.Add(noticeReholdDelays[0]))
			}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run before it is due: %v", err)
			}
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered %+v before the re-hold delay passed", delivered)
			}
			clock = clock.Add(noticeReholdDelays[0])
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once due: %v", err)
			}
			if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %v with %d rows left, want the re-held notice at the root architect", deliveredKinds(t, delivered), outboxRows(t, pool))
			}
		})
	}
}

// Only a failed forward of this daemon's notice about an issue of its project, to an architect of
// its project, is queued again. A late receipt from the claim's own live session is a slow holder
// that has the notice. Any other report — another project's role or issue, a phase worker's role,
// the merge queue's READY, another publisher's key, a summary that is not the executor's own, a
// payload that is not a notice — is someone else's, and changes nothing.
func TestAReportThatIsNotAFailedNoticeForwardChangesNothing(t *testing.T) {
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	root := "notifications.role.legion-legion-legion-1-architect"
	for _, tc := range []struct {
		name   string
		report laneReport
	}{
		{"a late receipt from the claim's live session", laneReport{"evt-exception", "receipt_timeout", root, "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}},
		{"another project's architect", laneReport{"evt-exception", "delivery_failed", "notifications.role.legion-other-legion-1-architect", "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}},
		{"another project's issue", laneReport{"evt-exception", "delivery_failed", root, "pr-blocked on OTHER-2", notice, "legion-outbox:7", "ses_live"}},
		{"a phase worker's role", laneReport{"evt-exception", "delivery_failed", "notifications.role.legion-legion-legion-2-planner", "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}},
		{"the merge queue's READY", laneReport{"evt-exception", "delivery_failed", "notifications.role.merge-queue", "READY #42 at abc123", "READY #42 at abc123", "legion-outbox:7", "ses_live"}},
		{"another publisher's key", laneReport{"evt-exception", "delivery_failed", root, "pr-blocked on LEGION-2", notice, "envoy.agent.42", "ses_live"}},
		{"a summary of another kind", laneReport{"evt-exception", "delivery_failed", root, "held on LEGION-2", notice, "legion-outbox:7", "ses_live"}},
		{"a summary naming no issue", laneReport{"evt-exception", "delivery_failed", root, "pr-blocked on everything", notice, "legion-outbox:7", "ses_live"}},
		{"a payload that is not a notice", laneReport{"evt-exception", "delivery_failed", root, "pr-blocked on LEGION-2", map[string]string{"kind": "pr-blocked", "packet": "READY"}, "legion-outbox:7", "ses_live"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateWorking, "ses_live")
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
			if err := runner.rehold(context.Background(), tc.report.envelope(t)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != 0 {
				t.Fatalf("queued %v, want nothing", got)
			}
		})
	}
}

// One published copy of a row is queued again once, however many reports name it: a publish whose
// 200 was lost is retried under the same row key, and the listener reports each failed forward
// with a fresh event id. A report that cannot be read is refused.
func TestARowCopyIsQueuedOnceAndAnUnreadableReportIsRefused(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	for _, eventID := range []string{"evt-first-forward", "evt-retried-forward", "evt-retried-forward"} {
		report := laneReport{eventID, "delivery_failed", "notifications.role.legion-legion-legion-1-architect", "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}
		if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
			t.Fatalf("rehold: %v", err)
		}
	}
	if got := queuedNotices(t, pool); len(got) != 1 {
		t.Fatalf("queued %v after three reports of one row copy, want one row", got)
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

// A notice is queued again at most three times, as the TypeScript daemon re-sends one at most three
// times: a holder that keeps its registration alive but never confirms a delivery would otherwise
// be sent copies without end. The report of the third copy is logged once and queues nothing.
func TestANoticeIsQueuedAgainAtMostThreeTimes(t *testing.T) {
	pool := isolatedOutboxPool(t)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	var logged bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
	topic := "notifications.role.legion-legion-legion-1-architect"
	second := laneReport{"evt-second", "delivery_failed", topic, "pr-blocked on LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: 2}, "legion-outbox:8", "ses_live"}
	if err := runner.rehold(context.Background(), second.envelope(t)); err != nil {
		t.Fatalf("rehold the second copy: %v", err)
	}
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-2 {"kind": "pr-blocked", "role": "architect", "resends": 3}` {
		t.Fatalf("queued %v, want the third copy", got)
	}
	third := laneReport{"evt-third", "delivery_failed", topic, "pr-blocked on LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: 3}, "legion-outbox:9", "ses_live"}
	if err := runner.rehold(context.Background(), third.envelope(t)); err != nil {
		t.Fatalf("rehold the third copy: %v", err)
	}
	if got := queuedNotices(t, pool); len(got) != 1 || strings.Count(logged.String(), `msg="outbox notice not re-held again`) != 1 {
		t.Fatalf("queued %v, log %q; want no fourth copy and one line saying so", got, logged.String())
	}
}

// The report arrives over core NATS on the original role topic's exceptions subject; the
// subscription hears it there and queues the notice again.
func TestARoleLaneExceptionOnNATSQueuesTheNoticeAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-1", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
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
	report := laneReport{"evt-exception", "delivery_failed", topic, "design-approved on LEGION-1", record.Notice{Kind: "design-approved", Version: 3}, "legion-outbox:9", "ses_live"}
	if err := conn.Publish("notifications.envoy.exceptions."+topic, report.envelope(t)); err != nil {
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
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-1 {"kind": "design-approved", "resends": 1, "version": 3}` {
		t.Fatalf("queued %v, want the design-approved notice of LEGION-1", got)
	}
}

// A relaunch can take minutes to take the Envoy role back. Until then the listener still forwards
// to the stopped session's registration and reports each copy as a late receipt from it, and once
// that registration lapses (here 5 minutes after the session stopped) it refuses the publish, and
// the executor holds the row. The copies are spaced so that the last is sent after any such
// registration has lapsed, so an architect that takes the role back after four minutes, or after
// seven, still receives the notice once, and nothing is dropped at the cap.
func TestANoticeReachesAnArchitectThatTakesMinutesToRelaunch(t *testing.T) {
	for _, relaunch := range []time.Duration{4 * time.Minute, 7 * time.Minute} {
		t.Run(relaunch.String(), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateRegistered, "ses_new")
			start := time.Now()
			clock := start
			lapsed, relaunched := start.Add(5*time.Minute), start.Add(relaunch)
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, start))

			taken, received := 0, []outboxPublish{}
			for clock.Before(start.Add(15*time.Minute)) && len(received) == 0 {
				if !clock.Before(lapsed) && clock.Before(relaunched) {
					publisher.setAbsent(architectTopic(t, "LEGION-1"))
				} else {
					publisher.setAbsent()
				}
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("run at %s: %v", clock.Sub(start), err)
				}
				_, delivered := publisher.snapshot()
				for _, copy := range delivered[taken:] {
					if clock.Before(relaunched) {
						// The stopped session's registration takes the copy, and the listener reports it.
						report := laneReport{fmt.Sprintf("evt-%s", copy.key), "receipt_timeout", copy.topic, "pr-blocked on LEGION-2", copy.payload, copy.key, "ses_stopped"}
						if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
							t.Fatalf("rehold at %s: %v", clock.Sub(start), err)
						}
						continue
					}
					received = append(received, copy)
				}
				taken = len(delivered)
				clock = clock.Add(5 * time.Second)
			}
			if len(received) != 1 || strings.Contains(logged.String(), "level=WARN") {
				t.Fatalf("after %s: %d copies taken, received %+v by the relaunched session, log %q; want the notice received once, nothing dropped",
					clock.Sub(start), taken, received, logged.String())
			}
		})
	}
}
