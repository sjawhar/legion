package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A child Dispatch already shows todo when its architect releases it — set todo before its tree was
// live, by a person or another session, so no todo event of it ever reached the live tree — is
// recorded under the tree and started by the release, as a child the release moves into todo is by
// that move's event. Dispatch makes no event for a status an issue already has, so before, the
// release answered the child released and nothing recorded or started it: read_record found no
// record, and park_child and rerun_child answered ISSUE_NOT_FOUND (LEGION-627). The architect's
// retry of the same wave changes nothing more.
func TestWaveReleaseRecordsAndStartsAChildDispatchAlreadyShowsTodo(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, child := "LEGION-208", "LEGION-209"
	approved := 1
	h.recordIssue(record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:  {Key: root, Status: "in_progress"},
		child: {Key: child, Title: "the child", Status: "todo", Parent: &root, LastSeq: 7},
	}
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	release := func() {
		t.Helper()
		recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
		}
	}

	release()
	recorded := recordedChild(t, h, child)
	if recorded == nil || recorded.Tree != root || recorded.Phase != phase.Planning || recorded.Generation != 1 || recorded.LastDispatchSeq != 7 || recorded.Title != "the child" {
		t.Fatalf("the released todo child's record = %#v, want it recorded in %s's tree at Dispatch's sequence 7 and planning under the open gate", recorded, root)
	}
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the child is already todo", statuses.writes)
	}

	release()
	if again := recordedChild(t, h, child); again == nil || again.Generation != 1 || again.Phase != phase.Planning {
		t.Fatalf("the child after the architect's retry = %#v, want its one run unchanged", again)
	}
	if recorder := h.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": architect.grant(t), "issue": child}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("park_child of the released child = %d: %s", recorder.Code, recorder.Body)
	}
}

func recordedChild(t *testing.T, h *harness, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		got, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return got
}
