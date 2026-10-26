package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// adoptionTree is the live tree each release here releases into, and adoptionChild the issue that
// queued in admission as a root of its own before a person made it a child of that tree.
const adoptionTree, adoptionChild = "LEGION-168", "LEGION-214"

// adoptionHarness is the API wired to the daemon's two handlers, the workflow's and admission's,
// with adoptionTree holding an admission slot and its design gate approved, so a todo child that
// enters the tree starts planning at once.
type adoptionHarness struct {
	*harness
	records   record.Store
	engine    *workflow.Engine
	admission *admit.Admission
	statuses  *statusRecorder
	architect liveClaim
}

func newAdoptionHarness(t *testing.T, cap int) *adoptionHarness {
	t.Helper()
	h := newHarness(t)
	records := record.NewStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, DesignGate: config.DesignGateRootIssues, Linger: time.Hour}, log)
	admission := admit.New(records, engine, cap, testProject, log)
	statuses := &statusRecorder{issues: map[string]dispatch.Issue{
		adoptionTree: {Key: adoptionTree, ResolvedRepo: "acme/widgets", Status: "in_progress"},
	}}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine, admission}, Dispatch: statuses,
	}).Handler
	approved := 1
	h.recordIssue(record.Issue{Key: adoptionTree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: adoptionTree, Project: testProject, Title: adoptionTree, Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "A"})
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: adoptionTree, Index: 0, AdmittedAt: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return records.PutGate(context.Background(), tx, record.DesignGate{Issue: adoptionTree, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree: %v", err)
	}
	return &adoptionHarness{harness: h, records: records, engine: engine, admission: admission, statuses: statuses,
		architect: newLiveClaim(t, h, adoptionTree, claim.RoleArchitect)}
}

// apply applies fact through both handlers, as the daemon's intake does.
func (a *adoptionHarness) apply(id string, fact intake.Fact) {
	a.t.Helper()
	if _, err := intake.ApplyFact(context.Background(), a.store.Pool(), "test", id, fact, a.engine, a.admission); err != nil {
		a.t.Fatalf("apply %s: %v", id, err)
	}
}

// observe applies one Dispatch event of key under no parent or parent.
func (a *adoptionHarness) observe(key string, seq int64, status, parent string, handed bool) {
	a.t.Helper()
	a.apply(fmt.Sprintf("%s-%d-%s", key, seq, status), intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated",
		Status: status, Title: key, Parent: parent, Rank: "B", HandedOver: handed})
}

// shows makes Dispatch show key at status under parent, at sequence seq.
func (a *adoptionHarness) shows(key, status, parent string, seq int64, handed bool) {
	a.statuses.issues[key] = dispatch.Issue{Key: key, ResolvedRepo: "acme/widgets", Title: key, Status: status, Parent: &parent, Rank: "B", HandedOver: handed, LastSeq: seq}
}

func (a *adoptionHarness) release(keys ...string) (int, Failure) {
	a.t.Helper()
	recorder := a.request(http.MethodPost, "/legion/v1/waves/release", map[string]any{"grantId": a.architect.grant(a.t), "issues": keys}, nil)
	var failure Failure
	if recorder.Code != http.StatusOK {
		decodeInto(a.t, recorder, &failure)
	}
	return recorder.Code, failure
}

func (a *adoptionHarness) assertParked(key string) {
	a.t.Helper()
	if recorder := a.request(http.MethodPost, "/legion/v1/children/park", map[string]any{"grantId": a.architect.grant(a.t), "issue": key}, nil); recorder.Code != http.StatusOK {
		a.t.Fatalf("park_child of %s by the architect of %s = %d: %s", key, adoptionTree, recorder.Code, recorder.Body)
	}
}

func (a *adoptionHarness) record(key string) record.Issue {
	a.t.Helper()
	got := recordedChild(a.t, a.harness, key)
	if got == nil {
		a.t.Fatalf("%s is not recorded", key)
	}
	return *got
}

// count is the number of rows query counts.
func (a *adoptionHarness) count(query string, args ...any) int {
	a.t.Helper()
	var n int
	if err := a.store.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		a.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// plannerStarts is the number of planner starts queued for key in tree at generation.
func (a *adoptionHarness) plannerStarts(key, tree string, generation uint64) int {
	a.t.Helper()
	return a.count(`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = $2 and payload->>'tree' = $3 and (payload->>'generation')::bigint = $4`, key, string(claim.RolePlanner), tree, int64(generation))
}

func (a *adoptionHarness) slotted(key string) bool {
	a.t.Helper()
	return a.count(`select count(*) from slots where issue = $1`, key) > 0
}

// waiting is the keys of admission's waiting line.
func (a *adoptionHarness) waiting() []string {
	a.t.Helper()
	var keys []string
	if err := a.store.Tx(context.Background(), func(tx pgx.Tx) error {
		issues, err := a.records.Issues(context.Background(), tx)
		if err != nil {
			return err
		}
		slots, err := a.records.Slots(context.Background(), tx)
		if err != nil {
			return err
		}
		for _, issue := range record.Waiting(issues, slots) {
			keys = append(keys, issue.Key)
		}
		return nil
	}); err != nil {
		a.t.Fatalf("read the waiting line: %v", err)
	}
	return keys
}

// assertJoinedAndPlanning asserts key runs in adoptionTree, planning at generation under its open
// gate with its planner's start queued, and holds no slot of its own.
func (a *adoptionHarness) assertJoinedAndPlanning(key string, generation uint64) {
	a.t.Helper()
	got := a.record(key)
	if got.Tree != adoptionTree || got.Phase != phase.Planning || got.Generation != generation {
		a.t.Fatalf("%s = %#v, want it planning in the tree of %s at generation %d", key, got, adoptionTree, generation)
	}
	if starts := a.plannerStarts(key, adoptionTree, generation); starts != 1 {
		a.t.Fatalf("%s has %d planner starts queued in %s at generation %d, want one", key, starts, adoptionTree, generation)
	}
	if a.slotted(key) || slices.Contains(a.waiting(), key) {
		a.t.Fatalf("%s holds or waits for a slot (waiting %q), want neither: it runs in the tree of %s", key, a.waiting(), adoptionTree)
	}
}

// A labelled root that queued in admission and was then made a child of a live tree in Dispatch,
// whose re-parent event has not reached the daemon, is adopted by one release: Dispatch already
// shows it todo, so the release writes nothing, and the answer comes once the record runs it in the
// tree, planning under the open gate. Before, the release counted the record as released whatever
// its tree, and park_child answered ISSUE_OUTSIDE_TREE (INFRA-214).
func TestReleaseAdoptsAQueuedRootDispatchShowsTodo(t *testing.T) {
	a := newAdoptionHarness(t, 1)
	a.observe(adoptionChild, 1, "todo", "", true)
	if got := a.record(adoptionChild); got.Tree != adoptionChild || got.Parent != nil || !reflect.DeepEqual(a.waiting(), []string{adoptionChild}) {
		t.Fatalf("%s = %#v, waiting %q; want it queued as a root of its own with no parent recorded", adoptionChild, got, a.waiting())
	}
	a.shows(adoptionChild, "todo", adoptionTree, 2, true)

	if status, failure := a.release(adoptionChild); status != http.StatusOK {
		t.Fatalf("release of %s = %d %#v, want released", adoptionChild, status, failure)
	}
	a.assertJoinedAndPlanning(adoptionChild, 2)
	if got := a.record(adoptionChild); got.Parent == nil || *got.Parent != adoptionTree {
		t.Fatalf("%s's parent = %v, want %s, the parent the release read", adoptionChild, got.Parent, adoptionTree)
	}
	if len(a.statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the child is already todo", a.statuses.writes)
	}
	a.assertParked(adoptionChild)
}

// A queued root that left the line for backlog, lingering as a root of its own, and was then made
// a child of a live tree is in the tree when its release answers, parked at done until the todo the
// release writes reaches the daemon; that todo runs it in the tree as any parked child's does.
func TestReleaseAdoptsAQueuedRootFromBacklog(t *testing.T) {
	a := newAdoptionHarness(t, 1)
	a.observe(adoptionChild, 1, "todo", "", true)
	a.observe(adoptionChild, 2, "backlog", "", true)
	if got := a.record(adoptionChild); got.Tree != adoptionChild || got.Phase != phase.Done || !got.Lingers() {
		t.Fatalf("%s = %#v, want its own tree lingering at done", adoptionChild, got)
	}
	a.shows(adoptionChild, "backlog", adoptionTree, 3, true)

	if status, failure := a.release(adoptionChild); status != http.StatusOK {
		t.Fatalf("release of %s = %d %#v, want released", adoptionChild, status, failure)
	}
	if got := a.record(adoptionChild); got.Tree != adoptionTree || got.Generation != 2 || got.Phase != phase.Done || got.Lingers() {
		t.Fatalf("%s when the release answered = %#v, want it parked at done in the tree of %s at generation 2, no linger of its own", adoptionChild, got, adoptionTree)
	}
	if want := []statusWrite{{issue: adoptionChild, status: "todo"}}; !reflect.DeepEqual(a.statuses.writes, want) {
		t.Fatalf("Dispatch writes = %#v, want %#v", a.statuses.writes, want)
	}

	// The daemon's own todo write reaches the daemon as Dispatch's event.
	a.apply("todo-echo", intake.DispatchIssue{Key: adoptionChild, ResolvedRepo: "acme/widgets", Seq: 4, Type: "issue.updated",
		Status: "todo", Title: adoptionChild, Parent: adoptionTree, Rank: "B", HandedOver: true, DaemonActor: true})
	got := a.record(adoptionChild)
	a.assertJoinedAndPlanning(adoptionChild, got.Generation)
	a.assertParked(adoptionChild)
}

// The state INFRA-214 reached: queued with the label, moved to backlog (its own tree lingers), the
// label taken off, then todo again, which re-admits nothing without the label and leaves the record
// todo at done, its linger's close and suspend still queued. One release runs it in the live tree,
// and that old linger's expiry closes nothing of it.
func TestReleaseAdoptsTheStateInfra214Reached(t *testing.T) {
	a := newAdoptionHarness(t, 1)
	a.observe(adoptionChild, 1, "todo", "", true)
	a.observe(adoptionChild, 2, "backlog", "", true)
	a.observe(adoptionChild, 3, "backlog", "", false)
	a.observe(adoptionChild, 4, "todo", "", false)
	queued := a.record(adoptionChild)
	g := queued.Generation
	if queued.Tree != adoptionChild || queued.Status != "todo" || queued.HandedOver || queued.Phase != phase.Done || !queued.Lingers() {
		t.Fatalf("%s = %#v, want its own tree, todo without the label, done and lingering", adoptionChild, queued)
	}
	if closes := a.count(`select count(*) from outbox where issue = $1 and kind = 'linger_close' and (payload->>'generation')::bigint = $2`, adoptionChild, int64(g)); closes != 1 {
		t.Fatalf("%s has %d linger closes at generation %d queued, want one", adoptionChild, closes, g)
	}
	if suspends := a.count(`select count(*) from outbox where issue = $1 and kind = 'issue_suspend' and payload->>'tree' = $1 and (payload->>'generation')::bigint = $2`, adoptionChild, int64(g)); suspends != 1 {
		t.Fatalf("%s has %d issue suspends of its own tree at generation %d queued, want one", adoptionChild, suspends, g)
	}
	a.shows(adoptionChild, "todo", adoptionTree, 5, false)

	if status, failure := a.release(adoptionChild); status != http.StatusOK {
		t.Fatalf("release of %s = %d %#v, want released", adoptionChild, status, failure)
	}
	a.assertJoinedAndPlanning(adoptionChild, g+1)
	if len(a.statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the child is already todo", a.statuses.writes)
	}
	adopted := a.record(adoptionChild)

	a.apply("old-linger-expired", intake.LingerExpired{Issue: adoptionChild, Generation: g})
	if closes := a.count(`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'tree_close'`, adoptionChild); closes != 0 {
		t.Fatalf("%s has %d tree closes queued after its old linger expired, want none", adoptionChild, closes)
	}
	if removals := a.count(`select count(*) from outbox where issue = $1 and kind = 'workspace_remove'`, adoptionChild); removals != 0 {
		t.Fatalf("%s has %d workspace removals queued after its old linger expired, want none", adoptionChild, removals)
	}
	if got := a.record(adoptionChild); !reflect.DeepEqual(got, adopted) {
		t.Fatalf("%s after its old linger expired = %#v, want it unchanged from %#v", adoptionChild, got, adopted)
	}
	a.assertParked(adoptionChild)
}

// A labelled todo issue under a backlog child of the live tree, which the tree's architect has not
// released, is recorded as a root of its own that waits for no slot (its preparation read the tree
// above its unrecorded parent). One release of it runs it in the tree, planning under the open
// gate, its parent recorded as the backlog child it sits under.
func TestReleaseAdoptsAHeldRootUnderAnUnreleasedChild(t *testing.T) {
	const parent = "LEGION-169"
	a := newAdoptionHarness(t, 2)
	a.apply("queued", intake.DispatchIssue{Key: adoptionChild, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: adoptionChild, Parent: parent, Rank: "B", HandedOver: true, AncestorTree: adoptionTree})
	if got := a.record(adoptionChild); got.Tree != adoptionChild || a.slotted(adoptionChild) || len(a.waiting()) != 0 {
		t.Fatalf("%s = %#v, waiting %q; want it recorded as a root of its own that waits for no slot", adoptionChild, got, a.waiting())
	}
	a.shows(adoptionChild, "todo", parent, 1, true)
	a.shows(parent, "backlog", adoptionTree, 1, false)

	if status, failure := a.release(adoptionChild); status != http.StatusOK {
		t.Fatalf("release of %s = %d %#v, want released", adoptionChild, status, failure)
	}
	a.assertJoinedAndPlanning(adoptionChild, 2)
	if got := a.record(adoptionChild); got.Parent == nil || *got.Parent != parent {
		t.Fatalf("%s's parent = %v, want %s", adoptionChild, got.Parent, parent)
	}
	if len(a.statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %#v, want none: the child is already todo", a.statuses.writes)
	}
	a.assertParked(adoptionChild)
}

// A child the release cannot adopt is refused with the reason, naming the child, and nothing is
// written: a root that ran as a tree of its own, one the operator runs a tree under, one carrying
// a run handed over to it, and a child running in another live tree. In a wave, one such child
// refuses the whole wave: an adoptable child beside it stays queued where it was, takes no slot
// that stands free, and no Dispatch write is made.
func TestReleaseRefusesAChildItCannotAdopt(t *testing.T) {
	const other, otherChild = "LEGION-300", "LEGION-301"
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	queuedRoot := func(key, rank string) record.Issue {
		return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: key, Tree: key,
			Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: rank, HandedOver: true, LastDispatchSeq: 1}
	}
	seed := func(a *adoptionHarness, write func(tx pgx.Tx) error) {
		a.t.Helper()
		if err := a.store.Tx(context.Background(), write); err != nil {
			a.t.Fatalf("seed: %v", err)
		}
	}
	for _, tc := range []struct {
		name, child, code, reason string
		seed                      func(*adoptionHarness)
	}{
		{name: "a root that ran as a tree of its own", child: adoptionChild, code: "CHILD_NOT_ADOPTABLE", reason: "it ran as a tree of its own",
			seed: func(a *adoptionHarness) {
				a.recordIssue(queuedRoot(adoptionChild, "B"))
				seed(a, func(tx pgx.Tx) error {
					_, err := a.records.OpenTreeLifecycle(context.Background(), tx, project, adoptionChild, treelifecycle.AuthorityWorkflow)
					return err
				})
				a.shows(adoptionChild, "todo", adoptionTree, 2, true)
			}},
		{name: "a root the operator runs a tree under", child: adoptionChild, code: "CHILD_NOT_ADOPTABLE", reason: "the operator runs a tree under its key",
			seed: func(a *adoptionHarness) {
				a.recordIssue(queuedRoot(adoptionChild, "B"))
				seed(a, func(tx pgx.Tx) error {
					_, err := a.records.OpenTreeLifecycle(context.Background(), tx, project, adoptionChild, treelifecycle.AuthorityOperator)
					return err
				})
				a.shows(adoptionChild, "todo", adoptionTree, 2, true)
			}},
		{name: "a root carrying a handed-over run", child: adoptionChild, code: "CHILD_NOT_ADOPTABLE", reason: "it carries a run handed over to it as a root of its own",
			seed: func(a *adoptionHarness) {
				a.recordIssue(queuedRoot(adoptionChild, "B"))
				seed(a, func(tx pgx.Tx) error {
					return a.records.PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: adoptionChild, Generation: 1})
				})
				a.shows(adoptionChild, "todo", adoptionTree, 2, true)
			}},
		{name: "a child running in another live tree", child: otherChild, code: "CHILD_IN_OTHER_TREE", reason: "runs in the tree of " + other + ", not " + adoptionTree,
			seed: func(a *adoptionHarness) {
				parent := other
				a.recordIssue(record.Issue{Key: other, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: other, Project: testProject, Title: other,
					Phase: phase.Planning, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "C"})
				a.recordIssue(record.Issue{Key: otherChild, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: other, Project: testProject, Title: otherChild,
					Parent: &parent, Phase: phase.Done, Generation: 1, Status: "backlog", DispatchStatus: "backlog", Rank: "D", LastDispatchSeq: 1})
				seed(a, func(tx pgx.Tx) error {
					return a.records.PutSlot(context.Background(), tx, record.Slot{Issue: other, Index: 1, AdmittedAt: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)})
				})
				a.shows(otherChild, "backlog", adoptionTree, 2, false)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdoptionHarness(t, 1)
			tc.seed(a)
			before := a.record(tc.child)

			status, failure := a.release(tc.child)
			if status != http.StatusConflict || failure.Code != tc.code || !strings.Contains(failure.Error, tc.child) || !strings.Contains(failure.Error, tc.reason) {
				t.Fatalf("release of %s = %d %#v, want 409 %s naming %s and %q", tc.child, status, failure, tc.code, tc.child, tc.reason)
			}
			if got := a.record(tc.child); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s after the refusal = %#v, want it unchanged from %#v", tc.child, got, before)
			}
			if len(a.statuses.writes) != 0 {
				t.Fatalf("Dispatch writes = %#v, want none", a.statuses.writes)
			}
		})
	}

	t.Run("a wave with one refused child writes nothing", func(t *testing.T) {
		const adoptable, fresh, refused = "LEGION-215", "LEGION-216", "LEGION-217"
		a := newAdoptionHarness(t, 2)
		a.recordIssue(queuedRoot(adoptable, "B"))
		a.recordIssue(queuedRoot(refused, "C"))
		seed(a, func(tx pgx.Tx) error {
			_, err := a.records.OpenTreeLifecycle(context.Background(), tx, project, refused, treelifecycle.AuthorityWorkflow)
			return err
		})
		a.shows(adoptable, "todo", adoptionTree, 2, true)
		a.shows(fresh, "backlog", adoptionTree, 1, false)
		a.shows(refused, "todo", adoptionTree, 2, true)
		before := a.record(adoptable)
		if a.count(`select count(*) from slots`) != 1 {
			t.Fatalf("slots before the release = %d, want only %s's, leaving one of two free", a.count(`select count(*) from slots`), adoptionTree)
		}

		status, failure := a.release(adoptable, fresh, refused)
		if status != http.StatusConflict || failure.Code != "CHILD_NOT_ADOPTABLE" || !strings.Contains(failure.Error, refused) {
			t.Fatalf("release of the wave = %d %#v, want 409 CHILD_NOT_ADOPTABLE naming %s", status, failure, refused)
		}
		if got := a.record(adoptable); !reflect.DeepEqual(got, before) {
			t.Fatalf("%s after the refused wave = %#v, want it as it was, %#v", adoptable, got, before)
		}
		if a.slotted(adoptable) {
			t.Fatalf("%s took the free slot, want no promotion on a refused wave", adoptable)
		}
		if len(a.statuses.writes) != 0 {
			t.Fatalf("Dispatch writes = %#v, want none", a.statuses.writes)
		}
		if got := recordedChild(t, a.harness, fresh); got != nil {
			t.Fatalf("%s = %#v, want it unrecorded", fresh, got)
		}
	})
}
