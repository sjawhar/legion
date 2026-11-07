package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
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

// heldChildHarness is the daemon's own handler chain (the mirror, the engine, admission) behind
// the architect's routes, as statusHarness builds it, over the tree this proof needs: the root
// LEGION-208 stands in retro, its own reviewed head accepted, and its child LEGION-209 is the
// labelled issue a person moved to todo there, which the daemon records at admitted and holds
// (record.HeldUnderAccepted). The engine's log is read, which is why the chain is built here
// rather than taken from statusHarness.
func heldChildHarness(t *testing.T) (*harness, []intake.Handler, *statusRecorder, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, log)
	handlers := []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine, admit.New(records, engine, 2, testDispatchProject, log)}
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken, Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: handlers, Dispatch: statuses, Log: log,
	}).Handler
	root := statusRoot
	h.recordIssue(record.Issue{Key: statusRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot,
		Project: testDispatchProject, Title: statusRoot, Phase: phase.Retro, Generation: 1, Status: "retro", DispatchStatus: "retro",
		Rank: "A", HandedOver: true, TakenBack: true, LastDispatchSeq: 3})
	h.recordIssue(record.Issue{Key: statusChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: statusRoot, Parent: &root,
		Project: testDispatchProject, Title: "the held child", Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo",
		Rank: "B", HandedOver: true, LastDispatchSeq: 4})
	approved := 1
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: statusRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: statusRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the accepted tree: %v", err)
	}
	h.openTree(t, testDispatchProject, statusRoot)
	statuses.issues = map[string]dispatch.Issue{
		statusRoot:  {Key: statusRoot, ResolvedRepo: "acme/widgets", Title: statusRoot, Status: "retro", Rank: "A", Labels: []string{"legion"}, HandedOver: true, LastSeq: 3},
		statusChild: {Key: statusChild, ResolvedRepo: "acme/widgets", Title: "the held child", Status: "todo", Parent: &root, Rank: "B", Labels: []string{"legion"}, HandedOver: true, LastSeq: 4},
	}
	return h, handlers, statuses, logs
}

// A root that closes while it is accepted leaves nothing behind on the child it was holding at
// admitted, so the architect's rerun of that child, after the root comes back, runs it.
//
// The close used to record the hand-over it would make at its linger's end as the linger began,
// and the root's re-admission only marked that row cancelled: with no Dispatch write of its own
// behind it, nothing ever deleted it. The next daemon-authored todo on the child — the one
// rerun_child writes in the new run — found the cancelled row and was read as a hand-over event
// of the closed tree arriving late (the workflow's rejectStaleHandover): the route answered 200,
// the child stayed parked, and a write putting Dispatch back to backlog was queued. Nothing is
// written at the linger's start now, and the hand-over is decided at its end from the member's
// own state (the workflow's unfinishedHandOvers).
func TestARerunUnderAReopenedRootIsNotTakenForAStaleHandOver(t *testing.T) {
	h, handlers, _, logs := heldChildHarness(t)
	ctx := context.Background()
	apply := func(id string, fact intake.Fact) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, h.store.Pool(), "dispatch", id, fact, handlers...); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}

	// A person closes the accepted root by hand, and its tree lingers.
	apply("root-closed", intake.DispatchIssue{Key: statusRoot, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.closed",
		Status: "done", Title: statusRoot, Rank: "A", HandedOver: true})
	if root := recordedChild(t, h, statusRoot); root == nil || !root.Lingers() {
		t.Fatalf("the hand-closed root = %#v, want its tree lingering", root)
	}
	// Reported rather than fatal: what a fence left here costs is what the rest of this proof
	// drives, and that is the finding.
	if got := handoverRows(t, h, statusChild); got != 0 {
		t.Errorf("hand-over rows on %s while its tree lingers = %d, want none: the close writes no fence of its own, and a row left here outlives the tree that wrote it",
			statusChild, got)
	}

	// A person reopens the root inside its linger: the tree is Legion's again, at a new generation.
	apply("root-reopened", intake.DispatchIssue{Key: statusRoot, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated",
		Status: "todo", Title: statusRoot, Rank: "A", HandedOver: true})
	root := recordedChild(t, h, statusRoot)
	if root == nil || root.Lingers() || root.Generation != 2 || root.Phase != phase.Admitted {
		t.Fatalf("the reopened root = %#v, want it re-admitted at generation 2, no longer lingering", root)
	}

	// In the new run its architect starts the child over: park_child, then rerun_child.
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("rerun = %d %s, want 200", recorder.Code, recorder.Body)
	}

	child := recordedChild(t, h, statusChild)
	if child == nil || child.Status != "todo" || child.Phase != phase.Admitted || child.Tree != statusRoot {
		t.Errorf("the reran child = %#v, want it todo at admitted in %s's tree", child, statusRoot)
	}
	if got := backlogWrites(t, h, statusChild); got != 0 {
		t.Errorf("backlog writes queued for %s = %d, want none: its rerun is the architect's, not a hand-over of a closed tree arriving late", statusChild, got)
	}
	if strings.Contains(logs.String(), "rejected stale hand-over event") {
		t.Errorf("the rerun of %s was read as a stale hand-over event of its closed tree", statusChild)
	}
}

// handoverRows is how many durable hand-over fences name issue.
func handoverRows(t *testing.T, h *harness, issue string) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(), "select count(*) from handovers where issue = $1", issue).Scan(&rows); err != nil {
		t.Fatalf("count the hand-over rows of %s: %v", issue, err)
	}
	return rows
}

// backlogWrites is how many Dispatch writes putting issue back to backlog are queued.
func backlogWrites(t *testing.T, h *harness, issue string) int {
	t.Helper()
	var rows int
	if err := h.store.Pool().QueryRow(context.Background(),
		`select count(*) from outbox where issue = $1 and kind = 'dispatch_status' and payload->>'status' = 'backlog'`, issue).Scan(&rows); err != nil {
		t.Fatalf("count the backlog writes of %s: %v", issue, err)
	}
	return rows
}
