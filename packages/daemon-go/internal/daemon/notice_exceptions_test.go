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

// A notice the listener accepted but could not forward to the owning architect's session is queued
// again, counted as one re-send and published under the key of the row it copies, so a session that
// did get the forward recognises the copy. Every report is queued, a late receipt from the claim's
// own session too: a Go relaunch resumes the session id, so that report may come from the stopped
// process. The first copy is due at once when the architect's agent has taken its role, whichever
// session the forward went to, since that ready will not come again to release it; otherwise it
// waits the first re-send delay.
func TestANoticeTheListenerCouldNotForwardIsQueuedAgain(t *testing.T) {
	for _, tc := range []struct {
		name, reason       string
		architect          supervise.ClaimState
		session, recipient string
		due                time.Duration
	}{
		{"a failed forward to the live session", "delivery_failed", supervise.StateWorking, "ses_live", "ses_live", 0},
		{"a late receipt from the live session", "receipt_timeout", supervise.StateIdle, "ses_live", "ses_live", 0},
		{"a holder that lapsed, the role taken since", "no_holder", supervise.StateWorking, "ses_live", "", 0},
		{"a holder that lapsed, the claim relaunching", "no_holder", supervise.StateLaunching, "", "", noticeReholdDelays[0]},
		{"a late receipt while the claim relaunches", "receipt_timeout", supervise.StateLaunching, "", "ses_stopped", noticeReholdDelays[0]},
		{"a late receipt from the stopped session after the new one registered", "receipt_timeout", supervise.StateRegistered, "ses_new", "ses_stopped", noticeReholdDelays[0]},
		{"a late receipt from the stopped session after the new one is ready", "receipt_timeout", supervise.StateReady, "ses_new", "ses_stopped", 0},
		{"a late receipt from a resumed session before it takes its role", "receipt_timeout", supervise.StateRegistered, "ses_arch", "ses_arch", noticeReholdDelays[0]},
		{"a late receipt from a resumed session that took its role inside the receipt window", "receipt_timeout", supervise.StateReady, "ses_arch", "ses_arch", 0},
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
			want := `LEGION-2 {"kind": "pr-blocked", "role": "architect", "reason": "max_fix_attempts", "resends": 1, "resend_of": 7}`
			if got := queuedNotices(t, pool); len(got) != 1 || got[0] != want || !nextAt.Equal(clock.Add(tc.due).Truncate(time.Microsecond)) {
				t.Fatalf("queued %v due %s; want %s, counted as one re-send, copying row 7, due %s", got, nextAt, want, clock.Add(tc.due))
			}

			if tc.due > 0 {
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("run before it is due: %v", err)
				}
				if _, delivered := publisher.snapshot(); len(delivered) != 0 {
					t.Fatalf("delivered %+v before the re-hold delay passed", delivered)
				}
				clock = clock.Add(tc.due)
			}
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run once due: %v", err)
			}
			_, delivered := publisher.snapshot()
			if fmt.Sprint(deliveredKinds(t, delivered)) != "[pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %v with %d rows left, want the re-held notice at the root architect", deliveredKinds(t, delivered), outboxRows(t, pool))
			}
			if sent := delivered[0]; sent.key != "legion-outbox:7" || sent.payload.(record.Notice).ResendOf != 0 {
				t.Fatalf("the copy went out under key %q with payload %+v; want the key of row 7, and the notice without the row it copies", sent.key, sent.payload)
			}
		})
	}
}

// The listener's report echoes the summary and dedupe key the executor published the notice with,
// and the re-hold reads both back: a report of the executor's own publish is queued again.
func TestAReportOfTheExecutorsOwnPublishIsQueuedAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaimOn(t, sup, "LEGION-1", supervise.StateWorking, "ses_live")
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, time.Now()))
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("publish the notice: %v", err)
	}
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 {
		t.Fatalf("published %+v; want the one notice", delivered)
	}
	sent := delivered[0]
	report := laneReport{"evt-exception", "delivery_failed", sent.topic, sent.message, sent.payload, sent.key, "ses_live"}
	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold: %v", err)
	}
	published, _ := record.ParseOutboxKey(sent.key)
	want := fmt.Sprintf(`LEGION-2 {"kind": "pr-blocked", "role": "architect", "reason": "max_fix_attempts", "resends": 1, "resend_of": %d}`, published)
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != want {
		t.Fatalf("after a report of the publish %+v, queued %v; want %s", sent, got, want)
	}
}

// Only a failed forward of this daemon's notice about an issue of its project, to an architect of
// its project, is queued again. Any other report — another project's role or issue, a phase
// worker's role, the merge queue's READY, another publisher's key, a summary that is not the
// executor's own, a payload that is not a notice — is someone else's, and changes nothing.
func TestAReportThatIsNotAFailedNoticeForwardChangesNothing(t *testing.T) {
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}
	root := "notifications.role.legion-legion-legion-1-architect"
	for _, tc := range []struct {
		name   string
		report laneReport
	}{
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
// with a fresh event id. A copy is published under the key of the row it copies, so the report of
// the copy is queued again too. A report that cannot be read is refused.
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
	copied := notice
	copied.Resends = 1
	report := laneReport{"evt-copy-forward", "delivery_failed", "notifications.role.legion-legion-legion-1-architect", "pr-blocked on LEGION-2", copied, "legion-outbox:7", "ses_live"}
	if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
		t.Fatalf("rehold the copy: %v", err)
	}
	if got := queuedNotices(t, pool); len(got) != 2 {
		t.Fatalf("queued %v after a report of the copy, want its copy too", got)
	}
	for _, malformed := range [][]byte{[]byte("not json"), []byte(`{"event_id":"evt-2","source":"agent","payload":"{}"}`), []byte(`{"event_id":"evt-3","source":"envoy","payload":"not json"}`)} {
		if err := runner.rehold(context.Background(), malformed); err == nil {
			t.Fatalf("rehold(%s) = nil, want a refusal", malformed)
		}
	}
	if got := queuedNotices(t, pool); len(got) != 2 {
		t.Fatalf("queued %v after the malformed reports, want still two rows", got)
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
	second := laneReport{"evt-second", "delivery_failed", topic, "pr-blocked on LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: 2}, "legion-outbox:7", "ses_live"}
	if err := runner.rehold(context.Background(), second.envelope(t)); err != nil {
		t.Fatalf("rehold the second copy: %v", err)
	}
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-2 {"kind": "pr-blocked", "role": "architect", "resends": 3, "resend_of": 7}` {
		t.Fatalf("queued %v, want the third copy", got)
	}
	third := laneReport{"evt-third", "delivery_failed", topic, "pr-blocked on LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: 3}, "legion-outbox:7", "ses_live"}
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
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-1 {"kind": "design-approved", "resends": 1, "version": 3, "resend_of": 9}` {
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
						report := laneReport{fmt.Sprintf("evt-%s", copy.key), "receipt_timeout", copy.topic, copy.message, copy.payload, copy.key, "ses_stopped"}
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

// Once the architect's claim is ready again, the copy waiting out its re-send delay is due at once.
// So a notice written after the relaunch is not held behind that copy by the per-architect fence for
// minutes: it arrives within seconds, after the waiting copy, and the waiting copy arrives once. A Go
// relaunch resumes the claim's session file, so the relaunched agent registers the same session id
// the stopped one had; a relaunch onto a new session is released the same way.
func TestANoticeWrittenAfterTheRelaunchIsNotHeldBehindAWaitingCopy(t *testing.T) {
	for _, tc := range []struct{ name, stopped, relaunched string }{
		{"the session resumed", "ses_arch", "ses_arch"},
		{"a new session", "ses_stopped", "ses_new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateLaunching, tc.stopped)
			start := time.Now()
			clock := start
			relaunched := start.Add(4 * time.Minute)
			written := relaunched.Add(30 * time.Second)
			publisher := &holderPublisher{}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
				now: func() time.Time { return clock }}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, start))

			taken, received, readyDone, writtenDone := 0, []string{}, false, false
			var arrivedB time.Time
			for clock.Before(start.Add(15*time.Minute)) && arrivedB.IsZero() {
				if !readyDone && !clock.Before(relaunched) {
					// The relaunched agent takes the role back and says it is ready.
					if err := runner.releaseWaiting(context.Background(), architectTopicToken(t, "LEGION-1")); err != nil {
						t.Fatalf("release at %s: %v", clock.Sub(start), err)
					}
					readyDone = true
				}
				if !writtenDone && !clock.Before(written) {
					enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, clock))
					writtenDone = true
				}
				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("run at %s: %v", clock.Sub(start), err)
				}
				_, delivered := publisher.snapshot()
				for _, copy := range delivered[taken:] {
					if clock.Before(relaunched) {
						report := laneReport{fmt.Sprintf("evt-%s", copy.key), "receipt_timeout", copy.topic, copy.message, copy.payload, copy.key, tc.stopped}
						if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
							t.Fatalf("rehold at %s: %v", clock.Sub(start), err)
						}
						continue
					}
					kind := copy.payload.(record.Notice).Kind
					received = append(received, string(kind))
					if kind == "design-approved" {
						arrivedB = clock
					}
				}
				taken = len(delivered)
				clock = clock.Add(5 * time.Second)
			}
			if fmt.Sprint(received) != "[pr-blocked design-approved]" || arrivedB.IsZero() || arrivedB.Sub(written) > 10*time.Second {
				t.Fatalf("received %v, the later notice %s after it was written; want the waiting copy then the later notice, within seconds", received, arrivedB.Sub(written))
			}
		})
	}
}

// architectTopicToken is issue's architect claim token.
func architectTopicToken(t *testing.T, issue string) claim.Token {
	t.Helper()
	return mustClaimToken(t, issue, claim.RoleArchitect)
}

// A ready releases every notice its architect owns now that waits for a later attempt, a re-held
// copy or one held on the outbox's backoff; a notice owned by another architect of the tree keeps
// its time. One release serves every architect that became ready, each in its own tree.
func TestAReadyArchitectReleasesOnlyItsOwnNotices(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-9", Project: "LEGION", Tree: "LEGION-9", Title: "Another root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaimOn(t, sup, "LEGION-1", supervise.StateRegistered, "ses_arch")
	architectClaimOn(t, sup, "LEGION-2", supervise.StateWorking, "ses_sub")
	architectClaimOn(t, sup, "LEGION-9", supervise.StateReady, "ses_other")
	now := time.Now()
	later := now.Add(noticeReholdDelays[1])
	for _, c := range []struct {
		issue   string
		resends int
	}{
		{"LEGION-1", 1}, // a re-held copy of the root's
		{"LEGION-1", 0}, // the root's, held on backoff
		{"LEGION-3", 1}, // the sub-architect's: LEGION-3 is under LEGION-2, whose sub-architect runs
		{"LEGION-9", 1}, // another tree's root's
	} {
		enqueueOutbox(t, pool, records, mustOutboxRow(t, c.issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: c.resends}, later))
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: func() time.Time { return now }}
	if err := runner.releaseWaiting(context.Background(), architectTopicToken(t, "LEGION-1"), architectTopicToken(t, "LEGION-9")); err != nil {
		t.Fatalf("release: %v", err)
	}
	rows, err := pool.Query(context.Background(), "select issue, next_at <= $1 from outbox order by id", now)
	if err != nil {
		t.Fatalf("read the copies: %v", err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var issue string
		var due bool
		if err := rows.Scan(&issue, &due); err != nil {
			t.Fatalf("scan a copy: %v", err)
		}
		got = append(got, fmt.Sprintf("%s due=%t", issue, due))
	}
	if fmt.Sprint(got) != "[LEGION-1 due=true LEGION-1 due=true LEGION-3 due=false LEGION-9 due=true]" {
		t.Fatalf("notices %v; want the roots' three due now and the sub-architect's kept", got)
	}
}

// The ready route's hook only records a ready architect, so the agent's ready is answered without
// waiting on a release; the workflow's release loop then releases the architect's waiting notice.
func TestAReadyArchitectsNoticesAreReleasedOffTheReadyRoute(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaimOn(t, sup, "LEGION-1", supervise.StateRegistered, "ses_arch")
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Resends: 1}, time.Now().Add(noticeReholdDelays[1])))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
	w := &workflowRuntime{outbox: runner, log: quietLogger(), readied: map[claim.Token]bool{}, readyWake: make(chan struct{}, 1)}
	due := func() bool {
		t.Helper()
		var due bool
		if err := pool.QueryRow(context.Background(), "select next_at <= $1 from outbox", time.Now()).Scan(&due); err != nil {
			t.Fatalf("read the waiting notice: %v", err)
		}
		return due
	}

	w.claimReady(supervise.Claim{Token: architectTopicToken(t, "LEGION-1"), Role: claim.RoleArchitect})
	if due() {
		t.Fatalf("the hook released the notice itself, so the ready route waited on the release")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		w.releaseReadied(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	for deadline := time.Now().Add(5 * time.Second); !due(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the release loop left the ready architect's notice waiting")
		}
	}
}

// A claim can read live while its session hears nothing: a pane's NATS connection is reconnecting,
// or the process died and the supervisor has not seen it yet. Every forward then comes back a late
// receipt. Only the first copy goes at once, for a relaunch that took its role inside the receipt
// window; the rest keep the re-send spacing, so the notice stays queued for the whole schedule, not
// a few seconds, before the cap drops it. The run ticks every 5 seconds, so a copy due at once goes
// on the next tick.
func TestANoticeToALiveClaimThatHearsNothingKeepsItsSchedule(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaimOn(t, sup, "LEGION-1", supervise.StateWorking, "ses_live")
	start := time.Now()
	clock := start
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, start))

	taken, sent, dropped := 0, []string{}, time.Duration(0)
	for clock.Before(start.Add(15*time.Minute)) && dropped == 0 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run at %s: %v", clock.Sub(start), err)
		}
		_, delivered := publisher.snapshot()
		for _, copy := range delivered[taken:] {
			sent = append(sent, clock.Sub(start).String())
			report := laneReport{fmt.Sprintf("evt-%d", len(sent)), "receipt_timeout", copy.topic, copy.message, copy.payload, copy.key, "ses_live"}
			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold at %s: %v", clock.Sub(start), err)
			}
		}
		taken = len(delivered)
		if strings.Contains(logged.String(), `msg="outbox notice not re-held again`) {
			dropped = clock.Sub(start)
		}
		clock = clock.Add(5 * time.Second)
	}
	if fmt.Sprint(sent) != "[0s 5s 2m5s 8m5s]" || dropped != 8*time.Minute+5*time.Second {
		t.Fatalf("forwards at %v, dropped at %s; want the original, one copy at once, then copies 2 and 6 minutes apart, dropped only at the cap after them", sent, dropped)
	}
}

// A later copy whose report names a session other than the one the claim is ready on went to a
// session the role has since left, so it is due at once, as the first copy is: waiting its delay
// would hold the live architect's later notices behind it. A later copy that failed on the claim's
// own ready session keeps its delay, since that session may be the one hearing nothing, and so
// does one whose report names no session: the listener names none when the role had no holder or
// it could not read the holder, which says nothing about where the role went.
func TestALaterCopyToAnArchitectBackOnAnotherSessionIsDueAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name, reason, recipient string
		due                     time.Duration
	}{
		{"the report names the session the role left", "receipt_timeout", "ses_stopped", 0},
		{"the report names the claim's own ready session", "receipt_timeout", "ses_new", noticeReholdDelays[1]},
		{"a lapsed holder, naming no session", "no_holder", "", noticeReholdDelays[1]},
		{"a failed holder read, naming no session", "delivery_failed", "", noticeReholdDelays[1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateReady, "ses_new")
			clock := time.Now()
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: func() time.Time { return clock }}
			copied := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts", Resends: 1}
			report := laneReport{"evt-exception", tc.reason, architectTopic(t, "LEGION-1"), "pr-blocked on LEGION-2", copied, "legion-outbox:7", tc.recipient}
			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			var nextAt time.Time
			if err := pool.QueryRow(context.Background(), "select next_at from outbox where kind = 'notice'").Scan(&nextAt); err != nil {
				t.Fatalf("read the re-held row: %v", err)
			}
			if !nextAt.Equal(clock.Add(tc.due).Truncate(time.Microsecond)) {
				t.Fatalf("the second copy is due %s; want %s", nextAt, clock.Add(tc.due))
			}
		})
	}
}
