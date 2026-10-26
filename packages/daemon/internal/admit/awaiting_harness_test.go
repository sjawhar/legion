package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// architectToken is the claim token of the root architect of the tree rooted at root.
func architectToken(t *testing.T, root string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token of %s: %v", root, err)
	}
	return token
}

// seedArchitectClaim records session as the architect of the tree rooted at root, running a turn
// it has taken its task for: the session SessionClaimsTree finds within that tree, and an
// architect that owes nothing (workflow's owed), so the root may wait on a person.
func seedArchitectClaim(t *testing.T, pool *pgxpool.Pool, root, session string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', $2, $2, 'architect', 1, $3, '', 'working', 0, 0, 0, 0)
			on conflict (token) do update set session = excluded.session, state = excluded.state`,
			string(architectToken(t, root)), root, session); err != nil {
			t.Fatalf("seed architect claim: %v", err)
		}
	})
}

// setArchitectState moves root's architect claim to state.
func setArchitectState(t *testing.T, pool *pgxpool.Pool, root string, state supervise.ClaimState) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `update claims set state = $2 where token = $1`, string(architectToken(t, root)), string(state)); err != nil {
			t.Fatalf("set the architect's state: %v", err)
		}
	})
}

// giveArchitectTask records task as delivered to root's architect, confirmed (taken) or not.
func giveArchitectTask(t *testing.T, pool *pgxpool.Pool, root, task string, confirmed bool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, delivered_at, confirmed_at)
			values ($1, 'delivery-1', $2, now(), now(), case when $3 then now() end)
			on conflict (claim_token) do update set task = excluded.task, confirmed_at = excluded.confirmed_at`,
			string(architectToken(t, root)), task, confirmed); err != nil {
			t.Fatalf("give the architect its task: %v", err)
		}
	})
}

// runQueuedArchitectStarts stands in for the outbox running root's queued architect starts: each
// is taken off the queue, and its task (the last start's) is delivered to the claim unconfirmed.
// It reports whether a start was queued.
func runQueuedArchitectStarts(t *testing.T, pool *pgxpool.Pool, root string) bool {
	t.Helper()
	task, started := "", false
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), `delete from outbox where kind = 'supervise' and issue = $1
			and payload->>'role' = 'architect' and payload->>'op' = 'start' returning coalesce(payload->>'task', '')`, root)
		if err != nil {
			t.Fatalf("run the architect's starts: %v", err)
		}
		for rows.Next() {
			started = true
			if err := rows.Scan(&task); err != nil {
				t.Fatalf("run the architect's starts: %v", err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("run the architect's starts: %v", err)
		}
	})
	if started {
		giveArchitectTask(t, pool, root, task, false)
	}
	return started
}

// takeArchitectTask stands in for root's architect taking the work it was last given: its queued
// starts run, and the task they deliver is confirmed in the turn it works.
func takeArchitectTask(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	runQueuedArchitectStarts(t, pool, root)
	setArchitectState(t, pool, root, supervise.StateWorking)
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `update pending_task_deliveries set confirmed_at = now() where claim_token = $1`,
			string(architectToken(t, root))); err != nil {
			t.Fatalf("confirm the architect's task: %v", err)
		}
	})
}

// architectOps is every supervise operation the outbox holds for root's architect, in order.
func architectOps(t *testing.T, pool *pgxpool.Pool, root string) []string {
	t.Helper()
	var ops []string
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == root && request.Role == claim.RoleArchitect {
			ops = append(ops, string(request.Op))
		}
	}
	return ops
}

// lastArchitectTask is the task of the last architect start the outbox holds for root, "" for none.
func lastArchitectTask(t *testing.T, pool *pgxpool.Pool, root string) string {
	t.Helper()
	task := ""
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == root && start.Op == "start" && start.Role == claim.RoleArchitect {
			task = start.Task
		}
	}
	return task
}

// mirrorAsk writes ask's mirror row on root as given.
func mirrorAsk(t *testing.T, pool *pgxpool.Pool, ask record.RootAsk) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutRootAsk(context.Background(), tx, ask); err != nil {
			t.Fatalf("mirror ask %s: %v", ask.Ask, err)
		}
	})
}

// countSuspends counts the architect suspends in ops.
func countSuspends(ops []string) int {
	return strings.Count(strings.Join(ops, " "), "suspend")
}
