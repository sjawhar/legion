package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// statusRoot and statusChild are the live tree and the running child these tests move.
const (
	statusRoot  = "LEGION-208"
	statusChild = "LEGION-209"
)

// statusHarness is the architect's and the controller's status routes over the daemon's own handler
// chain (daemon/workflow.go: the mirror, the engine, admission), with no Dispatch event consumer:
// whatever moves the record moves it inside the route's own request. The tree LEGION-208 is live
// (slotted, its design gate approved) and its child LEGION-209 is in planning at Dispatch event
// sequence 4.
func statusHarness(t *testing.T) (*harness, []intake.Handler, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, quiet)
	handlers := []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine, admit.New(records, engine, 2, testDispatchProject, quiet)}
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: handlers, Dispatch: statuses,
	}).Handler
	root := statusRoot
	h.recordIssue(record.Issue{Key: statusRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot,
		Project: testDispatchProject, Title: statusRoot, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, LastDispatchSeq: 3})
	h.recordIssue(record.Issue{Key: statusChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: "the child", Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "B", LastDispatchSeq: 4})
	approved := 1
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: statusRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: statusRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		statusRoot:  {Key: statusRoot, ResolvedRepo: "acme/widgets", Title: statusRoot, Status: "in_progress", Rank: "A", Labels: []string{"legion"}, HandedOver: true, LastSeq: 3},
		statusChild: {Key: statusChild, ResolvedRepo: "acme/widgets", Title: "the child", Status: "in_progress", Parent: &root, Rank: "B", LastSeq: 4},
	}
	return h, handlers, statuses
}

// statusEcho is the Dispatch event the daemon's own status write of the child makes, as the
// consumer decodes it (intake's decodeDispatchEvent): it arrives after the route has answered,
// behind every event queued before it.
func statusEcho(seq int64, status string) intake.DispatchIssue {
	return intake.DispatchIssue{Key: statusChild, Seq: seq, Type: "issue.updated", Status: status, Title: "the child", Parent: statusRoot, Rank: "B",
		ResolvedRepo: "acme/widgets", DaemonActor: true, ClaimKnown: true}
}

// superviseRows counts the child's queued supervise rows of op for role.
func superviseRows(t *testing.T, h *harness, op string, role claim.Role) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = $2 and payload->>'role' = $3`,
		statusChild, op, string(role)).Scan(&rows); err != nil {
		t.Fatalf("count the %s rows of %s: %v", op, role, err)
	}
	return rows
}

// outboxRows counts every row queued for the child.
func outboxRows(t *testing.T, h *harness) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1`, statusChild).Scan(&rows); err != nil {
		t.Fatalf("count the outbox rows of %s: %v", statusChild, err)
	}
	return rows
}

// assertEchoChangesNothing applies the route's own Dispatch event once the route has answered, as
// the lagging consumer finally delivers it, and holds the child's record and its queued rows to what
// the route left.
func assertEchoChangesNothing(t *testing.T, h *harness, handlers []intake.Handler, echo intake.DispatchIssue) {
	t.Helper()
	before, rows := recordedChild(t, h, statusChild), outboxRows(t, h)
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo", echo, handlers...); err != nil {
		t.Fatalf("apply the echo of the route's own write: %v", err)
	}
	if after := recordedChild(t, h, statusChild); !reflect.DeepEqual(after, before) {
		t.Fatalf("the child after its own write's echo = %#v, want it as the route left it, %#v", after, before)
	}
	if after := outboxRows(t, h); after != rows {
		t.Fatalf("the child's outbox rows after its own write's echo = %d, want the %d the route left", after, rows)
	}
}

// park_child parks the child in the record within its own request, as the backlog move's event
// would, without waiting for that event: a deployment's Dispatch event consumer ran half an hour
// behind, so a child its architect parked was still in its phase in the record, read_record said so,
// and its planner's completion then started the implementer. Here no Dispatch event is delivered:
// the record is parked at the write's own sequence, every claim of the child is suspended, the
// planner's completion starts nothing, and the write's echo, once it arrives, changes nothing.
func TestParkChildParksTheRecordWithoutWaitingForItsDispatchEvent(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)

	recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("park = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: statusChild, status: "backlog"}) {
		t.Fatalf("Dispatch writes = %#v, want the child's backlog", statuses.writes)
	}
	parked := recordedChild(t, h, statusChild)
	if parked == nil || parked.Phase != phase.Done || parked.Status != "backlog" || parked.DispatchStatus != "backlog" || parked.LastDispatchSeq != 5 || parked.Generation != 1 {
		t.Fatalf("the child's record once park_child answered = %#v, want it parked (phase done, status backlog) at its write's sequence 5", parked)
	}
	if got := superviseRows(t, h, "suspend", claim.RolePlanner); got != 1 {
		t.Fatalf("planner suspend rows = %d, want the park to stop the planner once", got)
	}

	// The planner was mid-turn when its architect parked the child, and completes its plan.
	result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "api", "planner-completed", intake.HandoffComplete{
		Generation: 1, Issue: statusChild, Role: claim.RolePlanner, Claim: "planner-claim", Summary: "the plan", Commit: "c0ffee",
	}, handlers...)
	if err != nil {
		t.Fatalf("apply the planner's completion: %v", err)
	}
	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" {
		t.Fatalf("the planner's completion after the park = %+v, want it refused: the child left the workflow", result.Refusal)
	}
	if got := superviseRows(t, h, "start", claim.RoleImplementer); got != 0 {
		t.Fatalf("implementer start rows = %d, want none: a parked child starts no phase", got)
	}

	assertEchoChangesNothing(t, h, handlers, statusEcho(5, "backlog"))
}

// rerun_child starts the child's next run in the record within its own request, as the todo move's
// event would, without waiting for that event; the write's echo, once it arrives, changes nothing.
func TestRerunChildStartsTheNextRunWithoutWaitingForItsDispatchEvent(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park = %d %s", recorder.Code, recorder.Body)
	}
	planners := superviseRows(t, h, "start", claim.RolePlanner)

	recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("rerun = %d %s, want 200 {}", recorder.Code, recorder.Body)
	}
	want := []statusWrite{{issue: statusChild, status: "backlog"}, {issue: statusChild, status: "todo"}}
	if !reflect.DeepEqual(statuses.writes, want) {
		t.Fatalf("Dispatch writes = %#v, want %#v", statuses.writes, want)
	}
	rerun := recordedChild(t, h, statusChild)
	if rerun == nil || rerun.Phase != phase.Planning || rerun.Status != "todo" || rerun.LastDispatchSeq != 6 || rerun.Generation != 2 {
		t.Fatalf("the child's record once rerun_child answered = %#v, want its next run (generation 2) planning at its write's sequence 6", rerun)
	}
	if got := superviseRows(t, h, "start", claim.RolePlanner); got != planners+1 {
		t.Fatalf("planner start rows = %d, want the rerun to start the planner once more than the %d before it", got, planners)
	}

	assertEchoChangesNothing(t, h, handlers, statusEcho(6, "todo"))
}

// The controller's set_status moves the record within its own request too: the controller parking
// a running child stops it at once, and the echo changes nothing.
func TestControllerStatusParksTheRecordWithoutWaitingForItsDispatchEvent(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusChild, Status: "backlog"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: statusChild, status: "backlog"}) {
		t.Fatalf("Dispatch writes = %#v, want the child's backlog", statuses.writes)
	}
	parked := recordedChild(t, h, statusChild)
	if parked == nil || parked.Phase != phase.Done || parked.Status != "backlog" || parked.LastDispatchSeq != 5 {
		t.Fatalf("the child's record once set_status answered = %#v, want it parked at its write's sequence 5", parked)
	}
	if got := superviseRows(t, h, "suspend", claim.RolePlanner); got != 1 {
		t.Fatalf("planner suspend rows = %d, want the move to stop the planner once", got)
	}

	assertEchoChangesNothing(t, h, handlers, statusEcho(5, "backlog"))
}
