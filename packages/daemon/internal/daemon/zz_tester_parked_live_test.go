//go:build e2e

// THROWAWAY tester drivers (INFRA-251 round 13): the whole daemon (startDaemonWithin, the function
// `legion start` runs) over a real PostgreSQL and a real nats-server; Dispatch events enter only as
// envelopes on ENVOY_NOTIFICATIONS; notices are read back from the Envoy listener stand-in. Not
// committed.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

type parkedLive struct {
	liveProof
	rt    *fake.Runtime
	built *built
	// P parked root, PC its child, W awaiting_person root, S slotted root, Q root waiting for its
	// slot, R a lingering root someone moved back to in_progress.
	P, PC, W, S, Q, R string
	seq               map[string]int64
	raw               map[string][2]string
	github            string
	blockersMu        sync.Mutex
	blockers          map[string][]string
	// artifacts maps an artifact id to its issue and latest version (the stand-in's artifact read);
	// primaries maps an issue to its primary_artifact_id (the stand-in's issue read).
	artifacts map[string][2]any
	primaries map[string]string
}

func newParkedLive(t *testing.T) *parkedLive {
	r := &parkedLive{liveProof: newLiveProof(t, "tester"), rt: fake.NewRuntime(), built: &built{}, seq: map[string]int64{}, raw: map[string][2]string{}, blockers: map[string][]string{}, artifacts: map[string][2]any{}, primaries: map[string]string{}}
	pr := r.cfg.Project
	r.P, r.PC, r.W, r.S, r.Q, r.R = pr+"-1", pr+"-2", pr+"-3", pr+"-4", pr+"-5", pr+"-6"
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if id, ok := strings.CutPrefix(req.URL.Path, "/api/v1/artifacts/"); ok && req.Method == http.MethodGet {
			key, latest := r.W, 1
			r.blockersMu.Lock()
			if a, ok := r.artifacts[id]; ok {
				key, latest = a[0].(string), a[1].(int)
			}
			r.blockersMu.Unlock()
			switch id {
			case "artifact-p":
				key, latest = r.P, 2
			case "artifact-r":
				key, latest = r.R, 2
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"issue_key":%q,"approval":{"state":"none","latest_version":%d}}`, key, latest)
			return
		}
		if key, ok := strings.CutPrefix(req.URL.Path, "/api/v1/issues/"); ok && req.Method == http.MethodGet && !strings.Contains(key, "/") {
			r.blockersMu.Lock()
			blocked, has := r.blockers[key]
			primary, hasPrimary := r.primaries[key]
			r.blockersMu.Unlock()
			if has || hasPrimary {
				rec := httptest.NewRecorder()
				r.board.ServeHTTP(rec, req)
				var body map[string]any
				if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &body) == nil {
					if has {
						body["blocked_by"] = blocked
					}
					if hasPrimary {
						body["primary_artifact_id"] = primary
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(body)
					return
				}
			}
		}
		r.board.ServeHTTP(w, req)
	}))
	t.Cleanup(wrapped.Close)
	r.cfg.DispatchURL = wrapped.URL
	return r
}

func TestTesterParkedDecisionLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 1
	r.seed()
	r.boot()

	r.step("boot-state", func() {
		if r.issue(r.W).Phase != phase.AwaitingPerson || r.issue(r.W).WaitingOn == nil || r.issue(r.W).WaitingOn.AnsweredAt != nil {
			t.Fatalf("W is not an unanswered awaiting_person root at boot")
		}
		if r.issue(r.P).Phase != phase.Done {
			t.Fatalf("P is not parked")
		}
		r.settleCount(0)
	})

	var answerP string
	r.step("person-answers-parked-root-ask-once", func() {
		answerP = r.askEvent(r.P, "ask.answered", personActor("alice"), map[string]any{"id": "ask-p", "kind": "question"})
		got := r.waitParked(1)
		r.want(got[0], "ask-answered", r.P, "dispatch://"+r.P+"/ask/ask-p", "user:alice", r.P)
		r.republish(answerP)
		r.waitProcessedDispatch(answerP)
		r.settleCount(1)
		if n := len(r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(r.P)))); n != 0 {
			t.Fatalf("P's suspended architect topic got %d publishes", n)
		}
	})

	r.step("person-reply-on-parked-child-names-root", func() {
		r.askEvent(r.PC, "comment.created", personActor("bob"), map[string]any{"id": "cmt-pc-1", "ask_id": "ask-pc", "ask_state": "open", "ask_waiting_on": "agent"})
		got := r.waitParked(2)
		r.want(got[1], "ask-reply", r.P, "dispatch://"+r.PC+"/comment/cmt-pc-1", "user:bob", r.PC)
	})

	r.step("session-reply-and-human-turn-reply-on-parked-tree-relay-nothing", func() {
		e1 := r.askEvent(r.P, "comment.created", sessionActor("ses-someone"), map[string]any{"id": "cmt-p-s", "ask_id": "ask-p2", "ask_state": "open", "ask_waiting_on": "agent"})
		e2 := r.askEvent(r.P, "comment.created", personActor("alice"), map[string]any{"id": "cmt-p-h", "ask_id": "ask-p2", "ask_state": "open", "ask_waiting_on": "human"})
		e3 := r.askEvent(r.P, "ask.answered", sessionActor("ses-someone"), map[string]any{"id": "ask-p3", "kind": "question"})
		for _, e := range []string{e1, e2, e3} {
			r.waitProcessedDispatch(e)
		}
		r.settleCount(2)
	})

	r.step("person-approves-parked-spec", func() {
		r.askEvent(r.P, "artifact.approved", personActor("carol"), map[string]any{"artifact_id": "artifact-p", "version": 2})
		got := r.waitParked(3)
		r.want(got[2], "design-approved", r.P, "dispatch://"+r.P+"/spec", "user:carol", r.P)
	})

	r.step("awaiting-person-root-answer-goes-through-main-not-relay", func() {
		e := r.askEvent(r.W, "ask.answered", personActor("alice"), map[string]any{"id": "ask-w", "kind": "question"})
		r.waitProcessedDispatch(e)
		testwait.Eventually(t, "W answered", func() bool {
			i := r.issue(r.W)
			return i.WaitingOn != nil && i.WaitingOn.AnsweredAt != nil
		})
		e2 := r.askEvent(r.W, "artifact.approved", personActor("alice"), map[string]any{"artifact_id": "artifact-w", "version": 1})
		e3 := r.askEvent(r.W, "comment.created", personActor("alice"), map[string]any{"id": "cmt-w", "ask_id": "ask-w2", "ask_state": "open", "ask_waiting_on": "agent"})
		r.waitProcessedDispatch(e2)
		r.waitProcessedDispatch(e3)
		r.settleCount(3)
	})

	r.step("slotted-root-answer-relays-nothing", func() {
		e := r.askEvent(r.S, "ask.answered", personActor("alice"), map[string]any{"id": "ask-s", "kind": "question"})
		e2 := r.askEvent(r.S, "comment.created", personActor("alice"), map[string]any{"id": "cmt-s", "ask_id": "ask-s2", "ask_state": "open", "ask_waiting_on": "agent"})
		r.waitProcessedDispatch(e)
		r.waitProcessedDispatch(e2)
		r.settleCount(3)
	})

	r.step("waiting-for-slot-root-decision-kept-not-sent", func() {
		e := r.askEvent(r.Q, "ask.answered", personActor("dave"), map[string]any{"id": "ask-q", "kind": "question"})
		r.waitProcessedDispatch(e)
		r.settleCount(3)
		if kept := r.kept(r.Q); len(kept) != 1 || kept[0].Actor != "user:dave" {
			t.Fatalf("kept for Q = %+v", kept)
		}
	})

	r.step("awaiting-root-a-person-set-to-todo-keeps-nothing", func() {
		r.publishStatus(r.W, "todo")
		e := r.askEvent(r.W, "comment.created", personActor("alice"), map[string]any{"id": "cmt-w2", "ask_id": "ask-w", "ask_state": "closed"})
		r.waitProcessedDispatch(e)
		r.settleCount(3)
		if kept := r.kept(r.W); len(kept) != 0 {
			t.Fatalf("kept for awaiting W = %+v, want none", kept)
		}
	})

	r.step("lingering-root-in-a-running-status-keeps-and-sends-nothing", func() {
		e1 := r.askEvent(r.R, "ask.answered", personActor("alice"), map[string]any{"id": "ask-r", "kind": "question"})
		e2 := r.askEvent(r.R, "comment.created", personActor("alice"), map[string]any{"id": "cmt-r", "ask_id": "ask-r2", "ask_state": "open", "ask_waiting_on": "agent"})
		e3 := r.askEvent(r.R, "artifact.approved", personActor("carol"), map[string]any{"artifact_id": "artifact-r", "version": 2})
		for _, e := range []string{e1, e2, e3} {
			r.waitProcessedDispatch(e)
		}
		r.settleCount(3)
		if len(r.kept(r.R)) != 0 {
			t.Fatalf("kept for R = %+v", r.kept(r.R))
		}
	})

	r.step("restart-sends-nothing-again", func() {
		r.restart(4)
		r.republish(answerP)
		r.waitProcessedDispatch(answerP)
		r.settleCount(3)
		if kept := r.kept(r.P); len(kept) != 3 {
			t.Fatalf("kept for P = %d, want 3", len(kept))
		}
	})

	r.step("slot-waiting-root-admitted-names-and-forgets", func() {
		testwait.Eventually(t, "Q's kept decision held by its start's delivery", func() bool {
			return len(r.kept(r.Q)) == 0 || (len(r.unheld(r.Q)) == 0 && r.pendingDeliveries(r.Q) == 1)
		})
		r.note("Q: kept=%d unheld=%d pending=%d", len(r.kept(r.Q)), len(r.unheld(r.Q)), r.pendingDeliveries(r.Q))
		if task := r.lastTask(r.Q); !strings.Contains(task, "dispatch://"+r.Q+"/ask/ask-q") {
			t.Fatalf("Q's start task %q does not name its kept decision", task)
		}
	})

	r.step("real-dispatch-payloads-on-parked-root", func() {
		data, err := os.ReadFile(os.Getenv("TESTER_REAL_EVENTS"))
		if err != nil {
			t.Fatalf("real events: %v", err)
		}
		base := len(r.parked())
		var ids []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			r.seq[r.P]++
			seq := 100 + r.seq[r.P]
			ev["issue_key"], ev["seq"], ev["id"], ev["project"] = r.P, seq, seq*10+3, r.cfg.Project
			ev["payload"].(map[string]any)["issue_key"] = r.P
			raw, _ := json.Marshal(ev)
			typ := ev["type"].(string)
			ids = append(ids, r.publish("notifications.dispatch.issue."+r.P+"."+typ, "dispatch", fmt.Sprintf("%s-real-%d", r.P, seq), string(raw)))
		}
		for _, id := range ids {
			r.waitProcessedDispatch(id)
		}
		time.Sleep(liveSettle)
		got := r.parked()[base:]
		r.note("13 real events relayed %d", len(got))
		if len(got) != 3 {
			t.Fatalf("real payloads relayed %d, want 3", len(got))
		}
	})

	r.step("a-delivery-that-fails-keeps-a-delivered-start-forgets", func() {
		before := len(r.kept(r.P))
		baseResumes := len(r.rt.CallsOf("Resume"))
		fail := fake.SpawnResult{Err: errors.New("tester: the pod cannot be resumed")}
		r.rt.ScriptResume(fail, fail, fail)
		r.publishStatus(r.P, "todo")
		testwait.Eventually(t, "P's architect resume refused twice", func() bool { return len(r.rt.CallsOf("Resume")) >= baseResumes+2 })
		startRow := r.startRow(r.P)
		claimedByStart := r.scalar(`select count(*) from parked_decisions where issue in (select key from issues where tree = $1) and claimed_by = $2`, r.P, startRow)
		r.note("while the resume fails: kept %d (before %d), start row %d claims %d", len(r.kept(r.P)), before, startRow, claimedByStart)
		if got := len(r.kept(r.P)); got != before || claimedByStart != int64(before) {
			t.Fatalf("while the start retries: kept %d (want %d), start row %d claiming %d", got, before, startRow, claimedByStart)
		}
		testwait.Eventually(t, "P's kept decisions held by its start's delivery", func() bool {
			return len(r.unheld(r.P)) == 0 && r.pendingDeliveries(r.P) == 1
		})
		sh := r.ready(r.P, "ses_arch_"+r.P, true)
		got := r.prompts(sh, 4*time.Second)
		r.note("P's architect prompts=%d; kept after its turn started: %d", len(got), len(r.kept(r.P)))
		testwait.Eventually(t, "P's kept decisions told at the confirmation", func() bool { return len(r.kept(r.P)) == 0 })
		sh.conn.Close()
		task := strings.Join(got, " || ")
		if len(got) != 1 {
			t.Fatalf("P's architect was prompted %d times, want its start task once: %q", len(got), got)
		}
		for _, ref := range []string{"dispatch://" + r.P + "/ask/ask-p", "dispatch://" + r.PC + "/comment/cmt-pc-1", "dispatch://" + r.P + "/spec"} {
			if !strings.Contains(task, ref) {
				t.Fatalf("P's start task does not name %s", ref)
			}
		}
		r.settleCount(6)
	})
}

// TestTesterCrashWindowLive: a re-admitted root's start delivers its task, its forget fails (a
// Postgres trigger refuses the delete), the architect's turn on the task ends, the daemon is
// stopped, the trigger dropped, and the daemon booted again: the retry queues nothing and forgets
// the decision; then a newer start for the same claim still delivers.
func TestTesterCrashWindowLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 2
	r.cfg.Linger = 10 * time.Minute
	T := r.P
	r.parkedRoot(T)
	trigger := "tester_block_" + strings.ToLower(r.cfg.Project)
	t.Cleanup(func() { r.dropTrigger(trigger) })
	r.boot()
	tok := r.token(T)
	ref1, ref2 := "dispatch://"+T+"/ask/ask-1", "dispatch://"+T+"/ask/ask-2"

	r.step("person-answers-on-the-parked-tree", func() {
		e := r.askEvent(T, "ask.answered", personActor("alice"), map[string]any{"id": "ask-1", "kind": "question"})
		r.waitProcessedDispatch(e)
		r.waitParked(1)
	})

	var startRow int64
	r.step("readmission-delivers-and-its-forget-fails", func() {
		r.addTrigger(trigger, T)
		resumes := len(r.rt.CallsOf("Resume"))
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "T's start task queued", func() bool { return strings.Contains(r.lastTask(T), ref1) })
		testwait.Eventually(t, "the start's delivery holds the decision", func() bool {
			startRow = r.scalar("select coalesce(max(r), 0) from claims, unnest(delivered_rows) as r where token = $1", string(tok))
			return startRow > 0 && len(r.unheld(T)) == 0 && len(r.kept(T)) == 1
		})
		r.note("start row %d attempts=%d; delivered=%t; kept=%d unheld=%d pending=%d", startRow, r.rowAttempts(startRow), r.delivered(T, startRow), len(r.kept(T)), len(r.unheld(T)), r.pendingDeliveries(T))
		testwait.Eventually(t, "T's architect resumed", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
	})

	r.step("architect-takes-the-task-and-its-turn-ends", func() {
		sh := r.ready(T, "ses_arch_"+T, false)
		got := r.prompts(sh, 4*time.Second)
		if len(got) != 1 || !strings.Contains(got[0], ref1) {
			t.Fatalf("the architect's prompts %q, want one naming %s", got, ref1)
		}
		time.Sleep(liveSettle)
		r.note("after the turn whose confirmation's tell is refused: kept=%d unheld=%d pending=%d", len(r.kept(T)), len(r.unheld(T)), r.pendingDeliveries(T))
		sh.conn.Close()
	})

	r.step("daemon-stopped-trigger-dropped-daemon-restarted", func() {
		r.d.stop()
		r.dropTrigger(trigger)
		r.restart(2)
		testwait.Eventually(t, "the start row finished", func() bool { return r.rowGone(startRow) })
		r.note("after restart: kept=%d unheld=%d pending=%d", len(r.kept(T)), len(r.unheld(T)), r.pendingDeliveries(T))
		r.note("architect state after the restart: %+v", r.issue(T).Architect)
	})

	r.step("a-newer-start-for-the-same-claim-still-delivers", func() {
		r.publishStatus(T, "backlog")
		testwait.Eventually(t, "T parked again", func() bool { return r.issue(T).Phase == phase.Done })
		e := r.askEvent(T, "ask.answered", personActor("bob"), map[string]any{"id": "ask-2", "kind": "question"})
		r.waitProcessedDispatch(e)
		r.waitParked(2)
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "T's newer start task queued", func() bool { return strings.Contains(r.lastTask(T), ref2) })
		if task := r.lastTask(T); !strings.Contains(task, ref1) {
			t.Fatalf("the newer start's task %q does not name %s: after the failed confirmation write it must be told again, never lost (ruling on #2163)", task, ref1)
		} else if false {
			t.Fatalf("the newer start's task %q names %s again, which the architect was told once already, in the turn whose confirmation's write failed", task, ref1)
		}
		r.note("newer start delivered: %.160s", r.lastTask(T))
	})
}

// TestTesterStaleStartReleaseLive: start A claims a kept decision and keeps failing its resume; the
// tree is parked and re-admitted again, so start B runs and its architect comes up; A then finishes
// stale and releases the claim. The decision must reach the running architect once.
func TestTesterStaleStartReleaseLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 2
	r.cfg.Linger = 10 * time.Minute
	T := r.P
	r.parkedRoot(T)
	r.boot()
	tok := r.token(T)
	ref := "dispatch://" + T + "/ask/ask-k"
	e := r.askEvent(T, "ask.answered", personActor("alice"), map[string]any{"id": "ask-k", "kind": "question"})
	r.waitProcessedDispatch(e)
	r.waitParked(1)

	var startA int64
	r.step("start-A-claims-and-keeps-failing", func() {
		fail := fake.SpawnResult{Err: errors.New("tester: the pod cannot be resumed")}
		r.rt.ScriptResume(fail, fail, fail, fail, fail, fail, fail, fail, fail)
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "start A queued", func() bool { return r.startRow(T) != 0 })
		startA = r.startRow(T)
		testwait.Eventually(t, "start A failed 3 times", func() bool { return r.rowAttempts(startA) >= 3 })
	})

	r.step("park-and-readmit-so-start-B-runs-first", func() {
		r.publishStatus(T, "backlog")
		testwait.Eventually(t, "T parked", func() bool { return r.issue(T).Phase == phase.Done })
		resumes := len(r.rt.CallsOf("Resume"))
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "B's relaunch of the architect", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
		r.ready(T, "ses_arch_"+T, false)
		testwait.Eventually(t, "the ready's catch-up published", func() bool {
			for _, p := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(tok)) {
				if strings.Contains(p.Payload, `"catch-up"`) {
					return true
				}
			}
			return false
		})
	})

	r.step("A-finishes-stale-and-hands-over", func() {
		testwait.Eventually(t, "start A finished", func() bool { return r.rowGone(startA) })
		released := time.Now()
		for range 60 {
			if r.namings(tok, ref) > 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		r.note("reached the architect %s after A's release", time.Since(released).Round(100*time.Millisecond))
		time.Sleep(liveSettle)
		named := r.namings(tok, ref)
		if strings.Contains(r.lastTask(T), ref) {
			named++
		}
		r.note("after A finished: kept=%d; messages naming %s=%d", len(r.kept(T)), ref, named)
		if named != 1 {
			t.Fatalf("told %d times; want once", named)
		}
	})
}

// TestTesterRootHearsMainPathsLive: each way main gives a root's architect work, with RootHears
// wired, and a parked root's held start task sent by its next start; a foreign turn on the held
// task's claim does not consume it.
func TestTesterRootHearsMainPathsLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 4
	r.cfg.Linger = 10 * time.Minute
	pr := r.cfg.Project
	W, P, F, H := pr+"-3", pr+"-1", pr+"-7", pr+"-8"
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	r.board.show(W, "in_progress")
	r.board.show(P, "backlog")
	r.board.show(H, "backlog")
	r.write("fixture", func(tx pgx.Tx) error {
		w := r.rootIssue(W, phase.AwaitingPerson, "in_progress")
		w.Awaiting = &record.Awaiting{Ask: "ask-w", Since: record.EventMark{Seq: 5}}
		p := r.rootIssue(P, phase.Done, "backlog")
		p.LingerUntil = &past
		h := r.rootIssue(H, phase.Done, "backlog")
		h.LingerUntil = &past
		for _, i := range []record.Issue{w, p, h} {
			if err := r.records.PutIssue(ctx, tx, i); err != nil {
				return err
			}
		}
		return r.records.PutRootAsk(ctx, tx, record.RootAsk{Issue: W, Ask: "ask-w", Kind: record.AskQuestion, Open: true, Seq: 5,
			WaitingOn: "human", WaitingOnSeq: 5, ByTree: true})
	})
	for _, k := range []string{W, P, H} {
		r.suspendedArchitect(k)
	}
	r.boot()

	r.step("awaiting-person-root-answered-is-prompted-its-resume-once", func() {
		resumes := len(r.rt.CallsOf("Resume"))
		e := r.askEvent(W, "ask.answered", personActor("alice"), map[string]any{"id": "ask-w", "kind": "question"})
		r.waitProcessedDispatch(e)
		testwait.Eventually(t, "W's architect resumed", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
		sh := r.ready(W, "ses_arch_"+W, false)
		got := r.prompts(sh, 6*time.Second)
		r.note("W prompts=%d", len(got))
		if len(got) != 1 || !strings.Contains(got[0], "ask-w") {
			t.Fatalf("W prompts = %q, want its resume task once", got)
		}
		sh.conn.Close()
	})

	r.step("re-admission-is-prompted-its-start-task-once", func() {
		e := r.askEvent(P, "ask.answered", personActor("bob"), map[string]any{"id": "ask-p", "kind": "question"})
		r.waitProcessedDispatch(e)
		resumes := len(r.rt.CallsOf("Resume"))
		r.publishStatus(P, "todo")
		testwait.Eventually(t, "P's architect resumed", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
		sh := r.ready(P, "ses_arch_"+P, false)
		got := r.prompts(sh, 6*time.Second)
		r.note("P prompts=%d", len(got))
		if len(got) != 1 || !strings.Contains(got[0], "ask-p") {
			t.Fatalf("P prompts = %q, want its start task naming ask-p once", got)
		}
		sh.conn.Close()
	})

	r.step("fresh-admission-launches-with-no-task", func() {
		spawns := len(r.rt.CallsOf("Spawn"))
		r.publishStatus(F, "todo")
		testwait.Eventually(t, "F's architect spawned", func() bool { return len(r.rt.CallsOf("Spawn")) > spawns })
		sh := r.ready(F, "ses_arch_"+F, false)
		got := r.prompts(sh, 4*time.Second)
		r.note("F prompts=%d", len(got))
		if len(got) != 0 {
			t.Fatalf("F prompts = %q, want none", got)
		}
		sh.conn.Close()
	})

	r.step("a-parked-roots-held-task-survives-a-foreign-turn-and-is-sent-by-its-next-start", func() {
		e := r.askEvent(H, "ask.answered", personActor("carol"), map[string]any{"id": "ask-h", "kind": "question"})
		r.waitProcessedDispatch(e)
		resumes := len(r.rt.CallsOf("Resume"))
		r.publishStatus(H, "todo")
		testwait.Eventually(t, "H's start task queued", func() bool { return strings.Contains(r.lastTask(H), "ask-h") })
		testwait.Eventually(t, "H's architect resumed", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
		r.publishStatus(H, "backlog")
		testwait.Eventually(t, "H parked", func() bool { return r.issue(H).Phase == phase.Done })
		sh := r.ready(H, "ses_arch_"+H, false)
		// A turn the daemon never asked for, at once, while the claim is live.
		sh.send(shimwire.AgentStart{})
		sh.send(shimwire.AgentEnd{})
		got := r.prompts(sh, 5*time.Second)
		r.note("while parked: H architect=%s prompts=%d kept=%d pending=%d", r.issue(H).Architect.State, len(got), len(r.kept(H)), r.pendingDeliveries(H))
		if len(got) != 0 || r.pendingDeliveries(H) != 1 {
			t.Fatalf("while parked: prompts %q, pending %d; want the task held, unsent", got, r.pendingDeliveries(H))
		}
		sh.conn.Close()
		resumes = len(r.rt.CallsOf("Resume"))
		r.publishStatus(H, "todo")
		testwait.Eventually(t, "H's architect resumed again", func() bool { return len(r.rt.CallsOf("Resume")) > resumes })
		sh = r.ready(H, "ses_arch_"+H, true)
		got = r.prompts(sh, 6*time.Second)
		r.note("after re-admission: H prompts=%d; pending=%d", len(got), r.pendingDeliveries(H))
		if len(got) != 1 || !strings.Contains(got[0], "ask-h") {
			t.Fatalf("after re-admission H prompts = %q, want the held task naming ask-h once", got)
		}
	})
}

// TestTesterClosingTreeLogsOwedDecisionsLive: a parked tree with a decision kept for it lingers out;
// its close logs the decision it still owes, by ref, once.
func TestTesterClosingTreeLogsOwedDecisionsLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 2
	r.cfg.Linger = 6 * time.Second
	T := r.P
	ctx := context.Background()
	r.board.show(T, "in_progress")
	r.write("tree", func(tx pgx.Tx) error {
		i := r.rootIssue(T, phase.Planning, "in_progress")
		if err := r.records.PutIssue(ctx, tx, i); err != nil {
			return err
		}
		return r.admit(ctx, tx, T)
	})
	r.suspendedArchitect(T)
	r.boot()
	ref := "dispatch://" + T + "/ask/ask-owed"
	r.step("park-and-decide", func() {
		r.publishStatus(T, "backlog")
		testwait.Eventually(t, "T lingers", func() bool { return r.issue(T).Phase == phase.Done })
		e := r.askEvent(T, "ask.answered", personActor("alice"), map[string]any{"id": "ask-owed", "kind": "question"})
		r.waitProcessedDispatch(e)
		r.waitParked(1)
		r.note("kept=%d", len(r.kept(T)))
	})
	r.step("linger-expires-and-the-close-names-the-owed-decision", func() {
		path := os.Getenv("TESTER_JSON_LOG")
		var lines []string
		for range 60 {
			lines = r.logLines(path, ref)
			if len(lines) > 0 {
				break
			}
			time.Sleep(time.Second)
		}
		time.Sleep(liveSettle)
		lines = r.logLines(path, ref)
		for _, l := range lines {
			r.note("log: %.400s", l)
		}
		owed := 0
		for _, l := range lines {
			if strings.Contains(l, `"level":"WARN"`) {
				owed++
			}
		}
		if owed != 1 {
			t.Fatalf("WARN lines naming %s = %d, want 1", ref, owed)
		}
	})
}

func personActor(id string) map[string]string  { return map[string]string{"kind": "user", "id": id} }
func sessionActor(id string) map[string]string { return map[string]string{"kind": "session", "id": id} }

func (r *parkedLive) logLines(path, needle string) []string {
	data, _ := os.ReadFile(path)
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, needle) && !strings.Contains(l, `"level":"DEBUG"`) && !strings.Contains(l, "goes to the controller") {
			out = append(out, l)
		}
	}
	return out
}

func (r *parkedLive) namings(tok claim.Token, ref string) int {
	n := 0
	for _, p := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(tok)) {
		if strings.Contains(p.Payload, ref) {
			n++
		}
	}
	return n
}

func (r *parkedLive) token(key string) claim.Token {
	tok, err := claim.NewToken(r.project, key, claim.RoleArchitect)
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

func (r *parkedLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(context.Background(), r.st.Pool(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

func (r *parkedLive) rootIssue(key string, ph phase.Phase, status string) record.Issue {
	return record.Issue{Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: key, Project: r.cfg.Project,
		Title: key, Phase: ph, Generation: 1, Status: status, DispatchStatus: status, Rank: "A", LastDispatchSeq: 5, HandedOver: true}
}

func (r *parkedLive) parkedRoot(T string) {
	past := time.Now().Add(-time.Hour)
	r.board.show(T, "backlog")
	r.write("tree", func(tx pgx.Tx) error {
		i := r.rootIssue(T, phase.Done, "backlog")
		i.LingerUntil = &past
		return r.records.PutIssue(context.Background(), tx, i)
	})
	r.suspendedArchitect(T)
}

func (r *parkedLive) suspendedArchitect(k string) {
	ctx := context.Background()
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, k, treelifecycle.AuthorityWorkflow); err != nil {
		r.t.Fatalf("open lifecycle %s: %v", k, err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{Token: r.token(k), Project: r.project, Tree: k, Issue: k, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_arch_" + k,
		SessionFile: "/sessions/arch-" + k + ".jsonl"}); err != nil {
		r.t.Fatalf("seed architect claim %s: %v", k, err)
	}
}

func (r *parkedLive) seed() {
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	r.board.show(r.P, "backlog")
	r.board.place(r.PC, "backlog", r.P, false)
	r.board.show(r.W, "in_progress")
	r.board.show(r.S, "in_progress")
	r.board.show(r.Q, "todo")
	r.board.show(r.R, "in_progress")
	r.write("fixture", func(tx pgx.Tx) error {
		p := r.rootIssue(r.P, phase.Done, "backlog")
		p.LingerUntil = &past
		pc := r.rootIssue(r.PC, phase.Done, "backlog")
		pc.Tree, pc.Parent, pc.HandedOver = r.P, &r.P, false
		w := r.rootIssue(r.W, phase.AwaitingPerson, "in_progress")
		w.Awaiting = &record.Awaiting{Ask: "ask-w", Since: record.EventMark{Seq: 5}}
		s := r.rootIssue(r.S, phase.Planning, "in_progress")
		q := r.rootIssue(r.Q, phase.Admitted, "todo")
		q.Generation = 2
		rr := r.rootIssue(r.R, phase.Done, "in_progress")
		rr.LingerUntil = &past
		for _, i := range []record.Issue{p, pc, w, s, q, rr} {
			if err := r.records.PutIssue(ctx, tx, i); err != nil {
				return err
			}
		}
		for _, g := range []record.DesignGate{{Issue: r.P, ArtifactID: "artifact-p", LatestVersion: 2}, {Issue: r.W, ArtifactID: "artifact-w", LatestVersion: 1},
			{Issue: r.R, ArtifactID: "artifact-r", LatestVersion: 2}} {
			if err := r.records.PutGate(ctx, tx, g); err != nil {
				return err
			}
		}
		if err := r.records.PutRootAsk(ctx, tx, record.RootAsk{Issue: r.W, Ask: "ask-w", Kind: record.AskQuestion, Open: true, Seq: 5,
			WaitingOn: "human", WaitingOnSeq: 5, ByTree: true}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.S)
	})
	for _, k := range []string{r.P, r.W, r.R} {
		r.suspendedArchitect(k)
	}
}

func (r *parkedLive) boot() {
	r.t.Helper()
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	if r.github == "" {
		r.github = childAdoptionGitHub(r.t)
	}
	o.githubAPI = r.github
	logger := r.log()
	if path := os.Getenv("TESTER_JSON_LOG"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			r.t.Fatal(err)
		}
		r.t.Cleanup(func() { file.Close() })
		logger = slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	r.d = startDaemonWithin(r.t, r.cfg, o, logger, 60*time.Second)
}

func (r *parkedLive) restart(admissionCap int) {
	r.t.Helper()
	if r.d != nil {
		r.d.stop()
	}
	rebindHeldPorts(r.t, &r.cfg)
	r.rt, r.built = fake.NewRuntime(), &built{}
	r.cfg.AdmissionCap = admissionCap
	r.boot()
}

func (r *parkedLive) ready(key, session string, redial bool) *shim {
	r.t.Helper()
	tok := r.token(key)
	launch := lastLaunch(r.t, r.rt, tok)
	var sh *shim
	if redial {
		sh = dialShimUntilAcked(r.t, r.built.address, launch.BootToken)
	} else {
		sh = dialShim(r.t, r.built.address, launch.BootToken)
	}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{BootToken: launch.BootToken, SessionID: session,
		OmpSessionFile: "/sessions/" + session + ".jsonl", AgentID: session, PluginContract: api.DaemonAPIVersion}, false)
	if status != http.StatusOK {
		r.t.Fatalf("register = %d %s", status, body)
	}
	var reg claim.RegisterResponse
	_ = json.Unmarshal(body, &reg)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{ClaimToken: tok, SessionID: session, Secret: reg.Secret,
		Generation: launch.Generation}, false); status != http.StatusNoContent {
		r.t.Fatalf("ready = %d %s", status, body)
	}
	return sh
}

// prompts reads what the daemon sends sh for window, answering a negotiation and taking every
// prompt (answered success, then a whole turn), and returns the prompts' messages.
func (r *parkedLive) prompts(sh *shim, window time.Duration) []string {
	r.t.Helper()
	var got []string
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		_ = sh.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		line, err := sh.lines.ReadBytes('\n')
		if err != nil {
			continue
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			r.t.Fatalf("decode %q: %v", line, err)
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			sh.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.Prompt:
			got = append(got, f.Message)
			sh.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			sh.send(shimwire.AgentStart{})
			sh.send(shimwire.AgentEnd{})
			sh.send(shimwire.PromptResult{ID: f.ID, AgentInvoked: true})
		}
	}
	return got
}

func (r *parkedLive) askEvent(key, typ string, actor map[string]string, payload map[string]any) string {
	r.t.Helper()
	r.seq[key]++
	seq := 100 + r.seq[key]
	raw, err := json.Marshal(map[string]any{
		"id": seq*10 + int64(len(key)), "issue_key": key, "project": r.cfg.Project, "seq": seq, "type": typ, "actor": actor, "notify": true,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": payload,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	subject := "notifications.dispatch.issue." + key + "." + typ
	id := r.publish(subject, "dispatch", fmt.Sprintf("%s-%s-%d", key, typ, seq), string(raw))
	r.raw[id] = [2]string{subject, string(raw)}
	return id
}

func (r *parkedLive) publishStatus(key, status string) {
	r.t.Helper()
	r.board.show(key, status)
	r.seq[key]++
	seq := 100 + r.seq[key]
	raw, _ := json.Marshal(map[string]any{
		"id": seq*10 + 5, "issue_key": key, "project": r.cfg.Project, "seq": seq, "type": "issue.updated", "actor": personActor("alice"), "notify": true,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{"key": key, "status": status, "title": key, "parent": nil, "rank": "A", "labels": liveDispatchLabels(true),
			"last_seq": seq, "project": r.cfg.Project, "resolved_repo": liveRepo},
	})
	id := r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, seq), string(raw))
	r.waitProcessedDispatch(id)
	r.note("a person set %s to %s", key, status)
}

func (r *parkedLive) republish(id string) {
	r.t.Helper()
	m := r.raw[id]
	envelope, _ := json.Marshal(map[string]any{
		"event_id": id, "source": "dispatch", "source_event_id": id, "topic": m[0], "dedupe_key": id,
		"issued_at": time.Now().UnixMilli(), "payload_summary": m[0], "payload": m[1], "trace_id": id,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.js.Publish(ctx, m[0], envelope); err != nil {
		r.t.Fatal(err)
	}
}

func (r *parkedLive) waitProcessedDispatch(id string) {
	r.t.Helper()
	testwait.Eventually(r.t, "processed dispatch event "+id, func() bool {
		return r.scalar("select count(*) from processed_events where event_id = $1", id) > 0
	})
}

func (r *parkedLive) parked() []listenerPublish {
	var out []listenerPublish
	for _, p := range r.notices.acceptedOn(notify.ControllerTopic(r.project)) {
		var k struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal([]byte(p.Payload), &k)
		if k.Kind == "parked-decision" {
			out = append(out, p)
		}
	}
	return out
}

func (r *parkedLive) waitParked(n int) []listenerPublish {
	r.t.Helper()
	var got []listenerPublish
	testwait.Eventually(r.t, fmt.Sprintf("%d parked-decision notices", n), func() bool {
		got = r.parked()
		return len(got) >= n
	})
	if len(got) != n {
		r.t.Fatalf("parked-decision notices = %d, want %d: %+v", len(got), n, got)
	}
	return got
}

func (r *parkedLive) settleCount(n int) {
	r.t.Helper()
	time.Sleep(liveSettle)
	if got := r.parked(); len(got) != n {
		r.t.Fatalf("after settling, parked-decision notices = %d, want %d: %+v", len(got), n, got)
	}
}

func (r *parkedLive) want(p listenerPublish, decision, root, ref, actor, issue string) {
	r.t.Helper()
	var n struct{ Kind, Decision, Root, Ref, Actor string }
	if err := json.Unmarshal([]byte(p.Payload), &n); err != nil {
		r.t.Fatal(err)
	}
	if n.Decision != decision || n.Root != root || n.Ref != ref || n.Actor != actor || p.Message != "parked-decision on "+issue {
		r.t.Fatalf("notice %q payload %s, want decision %s root %s ref %s actor %s on %s", p.Message, p.Payload, decision, root, ref, actor, issue)
	}
}

func (r *parkedLive) kept(root string) []record.ParkedDecision {
	r.t.Helper()
	var out []record.ParkedDecision
	r.write("read kept", func(tx pgx.Tx) error {
		var err error
		out, err = r.records.ParkedDecisions(context.Background(), tx, root)
		return err
	})
	return out
}

// unheld is what tree is owed that no delivery holds yet: the rows a task has not taken.
func (r *parkedLive) unheld(tree string) []record.ParkedDecision {
	r.t.Helper()
	var out []record.ParkedDecision
	for _, d := range r.kept(tree) {
		if d.HeldBy.Claim == "" {
			out = append(out, d)
		}
	}
	return out
}

func (r *parkedLive) scalar(query string, args ...any) int64 {
	r.t.Helper()
	var v int64
	if err := r.st.Pool().QueryRow(context.Background(), query, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return v
}

func (r *parkedLive) lastTask(key string) string {
	var task string
	_ = r.st.Pool().QueryRow(context.Background(), "select task from pending_task_deliveries where claim_token = $1 order by queued_at desc limit 1", string(r.token(key))).Scan(&task)
	return task
}

func (r *parkedLive) startRow(key string) int64 {
	return r.scalar(`select coalesce(max(id), 0) from outbox where kind = 'supervise' and issue = $1 and payload->>'op' = 'start'`, key)
}

func (r *parkedLive) delivered(key string, row int64) bool {
	return r.scalar("select count(*) from claims where token = $1 and $2 = any(delivered_rows)", string(r.token(key)), row) > 0
}

func (r *parkedLive) pendingDeliveries(key string) int64 {
	return r.scalar("select count(*) from pending_task_deliveries where claim_token = $1", string(r.token(key)))
}

func (r *parkedLive) rowAttempts(id int64) int64 {
	return r.scalar("select coalesce(max(attempts), 0) from outbox where id = $1", id)
}

func (r *parkedLive) rowGone(id int64) bool {
	return r.scalar("select count(*) from outbox where id = $1", id) == 0
}

func (r *parkedLive) rowError(id int64) string {
	var e string
	_ = r.st.Pool().QueryRow(context.Background(), "select last_error from outbox where id = $1", id).Scan(&e)
	return e
}

func (r *parkedLive) addTrigger(name, root string) {
	r.t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		fmt.Sprintf(`create or replace function %s() returns trigger language plpgsql as $$ begin if old.issue = '%s' then raise exception 'tester: the tell is refused'; end if; return old; end $$`, name, root),
		fmt.Sprintf(`create trigger %s before delete on parked_decisions for each row execute function %s()`, name, name),
	} {
		if _, err := r.st.Pool().Exec(ctx, q); err != nil {
			r.t.Fatalf("install the trigger: %v", err)
		}
	}
}

func (r *parkedLive) dropTrigger(name string) {
	ctx := context.Background()
	_, _ = r.st.Pool().Exec(ctx, fmt.Sprintf("drop trigger if exists %s on parked_decisions", name))
	_, _ = r.st.Pool().Exec(ctx, fmt.Sprintf("drop function if exists %s()", name))
}

// TestTesterOrphanHandOverLive: a parked tree whose child is mid-phase lingers out; a person
// decided on the child while it was parked; the linger hands the child over into a tree of its
// own; the child's new architect's start task names that decision.
func TestTesterOrphanHandOverLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	r.cfg.Linger = 6 * time.Second
	T, C := r.P, r.PC
	ctx := context.Background()
	r.board.show(T, "in_progress")
	r.board.place(C, "in_progress", T, false)
	r.write("tree", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, r.rootIssue(T, phase.Planning, "in_progress")); err != nil {
			return err
		}
		c := r.rootIssue(C, phase.Implementing, "in_progress")
		c.Tree, c.Parent, c.HandedOver = T, &T, false
		if err := r.records.PutIssue(ctx, tx, c); err != nil {
			return err
		}
		return r.admit(ctx, tx, T)
	})
	r.suspendedArchitect(T)
	r.boot()
	childRef, rootRef := "dispatch://"+C+"/ask/ask-c", "dispatch://"+T+"/ask/ask-t"

	r.step("park-and-decide-on-child-and-root", func() {
		r.publishStatus(T, "done")
		testwait.Eventually(t, "T lingers", func() bool { return r.issue(T).Phase == phase.Done })
		e1 := r.askEvent(C, "ask.answered", personActor("alice"), map[string]any{"id": "ask-c", "kind": "question"})
		e2 := r.askEvent(T, "ask.answered", personActor("bob"), map[string]any{"id": "ask-t", "kind": "question"})
		r.waitProcessedDispatch(e1)
		r.waitProcessedDispatch(e2)
		r.waitParked(2)
		r.note("kept under %s: %d", T, len(r.kept(T)))
	})

	r.step("linger-ends-and-hands-the-child-over", func() {
		testwait.Eventually(t, "the daemon wrote the child todo", func() bool { return r.board.wrote(C, "todo") })
		r.board.place(C, "todo", T, true)
		r.seq[C]++
		seq := 100 + r.seq[C]
		raw, _ := json.Marshal(map[string]any{
			"id": seq*10 + 8, "issue_key": C, "project": r.cfg.Project, "seq": seq, "type": "issue.updated",
			"actor": map[string]string{"kind": "session", "id": dispatch.DaemonSession(C)}, "notify": true,
			"created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]any{"key": C, "status": "todo", "title": C, "parent": T, "rank": "B", "labels": liveDispatchLabels(true),
				"last_seq": seq, "project": r.cfg.Project, "resolved_repo": liveRepo},
		})
		id := r.publish("notifications.dispatch.issue."+C+".issue.updated", "dispatch", fmt.Sprintf("%s-handover-%d", C, seq), string(raw))
		r.waitProcessedDispatch(id)
		testwait.Eventually(t, "the child in a tree of its own", func() bool {
			var tree string
			_ = r.st.Pool().QueryRow(ctx, "select tree from issues where key = $1", C).Scan(&tree)
			return tree == C
		})
		testwait.Eventually(t, "the child's architect task queued", func() bool { return r.lastTask(C) != "" })
		task := r.lastTask(C)
		r.note("child's architect task: %.300s", task)
		r.note("kept under old root %s: %d, under child: %d", T, len(r.kept(T)), len(r.kept(C)))
		if !strings.Contains(task, childRef) {
			t.Fatalf("the child's architect task does not name %s", childRef)
		}
		if strings.Contains(task, rootRef) {
			t.Fatalf("the child's architect task names the old root's decision %s", rootRef)
		}
	})
}

// TestTesterHeldTaskHandOverLive: a re-admitted root's start task names a person's decision on its
// child and one on the root, and is held on the architect's claim because the root is closed done
// before the architect comes up; the linger hands the child over. The child's architect is told the
// child's decision once; when the old root runs again its architect is told only its own.
func TestTesterHeldTaskHandOverLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	r.cfg.Linger = 6 * time.Second
	T, C := r.P, r.PC
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	r.board.show(T, "backlog")
	r.board.place(C, "in_progress", T, false)
	r.write("tree", func(tx pgx.Tx) error {
		root := r.rootIssue(T, phase.Done, "backlog")
		root.LingerUntil = &past
		if err := r.records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		c := r.rootIssue(C, phase.Implementing, "in_progress")
		c.Tree, c.Parent, c.HandedOver = T, &T, false
		return r.records.PutIssue(ctx, tx, c)
	})
	r.suspendedArchitect(T)
	r.boot()
	childRef, rootRef := "dispatch://"+C+"/ask/ask-c", "dispatch://"+T+"/ask/ask-t"

	r.step("decisions-on-the-parked-child-and-root", func() {
		e1 := r.askEvent(C, "ask.answered", personActor("alice"), map[string]any{"id": "ask-c", "kind": "question"})
		e2 := r.askEvent(T, "ask.answered", personActor("bob"), map[string]any{"id": "ask-t", "kind": "question"})
		r.waitProcessedDispatch(e1)
		r.waitProcessedDispatch(e2)
		r.waitParked(2)
	})

	r.step("readmission-queues-the-task-then-the-root-closes-before-its-architect-comes-up", func() {
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "T's start task queued naming both", func() bool {
			task := r.lastTask(T)
			return strings.Contains(task, childRef) && strings.Contains(task, rootRef)
		})
		testwait.Eventually(t, "T's kept rows held by the task", func() bool { return len(r.kept(T)) == 2 && len(r.unheld(T)) == 0 })
		r.publishStatus(T, "done")
		testwait.Eventually(t, "T lingers", func() bool { return r.issue(T).Phase == phase.Done })
		r.note("held: pending=%d task names child=%t root=%t", r.pendingDeliveries(T), strings.Contains(r.lastTask(T), childRef), strings.Contains(r.lastTask(T), rootRef))
	})

	r.step("linger-hands-the-child-over-and-its-architect-is-told-the-childs-decision", func() {
		testwait.Eventually(t, "the daemon wrote the child todo", func() bool { return r.board.wrote(C, "todo") })
		r.board.place(C, "todo", T, true)
		r.seq[C]++
		seq := 100 + r.seq[C]
		raw, _ := json.Marshal(map[string]any{
			"id": seq*10 + 8, "issue_key": C, "project": r.cfg.Project, "seq": seq, "type": "issue.updated",
			"actor": map[string]string{"kind": "session", "id": dispatch.DaemonSession(C)}, "notify": true,
			"created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"payload": map[string]any{"key": C, "status": "todo", "title": C, "parent": T, "rank": "B", "labels": liveDispatchLabels(true),
				"last_seq": seq, "project": r.cfg.Project, "resolved_repo": liveRepo},
		})
		id := r.publish("notifications.dispatch.issue."+C+".issue.updated", "dispatch", fmt.Sprintf("%s-handover-%d", C, seq), string(raw))
		r.waitProcessedDispatch(id)
		// The child's implementer, launched by the old tree's re-admission, comes up so the old tree's
		// suspend of it can run and the new tree's starts stop waiting for it.
		impl, err := claim.NewToken(r.project, C, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		launch := lastLaunch(t, r.rt, impl)
		ish := dialShim(t, r.built.address, launch.BootToken)
		status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{BootToken: launch.BootToken, SessionID: "ses_impl",
			OmpSessionFile: "/sessions/ses_impl.jsonl", AgentID: "ses_impl", PluginContract: api.DaemonAPIVersion}, false)
		if status != http.StatusOK {
			t.Fatalf("register the implementer = %d %s", status, body)
		}
		var reg claim.RegisterResponse
		_ = json.Unmarshal(body, &reg)
		if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{ClaimToken: impl, SessionID: "ses_impl", Secret: reg.Secret,
			Generation: launch.Generation}, false); status != http.StatusNoContent {
			t.Fatalf("ready the implementer = %d %s", status, body)
		}
		go r.prompts(ish, 20*time.Second)
		testwait.Eventually(t, "the child's architect task queued", func() bool { return r.lastTask(C) != "" })
		task := r.lastTask(C)
		r.note("child's architect task: %.240s", task)
		if !strings.Contains(task, childRef) || strings.Contains(task, rootRef) {
			t.Fatalf("the child's architect task %q, want it to name %s and not %s", task, childRef, rootRef)
		}
		owed := r.logLines(os.Getenv("TESTER_JSON_LOG"), childRef)
		warns := 0
		for _, l := range owed {
			if strings.Contains(l, `"level":"WARN"`) {
				warns++
				r.note("WARN naming the child's decision: %.300s", l)
			}
		}
		for _, l := range r.logLines(os.Getenv("TESTER_JSON_LOG"), rootRef) {
			if strings.Contains(l, `"level":"WARN"`) {
				r.note("WARN naming the root's decision: %.300s", l)
			}
		}
		if warns != 0 {
			t.Fatalf("the close warned %d times about %s, which it handed to the child", warns, childRef)
		}
	})

	r.step("the-old-root-runs-again-and-its-architect-is-told-only-its-own", func() {
		resumes := len(r.rt.CallsOf("Resume"))
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "T's architect relaunched", func() bool {
			a := r.issue(T).Architect
			return len(r.rt.CallsOf("Resume")) > resumes && a != nil && a.State == string(supervise.StateLaunching)
		})
		sh := r.ready(T, "ses_arch_"+T, true)
		got := r.prompts(sh, 6*time.Second)
		joined := strings.Join(got, " || ")
		r.note("T prompts=%d: %.300s", len(got), joined)
		for _, p := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(T))) {
			joined += p.Payload
		}
		if strings.Count(joined, rootRef) != 1 || strings.Contains(joined, childRef) {
			t.Fatalf("the old root's architect was told %s %d times and %s %d times; want 1 and 0",
				rootRef, strings.Count(joined, rootRef), childRef, strings.Count(joined, childRef))
		}
	})
}

// publishIssue publishes Dispatch's issue.updated of key at status under parent, blocked by
// blockers, labelled when handed, by a person or (daemon) as the echo of the daemon's own write.
func (r *parkedLive) publishIssue(key, status, parent string, handed, daemon bool, blockers ...string) {
	r.t.Helper()
	r.board.place(key, status, parent, handed)
	r.seq[key]++
	seq := 100 + r.seq[key]
	actor := personActor("alice")
	if daemon {
		actor = map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}
	}
	var p any
	if parent != "" {
		p = parent
	}
	if blockers == nil {
		blockers = []string{}
	}
	r.blockersMu.Lock()
	r.blockers[key] = blockers
	r.blockersMu.Unlock()
	raw, _ := json.Marshal(map[string]any{
		"id": seq*10 + 7, "issue_key": key, "project": r.cfg.Project, "seq": seq, "type": "issue.updated", "actor": actor, "notify": true,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{"key": key, "status": status, "title": key, "parent": p, "rank": "B", "labels": liveDispatchLabels(handed),
			"last_seq": seq, "project": r.cfg.Project, "resolved_repo": liveRepo, "blocked_by": blockers},
	})
	id := r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-ev-%s-%d", key, status, seq), string(raw))
	r.waitProcessedDispatch(id)
	r.note("%s set %s to %s (parent %q, blocked by %v, labelled %t)", map[bool]string{true: "the daemon's echo", false: "a person"}[daemon], key, status, parent, blockers, handed)
}

func (r *parkedLive) treeOf(key string) string {
	var tree string
	_ = r.st.Pool().QueryRow(context.Background(), "select tree from issues where key = $1", key).Scan(&tree)
	return tree
}

// hears brings key's architect up on the claim its start launched and takes every prompt for
// window, returning them.
func (r *parkedLive) hears(key string, window time.Duration) []string {
	r.t.Helper()
	testwait.Eventually(r.t, key+"'s architect task queued", func() bool { return r.lastTask(key) != "" })
	sh := r.ready(key, "ses_arch_"+key, true)
	got := r.prompts(sh, window)
	sh.conn.Close()
	return got
}

func told(texts []string, ref string) int {
	n := 0
	for _, s := range texts {
		n += strings.Count(s, ref)
	}
	return n
}

func (r *parkedLive) childTree(T, C string, ph phase.Phase, status string, parent string) record.Issue {
	c := r.rootIssue(C, ph, status)
	c.Tree, c.Parent, c.HandedOver, c.Rank = T, &parent, false, "B"
	return c
}

// TestTesterHandOverWithoutRunLive: a lingering root's child never started, held by an unfinished
// Dispatch issue; a person decided on it; the linger hands it over without its run; once its
// blocker closes, its own architect is told the decision once.
func TestTesterHandOverWithoutRunLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	r.cfg.Linger = 6 * time.Second
	T, C, B := r.P, r.PC, r.W
	ctx := context.Background()
	r.board.show(T, "in_progress")
	r.board.place(B, "in_progress", "", false)
	r.write("tree", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, r.rootIssue(T, phase.Planning, "in_progress")); err != nil {
			return err
		}
		return r.admit(ctx, tx, T)
	})
	r.suspendedArchitect(T)
	r.boot()
	ref := "dispatch://" + C + "/ask/ask-c"

	r.step("a-child-held-by-an-unfinished-issue", func() {
		r.publishIssue(B, "in_progress", "", false, false)
		r.publishIssue(C, "todo", T, false, false, B)
		testwait.Eventually(t, "the child recorded in T's tree", func() bool { return r.treeOf(C) == T })
		r.note("child: phase=%s tree=%s", r.issue(C).Phase, r.treeOf(C))
	})
	r.step("root-closed-and-a-person-decides-on-the-child", func() {
		r.publishStatus(T, "done")
		testwait.Eventually(t, "T lingers", func() bool { return r.issue(T).Phase == phase.Done })
		e := r.askEvent(C, "ask.answered", personActor("alice"), map[string]any{"id": "ask-c", "kind": "question"})
		r.waitProcessedDispatch(e)
		r.waitParked(1)
	})
	r.step("linger-hands-it-over-without-its-run-and-it-waits-on-its-blocker", func() {
		testwait.Eventually(t, "the daemon wrote the child todo", func() bool { return r.board.wrote(C, "todo") })
		r.publishIssue(C, "todo", T, true, true, B)
		testwait.Eventually(t, "the child in a tree of its own", func() bool { return r.treeOf(C) == C })
		time.Sleep(liveSettle)
		r.note("child: phase=%s; architect task %q; owed to it: %d", r.issue(C).Phase, r.lastTask(C), len(r.kept(C)))
		if r.lastTask(C) != "" {
			t.Fatalf("the child started while its blocker is open")
		}
	})
	r.step("blocker-closes-and-the-childs-architect-is-told-once", func() {
		r.publishIssue(B, "done", "", false, false)
		got := r.hears(C, 6*time.Second)
		r.note("child's architect prompts: %q; kept after: %d", got, len(r.kept(C)))
		if told(got, ref) != 1 {
			t.Fatalf("the child's architect was told %s %d times, want once", ref, told(got, ref))
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(C)) == 0 })
	})
}

// TestTesterOrphanedParkedChildLive: a closed tree's parked child; a person decides on it; a person
// sets it todo, so it is an orphan root of its own; its architect is told the decision once.
func TestTesterOrphanedParkedChildLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	T, C := r.P, r.PC
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	r.board.show(T, "backlog")
	r.board.place(C, "backlog", T, false)
	r.write("parked tree", func(tx pgx.Tx) error {
		root := r.rootIssue(T, phase.Done, "backlog")
		root.LingerUntil = &past
		if err := r.records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return r.records.PutIssue(ctx, tx, r.childTree(T, C, phase.Done, "backlog", T))
	})
	r.boot()
	ref := "dispatch://" + C + "/ask/ask-c"
	r.step("a-person-decides-on-the-parked-child", func() {
		e := r.askEvent(C, "ask.answered", personActor("alice"), map[string]any{"id": "ask-c", "kind": "question"})
		r.waitProcessedDispatch(e)
		n := r.waitParked(1)
		r.note("controller notice: %.200s", n[0].Payload)
	})
	r.step("a-person-sets-it-todo-and-its-own-architect-is-told-once", func() {
		r.publishIssue(C, "todo", T, true, false)
		testwait.Eventually(t, "the orphan in a tree of its own", func() bool { return r.treeOf(C) == C })
		got := r.hears(C, 6*time.Second)
		r.note("orphan's architect prompts: %q", got)
		if told(got, ref) != 1 {
			t.Fatalf("the orphan's architect was told %s %d times, want once", ref, told(got, ref))
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(C)) == 0 })
	})
}

// TestTesterGrandchildHandOverLive: a lingering root's child and grandchild, both mid-phase, each
// with a person's decision; the linger hands both over and their echoes arrive in either order;
// each architect is told its own decision once and nothing of the other's.
func TestTesterGrandchildHandOverLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 6
	r.cfg.Linger = 6 * time.Second
	pr := r.cfg.Project
	trees := []struct{ T, C, G string }{{pr + "-1", pr + "-2", pr + "-3"}, {pr + "-4", pr + "-5", pr + "-6"}}
	ctx := context.Background()
	for _, x := range trees {
		r.board.show(x.T, "in_progress")
		r.board.place(x.C, "in_progress", x.T, false)
		r.board.place(x.G, "in_progress", x.C, false)
	}
	r.write("two trees", func(tx pgx.Tx) error {
		for _, x := range trees {
			if err := r.records.PutIssue(ctx, tx, r.rootIssue(x.T, phase.Planning, "in_progress")); err != nil {
				return err
			}
			if err := r.records.PutIssue(ctx, tx, r.childTree(x.T, x.C, phase.Implementing, "in_progress", x.T)); err != nil {
				return err
			}
			if err := r.records.PutIssue(ctx, tx, r.childTree(x.T, x.G, phase.Implementing, "in_progress", x.C)); err != nil {
				return err
			}
			if err := r.admit(ctx, tx, x.T); err != nil {
				return err
			}
		}
		return nil
	})
	for _, x := range trees {
		r.suspendedArchitect(x.T)
	}
	r.boot()
	r.step("both-roots-closed-and-a-person-decides-on-each-child-and-grandchild", func() {
		for _, x := range trees {
			r.publishStatus(x.T, "done")
		}
		n := 0
		for _, x := range trees {
			for _, k := range []string{x.C, x.G} {
				e := r.askEvent(k, "ask.answered", personActor("alice"), map[string]any{"id": "ask-" + k, "kind": "question"})
				r.waitProcessedDispatch(e)
				n++
			}
		}
		r.waitParked(n)
	})
	for i, x := range trees {
		order := []string{x.C, x.G}
		if i == 1 {
			order = []string{x.G, x.C}
		}
		r.step(fmt.Sprintf("hand-over-echoes-in-order-%s", strings.Join(order, "-then-")), func() {
			for _, k := range order {
				testwait.Eventually(t, "the daemon wrote "+k+" todo", func() bool { return r.board.wrote(k, "todo") })
			}
			for _, k := range order {
				parent := x.T
				if k == x.G {
					parent = x.C
				}
				r.publishIssue(k, "todo", parent, true, true)
				testwait.Eventually(t, k+" in a tree of its own", func() bool { return r.treeOf(k) == k })
			}
			refC, refG := "dispatch://"+x.C+"/ask/ask-"+x.C, "dispatch://"+x.G+"/ask/ask-"+x.G
			for _, k := range order {
				got := r.hears(k, 6*time.Second)
				r.note("%s's architect prompts: %q", k, got)
				own, other := refC, refG
				if k == x.G {
					own, other = refG, refC
				}
				if told(got, own) != 1 || told(got, other) != 0 {
					t.Fatalf("%s's architect was told its own %s %d times and %s %d times; want once and never", k, own, told(got, own), other, told(got, other))
				}
			}
			testwait.Eventually(t, "every decision told", func() bool { return len(r.kept(x.C)) == 0 && len(r.kept(x.G)) == 0 })
		})
	}
}

// TestTesterAdoptedQueuedRootLive: a labelled root waits for its slot (the cap is full) and keeps a
// person's decision, telling no controller; a person makes it a child of the live tree; the
// running architect of that tree is told the decision once, by the catch-up the adoption writes.
func TestTesterAdoptedQueuedRootLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 1
	L, Q := r.P, r.Q
	ctx := context.Background()
	r.board.show(L, "in_progress")
	r.board.show(Q, "todo")
	r.write("live tree and a queued root", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, r.rootIssue(L, phase.Planning, "in_progress")); err != nil {
			return err
		}
		q := r.rootIssue(Q, phase.Admitted, "todo")
		q.Rank = "B"
		if err := r.records.PutIssue(ctx, tx, q); err != nil {
			return err
		}
		return r.admit(ctx, tx, L)
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, L, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{Token: r.token(L), Project: r.project, Tree: L, Issue: L, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t))}); err != nil {
		t.Fatal(err)
	}
	writePrompt(t, r.cfg, r.token(L))
	r.boot()
	ref := "dispatch://" + Q + "/ask/ask-q"
	catchUpsNaming := func() int {
		n := 0
		for _, p := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(L))) {
			var n0 struct {
				CatchUp *struct {
					Decided string `json:"decided"`
				} `json:"catch_up"`
			}
			_ = json.Unmarshal([]byte(p.Payload), &n0)
			if n0.CatchUp != nil {
				n += strings.Count(n0.CatchUp.Decided, ref)
			} else {
				n += strings.Count(p.Payload, ref)
			}
		}
		return n
	}
	var sh *shim
	r.step("the-live-trees-architect-runs", func() {
		testwait.Eventually(t, "L's architect launched", func() bool { return len(r.rt.CallsOf("Spawn"))+len(r.rt.CallsOf("Resume")) > 0 })
		sh = r.ready(L, "ses_arch_"+L, false)
		go r.prompts(sh, 30*time.Second)
		testwait.Eventually(t, "L's architect ready", func() bool {
			a := r.issue(L).Architect
			return a != nil && (a.State == string(supervise.StateReady) || a.State == string(supervise.StateWorking) || a.State == string(supervise.StateIdle))
		})
		r.note("L's architect: %s; Q waiting=%v", r.issue(L).Architect.State, r.issue(Q).Phase)
	})
	r.step("a-person-decides-on-the-queued-root-kept-not-relayed", func() {
		e := r.askEvent(Q, "ask.answered", personActor("alice"), map[string]any{"id": "ask-q", "kind": "question"})
		r.waitProcessedDispatch(e)
		testwait.Eventually(t, "Q keeps the decision", func() bool { return len(r.kept(Q)) == 1 })
		time.Sleep(liveSettle)
		if n := len(r.parked()); n != 0 {
			t.Fatalf("the controller was told %d parked-decisions for a root waiting for its slot", n)
		}
	})
	r.step("a-person-makes-it-a-child-of-the-live-tree-and-its-architect-is-told-once", func() {
		r.publishIssue(Q, "todo", L, true, false)
		testwait.Eventually(t, "Q adopted into L's tree", func() bool { return r.treeOf(Q) == L })
		testwait.Eventually(t, "a catch-up to L's architect names the decision", func() bool { return catchUpsNaming() > 0 })
		time.Sleep(liveSettle)
		r.note("notices to L's architect naming %s: %d; owed to L: %d", ref, catchUpsNaming(), len(r.kept(L)))
		for _, p := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(L))) {
			if strings.Contains(p.Payload, ref) {
				r.note("notice naming it: %s", p.Payload)
			}
		}
		if catchUpsNaming() != 1 {
			t.Fatalf("L's architect was told %s %d times, want once", ref, catchUpsNaming())
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(L)) == 0 })
	})
}

// nextPrompt reads frames until a prompt arrives (answering a negotiation), or window ends.
func (r *parkedLive) nextPrompt(sh *shim, window time.Duration) (shimwire.Prompt, bool) {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		_ = sh.conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		line, err := sh.lines.ReadBytes('\n')
		if err != nil {
			continue
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			r.t.Fatalf("decode %q: %v", line, err)
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			sh.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.Prompt:
			return f, true
		}
	}
	return shimwire.Prompt{}, false
}

// TestTesterTakenBackConfirmationLive: a re-admitted root's task names a person's decision. (a) A
// turn of the agent's own starts while the prompt is in flight, then the prompt is refused busy:
// the decision is told on the resend. (b) The same foreign turn, then the prompt accepted: told
// once, no resend. (c) The agent dies mid-turn after the task's turn started: the decision is
// named again on the relaunch, never lost.
func TestTesterTakenBackConfirmationLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 2
	r.cfg.Linger = 10 * time.Minute
	T := r.P
	r.parkedRoot(T)
	r.boot()
	parked := 0
	cycle := func(id string) (string, *shim) {
		ref := "dispatch://" + T + "/ask/" + id
		e := r.askEvent(T, "ask.answered", personActor("alice"), map[string]any{"id": id, "kind": "question"})
		r.waitProcessedDispatch(e)
		parked++
		r.waitParked(parked)
		launches := len(r.rt.CallsOf("Resume")) + len(r.rt.CallsOf("Spawn"))
		r.publishStatus(T, "todo")
		testwait.Eventually(t, "T's architect relaunched", func() bool {
			a := r.issue(T).Architect
			return len(r.rt.CallsOf("Resume"))+len(r.rt.CallsOf("Spawn")) > launches && a != nil && a.State == string(supervise.StateLaunching)
		})
		return ref, r.ready(T, "ses_arch_"+T, true)
	}
	park := func() {
		r.publishStatus(T, "backlog")
		testwait.Eventually(t, "T parked", func() bool { return r.issue(T).Phase == phase.Done })
	}

	r.step("a-foreign-turn-confirms-then-the-prompt-is-refused-busy", func() {
		ref, sh := cycle("ask-a")
		p, ok := r.nextPrompt(sh, 6*time.Second)
		if !ok || !strings.Contains(p.Message, ref) {
			t.Fatalf("no prompt naming %s: %q", ref, p.Message)
		}
		sh.send(shimwire.AgentStart{})
		testwait.Eventually(t, "the foreign turn confirmed the task", func() bool { return len(r.unheld(T)) == 0 && len(r.kept(T)) == 0 })
		r.note("after the foreign turn's start: kept=%d", len(r.kept(T)))
		sh.send(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is already processing"})
		time.Sleep(time.Second)
		r.note("after the busy refusal: kept=%d unheld=%d pending=%d", len(r.kept(T)), len(r.unheld(T)), r.pendingDeliveries(T))
		sh.send(shimwire.AgentEnd{})
		got := r.prompts(sh, 8*time.Second)
		r.note("prompts after the foreign turn ended: %d, naming %s: %d", len(got), ref, told(got, ref))
		if told(got, ref) < 1 {
			t.Fatalf("the decision %s was not sent again after the confirmation was taken back: %q (kept=%d)", ref, got, len(r.kept(T)))
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(T)) == 0 })
		sh.conn.Close()
		park()
	})

	r.step("a-foreign-turn-confirms-then-the-prompt-is-accepted", func() {
		ref, sh := cycle("ask-b")
		p, ok := r.nextPrompt(sh, 6*time.Second)
		if !ok || !strings.Contains(p.Message, ref) {
			t.Fatalf("no prompt naming %s: %q", ref, p.Message)
		}
		sh.send(shimwire.AgentStart{})
		time.Sleep(500 * time.Millisecond)
		sh.send(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: true})
		time.Sleep(500 * time.Millisecond)
		sh.send(shimwire.AgentEnd{})
		time.Sleep(time.Second)
		pendingBefore := r.pendingDeliveries(T)
		sh.send(shimwire.PromptResult{ID: p.ID, AgentInvoked: true})
		testwait.Eventually(t, "the prompt's result retired the delivery", func() bool { return r.pendingDeliveries(T) == 0 })
		r.note("after the turn's end, before the prompt's result: pending=%d; after the result: pending=%d", pendingBefore, r.pendingDeliveries(T))
		if pendingBefore != 1 {
			t.Fatalf("the delivery was retired at the turn's end with its prompt's result still outstanding (pending=%d)", pendingBefore)
		}
		got := r.prompts(sh, 6*time.Second)
		r.note("prompts after: %d naming %s: %d; kept=%d", len(got), ref, told(got, ref), len(r.kept(T)))
		if told(got, ref) != 0 {
			t.Fatalf("%s was named again with no failed write or taken-back confirmation behind it: %q", ref, got)
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(T)) == 0 })
		sh.conn.Close()
		park()
	})

	r.step("a-background-turn-ends-before-the-tasks-prompt-result-and-retires-nothing", func() {
		ref, sh := cycle("ask-d")
		p, ok := r.nextPrompt(sh, 6*time.Second)
		if !ok || !strings.Contains(p.Message, ref) {
			t.Fatalf("no prompt naming %s: %q", ref, p.Message)
		}
		sh.send(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: true})
		sh.send(shimwire.AgentStart{})
		testwait.Eventually(t, "the task's turn confirmed it", func() bool { return len(r.kept(T)) == 0 })
		sh.send(shimwire.AgentEnd{})
		// A background result starts and ends a turn of its own while the task's result is still to come.
		sh.send(shimwire.AgentStart{})
		sh.send(shimwire.AgentEnd{})
		time.Sleep(time.Second)
		before := r.pendingDeliveries(T)
		sh.send(shimwire.PromptResult{ID: p.ID, AgentInvoked: true})
		testwait.Eventually(t, "the result retired the delivery", func() bool { return r.pendingDeliveries(T) == 0 })
		got := r.prompts(sh, 4*time.Second)
		r.note("pending before the result=%d; prompts after=%d naming %s: %d; kept=%d", before, len(got), ref, told(got, ref), len(r.kept(T)))
		if before != 1 || told(got, ref) != 0 || len(r.kept(T)) != 0 {
			t.Fatalf("want the delivery kept until its result, then retired, the decision told once: before=%d retold=%d kept=%d", before, told(got, ref), len(r.kept(T)))
		}
		sh.conn.Close()
		park()
	})

	r.step("the-agent-dies-mid-turn-and-the-decision-is-named-again-not-lost", func() {
		ref, sh := cycle("ask-c")
		p, ok := r.nextPrompt(sh, 6*time.Second)
		if !ok || !strings.Contains(p.Message, ref) {
			t.Fatalf("no prompt naming %s: %q", ref, p.Message)
		}
		sh.send(shimwire.Response{ID: p.ID, Command: shimwire.TypePrompt, Success: true})
		sh.send(shimwire.AgentStart{})
		testwait.Eventually(t, "the turn confirmed", func() bool { return len(r.kept(T)) == 0 })
		launches := len(r.rt.CallsOf("Resume")) + len(r.rt.CallsOf("Spawn"))
		loc := r.d.claim(r.token(T)).Locator
		sh.conn.Close()
		r.rt.Emit(runtime.Observation{Locator: *loc, Kind: runtime.Gone, Detail: "tester: the pane died mid-turn"})
		testwait.Eventually(t, "T's architect relaunched after the death", func() bool {
			a := r.issue(T).Architect
			return len(r.rt.CallsOf("Resume"))+len(r.rt.CallsOf("Spawn")) > launches && a != nil && a.State == string(supervise.StateLaunching)
		})
		r.note("after the death: kept=%d unheld=%d pending=%d", len(r.kept(T)), len(r.unheld(T)), r.pendingDeliveries(T))
		sh2 := r.ready(T, "ses_arch_"+T, true)
		got := r.prompts(sh2, 8*time.Second)
		r.note("prompts after the relaunch: %d naming %s: %d; kept=%d", len(got), ref, told(got, ref), len(r.kept(T)))
		if told(got, ref) < 1 && len(r.kept(T)) == 0 {
			// Not named again and nothing kept: was it confirmed for good (the death after the turn
			// started does not take it back)? Then it was told once, which is fine.
			r.note("the death did not take the confirmation back; told once")
		}
		testwait.Eventually(t, "the decision told", func() bool { return len(r.kept(T)) == 0 })
		sh2.conn.Close()
	})
}

// TestTesterApprovalHandoverLive: main's #2619 beside this change's relay. A person's approval of a
// parked labelled root's primary document is queued by the daemon as their handover and tells the
// controller nothing; an unlabelled parked root's approval, an approval of a non-primary document,
// a child's approval, and a person's ask answer, resolution and agent-turn reply on a parked tree
// each reach the controller once; a session's approval of the labelled root queues nothing.
func TestTesterApprovalHandoverLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	pr := r.cfg.Project
	L, U, N, T, C, S := pr+"-1", pr+"-3", pr+"-4", pr+"-5", pr+"-6", pr+"-7"
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	for _, k := range []string{L, N, T, S} {
		r.board.show(k, "backlog")
	}
	r.board.place(U, "backlog", "", false)
	r.board.place(C, "backlog", T, false)
	r.blockersMu.Lock()
	r.artifacts["artifact-l"] = [2]any{L, 1}
	r.artifacts["artifact-u"] = [2]any{U, 1}
	r.artifacts["artifact-n-other"] = [2]any{N, 1}
	r.artifacts["artifact-c"] = [2]any{C, 1}
	r.artifacts["artifact-s"] = [2]any{S, 1}
	r.primaries[L], r.primaries[U], r.primaries[N], r.primaries[C], r.primaries[S] = "artifact-l", "artifact-u", "artifact-n", "artifact-c", "artifact-s"
	r.blockersMu.Unlock()
	r.write("parked roots", func(tx pgx.Tx) error {
		for _, k := range []string{L, U, N, T, S} {
			i := r.rootIssue(k, phase.Done, "backlog")
			i.LingerUntil = &past
			i.HandedOver = k != U
			if err := r.records.PutIssue(ctx, tx, i); err != nil {
				return err
			}
		}
		c := r.childTree(T, C, phase.Done, "backlog", T)
		if err := r.records.PutIssue(ctx, tx, c); err != nil {
			return err
		}
		for _, g := range []record.DesignGate{{Issue: L, ArtifactID: "artifact-l", LatestVersion: 1}, {Issue: U, ArtifactID: "artifact-u", LatestVersion: 1},
			{Issue: N, ArtifactID: "artifact-n-other", LatestVersion: 1}, {Issue: C, ArtifactID: "artifact-c", LatestVersion: 1}, {Issue: S, ArtifactID: "artifact-s", LatestVersion: 1}} {
			if err := r.records.PutGate(ctx, tx, g); err != nil {
				return err
			}
		}
		return nil
	})
	for _, k := range []string{L, U, N, T, S} {
		r.suspendedArchitect(k)
	}
	r.boot()
	notices := func(ref string) int {
		n := 0
		for _, p := range r.parked() {
			if strings.Contains(p.Payload, ref) {
				n++
			}
		}
		return n
	}
	approve := func(key, artifact string, actor map[string]string) string {
		e := r.askEvent(key, "artifact.approved", actor, map[string]any{"artifact_id": artifact, "version": 1})
		r.waitProcessedDispatch(e)
		return e
	}

	r.step("a-sessions-approval-of-the-labelled-root-queues-nothing", func() {
		approve(S, "artifact-s", sessionActor("ses-outside"))
		time.Sleep(liveSettle)
		r.note("todo written for %s: %t; notices naming its spec: %d", S, r.board.wrote(S, "todo"), notices("dispatch://"+S+"/spec"))
		if r.board.wrote(S, "todo") || notices("dispatch://"+S+"/spec") != 0 {
			t.Fatalf("a session's approval queued %s or told the controller", S)
		}
	})
	r.step("a-persons-approval-of-a-parked-labelled-roots-primary-document-is-queued-and-tells-no-controller", func() {
		approve(L, "artifact-l", personActor("alice"))
		testwait.Eventually(t, "the daemon wrote "+L+" todo", func() bool { return r.board.wrote(L, "todo") })
		time.Sleep(liveSettle)
		r.note("todo written for %s: true; parked-decision notices naming its spec: %d", L, notices("dispatch://"+L+"/spec"))
		if n := notices("dispatch://" + L + "/spec"); n != 0 {
			t.Fatalf("the controller was told %d parked-decision notices for %s's approval, which the daemon queued as the person's handover", n, L)
		}
	})
	r.step("the-daemons-todo-echo-re-admits-the-root", func() {
		r.publishIssue(L, "todo", "", true, true)
		testwait.Eventually(t, L+" re-admitted", func() bool { p := r.issue(L).Phase; return p != "" && p != phase.Done })
		r.note("%s after its handover's echo: phase %s", L, r.issue(L).Phase)
	})
	r.step("the-same-approval-again-queues-and-tells-nothing-more", func() {
		writes := 0
		for _, w := range r.board.writes() {
			if w.Issue == L && w.Status == "todo" {
				writes++
			}
		}
		approve(L, "artifact-l", personActor("alice"))
		time.Sleep(liveSettle)
		after := 0
		for _, w := range r.board.writes() {
			if w.Issue == L && w.Status == "todo" {
				after++
			}
		}
		r.note("todo writes for %s before %d, after the second approval %d; notices %d", L, writes, after, notices("dispatch://"+L+"/spec"))
		if notices("dispatch://"+L+"/spec") != 0 {
			t.Fatalf("the second approval told the controller")
		}
	})
	r.step("an-unlabelled-parked-roots-approval-reaches-the-controller-once", func() {
		approve(U, "artifact-u", personActor("bob"))
		testwait.Eventually(t, "the notice for "+U, func() bool { return notices("dispatch://"+U+"/spec") >= 1 })
		time.Sleep(liveSettle)
		r.note("notices naming %s's spec: %d; todo written: %t", U, notices("dispatch://"+U+"/spec"), r.board.wrote(U, "todo"))
		if notices("dispatch://"+U+"/spec") != 1 || r.board.wrote(U, "todo") {
			t.Fatalf("want one notice and no todo for the unlabelled root")
		}
	})
	r.step("an-approval-of-a-non-primary-document-reaches-the-controller-once", func() {
		approve(N, "artifact-n-other", personActor("bob"))
		testwait.Eventually(t, "the notice for "+N, func() bool { return notices("dispatch://"+N+"/spec") >= 1 })
		time.Sleep(liveSettle)
		r.note("notices naming %s's spec: %d; todo written: %t", N, notices("dispatch://"+N+"/spec"), r.board.wrote(N, "todo"))
		if notices("dispatch://"+N+"/spec") != 1 || r.board.wrote(N, "todo") {
			t.Fatalf("want one notice and no todo for the non-primary document")
		}
	})
	r.step("a-childs-approval-reaches-the-controller-once", func() {
		approve(C, "artifact-c", personActor("bob"))
		testwait.Eventually(t, "the notice for "+C, func() bool { return notices("dispatch://"+C+"/spec") >= 1 })
		time.Sleep(liveSettle)
		r.note("notices naming %s's spec: %d; todo written: %t", C, notices("dispatch://"+C+"/spec"), r.board.wrote(C, "todo"))
		if notices("dispatch://"+C+"/spec") != 1 || r.board.wrote(C, "todo") || r.board.wrote(T, "todo") {
			t.Fatalf("want one notice and no todo for the child's approval")
		}
	})
	r.step("a-persons-answer-resolution-and-agent-turn-reply-on-the-parked-tree-each-reach-the-controller-once", func() {
		e1 := r.askEvent(T, "ask.answered", personActor("alice"), map[string]any{"id": "ask-a", "kind": "question"})
		e2 := r.askEvent(C, "ask.resolved", personActor("alice"), map[string]any{"id": "ask-r", "kind": "question"})
		e3 := r.askEvent(T, "comment.created", personActor("alice"), map[string]any{"id": "cmt-1", "ask_id": "ask-y", "ask_state": "open", "ask_waiting_on": "agent"})
		for _, e := range []string{e1, e2, e3} {
			r.waitProcessedDispatch(e)
		}
		refs := []string{"dispatch://" + T + "/ask/ask-a", "dispatch://" + C + "/ask/ask-r", "dispatch://" + T + "/comment/cmt-1"}
		testwait.Eventually(t, "three notices", func() bool { return notices(refs[0]) >= 1 && notices(refs[1]) >= 1 && notices(refs[2]) >= 1 })
		time.Sleep(liveSettle)
		r.note("notices: answer %d, resolution %d, agent-turn reply %d; total parked-decision notices %d", notices(refs[0]), notices(refs[1]), notices(refs[2]), len(r.parked()))
		for _, ref := range refs {
			if notices(ref) != 1 {
				t.Fatalf("%s told the controller %d times, want once", ref, notices(ref))
			}
		}
	})
}

// startTasks is the task text of every architect start row of key in the outbox, oldest first.
func (r *parkedLive) startTasks(key string) []string {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select coalesce(payload->>'task', '') from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = 'architect' order by id`, key)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestTesterHandoverApprovalNoStalePairLive: review 5728675512's Important. A parked labelled root
// whose gate held approved=1 below latest=2: a person approves version 2, the daemon queues the
// handover, and the re-admission's start names no stale approval pair. A person's approval of the
// old version 1 still queues the handover and the start does name the pair, since version 2 is
// unapproved.
func TestTesterHandoverApprovalNoStalePairLive(t *testing.T) {
	r := newParkedLive(t)
	r.cfg.AdmissionCap = 3
	pr := r.cfg.Project
	A, B, CA := pr+"-1", pr+"-3", pr+"-4"
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	for _, k := range []string{A, B} {
		r.board.show(k, "backlog")
	}
	r.board.place(CA, "todo", A, false)
	r.blockersMu.Lock()
	r.artifacts["artifact-a"] = [2]any{A, 2}
	r.artifacts["artifact-b"] = [2]any{B, 2}
	r.primaries[A], r.primaries[B] = "artifact-a", "artifact-b"
	r.blockersMu.Unlock()
	one := 1
	r.write("parked roots approved at 1, now at 2", func(tx pgx.Tx) error {
		ca := r.childTree(A, CA, phase.Admitted, "todo", A)
		if err := r.records.PutIssue(ctx, tx, ca); err != nil {
			return err
		}
		for k, art := range map[string]string{A: "artifact-a", B: "artifact-b"} {
			i := r.rootIssue(k, phase.Done, "backlog")
			i.LingerUntil = &past
			if err := r.records.PutIssue(ctx, tx, i); err != nil {
				return err
			}
			if err := r.records.PutGate(ctx, tx, record.DesignGate{Issue: k, ArtifactID: art, LatestVersion: 2, ApprovedVersion: &one}); err != nil {
				return err
			}
		}
		return nil
	})
	for _, k := range []string{A, B} {
		r.suspendedArchitect(k)
	}
	r.boot()
	stalePair := "The spec is approved at version 1 and is now at version 2"
	run := func(key, art string, version int) []string {
		e := r.askEvent(key, "artifact.approved", personActor("alice"), map[string]any{"artifact_id": art, "version": version})
		r.waitProcessedDispatch(e)
		testwait.Eventually(t, "the daemon wrote "+key+" todo", func() bool { return r.board.wrote(key, "todo") })
		var gate struct{ approved, latest int64 }
		_ = r.st.Pool().QueryRow(ctx, `select coalesce(approved_version, 0), latest_version from design_gates where issue = $1`, key).Scan(&gate.approved, &gate.latest)
		r.note("%s's gate after the approval: approved %d, latest %d", key, gate.approved, gate.latest)
		// Before the echo: the approval opened nothing. The root is still phase done with no slot,
		// no supervise start row exists for it or its child, the child is still admitted, and a
		// second approval queues no second todo and tells the controller nothing.
		time.Sleep(liveSettle)
		var starts int64
		_ = r.st.Pool().QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and issue = any($1) and payload->>'op' = 'start'`,
			[]string{key, CA}).Scan(&starts)
		todos := func() int {
			n := 0
			for _, w := range r.board.writes() {
				if w.Issue == key && w.Status == "todo" {
					n++
				}
			}
			return n
		}
		before := todos()
		if os.Getenv("TESTER_NO_SECOND") == "" {
			e2 := r.askEvent(key, "artifact.approved", personActor("alice"), map[string]any{"artifact_id": art, "version": version})
			r.waitProcessedDispatch(e2)
		}
		time.Sleep(liveSettle)
		r.note("before the echo: %s phase %s; supervise starts for it and %s: %d; %s phase %s; todo writes %d, after a second approval %d",
			key, r.issue(key).Phase, CA, starts, CA, r.issue(CA).Phase, before, todos())
		if r.issue(key).Phase != phase.Done || starts != 0 || (key == A && r.issue(CA).Phase != phase.Admitted) || todos() != before {
			t.Fatalf("the approval of a parked root opened something before its re-admission")
		}
		r.publishIssue(key, "todo", "", true, true)
		seen := map[string]bool{}
		sawStart := false
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			for _, task := range r.startTasks(key) {
				sawStart = true
				seen[task] = true
			}
			if task := r.lastTask(key); task != "" {
				seen[task] = true
			}
			time.Sleep(50 * time.Millisecond)
		}
		var tasks []string
		for task := range seen {
			tasks = append(tasks, task)
		}
		r.note("%s: a start row was seen: %t; architect phase now %s", key, sawStart, r.issue(key).Phase)
		var kept int64
		_ = r.st.Pool().QueryRow(ctx, `select count(*) from readmitted_approvals where issue = $1`, key).Scan(&kept)
		notices := 0
		for _, p := range r.parked() {
			if strings.Contains(p.Payload, "dispatch://"+key+"/spec") {
				notices++
			}
		}
		r.note("%s: start tasks %q; readmitted_approvals rows %d; parked-decision notices %d", key, tasks, kept, notices)
		if notices != 0 {
			t.Fatalf("%s's approval told the controller %d times; the daemon queued it as the handover", key, notices)
		}
		return tasks
	}
	r.step("a-persons-approval-of-the-latest-version-re-admits-with-no-stale-pair", func() {
		tasks := run(A, "artifact-a", 2)
		for _, task := range tasks {
			if strings.Contains(task, stalePair) || strings.Contains(task, "request approval of version 2") {
				t.Fatalf("%s's start task %q names the stale pair for a version the person just approved", A, task)
			}
		}
	})
	r.step("a-persons-approval-of-the-older-version-re-admits-naming-the-pair", func() {
		tasks := run(B, "artifact-b", 1)
		if len(tasks) == 0 || !strings.Contains(strings.Join(tasks, " "), stalePair) {
			t.Fatalf("%s's start tasks %q do not name the pair, though version 2 is unapproved", B, tasks)
		}
	})
}
