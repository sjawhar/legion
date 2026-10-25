package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// quiet is an admission of this cap that logs nowhere: these tests read records and outbox rows,
// never the log.
func quiet(t *testing.T, cap int) *Admission {
	t.Helper()
	return newAdmission(t, cap, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedMirror writes what Dispatch says of key — its status and, in order, the keys it waits on —
// as the blockers.Mirror handler writes one from an event. Its sequence is 1, so any event a test
// then applies through the mirror (sequence 2 or more) wins the row's fence.
func seedMirror(t *testing.T, pool *pgxpool.Pool, key, status string, waits ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		row := record.DispatchIssueRow{Key: key, Project: testProject, Status: status, Blockers: waits, Seq: 1}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, row); err != nil {
			t.Fatalf("seed the mirror of %s: %v", key, err)
		}
	})
}

// applyMirrored applies fact through the daemon's own handler order (daemon/workflow.go): the
// mirror first, so the deciders behind it read the row this very event writes.
func applyMirrored(t *testing.T, pool *pgxpool.Pool, admission *Admission, eventID string, fact intake.DispatchIssue, engine intake.Handler) {
	t.Helper()
	mirror := blockers.NewMirror(record.NewStore(), testProject)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, fact, mirror, engine, admission); err != nil {
		t.Fatalf("ApplyFact %s: %v", eventID, err)
	}
}

// A root that waits on an issue Dispatch has not finished takes no slot: promotion skips it and
// admits the next root the waiting line offers, while the blocked root keeps its place for the
// pass that finds its blocker done.
func TestABlockedRootIsSkippedAndTheNextRootTakesItsSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-FIRST", "A")
	seedWaiting(t, pool, "LEGION-SECOND", "B")
	seedMirror(t, pool, "LEGION-FIRST", "todo", "LEGION-BLOCKER")
	seedMirror(t, pool, "LEGION-BLOCKER", "in_progress")
	seedMirror(t, pool, "LEGION-SECOND", "todo")

	tick(t, pool, admission, "tick")

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-SECOND", Index: 0, AdmittedAt: fixedNow}})
	if first := issue(t, pool, "LEGION-FIRST"); first.Status != "todo" || first.Phase != phase.Admitted {
		t.Fatalf("the blocked root = %s in %s, want it left waiting in todo at admitted", first.Status, first.Phase)
	}
	assertWaiting(t, pool, []string{"LEGION-FIRST"})
}

// Done is the only Dispatch status that finishes a blocker: a root waiting on one in backlog or
// icebox waits on, and so does one waiting on a blocker the daemon keeps no record of, which may
// be an issue whose creation it has not consumed yet.
func TestABlockerThatIsNotDoneHoldsTheRoot(t *testing.T) {
	for _, status := range []string{"backlog", "icebox", "in_progress", "todo"} {
		t.Run("a blocker in "+status, func(t *testing.T) {
			pool := migratedPool(t)
			seedWaiting(t, pool, "LEGION-ROOT", "A")
			seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-BLOCKER")
			seedMirror(t, pool, "LEGION-BLOCKER", status)

			tick(t, pool, quiet(t, 1), "tick")

			assertSlots(t, pool, nil)
		})
	}
	t.Run("a blocker the daemon has no record of", func(t *testing.T) {
		pool := migratedPool(t)
		seedWaiting(t, pool, "LEGION-ROOT", "A")
		seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-UNKNOWN")

		tick(t, pool, quiet(t, 1), "tick")

		assertSlots(t, pool, nil)
		assertEffects(t, pool, nil)
	})
}

// A root whose blockers are all done is admitted exactly as a root that waited on nothing: the
// close of the last issue it waited on writes the same records and the same outbox rows an
// unblocked admission writes, compared here row for row against a control admission of the same
// root. The blocker is a recorded child of another tree, since admission promotes on an
// observation of an issue it records.
func TestTheRootIsAdmittedOnItsLastBlockersCloseExactlyAsAnUnblockedOneIs(t *testing.T) {
	seed := func(t *testing.T, pool *pgxpool.Pool, blockerStatus string) {
		t.Helper()
		seedSlotted(t, pool, "LEGION-OTHER", "A")
		seedWaiting(t, pool, "LEGION-ROOT", "B")
		putIssue(t, pool, record.Issue{Key: "LEGION-BLOCKER", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: "the blocker", Tree: "LEGION-OTHER", Parent: new("LEGION-OTHER"), Phase: phase.Implementing,
			Generation: 1, Status: blockerStatus, Rank: "C", LastDispatchSeq: 1})
		seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-BLOCKER")
		seedMirror(t, pool, "LEGION-BLOCKER", blockerStatus)
	}

	control := migratedPool(t)
	seed(t, control, "done")
	tick(t, control, quiet(t, 2), "tick")

	subject := migratedPool(t)
	admission := quiet(t, 2)
	seed(t, subject, "in_progress")
	tick(t, subject, admission, "tick")
	assertSlots(t, subject, []record.Slot{{Issue: "LEGION-OTHER", Index: 0, AdmittedAt: fixedNow}})
	assertEffects(t, subject, nil)

	applyMirrored(t, subject, admission, "blocker-done", intake.DispatchIssue{
		Key: "LEGION-BLOCKER", ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated", Status: "done",
		Title: "the blocker", Parent: "LEGION-OTHER", Rank: "C"}, engineStub{})

	if got, want := effects(t, subject), effects(t, control); !reflect.DeepEqual(got, want) {
		t.Fatalf("the admission of the unblocked root wrote %#v, want the rows a control admission writes, %#v", got, want)
	}
	if got, want := issue(t, subject, "LEGION-ROOT"), issue(t, control, "LEGION-ROOT"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the admitted root = %#v, want the record a control admission leaves, %#v", got, want)
	}
	if got, want := slots(t, subject), slots(t, control); !reflect.DeepEqual(got, want) {
		t.Fatalf("slots = %#v, want a control admission's, %#v", got, want)
	}
}

// A blocker a person adds to a root already waiting stops it where it stands, and the link taken
// off again lets it through: each is one Dispatch event of the root itself, whose row the mirror
// writes before admission reads it in that same transaction.
func TestABlockerAddedToAWaitingRootStopsItAndTakenOffLetsItThrough(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-ROOT", "A")
	seedMirror(t, pool, "LEGION-ROOT", "todo")
	root := func(seq int64, waits ...string) intake.DispatchIssue {
		return intake.DispatchIssue{Key: "LEGION-ROOT", ResolvedRepo: "acme/widgets", Seq: seq, Type: "issue.updated",
			Status: "todo", Title: "LEGION-ROOT", Rank: "A", HandedOver: handed, Blockers: waits}
	}

	applyMirrored(t, pool, admission, "blocked", root(2, "LEGION-UNKNOWN"), engineStub{})
	assertSlots(t, pool, nil)
	assertEffects(t, pool, nil)

	applyMirrored(t, pool, admission, "unblocked", root(3), engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
}

// A blocked root is still the live tree of its children: it keeps its place in the waiting line
// (record.Waiting), so a child of it is recorded under its tree rather than as an orphan admitted
// as a root of its own.
func TestAChildOfABlockedRootIsRecordedUnderItAndNotAsARootOfItsOwn(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedWaiting(t, pool, "LEGION-ROOT", "A")
	seedMirror(t, pool, "LEGION-ROOT", "todo", "LEGION-UNKNOWN")

	applyMirrored(t, pool, admission, "child-todo", intake.DispatchIssue{
		Key: "LEGION-CHILD", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a child", Parent: "LEGION-ROOT", Rank: "B"}, admission.engine)

	child := issue(t, pool, "LEGION-CHILD")
	if child.Tree != "LEGION-ROOT" || child.Parent == nil || *child.Parent != "LEGION-ROOT" || child.Phase != phase.Admitted {
		t.Fatalf("the child = tree %s parent %v in %s, want it recorded under LEGION-ROOT at admitted", child.Tree, child.Parent, child.Phase)
	}
	assertSlots(t, pool, nil)
	assertWaiting(t, pool, []string{"LEGION-ROOT"})
}

// A blocked root takes no slot, so the slot stands free: the controller is woken about it and may
// hand Legion work that can actually run, rather than the slot being held for a root nothing will
// promote until an issue outside Legion closes.
func TestASlotIsFreeWhenEveryRootInTheQueueIsBlocked(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	registerController(t, pool)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	seedWaiting(t, pool, "LEGION-WAITING", "B")
	seedMirror(t, pool, "LEGION-WAITING", "todo", "LEGION-UNKNOWN")

	applyMirrored(t, pool, admission, "phase-done", intake.DispatchIssue{
		Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress",
		Title: "active", Rank: "A", HandedOver: handed}, engineStub{store: record.NewStore(), done: "LEGION-ACTIVE"})

	assertSlots(t, pool, nil)
	assertControllerNotices(t, pool, []effect{{kind: record.OutboxKindControllerNotice, issue: "LEGION-ACTIVE",
		payload: record.ControllerNotice{Kind: record.SlotFreeNotice}}})
}

// Work already under way is never stopped and never re-checked: a blocker put on the issue of a
// tree that already runs leaves its slot and its record alone.
func TestATreeAlreadyRunningIsUntouchedWhenABlockerIsAddedToItsIssue(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	applyMirrored(t, pool, admission, "blocked", intake.DispatchIssue{
		Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "in_progress",
		Title: "active", Rank: "A", HandedOver: handed, Blockers: []string{"LEGION-UNKNOWN"}}, engineStub{})

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})
	if active := issue(t, pool, "LEGION-ACTIVE"); active.Status != "in_progress" {
		t.Fatalf("the running root = %s, want it left in_progress", active.Status)
	}
}

// A candidate that carries a run into a tree of its own — a handed-over child, a re-admitted tree
// — is work already under way, and is admitted whatever its links say: startedRun is the whole of
// waitsOn's answer for it. A candidate that carries none is checked like any other root, and its
// unfinished blocker keeps it in the waiting line. Every door that brings such a candidate to
// promotion is here with that pair, so the tests say which side of the rule each door is on.
func TestACandidateThatCarriesARunIsAdmittedWhateverItsLinksSay(t *testing.T) {
	// The tick's candidate is a root already recorded mid-phase; the other three doors act on the
	// child of a tree that signed off and lingers, behind the durable hand-over fence. Each is
	// keyed as Dispatch keys an issue: a carried candidate's own phase role is started with it
	// (startMidPhaseMembers), which mints a claim token.
	const recorded, root, child = "LEGION-701", "LEGION-208", "LEGION-209"
	seedRecorded := func(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
		putIssue(t, pool, record.Issue{Key: recorded, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: "a carried root", Tree: recorded, Phase: at, Generation: 2,
			Status: "todo", Rank: "A", HandedOver: true})
		seedMirror(t, pool, recorded, "todo", "LEGION-UNKNOWN")
	}
	seedHandedOver := func(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
		seedLingeringHandovers(t, pool, root, child)
		carried := issue(t, pool, child)
		carried.Phase = at
		putIssue(t, pool, carried)
		seedMirror(t, pool, child, "todo", "LEGION-UNKNOWN")
	}
	// The child's todo as Dispatch delivers it: the hand-over's own event is the daemon's write
	// (DaemonActor), a person's labelling of the orphan is not. Either way the event names what
	// Dispatch says the child waits on, and the mirror writes that row before admission reads it.
	todo := func(daemon bool) intake.DispatchIssue {
		return intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
			Title: child, Parent: root, Rank: "B", HandedOver: handed, DaemonActor: daemon, Blockers: []string{"LEGION-UNKNOWN"}}
	}
	for _, door := range []struct {
		name      string
		candidate string
		seed      func(*testing.T, *pgxpool.Pool, phase.Phase)
		arrive    func(*testing.T, *pgxpool.Pool, *Admission)
	}{
		{name: "a controller tick promoting a candidate already recorded mid-phase", candidate: recorded, seed: seedRecorded,
			arrive: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) { tick(t, pool, admission, "tick") }},
		{name: "a hand-over's own Dispatch event", candidate: child, seed: seedHandedOver,
			arrive: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyMirrored(t, pool, admission, "handover", todo(true), admission.engine)
			}},
		{name: "the outbox's direct consumption", candidate: child, seed: seedHandedOver,
			arrive: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover",
					intake.Handover{Issue: child, Tree: root, Generation: 1}, admission.engine, admission); err != nil {
					t.Fatalf("consume the hand-over: %v", err)
				}
			}},
		{name: "a person's labelling of an orphan", candidate: child, seed: seedHandedOver,
			arrive: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyMirrored(t, pool, admission, "person-labels", todo(false), admission.engine)
			}},
	} {
		t.Run(door.name, func(t *testing.T) {
			for _, carry := range []struct {
				name string
				at   phase.Phase
			}{
				{name: "carrying a run", at: phase.Implementing},
				{name: "carrying no run", at: phase.Admitted},
			} {
				t.Run(carry.name, func(t *testing.T) {
					pool := migratedPool(t)
					admission := quiet(t, 1)
					door.seed(t, pool, carry.at)

					door.arrive(t, pool, admission)

					if carry.at == phase.Admitted {
						// Nothing refused it a tree of its own: it is a root in the waiting line,
						// held there by the blocker Dispatch says is unfinished.
						assertSlots(t, pool, nil)
						assertWaiting(t, pool, []string{door.candidate})
						return
					}
					assertSlots(t, pool, []record.Slot{{Issue: door.candidate, Index: 0, AdmittedAt: fixedNow}})
				})
			}
		})
	}
}

// A tree Legion takes back after it has finished and lingered starts again whatever its links
// say. Re-admission marks the root's own tree coming back (record.Issue.TakenBack) and the
// promotion rule exempts a candidate carrying that mark beside one carrying a run
// (blockers.Exempt), which such a root does not: re-admitting a root clears its phase to
// phase.Admitted, its carry being an orphan's alone. So a root set back to todo during its linger
// takes its slot at once while Dispatch still says it waits on an unfinished issue, and a root
// whose linger has already expired, left closed at phase done, comes back the same way.
func TestATreeTakenBackAfterItsLingerStartsAgainWhateverItsLinksSay(t *testing.T) {
	const root, blocker = "LEGION-LINGER", "LEGION-BLOCKER"
	for _, tc := range []struct {
		name    string
		lingers bool
	}{
		{name: "a root still lingering after its sign-off", lingers: true},
		{name: "a root whose linger has expired", lingers: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			finished := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: "a finished tree", Tree: root, Phase: phase.Done, Generation: 3,
				Status: "done", Rank: "A", HandedOver: true, LastDispatchSeq: 1}
			if tc.lingers {
				until := fixedNow.Add(time.Hour)
				finished.LingerUntil = &until
			}
			putIssue(t, pool, finished)
			seedMirror(t, pool, root, "done", blocker)
			seedMirror(t, pool, blocker, "in_progress")

			applyMirrored(t, pool, admission, "set-back", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: "a finished tree", Rank: "A", HandedOver: handed,
				Blockers: []string{blocker}}, engineStub{})

			if got := issue(t, pool, root); got.Generation != 4 || got.Phase != phase.Admitted || !got.TakenBack || got.LingerUntil != nil {
				t.Fatalf("the tree taken back = %#v, want its next generation at admitted, marked taken back, not lingering", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
			assertWaiting(t, pool, nil)
		})
	}
}

// A child still waiting on blockers when its tree finishes is handed to Legion as a root of its
// own, carrying no run, and starting no phase, where the blocked-root rule admits it once its
// blockers are done. It is no tree taken back: the re-admission that gives it a tree of its own
// is another tree's work arriving, which writes no mark (readmit), so nothing exempts it and the
// unfinished blocker Dispatch names holds it in the waiting line. The blocker's close is what
// admits it, exactly as for a root that never ran.
func TestANeverStartedChildHandedOverFromAFinishedTreeIsStillChecked(t *testing.T) {
	const root, child, blocker = "LEGION-208", "LEGION-209", "LEGION-BLOCKER"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedLingeringHandovers(t, pool, root, child)
	// The child its tree released but never started: still at the phase its tree's gate opened it
	// at, with the hand-over fence its root's linger wrote standing behind it.
	unstarted := issue(t, pool, child)
	unstarted.Phase = phase.Admitted
	putIssue(t, pool, unstarted)
	seedMirror(t, pool, child, "todo", blocker)
	seedMirror(t, pool, blocker, "in_progress")

	applyMirrored(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: handed,
		DaemonActor: true, Blockers: []string{blocker}}, admission.engine)

	if got := issue(t, pool, child); got.Tree != child || got.Phase != phase.Admitted || got.TakenBack {
		t.Fatalf("the handed-over child = %#v, want a root of its own at admitted, carrying no mark", got)
	}
	assertSlots(t, pool, nil)
	assertWaiting(t, pool, []string{child})

	applyMirrored(t, pool, admission, "blocker-done", intake.DispatchIssue{Key: blocker, ResolvedRepo: "acme/widgets",
		Seq: 5, Type: "issue.updated", Status: "done", Title: blocker, Rank: "Z"}, engineStub{})
	tick(t, pool, admission, "tick")

	assertSlots(t, pool, []record.Slot{{Issue: child, Index: 0, AdmittedAt: fixedNow}})
}

// What promotion exempts is never reported as waiting: the tick notice and the state's `blocked`
// are the deciders' own rule read rather than made (blockers.Blocked, blockers.Exempt), so a tree
// Legion took back is named in neither while a blocker of it stands unfinished. The mark is
// durable for exactly this moment: with every slot taken the returning root keeps its place in
// the waiting line, and the pass that judges it when a slot frees has nothing of the re-admission
// left to read but the record — and admits it, its blocker still unfinished.
func TestATreeTakenBackIsNeitherHeldNorReportedBlockedOnALaterPass(t *testing.T) {
	const running, root, blocker = "LEGION-208", "LEGION-LINGER", "LEGION-BLOCKER"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	registerController(t, pool)
	seedSlotted(t, pool, running, "A")
	until := fixedNow.Add(time.Hour)
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "a finished tree", Tree: root, Phase: phase.Done, Generation: 3,
		Status: "done", Rank: "B", HandedOver: true, LastDispatchSeq: 1, LingerUntil: &until})
	seedMirror(t, pool, root, "done", blocker)
	seedMirror(t, pool, blocker, "in_progress")

	applyMirrored(t, pool, admission, "set-back", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets",
		Seq: 2, Type: "issue.updated", Status: "todo", Title: "a finished tree", Rank: "B", HandedOver: handed,
		Blockers: []string{blocker}}, engineStub{})
	tick(t, pool, admission, "tick-1")

	// It waits for capacity, not for its blocker, and the tick names nothing blocked.
	assertSlots(t, pool, []record.Slot{{Issue: running, Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, []string{root})
	assertControllerNotices(t, pool, []effect{
		{kind: record.OutboxKindControllerNotice, issue: testProject, payload: record.ControllerNotice{Kind: record.TickNotice}},
	})

	done := issue(t, pool, running)
	done.Phase = phase.Done
	putIssue(t, pool, done)
	tick(t, pool, admission, "tick-2")

	assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
}

// countingStore counts the mirror reads of the calls under test; every other read and write is
// the real store's.
type countingStore struct {
	record.Store
	mirrorReads int
}

func (s *countingStore) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]record.DispatchIssueRow, error) {
	s.mirrorReads++
	return s.Store.DispatchIssues(ctx, tx, project)
}

// Promotion judges every candidate from one read of the project's mirror, however many candidates
// the waiting line holds: the rows are read before the loop, not inside it.
func TestPromotionReadsTheMirrorOnceWhateverTheNumberOfBlockedRoots(t *testing.T) {
	pool := migratedPool(t)
	store := &countingStore{Store: record.NewStore()}
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Clock: func() time.Time { return fixedNow }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission := New(store, engine, 1, testProject, slog.New(slog.NewTextHandler(io.Discard, nil)))
	admission.now = func() time.Time { return fixedNow }
	for _, key := range []string{"LEGION-ONE", "LEGION-TWO", "LEGION-THREE"} {
		seedWaiting(t, pool, key, key)
		seedMirror(t, pool, key, "todo", "LEGION-UNKNOWN")
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "controller", "tick", intake.ControllerTick{}, admission); err != nil {
		t.Fatalf("ApplyFact tick: %v", err)
	}

	if store.mirrorReads != 1 {
		t.Fatalf("the mirror was read %d times for three blocked candidates, want exactly once per promotion", store.mirrorReads)
	}
	assertSlots(t, pool, nil)
}
