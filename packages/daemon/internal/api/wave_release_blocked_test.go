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

// releaseRoot is the live tree every release of these tests belongs to.
const releaseRoot = "LEGION-208"

// releaseHarness is release_children over the daemon's own handler chain (daemon/workflow.go): the
// mirror first, so the one fact that records a released child also records what Dispatch says it
// waits on, and the engine behind it withholds the first phase of a child that waits. The tree is
// live, its design gate approved only when open says so — the gate, not a blocker, is what holds
// every child of a tree whose design is not approved yet.
func releaseHarness(t *testing.T, open bool) (*harness, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	statuses := &statusRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{blockers.NewMirror(records, testProject), engine}, Dispatch: statuses,
	}).Handler
	h.recordIssue(record.Issue{Key: releaseRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: releaseRoot,
		Project: testProject, Title: releaseRoot, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
	gate := record.DesignGate{Issue: releaseRoot, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1}
	if open {
		approved := 1
		gate.ApprovedVersion = &approved
	}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: releaseRoot, Index: 0,
			AdmittedAt: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	statuses.issues = map[string]dispatch.Issue{
		releaseRoot: {Key: releaseRoot, ResolvedRepo: "acme/widgets", Status: "in_progress"},
	}
	return h, statuses
}

// todoChild is a child Dispatch already shows todo, so the release records it from that view and
// the workflow decides there and then whether its first phase starts (enterTodoChild).
func todoChild(key string, waits ...string) dispatch.Issue {
	parent := releaseRoot
	return dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: "todo", Parent: &parent, LastSeq: 7, BlockedBy: waits}
}

// seedMirrorRow writes what Dispatch says of an issue the release's own children wait on, as the
// mirror handler writes one from that issue's event.
func seedMirrorRow(t *testing.T, h *harness, key, status string) {
	t.Helper()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx,
			record.DispatchIssueRow{Key: key, Project: testProject, Status: status, Seq: 1})
	}); err != nil {
		t.Fatalf("seed the mirror row of %s: %v", key, err)
	}
}

// releaseWave posts one accepted release_children and keeps its body, which the answer's arrays
// are read back from unparsed.
func releaseWave(t *testing.T, h *harness, architect liveClaim, issues ...string) (WaveReleaseResponse, string) {
	t.Helper()
	recorder := h.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": architect.grant(t), "issues": issues}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("wave release = %d: %s", recorder.Code, recorder.Body)
	}
	body := recorder.Body.String()
	var response WaveReleaseResponse
	decodeInto(t, recorder, &response)
	return response, body
}

// A release is accepted whatever its children wait on: every one of them is released, and the
// answer says which of them this request actually started and which it held, with the unfinished
// issues each held child waits on. A blocker Dispatch has already finished is no part of the wait.
// Both lists follow the request's own order, so the architect reads them against the wave it sent.
func TestWaveReleaseAnswersWhatItStartedAndWhatItHeld(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	held, started, waiting := "LEGION-209", "LEGION-210", "LEGION-211"
	statuses.issues[held] = todoChild(held, "LEGION-300")
	statuses.issues[started] = todoChild(started)
	statuses.issues[waiting] = todoChild(waiting, "LEGION-301", "LEGION-302")
	seedMirrorRow(t, h, "LEGION-300", "in_progress")
	seedMirrorRow(t, h, "LEGION-301", "done")
	seedMirrorRow(t, h, "LEGION-302", "todo")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, held, started, waiting)

	want := WaveReleaseResponse{
		Released: []string{held, started, waiting},
		Started:  []string{started},
		Held:     []HeldChild{{Issue: held, BlockedBy: []string{"LEGION-300"}}, {Issue: waiting, BlockedBy: []string{"LEGION-302"}}},
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	for key, wantPhase := range map[string]phase.Phase{held: phase.Admitted, started: phase.Planning, waiting: phase.Admitted} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Phase != wantPhase {
			t.Fatalf("the record of %s = %#v, want it at %s", key, recorded, wantPhase)
		}
	}
}

// A child waiting on an issue the daemon has never observed has no mirror row to judge that issue
// by, and an unobserved issue is not a finished one: the release holds the child and names the
// issue it waits on, so the architect sees what the wait is even when the daemon has never heard
// of it. The answer's other lists are arrays, never null, for the plugin's strict reader.
func TestWaveReleaseHoldsAChildOnABlockerTheDaemonHasNeverObserved(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	child := "LEGION-209"
	statuses.issues[child] = todoChild(child, "LEGION-400")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, body := releaseWave(t, h, architect, child)

	want := WaveReleaseResponse{Released: []string{child}, Started: []string{}, Held: []HeldChild{{Issue: child, BlockedBy: []string{"LEGION-400"}}}}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if !strings.Contains(body, `"started":[]`) {
		t.Fatalf("release body = %s, want an empty started array rather than null", body)
	}
	if recorded := recordedChild(t, h, child); recorded == nil || recorded.Phase != phase.Admitted {
		t.Fatalf("the record of %s = %#v, want it left admitted by the blocker the daemon has never seen", child, recorded)
	}
}

// A tree whose design gate is still closed starts none of the children a release names, and that
// is not a wait on another issue: every child is released, none is started, and none is held. The
// gate's own opening starts them (the workflow's advanceAdmittedTree), so calling them started
// here would be a lie and holding them would name a blocker that does not exist.
func TestWaveReleaseUnderAClosedDesignGateHoldsNoChild(t *testing.T) {
	h, statuses := releaseHarness(t, false)
	first, second := "LEGION-209", "LEGION-210"
	statuses.issues[first] = todoChild(first)
	statuses.issues[second] = todoChild(second)
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, body := releaseWave(t, h, architect, first, second)

	want := WaveReleaseResponse{Released: []string{first, second}, Started: []string{}, Held: []HeldChild{}}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if !strings.Contains(body, `"started":[]`) || !strings.Contains(body, `"held":[]`) {
		t.Fatalf("release body = %s, want empty started and held arrays rather than null", body)
	}
	for _, key := range []string{first, second} {
		if recorded := recordedChild(t, h, key); recorded == nil || recorded.Phase != phase.Admitted {
			t.Fatalf("the record of %s = %#v, want it admitted behind the closed gate", key, recorded)
		}
	}
}
