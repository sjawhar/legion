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

// releaseHarness is release_children over the daemon's own handler chain (daemon/workflow.go,
// releaseHandlers): the mirror first, so the one fact that records a released child also records
// what Dispatch says it waits on, and the engine behind it withholds the first phase of a child that
// waits. The tree is live, its design gate approved only when open says so — the gate, not a
// blocker, is what holds every child of a tree whose design is not approved yet.
func releaseHarness(t *testing.T, open bool) (*harness, *statusRecorder) {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	statuses := &statusRecorder{}
	// The server is given both names of the one project, and they differ: the claims it reads are
	// filed under the claim token, the issues and the mirror under the Dispatch key, and a route
	// that confuses them reads an empty mirror here.
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, DispatchProject: testDispatchProject,
		OperatorToken: testOperatorToken,
		Controller:    h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: releaseHandlers(), Dispatch: statuses,
	}).Handler
	h.recordIssue(record.Issue{Key: releaseRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: releaseRoot,
		Project: testDispatchProject, Title: releaseRoot, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true})
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

// releaseHandlers is the handler chain the release harness's server applies facts with, and the one
// the Dispatch consumer applies a delivered event with: the mirror, then the engine.
func releaseHandlers() []intake.Handler {
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testDispatchProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return []intake.Handler{blockers.NewMirror(records, testDispatchProject), engine}
}

// deliverEcho applies the todo event a release's write made, as the Dispatch consumer delivers it
// later: decoded from the event Dispatch published, with the daemon's own session as its actor, at
// the sequence Dispatch answered the write with, through the same handlers.
func deliverEcho(t *testing.T, h *harness, written dispatch.Issue) {
	t.Helper()
	parent := ""
	if written.Parent != nil {
		parent = *written.Parent
	}
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "echo-"+written.Key, intake.DispatchIssue{
		Key: written.Key, Seq: written.LastSeq, Type: "issue.updated", Status: written.Status, Title: written.Title, Parent: parent,
		DaemonActor: true, ResolvedRepo: written.ResolvedRepo, Blockers: written.BlockedBy, ClaimKnown: true,
	}, releaseHandlers()...); err != nil {
		t.Fatalf("deliver the todo event of %s: %v", written.Key, err)
	}
}

// outboxRows is every outbox row queued on issue, by kind: what the workflow started or told for it.
func outboxRows(t *testing.T, h *harness, issue string) map[string]int {
	t.Helper()
	rows := map[string]int{}
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		found, err := tx.Query(context.Background(), `select kind, count(*) from outbox where issue = $1 group by kind`, issue)
		if err != nil {
			return err
		}
		defer found.Close()
		for found.Next() {
			var kind string
			var count int
			if err := found.Scan(&kind, &count); err != nil {
				return err
			}
			rows[kind] = count
		}
		return found.Err()
	}); err != nil {
		t.Fatalf("read the outbox rows of %s: %v", issue, err)
	}
	return rows
}

// todoChild is a child Dispatch already shows todo, so the release records it from that view and
// the workflow decides there and then whether its first phase starts (enterTodoChild).
func todoChild(key string, waits ...string) dispatch.Issue {
	parent := releaseRoot
	return dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: "todo", Parent: &parent, LastSeq: 7, BlockedBy: waits}
}

// backlogChild is a child Dispatch shows in backlog, so the release writes it todo and records it
// from that write, in the same request (applyOwnStatusWrite).
func backlogChild(key string, waits ...string) dispatch.Issue {
	child := todoChild(key, waits...)
	child.Status = "backlog"
	return child
}

// seedMirrorRow writes what Dispatch says of an issue the release's own children wait on, as the
// mirror handler writes one from that issue's event.
func seedMirrorRow(t *testing.T, h *harness, key, status string) {
	t.Helper()
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx,
			record.DispatchIssueRow{Key: key, Project: testDispatchProject, Status: status, Seq: 1})
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

// A child the release moves out of backlog is recorded by the release itself, from the issue
// Dispatch answered the write with, at that write's sequence: before, it was recorded only when the
// write's todo event came back through the Dispatch consumer, after the answer, so a consumer
// running behind left the child unrecorded and unstarted for as long as it ran behind. The blocked
// one is held at admitted and named with its blocker; the free one is started. The write's todo
// event, when the consumer delivers it, finds the record already at its sequence and changes
// nothing: no second record, no second start.
func TestWaveReleaseRecordsABacklogChildFromItsOwnWrite(t *testing.T) {
	h, statuses := releaseHarness(t, true)
	blocked, free := "LEGION-209", "LEGION-210"
	statuses.issues[blocked] = backlogChild(blocked, "LEGION-300")
	statuses.issues[free] = backlogChild(free)
	seedMirrorRow(t, h, "LEGION-300", "in_progress")
	architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

	response, _ := releaseWave(t, h, architect, blocked, free)

	want := WaveReleaseResponse{
		Released: []string{blocked, free},
		Started:  []string{free},
		Held:     []HeldChild{{Issue: blocked, BlockedBy: []string{"LEGION-300"}}},
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("release answered %#v, want %#v", response, want)
	}
	if wantWrites := []statusWrite{{blocked, "todo"}, {free, "todo"}}; !reflect.DeepEqual(statuses.writes, wantWrites) {
		t.Fatalf("Dispatch writes = %#v, want %#v", statuses.writes, wantWrites)
	}
	wantPhase := map[string]phase.Phase{blocked: phase.Admitted, free: phase.Planning}
	before := map[string]record.Issue{}
	rowsBefore := map[string]map[string]int{}
	for _, key := range []string{blocked, free} {
		recorded := recordedChild(t, h, key)
		if recorded == nil || recorded.Tree != releaseRoot || recorded.Phase != wantPhase[key] || recorded.Generation != 1 || recorded.LastDispatchSeq != 8 || recorded.Status != "todo" {
			t.Fatalf("the record of %s as the release answered = %#v, want it in %s's tree at %s, generation 1, at the write's sequence 8",
				key, recorded, releaseRoot, wantPhase[key])
		}
		before[key], rowsBefore[key] = *recorded, outboxRows(t, h, key)
	}
	if rowsBefore[free][string(record.OutboxKindSupervise)] == 0 {
		t.Fatalf("outbox of %s = %v, want its planner's start queued by the release", free, rowsBefore[free])
	}

	for _, key := range []string{blocked, free} {
		deliverEcho(t, h, statuses.issues[key])
	}
	for _, key := range []string{blocked, free} {
		if after := recordedChild(t, h, key); after == nil || !reflect.DeepEqual(*after, before[key]) {
			t.Fatalf("the record of %s after its todo event = %#v, want it unchanged from %#v", key, after, before[key])
		}
		if rows := outboxRows(t, h, key); !reflect.DeepEqual(rows, rowsBefore[key]) {
			t.Fatalf("outbox of %s after its todo event = %v, want %v: the event started or told nothing more", key, rows, rowsBefore[key])
		}
	}
}

// A parked child — its record at done, Dispatch at backlog — released again is re-entered by the
// release's own write, as its todo event would re-enter it: a new run, held at admitted while it
// waits on an unfinished issue (so not started), or started at planning when it waits on nothing.
// Before, the record stayed parked at done until that event came back through the consumer. The
// event itself, delivered later, changes nothing.
func TestWaveReleaseReentersAParkedChildFromItsOwnWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		waits   []string
		phase   phase.Phase
		started []string
		held    []HeldChild
	}{
		{name: "waiting on an unfinished issue", waits: []string{"LEGION-300"}, phase: phase.Admitted, started: []string{},
			held: []HeldChild{{Issue: "LEGION-210", BlockedBy: []string{"LEGION-300"}}}},
		{name: "waiting on nothing", phase: phase.Planning, started: []string{"LEGION-210"}, held: []HeldChild{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, statuses := releaseHarness(t, true)
			child, parent := "LEGION-210", releaseRoot
			h.recordIssue(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: releaseRoot, Parent: &parent,
				Project: testDispatchProject, Title: child, Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", LastDispatchSeq: 7})
			statuses.issues[child] = backlogChild(child, tc.waits...)
			seedMirrorRow(t, h, "LEGION-300", "in_progress")
			architect := newLiveClaim(t, h, releaseRoot, claim.RoleArchitect)

			response, _ := releaseWave(t, h, architect, child)

			want := WaveReleaseResponse{Released: []string{child}, Started: tc.started, Held: tc.held}
			if !reflect.DeepEqual(response, want) {
				t.Fatalf("release answered %#v, want %#v", response, want)
			}
			recorded := recordedChild(t, h, child)
			if recorded == nil || recorded.Phase != tc.phase || recorded.Generation != 2 || recorded.Status != "todo" || recorded.LastDispatchSeq != 8 {
				t.Fatalf("the record of %s as the release answered = %#v, want its next run, generation 2, at %s, at the write's sequence 8", child, recorded, tc.phase)
			}
			rows := outboxRows(t, h, child)

			deliverEcho(t, h, statuses.issues[child])
			if after := recordedChild(t, h, child); after == nil || !reflect.DeepEqual(*after, *recorded) {
				t.Fatalf("the record of %s after its todo event = %#v, want it unchanged from %#v", child, after, recorded)
			}
			if again := outboxRows(t, h, child); !reflect.DeepEqual(again, rows) {
				t.Fatalf("outbox of %s after its todo event = %v, want %v", child, again, rows)
			}
		})
	}
}

// A release names a child started only for a first phase it began: the record after the release
// runs a phase of a run that had none before it. A run already under way, a child still waiting at
// admitted, and a parked child its todo event has not re-entered yet were started by nothing here;
// a parked child that event did re-enter, as a new run, was.
func TestStartedByReleaseIsAFirstPhaseTheReleaseBegan(t *testing.T) {
	at := func(p phase.Phase, generation uint64) *record.Issue {
		return &record.Issue{Key: "LEGION-209", Phase: p, Generation: generation}
	}
	for _, tc := range []struct {
		name          string
		before, after *record.Issue
		want          bool
	}{
		{"unrecorded and still unrecorded", nil, nil, false},
		{"recorded and started", nil, at(phase.Planning, 1), true},
		{"recorded and held at admitted", nil, at(phase.Admitted, 1), false},
		{"admitted and started", at(phase.Admitted, 1), at(phase.Planning, 1), true},
		{"admitted and still admitted", at(phase.Admitted, 1), at(phase.Admitted, 1), false},
		{"already running", at(phase.Implementing, 1), at(phase.Implementing, 1), false},
		{"parked and not yet re-entered", at(phase.Done, 1), at(phase.Done, 1), false},
		{"parked and re-entered as a new run", at(phase.Done, 1), at(phase.Planning, 2), true},
	} {
		if got := startedByRelease(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: startedByRelease = %v, want %v", tc.name, got, tc.want)
		}
	}
}
