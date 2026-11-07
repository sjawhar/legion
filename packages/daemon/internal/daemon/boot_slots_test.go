package daemon

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// bootProject is the Dispatch project every issue of these tests is filed under. The starts a
// project owes its workers are read by the prefix of the issue key (record.Store.OwedStarts), so
// the keys spell it.
const bootProject = "LEGION"

// bootIssue is a record of key in tree at stands, with the Dispatch status an issue in that
// phase shows.
func bootIssue(key, tree, status string, stands phase.Phase) record.Issue {
	issue := record.Issue{Key: key, Tree: tree, Project: bootProject, Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: key, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if key != tree {
		parent := tree
		issue.Parent = &parent
	}
	return issue
}

// bootHandlers is the pair of handlers every fact of these tests is applied through, in the
// order the daemon applies them: the workflow, which resumes what the boot must resume, and then
// admission, whose pass releases and grants the slots.
func bootHandlers(t *testing.T, records record.Store, cap int) []intake.Handler {
	t.Helper()
	engine := workflow.New(records, workflow.Config{Project: bootProject, Repositories: testRepositories}, quietLogger())
	return []intake.Handler{engine, admit.New(records, engine, cap, bootProject, quietLogger())}
}

// bootAdmission is everything a restart must leave alone: the project's slots, the starts it owes
// its workers — the rows themselves, whose ids are the waiting line's ordering key
// (record.OwedStart) — and the line the state serves.
type bootAdmission struct {
	slots   []record.Slot
	owed    []record.OwedStart
	waiting []string
}

// readBootAdmission reads a bootAdmission in one transaction, as the state route reads the state.
func readBootAdmission(t *testing.T, pool *pgxpool.Pool) bootAdmission {
	t.Helper()
	ctx := context.Background()
	var got bootAdmission
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		var err error
		if got.slots, err = records.Slots(ctx, tx); err != nil {
			return err
		}
		if got.owed, err = records.OwedStarts(ctx, tx, bootProject); err != nil {
			return err
		}
		state, err := projection.Project(ctx, tx, records, bootProject, nil, nil, nil, time.Now().UTC())
		if err != nil {
			return err
		}
		got.waiting = state.Admission.Waiting
		return nil
	}); err != nil {
		t.Fatalf("read the boot state: %v", err)
	}
	return got
}

// deliverableStarts counts the start rows of issue that carry a task: the rows that deliver a
// worker its work. A start of a phase worker is two rows (workflow.StartOrOwe), and the relaunch
// beside this one carries none, so it delivers nothing however often it runs.
func deliverableStarts(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and coalesce(payload->>'task', '') <> ''`, issue).Scan(&count); err != nil {
		t.Fatalf("count the deliverable starts of %s: %v", issue, err)
	}
	return count
}

// A7's restart. Two boots over one store leave the admission state exactly as the first left it:
// the same slot rows, the same starts owed to workers — the same row ids, so the waiting line's
// order is the same order — and the same line. No worker is started twice: the issue whose work
// waits for a slot holds one row carrying a task after both boots, because the second boot finds
// the first boot's start still queued (workflow.StartFor) and queues none.
//
// The fixture is the state a restart is hardest in: a root whose architect is writing its spec
// holds the one slot, and a child of its tree is mid-phase with its worker's claim suspended by
// rule, which is exactly what the boot resumes. The resume's task therefore waits, and what the
// test watches is a durable row rather than a decision the daemon could make again for free.
func TestASecondBootOnTheSameStoreLeavesEverySlotStartAndPlaceAsItWas(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	project, err := claim.ProjectToken(bootProject)
	if err != nil {
		t.Fatal(err)
	}
	root, child := bootProject+"-1", bootProject+"-2"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, bootIssue(root, root, "in_progress", phase.Admitted)); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, bootIssue(child, root, "in_progress", phase.Implementing)); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return openTreeTx(ctx, tx, bootProject, root)
	}); err != nil {
		t.Fatalf("seed the running tree: %v", err)
	}
	implementer, err := claim.NewToken(project, child, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdmitClaim(ctx, supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: child,
		Role: claim.RoleImplementer, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon,
		Session: "ses_implementer", SessionFile: "/dev/null"}); err != nil {
		t.Fatalf("record the suspended implementer: %v", err)
	}

	// One slot, which the root's architect holds while it writes the spec, so the child's resume
	// has none to take and its task stays a durable row for the second boot to find.
	handlers := bootHandlers(t, records, 1)
	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-1", intake.DaemonBooted{Boot: 1}, handlers...); err != nil {
		t.Fatalf("apply the first boot: %v", err)
	}
	first := readBootAdmission(t, pool)
	if len(first.slots) != 1 || first.slots[0].Issue != root {
		t.Fatalf("slots after the first boot = %#v, want the root's alone while its architect writes the spec", first.slots)
	}
	if len(first.owed) != 1 || first.owed[0].Issue != child {
		t.Fatalf("the starts owed after the first boot = %#v, want the resumed child's alone", first.owed)
	}
	if !reflect.DeepEqual(first.waiting, []string{child}) {
		t.Fatalf("admission.waiting after the first boot = %#v, want the child whose start waits", first.waiting)
	}
	if got := deliverableStarts(t, pool, child); got != 1 {
		t.Fatalf("the child holds %d start rows carrying a task after the first boot, want one", got)
	}

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-2", intake.DaemonBooted{Boot: 2}, handlers...); err != nil {
		t.Fatalf("apply the second boot: %v", err)
	}
	second := readBootAdmission(t, pool)
	if !reflect.DeepEqual(second.slots, first.slots) {
		t.Fatalf("slots after the second boot = %#v, want the first boot's %#v", second.slots, first.slots)
	}
	if !reflect.DeepEqual(second.owed, first.owed) {
		t.Fatalf("the starts owed after the second boot = %#v, want the first boot's %#v: a restart moves no issue's place in the line",
			second.owed, first.owed)
	}
	if !reflect.DeepEqual(second.waiting, first.waiting) {
		t.Fatalf("admission.waiting after the second boot = %#v, want the first boot's %#v", second.waiting, first.waiting)
	}
	if got := deliverableStarts(t, pool, child); got != 1 {
		t.Fatalf("the child holds %d start rows carrying a task after the second boot, want the first boot's one: no worker starts twice", got)
	}
}

// issueSlotsMigration is the migration that gives an issue its own slot, the first schema change
// of this release (dispatch://INFRA-980). It is the one migration preReleaseStore withholds, and
// so the one every test of the first boot watches Store.Migrate apply.
const issueSlotsMigration = 20261103183833

// preReleaseStore is a database of this test's own carrying every embedded migration except
// issueSlotsMigration, each applied as Store.Migrate applies it — the migration's SQL, the
// widening of schema_version for a timestamped version, and the version row.
//
// It withholds that one migration rather than cutting the series at its version because the two
// halves of a pre-release store are wanted at once: the schema must take this release's own
// writers, so a test seeds its records through record.Store.PutIssue and every column that
// writer names is there, while what Store.Migrate is left to find is exactly the issue slots,
// design_gates.ever_opened and the lifecycle backfill that migration brings. Store.Migrate
// applies each migration the database has not recorded a row for, per version rather than by
// max(version) (store.migrate and store.apply), so the gap one withheld version leaves is a
// migration it still runs, and no later migration names anything that one creates.
//
// It is built rather than checked in because the first boot on the release is reachable no other
// way: nothing on intake.DaemonBooted says which boot is the first, so the grant that keeps every
// working issue at its turn is the migration's own (dispatch://INFRA-980, "The first boot above
// the cap is a migration, not a boot rule"), and the only way to watch that grant is to arrive at
// it from the schema that precedes it.
func preReleaseStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.Database(t, "legion_first_boot"))
	if err != nil {
		t.Fatalf("open the pre-release store: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	widened := false
	apply := func(migration migrations.Migration) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.SQL); err != nil {
				return err
			}
			if migration.Version > math.MaxInt32 && !widened {
				if _, err := tx.Exec(ctx, "alter table schema_version alter column version type bigint"); err != nil {
					return err
				}
				widened = true
			}
			_, err := tx.Exec(ctx, "insert into schema_version (version) values ($1::bigint)", migration.Version)
			return err
		}); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	for _, migration := range all {
		if migration.Version != issueSlotsMigration {
			apply(migration)
		}
	}
	return st
}

// A7's first boot on the release. A store carrying what a pre-release daemon left — one slot per
// running tree's root and none for any child — is migrated, and every issue that was at work
// keeps working: the migration grants each of them the slot the new cap will ask it to hold,
// even where that puts the project above its cap, so no turn is stopped by a deploy.
//
// What it must not grant is as much of the rule as what it must: an issue of a tree that lingers
// has left the workflow and is started by nothing but its re-admission, and an issue waiting on
// its gate or on its merge is waiting rather than working. The cap then does the draining: the
// surplus is above it, so the root that never ran takes nothing while the count stands at or
// above the cap, and takes a slot at the first pass where the count is under it.
func TestTheFirstBootOnTheReleaseKeepsEveryWorkingIssueAtItsTurn(t *testing.T) {
	ctx := context.Background()
	st := preReleaseStore(t)
	pool := st.Pool()
	records := record.NewStore()
	project, err := claim.ProjectToken(bootProject)
	if err != nil {
		t.Fatal(err)
	}
	spec, running, lingering, queued := bootProject+"-1", bootProject+"-2", bootProject+"-7", bootProject+"-9"
	building, testing := bootProject+"-3", bootProject+"-4"
	heldReady, merging, lingerChild := bootProject+"-5", bootProject+"-6", bootProject+"-8"
	pendingVersion := 2
	lingers := time.Now().UTC().Add(time.Hour)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// A root whose architect is writing its spec, a running tree whose own root waits in
		// accepting while its children work, and a tree that lingers after its close. Each
		// root holds the one slot its tree held before the release, and no child holds any.
		seeded := []record.Issue{
			bootIssue(spec, spec, "in_progress", phase.Admitted),
			bootIssue(running, running, "needs_review", phase.Accepting),
			bootIssue(building, running, "in_progress", phase.Implementing),
			bootIssue(testing, running, "testing", phase.Testing),
			bootIssue(merging, running, "retro", phase.AwaitingMerge),
			bootIssue(lingering, lingering, "done", phase.Done),
			bootIssue(lingerChild, lingering, "in_progress", phase.Implementing),
			bootIssue(queued, queued, "todo", phase.Admitted),
		}
		held := bootIssue(heldReady, running, "retro", phase.Merging)
		held.ReadyPending = &record.ReadyPending{Step: record.DesignStepSpec, Version: pendingVersion}
		seeded = append(seeded, held)
		for _, issue := range seeded {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `update issues set linger_until = $2 where key = $1`, lingering, lingers); err != nil {
			return err
		}
		for index, root := range []string{spec, running, lingering} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: index, AdmittedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the pre-release records: %v", err)
	}
	for _, root := range []string{spec, running, lingering} {
		if _, err := st.OpenTreeLifecycle(ctx, project, root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", root, err)
		}
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the pre-release store onto the release: %v", err)
	}
	granted := slotHolders(t, pool)
	if want := []string{spec, running, building, testing, lingering}; !reflect.DeepEqual(granted, want) {
		t.Fatalf("the slots the migration left = %v, want %v: every issue of a live tree at work, and the roots that already held one",
			granted, want)
	}

	// Two slots, which the five the migration left are well above: the boot starts nothing new
	// and stops nothing that runs.
	handlers := bootHandlers(t, records, 2)
	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-1", intake.DaemonBooted{Boot: 1}, handlers...); err != nil {
		t.Fatalf("apply the first boot on the release: %v", err)
	}
	working := slotHolders(t, pool)
	if want := []string{spec, building, testing}; !reflect.DeepEqual(working, want) {
		t.Fatalf("the slots after the first boot = %v, want %v: the spec's root and the two children at work, above the cap of 2",
			working, want)
	}
	if len(working) <= 2 {
		t.Fatalf("the first boot left %d slot holders, want it above the cap of 2: the surplus is the migration's and drains as issues finish", len(working))
	}
	if waiting := readBootAdmission(t, pool).waiting; !reflect.DeepEqual(waiting, []string{queued}) {
		t.Fatalf("admission.waiting after the first boot = %v, want the root that never ran", waiting)
	}

	// The count falls to the cap as the first child finishes, and nothing new starts there: the
	// loop admits while the holders are under the cap, never at it.
	finish(t, pool, records, building)
	tick(t, pool, handlers, "tick-at-the-cap")
	if holders := slotHolders(t, pool); !reflect.DeepEqual(holders, []string{spec, testing}) {
		t.Fatalf("the slots at the cap = %v, want the two still at work and nothing new started", holders)
	}

	finish(t, pool, records, testing)
	tick(t, pool, handlers, "tick-under-the-cap")
	if holders := slotHolders(t, pool); !reflect.DeepEqual(holders, []string{spec, queued}) {
		t.Fatalf("the slots under the cap = %v, want the root that never ran admitted into the slot the drain freed", holders)
	}
}

// slotHolders is the issue of every admission slot, by issue key. The key, and not the slot's
// index, is what these tests read it by: an index is a place in one sequence shared by every
// project's slots, which the migration's grant continues from the highest one a pre-release
// daemon left, so a slot granted to an issue at work sorts after the roots that already held one
// however the keys run. What each assertion here means is which issues hold a slot.
func slotHolders(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue from slots order by issue`)
	if err != nil {
		t.Fatalf("list the slot holders: %v", err)
	}
	defer rows.Close()
	var held []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatalf("scan a slot: %v", err)
		}
		held = append(held, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the slots: %v", err)
	}
	return held
}

// finish moves key to awaiting_merge, where its worker has nothing left to do, as the completion
// that ends its phase would: the next admission pass takes its slot back.
func finish(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		issue.Phase, issue.Status, issue.DispatchStatus = phase.AwaitingMerge, "retro", "retro"
		return records.PutIssue(ctx, tx, *issue)
	}); err != nil {
		t.Fatalf("finish %s: %v", key, err)
	}
}

// tick applies the controller's tick, the admission pass that runs on no fact of its own.
func tick(t *testing.T, pool *pgxpool.Pool, handlers []intake.Handler, eventID string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "controller", eventID, intake.ControllerTick{}, handlers...); err != nil {
		t.Fatalf("apply %s: %v", eventID, err)
	}
}
