package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The documents of the steps these tests gate. gateDoc is the root's primary document, which the
// spec step binds to; criteriaDoc is its Verification document and decisionsDoc its decisions
// document. register_gate carries no step of its own yet, so a registration writes the row the
// document it names already holds and records the spec step for a document no row holds: a step
// other than spec is given its row once with PutGate, to bind the document to the step, and every
// fact after that is the engine's own.
const (
	criteriaDoc  = "artifact-criteria"
	decisionsDoc = "artifact-decisions"
)

// gatedRoot is the root whose tree these tests gate.
const gatedRoot = "LEGION-208"

// stepsEngine is the workflow engine of a deployment that gates steps, in the order it names
// them. testEngine builds the two policies a configuration file can name today; a list of its own
// is what every reader of the step set takes.
func stepsEngine(steps []record.DesignStep, log *slog.Logger) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: steps, ReviewRoundCap: 3, MaxFixAttempts: 3, Linger: time.Hour, ReviewAppLogin: testReviewApp,
		Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
	}, log)
}

// twoSteps is the list these tests gate: the Verification document before the primary one, so the
// step the work stands on is the first of the two that is closed.
func twoSteps() []record.DesignStep {
	return []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec}
}

// bindStep records the row that binds document to step at version 1, unapproved: the registration
// that follows finds the row by its document and so writes that step's row rather than the spec's.
// Nothing is asserted of this row; the registration rewrites it at its own version.
func bindStep(t *testing.T, pool *pgxpool.Pool, step record.DesignStep, document string) {
	t.Helper()
	seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: step, ArtifactID: document, LatestVersion: 1})
}

// seedGatedTree records the root admitted in the one admission slot, carrying the legion label
// that hands it to Legion, with its architect running a turn it has taken: a root without both
// could not hand work to a person and wait on them (workflow's Unresumable and owed).
func seedGatedTree(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U",
		HandedOver: true, LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	// The lifecycle, not the slot, is what says the tree runs: a slot counts one issue at work
	// (record.TreeLive), so the waiting line reads nothing of this tree without it.
	seedTreeLifecycle(t, pool, gatedRoot)
	seedArchitectClaim(t, pool, gatedRoot)
}

// seedArchitectClaim records root's architect as a live session that owes nothing (workflow's
// owed), so an admission pass may decide the root waits on a person.
func seedArchitectClaim(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token of %s: %v", root, err)
	}
	if _, err := pool.Exec(t.Context(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'LEGION', $2, $2, 'architect', 1, 'session-architect', '', 'working', 0, 0, 0, 0)`, string(token), root); err != nil {
		t.Fatalf("seed the architect claim of %s: %v", root, err)
	}
}

// seedGatedMerger records a child of the gated tree in merging with its merger's phase row, the
// one member whose READY the design gate answers.
func seedGatedMerger(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "merging child", Parent: new(gatedRoot), Phase: phase.Merging, Generation: 1,
		Status: "retro", Rank: "W", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: key, Role: claim.RoleMerger, Claim: "merger-claim"})
}

// readyOf is the merger's READY completion of key, carrying the packet the daemon posts.
func readyOf(key string) intake.HandoffComplete {
	return intake.HandoffComplete{Issue: key, Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true,
		Summary: strings.ReplaceAll(readyPacket, "LEGION-208", key), Commit: "head"}
}

// stepApproval is a person's approval of version of document, keyed by the issue that owns it.
func stepApproval(key, document string, version int, mark record.EventMark) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: intake.DispatchArtifactApproved, Version: version,
		Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: mark}
}

// readyPending is the step and the version a refused READY left on key, the pair the issues table
// holds together.
func readyPending(t *testing.T, pool *pgxpool.Pool, key string) (record.DesignStep, int) {
	t.Helper()
	var step *string
	var version *int
	if err := pool.QueryRow(t.Context(), "select ready_pending_step, ready_pending_version from issues where key = $1", key).Scan(&step, &version); err != nil {
		t.Fatalf("read the pending READY of %s: %v", key, err)
	}
	if step == nil || version == nil {
		return "", 0
	}
	return record.DesignStep(*step), *version
}

// noticesOfKind is every notice of that kind the outbox holds for issue, in order.
func noticesOfKind(t *testing.T, pool *pgxpool.Pool, issue string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload from outbox where kind = 'notice' and issue = $1
		and payload->>'kind' = $2 order by id`, issue, string(kind))
	if err != nil {
		t.Fatalf("read the %s notices of %s: %v", kind, issue, err)
	}
	defer rows.Close()
	notices := []record.Notice{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan a %s notice of %s: %v", kind, issue, err)
		}
		var notice record.Notice
		if err := json.Unmarshal(payload, &notice); err != nil {
			t.Fatalf("decode a %s notice of %s: %v", kind, issue, err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the %s notices of %s: %v", kind, issue, err)
	}
	return notices
}

// awaitingNotices is the reason of every awaiting-person notice the outbox holds for issue: the
// row that tells the controller the root gave its slot back.
func awaitingNotices(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'reason' from outbox where kind = 'controller_notice' and issue = $1
		and payload->>'kind' = $2 order by id`, issue, string(record.AwaitingPersonNotice))
	if err != nil {
		t.Fatalf("read the awaiting-person notices of %s: %v", issue, err)
	}
	reasons, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the awaiting-person notices of %s: %v", issue, err)
	}
	return reasons
}

// reconcileAwaiting runs one admission pass of engine over the project's records as they stand,
// and answers the roots that entered the wait, whose slots admission releases
// (Admission.promoteHolds).
func reconcileAwaiting(t *testing.T, pool *pgxpool.Pool, engine *Engine) []string {
	t.Helper()
	store := record.NewStore()
	entered := []string{}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		scoped, _ := intake.WithCommitHooks(context.Background())
		issues, err := store.Issues(scoped, tx)
		if err != nil {
			return err
		}
		entered, _, err = engine.ReconcileAwaiting(scoped, tx, "LEGION", issues, nil)
		return err
	})
	return entered
}

// admissionPass runs what admission's pass over a project's live records runs of the engine, in
// its order (Admission.promoteHolds): the waiting line's first-phase starts, then the trees a
// step that left gates.design released. It holds no root back.
func admissionPass(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	admissionPassHolding(t, pool, engine, nil)
}

// admissionPassHolding is admissionPass with the pass's own held predicate, the one a boot gives
// it for every root whose catch-up has not applied Dispatch's window yet: the pass stops at the
// first candidate the predicate names, as the real cap loop does, and decides no tree whose root
// it holds, so the pass after the catch-up's release decides that tree on the record as it
// stands then. A nil predicate holds none.
//
// It is the engine's half of admit's promoteHolds, read through the same two statements that
// pass reads: record.Waiting over record.ReadLineFacts says which issues' first phase starts,
// and Engine.StartFirstPhase starts each of them — a root a shorter step list left at admitted
// among them. Engine.AdvanceOpenTrees then releases what no slot is needed for, the READY a
// dropped step refused. internal/admit's own tests run the real pass with its cap.
func admissionPassHolding(t *testing.T, pool *pgxpool.Pool, engine *Engine, held func(string) bool) {
	t.Helper()
	scoped, committed := intake.WithCommitHooks(context.Background())
	tx, err := pool.Begin(scoped)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(scoped)
	store := record.NewStore()
	issues, err := store.Issues(scoped, tx)
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	facts, err := record.ReadLineFacts(scoped, tx, store, "LEGION", issues, engine.cfg.Steps)
	if err != nil {
		t.Fatalf("read the line facts: %v", err)
	}
	// Every issue that no longer needs its slot gives it back before the line is read, as the
	// real pass does (admit's releaseSlots): a root whose gate has opened holds the slot its
	// architect wrote the design in, and the line offers a slot to no issue already holding one.
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	live := record.LiveRoots(issues, facts)
	slots, err := store.Slots(scoped, tx)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	for _, slot := range slots {
		issue, recorded := byKey[slot.Issue]
		_, running := live[issue.Tree]
		if recorded && record.NeedsSlot(issue, byKey[issue.Tree], facts.Gates[issue.Tree], running) {
			continue
		}
		if err := store.ReleaseSlot(scoped, tx, slot.Issue); err != nil {
			t.Fatalf("release the slot of %s: %v", slot.Issue, err)
		}
	}
	if slots, err = store.Slots(scoped, tx); err != nil {
		t.Fatalf("list slots again: %v", err)
	}
	mirrored, err := store.DispatchIssues(scoped, tx, "LEGION")
	if err != nil {
		t.Fatalf("list the mirror: %v", err)
	}
	rows := blockers.Rows(mirrored)
	for _, entry := range record.Waiting(issues, slots, facts) {
		// The real pass stops at the first candidate a hold holds, because the cap counts
		// issues and everything behind it competes for the same slots.
		if held != nil && held(entry.Issue.Key) {
			break
		}
		if entry.Kind != record.LineFirstPhase || len(blockers.Holds(rows, entry.Issue)) > 0 {
			continue
		}
		if err := engine.StartFirstPhase(scoped, tx, entry.Issue, rows[entry.Issue.Key].Blockers); err != nil {
			t.Fatalf("StartFirstPhase %s: %v", entry.Issue.Key, err)
		}
	}
	if err := engine.AdvanceOpenTrees(scoped, tx, issues, facts, held); err != nil {
		t.Fatalf("AdvanceOpenTrees: %v", err)
	}
	if err := tx.Commit(scoped); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed()
}

// readyPackets is the body of every message the outbox holds for issue: what a released READY
// posts on its Dispatch issue (tellReadyAudience).
func readyPackets(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload->>'body' from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindDispatchMessage), issue)
	if err != nil {
		t.Fatalf("read the messages posted for %s: %v", issue, err)
	}
	posted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the messages posted for %s: %v", issue, err)
	}
	return posted
}

// Under two gated steps every one of them holds the tree's work. With the Verification document
// and the primary document both registered and only one approved, the root starts no phase, a
// child released into its tree is recorded and left where it stands, and its merger's READY is
// refused, naming the step the work stands on and the version a person must approve. The approval
// of that step starts the root's first phase, starts the child the gate held, and releases the
// held READY with no second one asked for. Both approval orders are driven, because the step the
// work stands on is the first of the configured list that is closed, whichever a person approved
// first.
func TestGatedStepsHoldEveryPhaseUntilEveryStepIsApproved(t *testing.T) {
	orders := []struct {
		name                   string
		approvedDoc, closedDoc string
		approvedVersion        int
		closed                 record.DesignStep
		closedVersion          int
	}{
		{name: "the Verification document approved first", approvedDoc: criteriaDoc, approvedVersion: 2,
			closed: record.DesignStepSpec, closedDoc: gateDoc, closedVersion: 4},
		{name: "the primary document approved first", approvedDoc: gateDoc, approvedVersion: 4,
			closed: record.DesignStepCriteria, closedDoc: criteriaDoc, closedVersion: 2},
	}
	for _, tc := range orders {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepsEngine(twoSteps(), nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			bindStep(t, pool, record.DesignStepCriteria, criteriaDoc)

			apply("register-criteria", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Version: 2})
			apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
			applyMirrored(t, pool, engine, "child-todo", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion",
				Seq: 2, Type: "issue.updated", Status: "todo", Title: "child", Parent: gatedRoot, Rank: "V"})

			apply("approve-one-step", stepApproval(gatedRoot, tc.approvedDoc, tc.approvedVersion, record.EventMark{Seq: 7}))
			if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
				t.Fatalf("the root = %s with the %s step closed, want it admitted: every gated step holds its first phase", got, tc.closed)
			}
			if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
				t.Fatalf("the released child = %s with the %s step closed, want it admitted", got, tc.closed)
			}
			if started := superviseRequests(t, pool, "start"); len(started) != 0 {
				t.Fatalf("the tree started %v with the %s step closed, want nobody started", started, tc.closed)
			}

			seedGatedMerger(t, pool, "LEGION-210")
			result := apply("ready", readyOf("LEGION-210"))
			want := fmt.Sprintf("READY refused: approve design version %d of the tree's %s document before requesting READY.", tc.closedVersion, tc.closed)
			if refusalCode(result) != "DESIGN_GATE_CLOSED" || result.Refusal.Message != want {
				t.Fatalf("the merger's READY = %#v, want DESIGN_GATE_CLOSED saying %q", result.Refusal, want)
			}
			if step, version := readyPending(t, pool, "LEGION-210"); step != tc.closed || version != tc.closedVersion {
				t.Fatalf("the refused READY waits on %s version %d, want the %s step at version %d", step, version, tc.closed, tc.closedVersion)
			}
			if got := messageBodies(t, pool); len(got) != 0 {
				t.Fatalf("Dispatch messages while the %s step is closed = %q, want none", tc.closed, got)
			}

			apply("approve-the-closed-step", stepApproval(gatedRoot, tc.closedDoc, tc.closedVersion, record.EventMark{Seq: 8}))
			// The opening starts no phase itself: admission's pass gives each issue whose first
			// phase can start the slot it needs (record.Waiting's LineFirstPhase).
			admissionPass(t, pool, engine)
			if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
				t.Fatalf("the root = %s once every step is approved, want planning", got)
			}
			if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
				t.Fatalf("the released child = %s once every step is approved, want planning", got)
			}
			if got := phaseOf(t, pool, "LEGION-210"); got != phase.AwaitingMerge {
				t.Fatalf("the merger's issue = %s once every step is approved, want awaiting_merge: the held READY is released", got)
			}
			if got := messageBodies(t, pool); len(got) != 1 || got[0] != readyOf("LEGION-210").Summary {
				t.Fatalf("Dispatch messages = %q, want the held READY packet posted once, with no second READY asked for", got)
			}
			if step, version := readyPending(t, pool, "LEGION-210"); step != "" || version != 0 {
				t.Fatalf("the released READY still waits on %s version %d, want the pair cleared", step, version)
			}
		})
	}

	// The step the root hands to a person is the first gated step that is closed, and it moves on
	// as each step is approved: with one step approved and the other's approval request still with
	// the person, the root enters the wait once and gives its admission slot back, and the pass
	// after it finds nothing more to do. No member of this tree is in a phase, because a member
	// that is needs the slot and keeps the root where it stands.
	for _, tc := range orders {
		t.Run(tc.name+", the wait on the person", func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepsEngine(twoSteps(), nil)
			apply := applyFacts(t, pool, engine)
			seedGatedTree(t, pool)
			bindStep(t, pool, record.DesignStepCriteria, criteriaDoc)

			// Each registration reads an approval request of its own document open on the person's
			// turn, so each hands that document to the person.
			apply("register-criteria", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Version: 2,
				RequestAsk: "ask-criteria", Since: record.EventMark{Seq: 5}})
			apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4,
				RequestAsk: "ask-spec", Since: record.EventMark{Seq: 6}})
			apply("approve-one-step", stepApproval(gatedRoot, tc.approvedDoc, tc.approvedVersion, record.EventMark{Seq: 7}))

			if entered := reconcileAwaiting(t, pool, engine); len(entered) != 1 || entered[0] != gatedRoot {
				t.Fatalf("the pass returned %v, want the root alone, whose slot admission releases", entered)
			}
			waiting := recordedRoot(t, pool)
			if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting == nil || waiting.Awaiting.Artifact != tc.closedDoc ||
				waiting.Awaiting.GateVersion != tc.closedVersion || waiting.Answered() {
				t.Fatalf("the root = %#v, want it waiting unanswered on %s version %d", waiting.Awaiting, tc.closedDoc, tc.closedVersion)
			}
			reasons := awaitingNotices(t, pool, gatedRoot)
			if len(reasons) != 1 || !strings.Contains(reasons[0], "its slot is released") {
				t.Fatalf("awaiting-person notices = %q, want one saying the slot is released", reasons)
			}

			if entered := reconcileAwaiting(t, pool, engine); len(entered) != 0 {
				t.Fatalf("a second pass returned %v, want nothing: the root waits once", entered)
			}
			if got := awaitingNotices(t, pool, gatedRoot); len(got) != 1 {
				t.Fatalf("awaiting-person notices after a second pass = %q, want still the one", got)
			}
		})
	}
}

// A person's changes request reaches the step whose document they reviewed and no other. With
// both steps open, a changes request on the Verification document closes the criteria step,
// leaves the spec step open, tells the architect once with the reason the person gave, and has
// the tree's merger refused READY on the criteria step.
func TestGatedStepsAChangesRequestOnOneStepLeavesTheOtherOpen(t *testing.T) {
	pool := migratedPool(t)
	engine := stepsEngine(twoSteps(), nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	bindStep(t, pool, record.DesignStepCriteria, criteriaDoc)

	// Each registration carries the approval its read of Dispatch found, so both steps open.
	reviewer := dispatch.Actor{Kind: "user", ID: "alice"}
	apply("register-criteria", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, Version: 2, ApprovedVersion: 2, ReviewedBy: reviewer})
	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, ApprovedVersion: 4, ReviewedBy: reviewer})
	// The opening starts no phase itself: admission's pass gives each issue whose first phase
	// can start the slot it needs (record.Waiting's LineFirstPhase).
	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("the root = %s with both steps approved, want planning", got)
	}

	apply("changes-on-the-verification-document", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: criteriaDoc,
		Kind: intake.DispatchArtifactChangesRequested, Version: 2, Reason: "tighten the criteria", Actor: reviewer,
		Mark: record.EventMark{Seq: 9}})

	gates := gatesOf(t, pool, gatedRoot)
	criteria, spec := gates[record.DesignStepCriteria], gates[record.DesignStepSpec]
	if criteria.ApprovedVersion != nil {
		t.Fatalf("the criteria step keeps approved version %v, want the changes request to close it", criteria.ApprovedVersion)
	}
	if spec.ApprovedVersion == nil || *spec.ApprovedVersion != 4 {
		t.Fatalf("the spec step = approved %v at version %d, want it left open at 4: the person reviewed another document",
			spec.ApprovedVersion, spec.LatestVersion)
	}
	// The notice carries the version and the reason of the review, which is what tells the
	// criteria step's review from the spec step's: the two documents stand at versions of their
	// own. Its reference is the issue's primary document whatever step was reviewed, so the
	// version is the only part of it that names the document the person read.
	told := noticesOfKind(t, pool, gatedRoot, "design-changes-requested")
	if len(told) != 1 || told[0].Version != 2 || told[0].Reason != "tighten the criteria" {
		t.Fatalf("design-changes-requested notices = %#v, want one of version 2 with the person's reason", told)
	}

	seedGatedMerger(t, pool, "LEGION-210")
	result := apply("ready", readyOf("LEGION-210"))
	want := "READY refused: approve design version 2 of the tree's criteria document before requesting READY."
	if refusalCode(result) != "DESIGN_GATE_CLOSED" || result.Refusal.Message != want {
		t.Fatalf("the merger's READY = %#v, want DESIGN_GATE_CLOSED saying %q", result.Refusal, want)
	}
	if step, version := readyPending(t, pool, "LEGION-210"); step != record.DesignStepCriteria || version != 2 {
		t.Fatalf("the refused READY waits on %s version %d, want the criteria step at version 2", step, version)
	}
}

// gatesOf is every design gate issue holds, by step.
func gatesOf(t *testing.T, pool *pgxpool.Pool, issue string) map[record.DesignStep]record.DesignGate {
	t.Helper()
	var gates map[record.DesignStep]record.DesignGate
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		gates, err = record.NewStore().GatesOf(t.Context(), tx, issue)
		return err
	})
	return gates
}

// A hand-over carries a copy of every step of its old root's gate, and each copy holds the new
// root's work until it is open. The carry itself is admission's (internal/admit's test of this
// name drives it); here the new root holds the two copies the carry left, its architect's
// catch-up says the gate was carried and names both steps, and the approval of the old root's
// Verification document opens the copy that was closed, which starts the new root's first phase.
func TestGatedStepsCarryEveryStepToAHandedOverChild(t *testing.T) {
	pool := migratedPool(t)
	engine := stepsEngine(twoSteps(), nil)
	apply := applyFacts(t, pool, engine)
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-209", Project: "LEGION", Title: "the carried root", Parent: new(gatedRoot), Phase: phase.Admitted, Generation: 2,
		Status: "in_progress", Rank: "V", HandedOver: true, HandedOverFrom: gatedRoot, LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedTreeLifecycle(t, pool, "LEGION-209")
	// The old root's own rows, and the copies the hand-over left on the new root: the Verification
	// document took a new version while the old tree lingered, so that step's copy is closed.
	for _, issue := range []string{gatedRoot, "LEGION-209"} {
		seedGate(t, pool, record.DesignGate{Issue: issue, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, LatestVersion: 2, ApprovedVersion: new(1)})
		seedGate(t, pool, record.DesignGate{Issue: issue, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4)})
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: "LEGION-209", Generation: 2, GateCarried: true})
	})

	claimReady(t, pool, engine, "LEGION-209", claim.RoleArchitect, 1)
	caught := lastCatchUp(t, pool)
	if !caught.HandedOver || !caught.Carried {
		t.Fatalf("the catch-up says handed over %t, carried %t; want both of a root whose gate came with it", caught.HandedOver, caught.Carried)
	}
	want := []record.CatchUpGate{
		{Step: record.DesignStepCriteria, Gated: true, Artifact: criteriaDoc, Version: 2, Approved: 1},
		{Step: record.DesignStepSpec, Gated: true, Artifact: gateDoc, Version: 4, Approved: 4, Open: true},
	}
	if fmt.Sprint(caught.Gates) != fmt.Sprint(want) {
		t.Fatalf("the catch-up states %#v, want one entry for each carried step: %#v", caught.Gates, want)
	}
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the carried root = %s while a carried step is closed, want it admitted", got)
	}

	apply("approve-the-old-document", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 9}))
	// The opening starts no phase itself: admission's pass gives the carried root the slot its
	// first phase needs (record.Waiting's LineFirstPhase).
	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
		t.Fatalf("the carried root = %s once every carried step is open, want planning", got)
	}
	opened := noticesOfKind(t, pool, "LEGION-209", "carried-gate-opened")
	if len(opened) != 1 || opened[0].Version != 2 {
		t.Fatalf("the carried root's notices = %#v, want one carried-gate-opened of version 2", opened)
	}
}

// Every design decision a notice carries names the document the person reviewed, because a
// decision nobody in its tree hears is kept and relayed under that reference, newest per
// reference (the outbox's parkDecision, record.Store.AddParkedDecision): two steps of one root
// that answered to one reference would replace each other's decision. The spec step keeps the
// reference it has always had, so a one-step deployment's decisions are keyed as before.
func TestGatedStepsNameEachStepsOwnDocumentOnItsDecision(t *testing.T) {
	pool := migratedPool(t)
	engine := stepsEngine([]record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	bindStep(t, pool, record.DesignStepDecisions, decisionsDoc)

	apply("register-decisions", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, Version: 3})
	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	apply("approve-decisions", stepApproval(gatedRoot, decisionsDoc, 3, record.EventMark{Seq: 7}))
	apply("changes-on-the-spec", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "say what it does not do", Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: 8}})

	approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
	if len(approved) != 1 || approved[0].Ref != "dispatch://"+gatedRoot+"/artifact/decisions-md" {
		t.Fatalf("design-approved notices = %#v, want one naming the decisions document", approved)
	}
	changes := noticesOfKind(t, pool, gatedRoot, "design-changes-requested")
	if len(changes) != 1 || changes[0].Ref != "dispatch://"+gatedRoot+"/spec" {
		t.Fatalf("design-changes-requested notices = %#v, want one naming the spec, the reference that document has always had", changes)
	}
}

// A step the deployment gates that nobody has registered holds the tree's work and asks nobody:
// the root starts no phase, and no awaiting-person notice is written for it, because the record
// knows no document to ask a person about. The same root with that step registered and closed
// does wait, which is what tells the two apart, and it starts once that step is registered and
// approved.
func TestUnregisteredStepHoldsTheWorkAndRaisesNoWait(t *testing.T) {
	steps := []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}
	t.Run("the step nobody registered", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepsEngine(steps, nil)
		apply := applyFacts(t, pool, engine)
		seedGatedTree(t, pool)

		// The spec step alone is registered and approved, each through its own fact; the
		// decisions step has no row at all.
		apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4,
			RequestAsk: "ask-spec", Since: record.EventMark{Seq: 5}})
		apply("approve-spec", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 6}))
		if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
			t.Fatalf("the root = %s with the decisions step unregistered, want it admitted", got)
		}

		reconcileAwaiting(t, pool, engine)
		if got := recordedRoot(t, pool).Phase; got != phase.Admitted {
			t.Fatalf("the root = %s after an admission pass, want it admitted: an unregistered step asks nobody", got)
		}
		if got := awaitingNotices(t, pool, gatedRoot); len(got) != 0 {
			t.Fatalf("awaiting-person notices = %q, want none for a step nobody registered", got)
		}

		bindStep(t, pool, record.DesignStepDecisions, decisionsDoc)
		apply("register-decisions", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, Version: 3})
		if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
			t.Fatalf("the root = %s with the decisions step registered and closed, want it admitted", got)
		}
		apply("approve-decisions", stepApproval(gatedRoot, decisionsDoc, 3, record.EventMark{Seq: 8}))
		admissionPass(t, pool, engine)
		if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
			t.Fatalf("the root = %s once the decisions step is registered and approved, want planning", got)
		}
	})

	t.Run("the step registered and unapproved", func(t *testing.T) {
		pool := migratedPool(t)
		engine := stepsEngine(steps, nil)
		apply := applyFacts(t, pool, engine)
		seedGatedTree(t, pool)
		bindStep(t, pool, record.DesignStepDecisions, decisionsDoc)

		apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4,
			RequestAsk: "ask-spec", Since: record.EventMark{Seq: 5}})
		apply("approve-spec", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 6}))
		apply("register-decisions", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, Version: 3,
			RequestAsk: "ask-decisions", Since: record.EventMark{Seq: 7}})

		if entered := reconcileAwaiting(t, pool, engine); len(entered) != 1 || entered[0] != gatedRoot {
			t.Fatalf("the pass returned %v, want the root, which handed its decisions document to a person", entered)
		}
		waiting := recordedRoot(t, pool)
		if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting == nil || waiting.Awaiting.Artifact != decisionsDoc {
			t.Fatalf("the root = %#v, want it waiting on the decisions document", waiting.Awaiting)
		}
		if got := awaitingNotices(t, pool, gatedRoot); len(got) != 1 {
			t.Fatalf("awaiting-person notices = %q, want the one a registered closed step raises", got)
		}
	})
}

// A step nobody has registered holds the work, and that is all it does: the wait a registered
// step after it raised still reaches the person. Under the decisions step and the spec, with the
// decisions step unregistered and the spec registered, unapproved and its approval request with
// the person, the root enters the wait on its primary document and gives its slot back. Reading
// the unregistered step as the step the person has would leave the root holding a slot while the
// person holds its request, with nothing to answer the wait that never began.
func TestUnregisteredStepDoesNotHideTheWaitARegisteredStepRaises(t *testing.T) {
	pool := migratedPool(t)
	engine := stepsEngine([]record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)

	apply("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4,
		RequestAsk: "ask-spec", Since: record.EventMark{Seq: 5}})

	if entered := reconcileAwaiting(t, pool, engine); len(entered) != 1 || entered[0] != gatedRoot {
		t.Fatalf("the pass returned %v, want the root, whose primary document is with a person", entered)
	}
	waiting := recordedRoot(t, pool)
	if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting == nil || waiting.Awaiting.Artifact != gateDoc ||
		waiting.Awaiting.GateVersion != 4 {
		t.Fatalf("the root = %#v, want it waiting on the primary document at version 4", waiting.Awaiting)
	}
	if got := awaitingNotices(t, pool, gatedRoot); len(got) != 1 {
		t.Fatalf("awaiting-person notices = %q, want the one the registered closed step raises", got)
	}
}

// The step list is read at every boot and nothing of it is stored for a tree, so a tree admitted
// under one list is held by a step a longer list adds: the approval that would have started it
// leaves its root, the child released into it and its merger's READY where they stand. The boot
// that drops that step releases all three, and nothing else could: no fact about a gate is
// coming, since an approval of a document the deployment no longer gates is ignored, and the
// merger is never asked for a second READY. Both directions are driven from the one stored tree.
func TestChangedStepListTakesHoldAtTheNextBoot(t *testing.T) {
	pool := migratedPool(t)
	spec := stepsEngine([]record.DesignStep{record.DesignStepSpec}, nil)
	seedGatedTree(t, pool)
	applyFacts(t, pool, spec)("register-spec", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
		t.Fatalf("the root = %s under the one step it holds unapproved, want it admitted", got)
	}

	// The next boot gates the decisions step as well, which this tree has never registered.
	longer := stepsEngine([]record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}, nil)
	applyMirrored(t, pool, longer, "child-todo", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: "child", Parent: gatedRoot, Rank: "V"})
	seedGatedMerger(t, pool, "LEGION-210")
	result := applyFacts(t, pool, longer)("ready-held", readyOf("LEGION-210"))
	want := "READY refused: no design version of the tree's decisions document is approved; register and approve it before requesting READY."
	if refusalCode(result) != "DESIGN_GATE_CLOSED" || result.Refusal.Message != want {
		t.Fatalf("the merger's READY after the boot = %#v, want DESIGN_GATE_CLOSED saying %q", result.Refusal, want)
	}
	applyFacts(t, pool, longer)("approve-spec", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 7}))
	for _, held := range []string{gatedRoot, "LEGION-209"} {
		if got := phaseOf(t, pool, held); got != phase.Admitted {
			t.Fatalf("%s after the approval of the spec = %s, want it admitted: the added step holds it", held, got)
		}
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Merging {
		t.Fatalf("the merger's issue after the approval of the spec = %s, want it merging: the added step refused its READY", got)
	}

	// A boot reads its list before the catch-up applies what Dispatch recorded while the daemon
	// was down, so the pass decides no tree whose root the catch-up still holds: a spec version
	// written during the outage would close the gate this pass reads as open, and nothing moves a
	// phase back once it has started.
	off := stepsEngine(config.DesignGateOff.Steps(), nil)
	admissionPassHolding(t, pool, off, func(key string) bool { return key == gatedRoot })
	if got := phaseOf(t, pool, gatedRoot); got != phase.Admitted {
		t.Fatalf("the root after a boot that gates no step while its catch-up holds it = %s, want it admitted", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.Merging {
		t.Fatalf("the merger's issue while its root's catch-up holds it = %s, want it merging: the held READY waits for the catch-up", got)
	}

	// The pass after the catch-up's release decides the tree on the record as it stands: the spec
	// row it holds is open, so it starts the root and the child the added step held and posts the
	// READY it refused.
	admissionPass(t, pool, off)
	for _, released := range []string{gatedRoot, "LEGION-209"} {
		if got := phaseOf(t, pool, released); got != phase.Planning {
			t.Fatalf("%s after a boot that gates no step = %s, want planning", released, got)
		}
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.AwaitingMerge {
		t.Fatalf("the merger's issue after a boot that gates no step = %s, want awaiting_merge", got)
	}
	if posted := readyPackets(t, pool, "LEGION-210"); len(posted) != 1 || !strings.Contains(posted[0], "LEGION-210") {
		t.Fatalf("the READY packets posted for LEGION-210 = %q, want the one the refusal held", posted)
	}
}

// `gates.design: root-issues` resolves to the one step spec and behaves as it did before a
// deployment could name a list: the registration records that one step, an approval opens it and
// is told once, a changes request is told once with the person's reason, a READY before the
// approval is refused, and a carried copy opens on an approval of the old root's document.
func TestLegacyPolicyRootIssuesBehavesAsItDoesToday(t *testing.T) {
	alice := dispatch.Actor{Kind: "user", ID: "alice"}
	t.Run("the registration opens the one step and tells the architect once", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedGatedTree(t, pool)

		apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4, ApprovedVersion: 4, ReviewedBy: alice})
		admissionPass(t, pool, engine)
		gates := gatesOf(t, pool, gatedRoot)
		if len(gates) != 1 || gates[record.DesignStepSpec].ApprovedVersion == nil || *gates[record.DesignStepSpec].ApprovedVersion != 4 {
			t.Fatalf("the gates = %#v, want the one spec step open at version 4", gates)
		}
		if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
			t.Fatalf("the root = %s under root-issues with its spec approved, want planning", got)
		}
		if told := noticesOfKind(t, pool, gatedRoot, "design-approved"); len(told) != 1 || told[0].Version != 4 || told[0].Actor != "user:alice" {
			t.Fatalf("design-approved notices = %#v, want one of version 4 naming the person", told)
		}

		apply("new-version", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5,
			Mark: record.EventMark{Seq: 9}})
		apply("changes", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactChangesRequested,
			Version: 5, Reason: "say what it does not do", Actor: alice, Mark: record.EventMark{Seq: 10}})
		told := noticesOfKind(t, pool, gatedRoot, "design-changes-requested")
		if len(told) != 1 || told[0].Version != 5 || told[0].Reason != "say what it does not do" {
			t.Fatalf("design-changes-requested notices = %#v, want one of version 5 with the person's reason", told)
		}
	})

	t.Run("a READY before the approval is refused", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedGatedTree(t, pool)
		apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
		seedGatedMerger(t, pool, "LEGION-210")

		result := apply("ready", readyOf("LEGION-210"))
		want := "READY refused: approve design version 4 of the tree's spec document before requesting READY."
		if refusalCode(result) != "DESIGN_GATE_CLOSED" || result.Refusal.Message != want {
			t.Fatalf("the merger's READY = %#v, want DESIGN_GATE_CLOSED saying %q", result.Refusal, want)
		}
		apply("approve", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 9}))
		if got := phaseOf(t, pool, "LEGION-210"); got != phase.AwaitingMerge {
			t.Fatalf("the merger's issue = %s after the approval, want awaiting_merge", got)
		}
	})

	t.Run("a carried gate opens on an approval of the old root's document", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
		seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: "LEGION-209", Project: "LEGION", Title: "the carried root", Parent: new(gatedRoot), Phase: phase.Admitted,
			Generation: 2, Status: "in_progress", Rank: "V", HandedOver: true, HandedOverFrom: gatedRoot, LastDispatchSeq: 1})
		seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
		seedTreeLifecycle(t, pool, "LEGION-209")
		for _, issue := range []string{gatedRoot, "LEGION-209"} {
			seedGate(t, pool, record.DesignGate{Issue: issue, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(3)})
		}

		apply("approve-the-old-document", stepApproval(gatedRoot, gateDoc, 4, record.EventMark{Seq: 9}))
		admissionPass(t, pool, engine)
		if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
			t.Fatalf("the carried root = %s after the old document's approval, want planning", got)
		}
		if opened := noticesOfKind(t, pool, "LEGION-209", "carried-gate-opened"); len(opened) != 1 {
			t.Fatalf("the carried root's notices = %#v, want one carried-gate-opened", opened)
		}
	})
}

// `gates.design: off` resolves to the empty list and behaves as it did before: the registration
// records the spec step and opens it at once, with no person asked, a READY after it is accepted,
// and a READY before any registration is still refused, because the empty list requires the
// record the registration writes.
func TestLegacyPolicyOffBehavesAsItDoesToday(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateOff, nil)
	apply := applyFacts(t, pool, engine)
	seedGatedTree(t, pool)
	seedGatedMerger(t, pool, "LEGION-210")

	result := apply("ready-before-the-registration", readyOf("LEGION-210"))
	want := "READY refused: no design version of the tree's spec document is approved; register and approve it before requesting READY."
	if refusalCode(result) != "DESIGN_GATE_CLOSED" || result.Refusal.Message != want {
		t.Fatalf("the merger's READY before any registration = %#v, want DESIGN_GATE_CLOSED saying %q", result.Refusal, want)
	}
	if step, version := readyPending(t, pool, "LEGION-210"); step != "" || version != 0 {
		t.Fatalf("the refused READY waits on %s version %d, want neither: no step is registered to approve", step, version)
	}

	apply("register", intake.GateRegistered{Issue: gatedRoot, ArtifactID: gateDoc, Version: 4})
	gates := gatesOf(t, pool, gatedRoot)
	spec := gates[record.DesignStepSpec]
	if len(gates) != 1 || spec.ApprovedVersion == nil || *spec.ApprovedVersion != 4 {
		t.Fatalf("the gates = %#v, want the spec step recorded and open at once, with no person asked", gates)
	}
	admissionPass(t, pool, engine)
	if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
		t.Fatalf("the root = %s under the empty list once its spec is registered, want planning", got)
	}
	if got := phaseOf(t, pool, "LEGION-210"); got != phase.AwaitingMerge {
		t.Fatalf("the merger's issue = %s once the registration opened the gate, want awaiting_merge", got)
	}
}
