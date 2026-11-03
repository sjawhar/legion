package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// midPhaseStart is one supervise start row of an issue, with whether a lane may run it now
// (record.Store.DueRows, the predicate the outbox claims by).
type midPhaseStart struct {
	role      claim.Role
	task      string
	needsSlot bool
	claimable bool
}

// startsOf is every supervise start row issue holds, oldest first.
func startsOf(t *testing.T, pool *pgxpool.Pool, issue string) []midPhaseStart {
	t.Helper()
	var rows []midPhaseStart
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		due, err := record.NewStore().DueRows(ctx, tx, testProject, issue, []record.OutboxKind{record.OutboxKindSupervise}, fixedNow)
		if err != nil {
			t.Fatalf("list what a lane may run for %s: %v", issue, err)
		}
		runnable := make(map[int64]struct{}, len(due))
		for _, row := range due {
			runnable[row.ID] = struct{}{}
		}
		all, err := tx.Query(ctx, `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			t.Fatalf("list the starts of %s: %v", issue, err)
		}
		defer all.Close()
		for all.Next() {
			var id int64
			var row midPhaseStart
			if err := all.Scan(&id, &row.role, &row.task, &row.needsSlot); err != nil {
				t.Fatalf("scan a start of %s: %v", issue, err)
			}
			_, row.claimable = runnable[id]
			rows = append(rows, row)
		}
		if err := all.Err(); err != nil {
			t.Fatalf("read the starts of %s: %v", issue, err)
		}
	})
	return rows
}

// runBranchRows finishes the issue_branch row of each of issues, as that row's lane does once
// GitHub holds the branch: the starts queued behind it are held by it until it is gone.
func runBranchRows(t *testing.T, pool *pgxpool.Pool, issues ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		rows, err := tx.Query(ctx, `select id from outbox where kind = 'issue_branch' and issue = any($1) order by id`, issues)
		if err != nil {
			t.Fatalf("list the branch rows of %v: %v", issues, err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan a branch row: %v", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("read the branch rows of %v: %v", issues, err)
		}
		if len(ids) != len(issues) {
			t.Fatalf("the branch rows of %v are %v, want one each: every member past admitted gets one", issues, ids)
		}
		for _, id := range ids {
			if err := record.NewStore().FinishOutbox(ctx, tx, id, ""); err != nil {
				t.Fatalf("finish the branch row %d: %v", id, err)
			}
		}
	})
}

// midPhaseChild records key in root's tree standing mid-phase at stands, with the phases row and
// the claim of the role that works it as the tree's close left it: suspended by the daemon's own
// suspend of every claim, since a close retires nothing of the record and only a worker's own
// handoff moves a phase on.
func midPhaseChild(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase, status string) claim.Token {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: root, Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: key, LastDispatchSeq: 1})
	role := workflow.RoleFor(stands)
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		if err := record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: role, Claim: token}); err != nil {
			t.Fatalf("seed the %s's row of %s: %v", role, key, err)
		}
		if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $4, $5, 1, 'ses', '/sessions/ses.jsonl', 'suspended', 1, 0, 0, 0, 0, 'daemon')`,
			string(token), project, root, key, string(role)); err != nil {
			t.Fatalf("seed the %s of %s: %v", role, key, err)
		}
	})
	return token
}

// A3's eighth return, which is admission's own start rather than a transition: a re-admitted tree
// whose two children stand mid-phase. Each child waits for a slot of its own, because the cap
// counts the issues at work and the tree's one root slot no longer carries its children.
//
// The promotion writes everything but the tasks in the fact's own transaction: the root's new
// generation and Dispatch status, each child's branch row, and the relaunch of every role of
// every member (startMidPhaseMembers), so each member's sessions come back and answer while
// their work waits. Each child then owes exactly one start, held unclaimable until that child
// holds a slot, and nothing names its worker as a claim nothing will wake.
//
// Two assertions of the table in internal/workflow (owed_start_test.go) read differently here,
// and the rule is the re-admission's. A resume hands the worker the phase it is already on, so it
// mints no assignment, and the re-admission has cleared the generation's handoffs with the
// assignments on their rows (record.ClearTreeGeneration); both sides of the completion fence are
// then empty, which api/handoff.go compares only when both name one, so no completion in flight
// is refused for an assignment nothing ever delivered. And the claim this start brings back is
// the suspended one the tree's close left: the promotion itself stops and resumes nothing, and
// the relaunch row is what resumes it, holding no task of its own.
func TestAReadmittedTreesMidPhaseChildrenEachWaitForTheirOwnSlot(t *testing.T) {
	const root, testing_, retro = "LEGION-208", "LEGION-209", "LEGION-211"
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Linger: time.Hour,
		Clock: func() time.Time { return fixedNow }}, nil)
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "root", Tree: root, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", LastDispatchSeq: 1})
	tester := midPhaseChild(t, pool, root, testing_, phase.Testing, "in_progress")
	implementer := midPhaseChild(t, pool, root, retro, phase.Retro, "retro")

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: "root", Rank: "A", HandedOver: handed}, engine)

	// The re-admission applied: the root carries its new generation and its Dispatch status write,
	// and each child stands in the phase its own worker left it in.
	if again := issue(t, pool, root); again.Generation != 2 || again.Phase != phase.Admitted || again.Status != "in_progress" {
		t.Fatalf("the re-admitted root = %#v, want generation 2 at admitted, in_progress", again)
	}
	var written string
	if err := pool.QueryRow(context.Background(), `select coalesce((select payload->>'status' from outbox
		where kind = 'dispatch_status' and issue = $1 order by id desc limit 1), '')`, root).Scan(&written); err != nil {
		t.Fatalf("read the root's Dispatch status write: %v", err)
	}
	if written != "in_progress" {
		t.Fatalf("the root's Dispatch status write says %q, want in_progress", written)
	}
	// The tree's one slot is the root's, so neither child holds one.
	if !holdsSlot(t, pool, root) || holdsSlot(t, pool, testing_) || holdsSlot(t, pool, retro) {
		t.Fatalf("the slots are %#v, want the root's alone while the cap is one", slots(t, pool))
	}
	// Every start of an issue waits behind that issue's own branch row, which the promotion
	// queued first so that no role starts before its branch exists on GitHub (store.go's
	// outboxInOrder). Running those rows, as their lane does, leaves the slot as the one thing
	// still holding a task back.
	runBranchRows(t, pool, testing_, retro)
	for child, role := range map[string]claim.Role{testing_: claim.RoleTester, retro: claim.RoleImplementer} {
		if stands := issue(t, pool, child); stands.Phase != workflow.ResumePhase(stands) || stands.Generation != 1 {
			t.Fatalf("%s stands at %s on generation %d, want its own phase on its own generation", child, stands.Phase, stands.Generation)
		}
		if got := assignmentOf(t, pool, child, role); got != "" {
			t.Fatalf("the %s's row of %s carries assignment %q, want none: the re-admission cleared the generation's handoffs", role, child, got)
		}
		var owed, relaunch []midPhaseStart
		for _, row := range startsOf(t, pool, child) {
			if row.needsSlot {
				owed = append(owed, row)
				continue
			}
			relaunch = append(relaunch, row)
		}
		if len(owed) != 1 || owed[0].role != role || owed[0].task == "" || owed[0].claimable {
			t.Fatalf("%s owes %+v, want the %s one start, carrying its resume task and runnable by nobody while it holds no slot", child, owed, role)
		}
		if len(relaunch) != len(claim.Roles) {
			t.Fatalf("%s has the relaunch rows %+v, want one for each of its %d roles", child, relaunch, len(claim.Roles))
		}
		for _, row := range relaunch {
			if row.task != "" || !row.claimable {
				t.Fatalf("the relaunch row %+v of %s, want it carrying no task and runnable at once", row, child)
			}
		}
	}
	for token, state := range claimStates(t, pool) {
		if state != string(supervise.StateSuspended) {
			t.Fatalf("the claim %s is %s, want it as the tree's close left it: the promotion resumes nothing itself, its relaunch rows do", token, state)
		}
	}
	var pending int
	if err := pool.QueryRow(context.Background(), "select count(*) from pending_task_deliveries").Scan(&pending); err != nil {
		t.Fatalf("count the pending deliveries: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries are pending, want none: each child's task waits in the outbox", pending)
	}
	assertMidPhaseChildrenAreNotStranded(t, pool, root, map[string]claim.Token{testing_: tester, retro: implementer})

	// The first slot a child takes delivers that child's one task, and the other child's start
	// stays held: the cap is on the issues at work, one slot each.
	takeSlot(t, pool, testing_)
	for _, row := range startsOf(t, pool, testing_) {
		if row.needsSlot && !row.claimable {
			t.Fatalf("the start %+v of %s is still held, want its slot to have freed it", row, testing_)
		}
	}
	for _, row := range startsOf(t, pool, retro) {
		if row.needsSlot && row.claimable {
			t.Fatalf("the start %+v of %s runs on another issue's slot, want it held for its own", row, retro)
		}
	}
}

// assertMidPhaseChildrenAreNotStranded is A3's fourth assertion for the re-admission: nothing
// names a mid-phase child's worker as a claim nothing will wake, whether it is live or suspended
// next, because the start it waits for is queued and StartFor counts a row the slot holds.
func assertMidPhaseChildrenAreNotStranded(t *testing.T, pool *pgxpool.Pool, root string, workers map[string]claim.Token) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	var claims []supervise.Claim
	for child, token := range workers {
		stands := issue(t, pool, child)
		c := supervise.Claim{Token: token, Project: project, Tree: root, Issue: child, Role: workflow.RoleFor(stands.Phase),
			Generation: 1, State: supervise.StateSuspended}
		claims = append(claims, c)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		stranded, err := workflow.Stranded(context.Background(), tx, record.NewStore(), testProject, claims)
		if err != nil {
			t.Fatalf("read the stranded claims: %v", err)
		}
		if len(stranded) != 0 {
			t.Fatalf("Stranded names %+v, want nothing: each start is queued, held for its issue's slot", stranded)
		}
	})
}

// assignmentOf is the assignment role's phases row of issue carries.
func assignmentOf(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) string {
	t.Helper()
	var assignment string
	if err := pool.QueryRow(context.Background(), `select coalesce(assignment, '') from phases where issue = $1 and role = $2`,
		issue, string(role)).Scan(&assignment); err != nil {
		t.Fatalf("read the %s's assignment on %s: %v", role, issue, err)
	}
	return assignment
}
