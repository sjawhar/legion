package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, child := "LEGION-208", "LEGION-209"
	approved := 1
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:  {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
		child: {Key: child, ResolvedRepo: "acme/widgets", Title: "the child", Status: "todo", Parent: &root, LastSeq: 7},
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

// A todo child whose repository cannot be read before it is recorded (the daemon's preparation
// reads Dispatch's issue detail, the repository's Apps and its default branch from GitHub) is not
// released: the answer names the read that failed, whatever answered it, and records nothing, so
// the architect's retry of release_children records it. A child held back for a read of the
// repository mapping (intake.RetryLater) is answered the release's backoff, naming the wait.
func TestWaveReleaseNamesTheRepositoryReadThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
		status           int
		failure          error
	}{
		{"a read that failed", "REPOSITORY_READ_FAILED",
			"could not read the repository of LEGION-209: read the repository Dispatch resolves for LEGION-209: Dispatch is down; retry release_children",
			http.StatusBadGateway, errors.New("read the repository Dispatch resolves for LEGION-209: Dispatch is down")},
		{"a child held for a read of the mapping", "CHILD_RELEASE_BACKOFF",
			"LEGION-209: LEGION-209 resolves to zeta/tools, which the repository mapping did not serve when last read; retry after 1s",
			http.StatusConflict, &intake.RetryLater{After: time.Second, Err: errors.New("LEGION-209 resolves to zeta/tools, which the repository mapping did not serve when last read")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateOff}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			statuses := &statusRecorder{}
			h.handler = NewServer("127.0.0.1", 8437, Options{
				Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
				Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
				Handlers: []intake.Handler{engine}, Dispatch: statuses,
				PrepareRepository: func(_ context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error) {
					return issue, tc.failure
				},
			}).Handler
			root, child := "LEGION-208", "LEGION-209"
			h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
			if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
				return records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
			}); err != nil {
				t.Fatalf("seed the live tree: %v", err)
			}
			statuses.issues = map[string]dispatch.Issue{
				root:  {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
				child: {Key: child, ResolvedRepo: "acme/widgets", Title: "the child", Status: "todo", Parent: &root, LastSeq: 7},
			}
			architect := newLiveClaim(t, h, root, claim.RoleArchitect)
			recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil)
			var failure struct{ Code, Error string }
			if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
				t.Fatalf("wave release = %d %q, not a failure body: %v", recorder.Code, recorder.Body, err)
			}
			if recorder.Code != tc.status || failure.Code != tc.code || failure.Error != tc.want {
				t.Fatalf("wave release = %d %s %q, want %d %s %q", recorder.Code, failure.Code, failure.Error, tc.status, tc.code, tc.want)
			}
			if recorded := recordedChild(t, h, child); recorded != nil {
				t.Fatalf("the child's record = %#v after a failed preparation, want none", recorded)
			}
		})
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

// A todo child whose parent the same wave releases cannot be recorded until the parent is: the
// workflow records a child only under a recorded parent, and the parent's own todo event reaches
// the daemon after the release returns. The release answers CHILD_NOT_RECORDED, and the architect's
// retry, once the parent's event has arrived, records the child. Before, the first attempt spent
// the child's fact id, the retry was answered as a duplicate, and the child was never recorded.
func TestWaveReleaseRetryRecordsATodoChildOfAChildTheWaveReleased(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, parent, child := "LEGION-208", "LEGION-209", "LEGION-210"
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:   {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
		parent: {Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "triage", Parent: &root, LastSeq: 2},
		child:  {Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, LastSeq: 5},
	}
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	release := func() *httptest.ResponseRecorder {
		t.Helper()
		return h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{parent, child}}, nil)
	}

	assertFailure(t, release(), http.StatusConflict, "CHILD_NOT_RECORDED")
	if got := recordedChild(t, h, child); got != nil {
		t.Fatalf("the child before its parent is recorded = %#v, want none", got)
	}
	// The parent's todo event, from the release's write, reaches the daemon.
	statuses.issues[parent] = dispatch.Issue{Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "todo", Parent: &root, LastSeq: 3}
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "parent-todo", intake.DispatchIssue{
		Key: parent, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo", Title: parent, Parent: root,
	}, engine); err != nil {
		t.Fatalf("apply the parent's todo event: %v", err)
	}

	if recorder := release(); recorder.Code != http.StatusOK {
		t.Fatalf("the architect's retry = %d: %s", recorder.Code, recorder.Body)
	}
	if got := recordedChild(t, h, child); got == nil || got.Tree != root || got.LastDispatchSeq != 5 {
		t.Fatalf("the child after the retry = %#v, want it recorded in %s's tree at Dispatch's sequence 5", got, root)
	}
	if want := []statusWrite{{issue: parent, status: "todo"}}; len(statuses.writes) != 1 || statuses.writes[0] != want[0] {
		t.Fatalf("Dispatch writes = %#v, want only the parent's release", statuses.writes)
	}
}

// A child whose parent still has not reached the daemon does not make an architect retry forever.
// After the bounded backoff attempts, release_children answers CHILD_RELEASE_STALLED and queues one
// notice to the tree architect.
func TestWaveReleaseBoundsAnUnrecordedChildRetryAndNotifiesTheArchitect(t *testing.T) {
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine}, Dispatch: statuses,
	}).Handler
	root, parent, child := "LEGION-208", "LEGION-209", "LEGION-210"
	h.recordIssue(record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: testProject, Title: root, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		for _, put := range []error{
			records.PutSlot(context.Background(), tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now()}),
			records.PutReleaseRetry(context.Background(), tx, record.ReleaseRetry{Issue: child, Sequence: 5, Attempts: maxChildReleaseAttempts - 1, NextAt: time.Now().Add(-time.Second)}),
		} {
			if put != nil {
				return put
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed retry: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		root:   {Key: root, ResolvedRepo: "acme/widgets", Status: "in_progress"},
		parent: {Key: parent, ResolvedRepo: "acme/widgets", Title: parent, Status: "todo", Parent: &root, LastSeq: 3},
		child:  {Key: child, ResolvedRepo: "acme/widgets", Title: child, Status: "todo", Parent: &parent, LastSeq: 5},
	}
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	assertFailure(t, h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": []string{child}}, nil), http.StatusConflict, "CHILD_RELEASE_STALLED")
	var notices int
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = 'child-release-stalled'`, root).Scan(&notices)
	}); err != nil {
		t.Fatalf("read the stalled notice: %v", err)
	}
	if notices != 1 {
		t.Fatalf("stalled notices = %d, want one", notices)
	}
}
