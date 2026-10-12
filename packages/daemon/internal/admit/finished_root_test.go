package admit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	finishedRoot = "LEGION-1"
	planning     = "LEGION-2" // mid-run in its first phases: handed over
	heldTesting  = "LEGION-3" // held from testing: handed over
	inRetro      = "LEGION-4" // past an approved review: left for a person
	awaiting     = "LEGION-5" // its READY out: left for a person
	checking     = "LEGION-6" // its pull request merged: left for a person
	finished     = "LEGION-7" // done: stays
	parked       = "LEGION-8" // parked by a person: stays
	unstarted    = "LEGION-9" // still admitted: stays
	lingerOfRoot = uint64(1)
)

// A root whose tree stays done through its linger hands each child it left in its first phases
// (planning through reviewing, or held from one) to Legion as a root of its own: one Dispatch write
// sets the child todo with the legion label, and that write's event admits the child as an orphan
// with its own architect, as the label and the todo do when a person sets them. Before, the child
// stayed suspended, unlabelled and with no architect, and nothing ran it again (LEGION-624). A child
// stays done through its linger hands every child it left mid-run to Legion as a root of its own:
// one Dispatch write sets the child todo with the legion label, and that write's daemon event
// admits the child as an orphan with its own architect and the role of its carried phase started
// (LEGION-624). Every phase carries into the new generation: re-planning a merged child or
// withdrawing a READY would lose completed work. A child that is still admitted, finished or
// parked stays with the tree. The hand-over remains pending during the linger, so a root that comes
// back in it resumes its children where they stood and hands none over. A root a person parked and
// then closed settles the same way.
func TestAFinishedRootSettlesItsUnfinishedChildrenWhenItsLingerEnds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler)
	}{
		{name: "signed off", close: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			if result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: finishedRoot}, engine, admission); err != nil || result.Refusal != nil {
				t.Fatalf("sign off %s = %+v, %v", finishedRoot, result.Refusal, err)
			}
		}},
		{name: "parked, then closed by a person", close: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			apply(t, pool, admission, "root-parked", intake.DispatchIssue{Key: finishedRoot, Seq: 2, Type: "issue.updated", Status: "backlog", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
			apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: finishedRoot, Seq: 3, Type: "issue.closed", Status: "done", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission, engine := seedFinishingTree(t)
			tc.close(t, pool, admission, engine)
			if got := handOvers(t, pool); len(got) != 0 {
				t.Fatalf("hand-overs while the tree lingers = %v, want none: the root can still come back", got)
			}

			expireLinger(t, pool, admission, engine)
			if got, want := handOvers(t, pool), []string{planning, heldTesting, inRetro, awaiting, checking}; !slices.Equal(got, want) {
				t.Fatalf("hand-overs once the linger ended = %v, want %v", got, want)
			}
			if got := messagedChildren(t, pool); len(got) != 0 {
				t.Fatalf("children told a person finishes them = %v, want none: every phase carries over", got)
			}

			// The write's event carries the todo and label. The hand-over fence identifies it as a
			// state-carrying root promotion, rather than the ordinary child re-entry path.
			apply(t, pool, admission, "planning-handed-over", intake.DispatchIssue{Key: planning, Seq: 4, Type: "issue.updated", Status: "todo", Title: planning, Parent: finishedRoot, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
			got := issue(t, pool, planning)
			if got.Tree != planning || got.Phase != phase.Planning || got.Generation != 2 || got.Status != "in_progress" || architectStarts(t, pool, planning) != 1 {
				t.Fatalf("the handed-over child = %#v with %d architect starts, want its own planning root, generation 2, with its architect started once", got, architectStarts(t, pool, planning))
			}
			for _, key := range []string{heldTesting, inRetro, awaiting, checking, finished, parked, unstarted} {
				if got := issue(t, pool, key); got.Tree != finishedRoot || got.Generation != 1 {
					t.Fatalf("%s = %#v, want it left in %s's tree until its own hand-over event", key, got, finishedRoot)
				}
			}
		})
	}
}

// Every phase carries across a durable hand-over. The child is a new root generation, but its gate,
// pull request head, phase rows and phase itself stay intact; promotion starts the architect and the
// role that owns a worked phase. In particular production_check keeps a merged pull request that
// GitHub will never report merged again. The one phase that moves is awaiting_merge: the old tree's
// linger disarmed the pull request (beginLinger), so nothing would arm it again, and the carried
// root resumes in merging, as a re-admitted tree's child does (workflow.ResumePhase), its merger
// started to send READY again.
func TestAHandedOverChildCarriesItsPhaseGateAndPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   phase.Phase
		resumes phase.Phase
		prState record.PullRequestState
		role    claim.Role
	}{
		{name: "planning", phase: phase.Planning, resumes: phase.Planning, prState: record.PullRequestOpen, role: claim.RolePlanner},
		{name: "implementing", phase: phase.Implementing, resumes: phase.Implementing, prState: record.PullRequestOpen, role: claim.RoleImplementer},
		{name: "testing", phase: phase.Testing, resumes: phase.Testing, prState: record.PullRequestOpen, role: claim.RoleTester},
		{name: "reviewing", phase: phase.Reviewing, resumes: phase.Reviewing, prState: record.PullRequestOpen, role: claim.RoleReviewer},
		{name: "retro", phase: phase.Retro, resumes: phase.Retro, prState: record.PullRequestOpen, role: claim.RoleImplementer},
		{name: "merging", phase: phase.Merging, resumes: phase.Merging, prState: record.PullRequestOpen, role: claim.RoleMerger},
		{name: "awaiting merge", phase: phase.AwaitingMerge, resumes: phase.Merging, prState: record.PullRequestOpen, role: claim.RoleMerger},
		{name: "production check", phase: phase.ProductionCheck, resumes: phase.ProductionCheck, prState: record.PullRequestMerged, role: claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			const root, child, artifact = "LEGION-208", "LEGION-209", "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26"
			until := fixedNow.Add(time.Hour)
			parent := root
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				for _, put := range []error{
					records.PutIssue(context.Background(), tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
					records.PutIssue(context.Background(), tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: tc.phase, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}),
					records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, ArtifactID: artifact, LatestVersion: 2, ApprovedVersion: new(2)}),
					records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-42", Generation: 1, State: tc.prState}),
					records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
				} {
					if put != nil {
						t.Fatalf("seed: %v", put)
					}
				}
			})

			apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
			got := issue(t, pool, child)
			if got.Tree != child || got.Generation != 2 || got.Phase != tc.resumes || got.Status != "in_progress" {
				t.Fatalf("handed-over child = %#v, want own tree, generation 2, phase %s, in_progress", got, tc.resumes)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				gate, err := records.Gate(context.Background(), tx, child)
				if err != nil || gate == nil || gate.ArtifactID != artifact || gate.LatestVersion != 2 || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 2 {
					t.Fatalf("carried gate = %#v, %v", gate, err)
				}
				pr, err := records.PullRequest(context.Background(), tx, child)
				if err != nil || pr == nil || pr.HeadSHA != "head-42" || pr.State != tc.prState || pr.Generation != 2 {
					t.Fatalf("carried pull request = %#v, %v", pr, err)
				}
				handover, err := records.Handover(context.Background(), tx, child)
				if err != nil || handover != nil {
					t.Fatalf("handover fence after admission = %#v, %v, want none", handover, err)
				}
				if handedRoot, err := records.HandedOverRoot(context.Background(), tx, child); err != nil || handedRoot == nil || handedRoot.Generation != 2 || !handedRoot.GateCarried {
					t.Fatalf("handed-over root = %#v, %v; want generation 2 with its gate carried", handedRoot, err)
				}
			})
			if starts := phaseStarts(t, pool, child, tc.role, tc.resumes); starts != 1 {
				t.Fatalf("starts of %s at %s = %d, want one", tc.role, tc.resumes, starts)
			}
		})
	}
}

// A child carries its old root's gate when the root has one. A root reopened in its linger has its
// gate cleared with the generation, so a second close hands its children over with none. Such a
// child still carries its phase and pull request, but is not marked carried: its architect's
// catch-up does not forbid register_gate, so it registers its own spec as a fresh root's does, the
// gate its READY needs.
func TestAHandedOverChildWithNoGateToCarryIsNotMarkedCarried(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(context.Background(), tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 2, Status: "done", LingerUntil: &until}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}),
			records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head-42", Generation: 1, State: record.PullRequestOpen}),
			records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 2}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	got := issue(t, pool, child)
	if got.Tree != child || got.Generation != 2 || got.Phase != phase.Merging {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2, still merging", got)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if gate, err := records.Gate(context.Background(), tx, child); err != nil || gate != nil {
			t.Fatalf("gate = %#v, %v; want none, since the old root had none", gate, err)
		}
		if pr, err := records.PullRequest(context.Background(), tx, child); err != nil || pr == nil || pr.HeadSHA != "head-42" || pr.Generation != 2 {
			t.Fatalf("pull request = %#v, %v; want it carried at generation 2", pr, err)
		}
		if handedRoot, err := records.HandedOverRoot(context.Background(), tx, child); err != nil || handedRoot == nil || handedRoot.Generation != 2 || handedRoot.GateCarried {
			t.Fatalf("handed-over root = %#v, %v; want generation 2 with no gate carried", handedRoot, err)
		}
	})
	if starts := phaseStarts(t, pool, child, claim.RoleMerger, phase.Merging); starts != 1 {
		t.Fatalf("merger starts at merging = %d, want one: the carried phase still resumes", starts)
	}
}

// A root a hand-over created is told so in its architect's catch-up, whether or not it carried a
// gate: `handed_over`, since its workers already run their phases and the architect must not
// decompose the tree anew (section 2 of its skill). `carried` says the gate came with it, so it
// registers no spec; a root without it registers its own, and still starts nothing on that gate's
// approval.
func TestAHandedOverRootsCatchUpSaysHandedOverAndWhetherItCarriedAGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gate    bool
		carried string
	}{
		{name: "with its old root's gate", gate: true, carried: "true"},
		{name: "with no gate to carry", gate: false, carried: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			const root, child = "LEGION-208", "LEGION-209"
			until := fixedNow.Add(time.Hour)
			parent := root
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				puts := []error{
					records.PutIssue(ctx, tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
					records.PutIssue(ctx, tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
					records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
				}
				if tc.gate {
					puts = append(puts, records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: new(1)}))
				}
				for _, put := range puts {
					if put != nil {
						t.Fatalf("seed: %v", put)
					}
				}
			})

			apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
			if _, err := intake.ApplyFact(ctx, pool, "supervise", "supervise:architect-of-"+child+":1:ready",
				intake.ClaimReady{Issue: child, Role: claim.RoleArchitect, Launch: 1}, engine, admission); err != nil {
				t.Fatalf("apply the architect's ready: %v", err)
			}
			var handedOver, carried string
			if err := pool.QueryRow(ctx, `select coalesce(payload->'catch_up'->>'handed_over', 'false'), coalesce(payload->'catch_up'->>'carried', 'false')
				from outbox where kind = 'notice' and issue = $1 and payload->>'kind' = 'catch-up' order by id desc limit 1`, child).Scan(&handedOver, &carried); err != nil {
				t.Fatalf("read the architect's catch-up: %v", err)
			}
			if handedOver != "true" || carried != tc.carried {
				t.Fatalf("catch-up says handed_over %s, carried %s; want handed_over true, carried %s", handedOver, carried, tc.carried)
			}
		})
	}
}

// The outbox's own consumption of a hand-over (a write Dispatch already took, or a child Dispatch
// already shows todo with the label) is logged as admission's hand-over, marked as the outbox's,
// once the consumption commits, so the live proof can tell it from admission's orphan carry of the
// same child.
func TestTheOutboxsConsumptionOfAHandOverIsLogged(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewJSONHandler(&logged, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedLingeringHandovers(t, pool, "LEGION-208", "LEGION-209")

	if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover", intake.Handover{Issue: "LEGION-209", Tree: "LEGION-208", Generation: 1}, engine, admission); err != nil {
		t.Fatalf("consume the hand-over: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "admission hand-over" && entry["issue"] == "LEGION-209" &&
			entry["tree"] == "LEGION-208" && entry["via"] == "outbox" {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("admission hand-over lines via the outbox = %d, want one; admission logged:\n%s", lines, logged.String())
	}
	if got := issue(t, pool, "LEGION-209"); got.Tree != "LEGION-209" || got.Generation != 2 {
		t.Fatalf("consumed child = %#v, want its own tree at generation 2", got)
	}
}

// A carried root's workers resume from what the carry kept. A carried merger is told the head the
// carried review round approved, the one head its READY may name, so it does not send the round
// back to review. A carried root held from a phase starts no worker, and the only held notice went
// to the old tree's architect, so its own architect is told of the hold at promotion, to retry or
// escalate it.
func TestACarriedRootResumesFromTheApprovedHeadAndTheHold(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   phase.Phase
		hold *record.Hold
	}{
		{name: "merging", at: phase.Merging},
		{name: "held from testing", at: phase.Held, hold: &record.Hold{From: phase.Testing, Reason: record.HoldEscalated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			const root, child = "LEGION-208", "LEGION-209"
			until := fixedNow.Add(time.Hour)
			parent := root
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				for _, put := range []error{
					records.PutIssue(context.Background(), tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
					records.PutIssue(context.Background(), tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: tc.at, Hold: tc.hold, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}),
					records.PutPhase(context.Background(), tx, record.PhaseRow{Issue: child, Role: claim.RoleReviewer, Claim: "reviewer-claim", Decision: &record.ReviewDecision{State: "approved", Head: "approved-head"}}),
					records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
				} {
					if put != nil {
						t.Fatalf("seed: %v", put)
					}
				}
			})

			apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
			var mergerTasks []string
			var held []record.Notice
			for _, effect := range effects(t, pool) {
				if effect.issue != child {
					continue
				}
				if request, ok := effect.payload.(record.SuperviseRequest); ok && request.Op == "start" && request.Role == claim.RoleMerger {
					mergerTasks = append(mergerTasks, request.Task)
				}
				if notice, ok := effect.payload.(record.Notice); ok && notice.Kind == "held" {
					held = append(held, notice)
				}
			}
			if tc.hold == nil {
				if len(mergerTasks) != 1 || !strings.HasSuffix(mergerTasks[0], " Approved head: approved-head.") {
					t.Fatalf("carried merger tasks = %q, want one naming the carried approved head", mergerTasks)
				}
				return
			}
			want := record.Notice{Kind: "held", Role: claim.RoleTester, Phase: phase.Testing, Reason: string(record.HoldEscalated)}
			if len(held) != 1 || !reflect.DeepEqual(held[0], want) {
				t.Fatalf("held notices to the carried root = %+v, want %+v once", held, want)
			}
		})
	}
}

func phaseStarts(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role, at phase.Phase) int {
	t.Helper()
	starts := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == issue && request.Op == "start" && request.Role == role && request.Phase == at {
			starts++
		}
	}
	return starts
}

// A root's reopen can commit after the outbox handed its child to Dispatch but before the daemon
// consumes that event. The durable hand-over fence then rejects the stale daemon todo inside intake:
// the child stays in the reopened root's tree, and the daemon reasserts its phase status instead of
// re-entering it or making it an orphan.
func TestAStaleHandoverEventAfterTheRootReopensKeepsTheChild(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-208", "LEGION-209"
	parent := root
	until := fixedNow.Add(time.Hour)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(context.Background(), tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}),
			records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	// The reopen is an intake fact serialized before the child PATCH's daemon event, and it cancels
	// the durable hand-over fence instead of deleting it. The next event sees that cancellation.
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: root, Seq: 2, Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: true}, engine)
	apply(t, pool, admission, "late-handover", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	got := issue(t, pool, child)
	if got.Tree != root || got.Phase != phase.Planning || got.Generation != 1 || got.Status != "in_progress" || got.LastDispatchSeq != 2 {
		t.Fatalf("child after a stale hand-over = %#v, want it unchanged in %s's live tree with its event sequence fenced", got, root)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		handover, err := record.NewStore().Handover(context.Background(), tx, child)
		if err != nil || handover != nil {
			t.Fatalf("stale hand-over fence = %#v, %v, want none", handover, err)
		}
	})
	var reasserted int
	for _, effect := range effects(t, pool) {
		if write, ok := effect.payload.(record.StatusWrite); ok && effect.issue == child && write.Status == "in_progress" && write.ObservedStatus == "todo" {
			reasserted++
		}
	}
	if reasserted != 1 {
		t.Fatalf("reasserted child writes = %d, want one", reasserted)
	}
}

// Only the hand-over's own event consumes its fence: the daemon's todo carrying the legion label.
// A delayed ordinary daemon todo of the child, without the label, neither promotes it nor touches
// the fence; the hand-over still completes on its real event.
func TestADaemonTodoWithoutTheLabelLeavesTheHandOverFence(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-208", "LEGION-209"
	seedLingeringHandovers(t, pool, root, child)

	apply(t, pool, admission, "plain-echo", intake.DispatchIssue{Key: child, Seq: 2, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != root || got.Generation != 1 {
		t.Fatalf("child after an unlabelled daemon todo = %#v, want it still in %s's tree", got, root)
	}
	if fence := handoverOf(t, pool, child); fence == nil || fence.Cancelled {
		t.Fatalf("fence after an unlabelled daemon todo = %#v, want it standing", fence)
	}
	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, Seq: 3, Type: "issue.updated", Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Phase != phase.Planning {
		t.Fatalf("child after its hand-over event = %#v, want its own planning root", got)
	}
}

// A person labelling one child of a lingering tree todo admits that child as an ordinary orphan;
// it is not the tree's root coming back, so its siblings' hand-over fences stand.
func TestAnOrphanAdmittedByAPersonLeavesItsSiblingsFences(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, first, second = "LEGION-208", "LEGION-209", "LEGION-210"
	seedLingeringHandovers(t, pool, root, first, second)

	apply(t, pool, admission, "person-labels", intake.DispatchIssue{Key: first, Seq: 2, Type: "issue.updated", Status: "todo", Title: first, Parent: root, Rank: "B", HandedOver: true}, engine)
	if got := issue(t, pool, first); got.Tree != first {
		t.Fatalf("labelled child = %#v, want an orphan root of its own", got)
	}
	if fence := handoverOf(t, pool, second); fence == nil || fence.Cancelled {
		t.Fatalf("sibling's fence after an orphan admission = %#v, want it standing", fence)
	}
}

func seedLingeringHandovers(t *testing.T, pool *pgxpool.Pool, root string, children ...string) {
	t.Helper()
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}); err != nil {
			t.Fatalf("seed root: %v", err)
		}
		for _, child := range children {
			if err := records.PutIssue(context.Background(), tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 1}); err != nil {
				t.Fatalf("seed child: %v", err)
			}
			if err := records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}); err != nil {
				t.Fatalf("seed fence: %v", err)
			}
		}
	})
}

func handoverOf(t *testing.T, pool *pgxpool.Pool, issue string) *record.Handover {
	t.Helper()
	var got *record.Handover
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		got, err = record.NewStore().Handover(context.Background(), tx, issue)
		if err != nil {
			t.Fatalf("read fence: %v", err)
		}
	})
	return got
}

// A child left mid-run by a root that closed before hand-overs existed has no fence: its tree's
// cleanup confirmed with it still in production_check, its pull request merged. A person handing
// it to Legion (todo with the label) admits it as an orphan, and that orphan carries its state as a
// hand-over does: its own tree at a new generation, still in production_check with the merged pull
// request, its implementer started there and its architect told it is carried. A child that never
// started a phase (still admitted under the old root's unopened gate) has nothing to carry: it is
// admitted fresh, with no copied gate and no carried mark, so its architect registers its own spec.
func TestAStrandedMidRunChildHandedToLegionByAPersonCarriesItsState(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	root := record.Issue{Key: "LEGION-208", Project: testProject, Title: "finished root", Tree: "LEGION-208", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", LastDispatchSeq: 1}
	parent := root.Key
	child := record.Issue{Key: "LEGION-209", Project: testProject, Title: "merged child", Tree: root.Key, Parent: &parent,
		Phase: phase.ProductionCheck, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}
	unstarted := record.Issue{Key: "LEGION-210", Project: testProject, Title: "unstarted child", Tree: root.Key, Parent: &parent,
		Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "C", LastDispatchSeq: 1}
	putIssue(t, pool, root)
	putIssue(t, pool, child)
	putIssue(t, pool, unstarted)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child.Key, Repo: "acme/widgets", Number: 42,
			Branch: "legion/LEGION-209", HeadSHA: "merged-head", State: record.PullRequestMerged}); err != nil {
			t.Fatalf("seed merged pull request: %v", err)
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1}); err != nil {
			t.Fatalf("seed the old root's unopened gate: %v", err)
		}
		if _, err := treelifecycle.Open(ctx, tx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open old tree lifecycle: %v", err)
		}
		lifecycle, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", root.Key, treelifecycle.AuthorityWorkflow)
		if err != nil {
			t.Fatalf("reserve old tree cleanup: %v", err)
		}
		if err := treelifecycle.ConfirmCleanup(ctx, tx, "legion", root.Key, lifecycle.Epoch); err != nil {
			t.Fatalf("confirm old tree cleanup: %v", err)
		}
	})

	apply(t, pool, admission, "person-hands-over", intake.DispatchIssue{Key: child.Key, Seq: 2, Type: "issue.updated",
		Status: "todo", Title: child.Title, Parent: root.Key, Rank: child.Rank, HandedOver: handed}, engine)

	got := issue(t, pool, child.Key)
	if got.Tree != child.Key || got.Generation != 2 || got.Phase != phase.ProductionCheck {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2 in production_check", got)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		pr, err := records.PullRequest(ctx, tx, child.Key)
		if err != nil || pr == nil || pr.State != record.PullRequestMerged || pr.HeadSHA != "merged-head" {
			t.Fatalf("pull request = %#v, %v; want merged at merged-head", pr, err)
		}
		handedRoot, err := records.HandedOverRoot(ctx, tx, child.Key)
		if err != nil || handedRoot == nil || handedRoot.Generation != 2 {
			t.Fatalf("handed-over root = %#v, %v; want generation 2", handedRoot, err)
		}
	})
	if starts := architectStarts(t, pool, child.Key); starts != 1 {
		t.Fatalf("architect starts = %d, want one", starts)
	}
	if starts := phaseStarts(t, pool, child.Key, claim.RoleImplementer, phase.ProductionCheck); starts != 1 {
		t.Fatalf("production-check implementer starts = %d, want one", starts)
	}

	apply(t, pool, admission, "person-hands-over-unstarted", intake.DispatchIssue{Key: unstarted.Key, Seq: 2, Type: "issue.updated",
		Status: "todo", Title: unstarted.Title, Parent: root.Key, Rank: unstarted.Rank, HandedOver: handed}, engine)
	fresh := issue(t, pool, unstarted.Key)
	if fresh.Tree != unstarted.Key || fresh.Generation != 2 || fresh.Phase != phase.Admitted {
		t.Fatalf("unstarted orphan = %#v, want its own admitted root at generation 2", fresh)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		handedRoot, err := records.HandedOverRoot(ctx, tx, unstarted.Key)
		if err != nil || handedRoot != nil {
			t.Fatalf("unstarted orphan's handed-over record = %#v, %v; want none: it was admitted fresh", handedRoot, err)
		}
		gate, err := records.Gate(ctx, tx, unstarted.Key)
		if err != nil || gate != nil {
			t.Fatalf("unstarted orphan gate = %#v, %v; want none copied from the old root", gate, err)
		}
	})
	if starts := architectStarts(t, pool, unstarted.Key); starts != 1 {
		t.Fatalf("unstarted orphan architect starts = %d, want one", starts)
	}
}

// A root a person sets todo again while its tree lingers is re-admitted, and its mid-run children
// resume in its new run where they stood (startMidPhaseChildren). The end of the closed generation's
// linger hands none of them over: the reopen wins, and the two never both act on one child.
func TestARootReopenedInItsLingerKeepsItsChildren(t *testing.T) {
	pool, admission, engine := seedFinishingTree(t)
	if result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: finishedRoot}, engine, admission); err != nil || result.Refusal != nil {
		t.Fatalf("sign off %s = %+v, %v", finishedRoot, result.Refusal, err)
	}
	apply(t, pool, admission, "root-reopened", intake.DispatchIssue{Key: finishedRoot, Seq: 2, Type: "issue.updated", Status: "todo", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
	if root := issue(t, pool, finishedRoot); root.Generation != 2 || root.Lingers() {
		t.Fatalf("the reopened root = %#v, want generation 2, no longer lingering", root)
	}
	expireLinger(t, pool, admission, engine)
	if got := handOvers(t, pool); len(got) != 0 {
		t.Fatalf("hand-overs after the reopened root's old linger ended = %v, want none", got)
	}
	if got := issue(t, pool, planning); got.Tree != finishedRoot || got.Phase != phase.Planning {
		t.Fatalf("the reopened root's child = %#v, want it still in %s's tree, in planning", got, finishedRoot)
	}
}

func seedFinishingTree(t *testing.T) (*pgxpool.Pool, *Admission, intake.Handler) {
	t.Helper()
	pool := migratedPool(t)
	admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, DesignGate: config.DesignGateRootIssues, ReviewRoundCap: 3, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedSlotted(t, pool, finishedRoot, "A")
	parent := finishedRoot
	child := func(key string, at phase.Phase, status string) record.Issue {
		return record.Issue{Key: key, Tree: finishedRoot, Project: testProject, Title: key, Parent: &parent, Phase: at, Generation: 1, Status: status, DispatchStatus: status, Rank: "B", LastDispatchSeq: 3}
	}
	held := child(heldTesting, phase.Held, "testing")
	held.Hold = &record.Hold{From: phase.Testing}
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		root, err := records.Issue(ctx, tx, finishedRoot)
		if err != nil {
			t.Fatalf("read root: %v", err)
		}
		root.Phase = phase.ProductionCheck
		for _, put := range []error{
			records.PutIssue(ctx, tx, *root),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: finishedRoot, Role: claim.RoleImplementer, Claim: "root-implementer", HandoffCommit: "production-check", LastHandoff: "production-check"}),
			records.PutIssue(ctx, tx, child(planning, phase.Planning, "in_progress")),
			records.PutIssue(ctx, tx, held),
			records.PutIssue(ctx, tx, child(inRetro, phase.Retro, "retro")),
			records.PutIssue(ctx, tx, child(awaiting, phase.AwaitingMerge, "retro")),
			records.PutIssue(ctx, tx, child(checking, phase.ProductionCheck, "retro")),
			records.PutIssue(ctx, tx, child(finished, phase.Done, "done")),
			records.PutIssue(ctx, tx, child(parked, phase.Done, "backlog")),
			records.PutIssue(ctx, tx, child(unstarted, phase.Admitted, "todo")),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
	return pool, admission, engine
}

func expireLinger(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger", intake.LingerExpired{Issue: finishedRoot, Generation: lingerOfRoot}, engine, admission); err != nil {
		t.Fatalf("expire the linger: %v", err)
	}
}

// handOvers is the issues of every hand-over write queued, in order, each checked to name the
// closed tree and the generation whose linger ended.
func handOvers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var got []string
	for _, effect := range effects(t, pool) {
		write, ok := effect.payload.(record.StatusWrite)
		if !ok || !write.HandOver {
			continue
		}
		if write.Status != "todo" || write.HandOverTree != finishedRoot || write.HandOverLinger != lingerOfRoot {
			t.Fatalf("hand-over of %s = %+v, want todo fenced on %s's linger %d", effect.issue, write, finishedRoot, lingerOfRoot)
		}
		got = append(got, effect.issue)
	}
	return got
}

func messagedChildren(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var got []string
	for _, effect := range effects(t, pool) {
		if _, ok := effect.payload.(record.MessagePost); ok && effect.issue != finishedRoot {
			got = append(got, effect.issue)
		}
	}
	return got
}

func architectStarts(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	starts := 0
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == key && request.Op == "start" && request.Role == claim.RoleArchitect {
			starts++
		}
	}
	return starts
}

// A hand-over carries a child's run only when the child started one. A child its tree finished,
// which a person set back to todo while the tree lingered, is handed over at phase done; carried
// there, its new root would take a slot, show in_progress, start an architect, and lose the slot to
// releaseDoneSlots with nothing running. It is admitted fresh instead, whether its hand-over event
// arrives or the outbox consumes the hand-over directly: no carried mark, no copied gate, and its
// fence gone.
func TestAHandedOverChildThatFinishedIsAdmittedFresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		consume func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler)
	}{
		{name: "its hand-over event", consume: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			apply(t, pool, admission, "handover", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-209", Parent: "LEGION-208", Rank: "B", HandedOver: true, DaemonActor: true}, engine)
		}},
		{name: "the outbox's direct consumption", consume: func(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
			if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "handover", intake.Handover{Issue: "LEGION-209", Tree: "LEGION-208", Generation: 1}, engine, admission); err != nil {
				t.Fatalf("consume the hand-over: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			seedFinishedChildUnderLingeringRoot(t, pool, "LEGION-208", "LEGION-209")

			tc.consume(t, pool, admission, engine)
			got := issue(t, pool, "LEGION-209")
			if got.Tree != "LEGION-209" || got.Generation != 2 || got.Phase != phase.Admitted {
				t.Fatalf("handed-over finished child = %#v, want its own admitted root at generation 2", got)
			}
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				handedRoot, err := records.HandedOverRoot(ctx, tx, "LEGION-209")
				if err != nil || handedRoot != nil {
					t.Fatalf("handed-over record = %#v, %v; want none: it was admitted fresh", handedRoot, err)
				}
				gate, err := records.Gate(ctx, tx, "LEGION-209")
				if err != nil || gate != nil {
					t.Fatalf("gate = %#v, %v; want none copied", gate, err)
				}
			})
			if fence := handoverOf(t, pool, "LEGION-209"); fence != nil {
				t.Fatalf("fence after the fresh admission = %#v, want none", fence)
			}
		})
	}
}

// A fence whose child leaves its old tree some other way goes with it. A person who labels the
// finished child todo before the daemon's hand-over event arrives admits it fresh as an orphan; its
// fence is deleted then, so no later daemon todo of that root (a controller's reopen of it) is
// mistaken for the stale hand-over and swallowed.
func TestAFreshOrphanAdmissionDeletesTheChildsFence(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedFinishedChildUnderLingeringRoot(t, pool, "LEGION-208", "LEGION-209")

	apply(t, pool, admission, "person-labels", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-209", Parent: "LEGION-208", Rank: "B", HandedOver: true}, engine)
	if got := issue(t, pool, "LEGION-209"); got.Tree != "LEGION-209" || got.Phase != phase.Admitted {
		t.Fatalf("labelled finished child = %#v, want its own admitted root", got)
	}
	if fence := handoverOf(t, pool, "LEGION-209"); fence != nil {
		t.Fatalf("fence after the fresh orphan admission = %#v, want none", fence)
	}
}

// seedFinishedChildUnderLingeringRoot is a done root lingering with an approved gate, and a child it
// finished that a person set back to todo during the linger, with the hand-over fence its expiry
// wrote for it.
func seedFinishedChildUnderLingeringRoot(t *testing.T, pool *pgxpool.Pool, root, child string) {
	t.Helper()
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Done, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "B", LastDispatchSeq: 1}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1, ApprovedVersion: new(1)}),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
}

const (
	byHandNote    = "MANUAL_MERGE_REQUIRED: pull request #42 matches a by_hand merge rule; do not auto-arm it"
	carriedPacket = "READY #42 at head (approved at approved) for LEGION-209 (https://github.com/acme/widgets/pull/42)\nMerge: submit on normal acceptance with gh pr merge 42 -R acme/widgets --auto --squash --match-head-commit head\n\nA docs/solutions/legion/retro-LEGION-209.md"
)

// A hand-over carries the merger's phase row whole, the by-hand note of its READY included
// (PhaseRow.MergeNote, which #1906 records with the READY): a READY a person merges stays one
// after the carry, so the packet the carried root later tells still says so and asks no reader to
// arm the merge. Here the READY was refused while the old root's spec was unapproved, so the issue
// keeps its pending version too.
func TestAHandedOverChildKeepsItsMergersByHandNote(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedByHandREADYUnderLingeringRoot(t, pool)

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-209", Parent: "LEGION-208", Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	got := issue(t, pool, "LEGION-209")
	if got.Tree != "LEGION-209" || got.Generation != 2 || got.Phase != phase.Merging || got.ReadyPendingVersion == nil || *got.ReadyPendingVersion != 2 {
		t.Fatalf("handed-over child = %#v, want its own tree at generation 2, merging, its READY pending version 2", got)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := record.NewStore().Phases(context.Background(), tx, "LEGION-209")
		if err != nil {
			t.Fatalf("read phase rows: %v", err)
		}
		for _, row := range rows {
			if row.Role == claim.RoleMerger {
				if row.MergeNote != byHandNote || row.Summary != carriedPacket {
					t.Fatalf("carried merger row = %#v, want its by-hand note and packet kept", row)
				}
				return
			}
		}
		t.Fatalf("phase rows = %#v, want the merger's carried", rows)
	})
}

// End to end: once carried, a by-hand READY the closed gate refused is released by a human's
// approval of the old root's document, and the packet the carried root tells starts with the
// by-hand note and asks for no submitted merge, exactly as the READY was told it would be.
func TestACarriedByHandREADYIsReleasedWithItsNote(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	seedByHandREADYUnderLingeringRoot(t, pool)

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "todo", Title: "LEGION-209", Parent: "LEGION-208", Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "approval", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", Kind: intake.DispatchArtifactApproved, Version: 2}, engine, admission); err != nil {
		t.Fatalf("apply the old document's approval: %v", err)
	}
	if got := issue(t, pool, "LEGION-209"); got.Phase != phase.AwaitingMerge {
		t.Fatalf("carried child after the approval = %#v, want awaiting_merge", got)
	}
	var told []string
	for _, effect := range effects(t, pool) {
		if post, ok := effect.payload.(record.MessagePost); ok && effect.issue == "LEGION-209" {
			told = append(told, post.Body)
		}
	}
	if len(told) != 1 {
		t.Fatalf("messages told on LEGION-209 = %q, want the READY once", told)
	}
	if !strings.HasPrefix(told[0], byHandNote+"\n\n") || strings.Contains(told[0], "gh pr merge") || strings.Contains(told[0], "--auto") {
		t.Fatalf("told READY %q, want the by-hand note first and no submitted merge", told[0])
	}
}

// seedByHandREADYUnderLingeringRoot is a done root lingering, its spec revised to version 2 and not
// yet approved, and a child of it in merging whose merger's by-hand READY that closed gate refused,
// with the hand-over fence the linger's expiry wrote for it.
func seedByHandREADYUnderLingeringRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const root, child = "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent, pending := root, 2
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		for _, put := range []error{
			records.PutIssue(ctx, tx, record.Issue{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(ctx, tx, record.Issue{Key: child, Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1, ReadyPendingVersion: &pending}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: root, ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 2, ApprovedVersion: new(1)}),
			records.PutPullRequest(ctx, tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head", Generation: 1, State: record.PullRequestOpen}),
			records.PutPhase(ctx, tx, record.PhaseRow{Issue: child, Role: claim.RoleMerger, Claim: "merger-claim", HandoffCommit: "head", Summary: carriedPacket, MergeNote: byHandNote}),
			records.PutHandover(ctx, tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
}
