package store

import (
	"context"
	"testing"
)

// A tree that ran, taken back after it has finished and lingered, starts again whatever Dispatch
// says it waits on, while a root that only waited and a child handed over from a finished tree as a
// root of its own without its run are checked like any other root. Re-admission leaves the three
// records the same shape, so 0052 gives the issue the mark that tells them apart, durably: whether
// it has stood in a phase of its own under the tree it now roots. A returning root whose project
// has no free slot is judged again by a later pass, long after the run it is judged on.
//
// The rows an older daemon recorded are marked from what they still show of a run under their
// present tree — a phase that carries one, or a phase worker's row whose claim ran under that tree
// or is gone — so a tree that ran before the upgrade is not checked as though it never had. A row
// showing neither keeps the default and is checked, and so does a child the older daemon already
// handed over as a root of its own, whose phase rows are its old tree's run: no re-admission is
// left to clear a mark written for it.
func TestTheTakenBackMarkArrivesWith0052(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	var before int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
		where table_name = 'issues' and column_name = 'taken_back'`).Scan(&before); err != nil {
		t.Fatalf("look for issues.taken_back before 0052: %v", err)
	}
	if before != 0 {
		t.Fatalf("issues.taken_back exists at 51, want it to arrive with 0052")
	}
	// Records of the older daemon, which knew no such mark.
	for _, seed := range []string{
		// A root that only waited for its slot.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-910', 'LEGION-910', 'LEGION', 'an older root', 'admitted', 2, 'todo', 'A', 1, true)`,
		// A root running when the daemon was upgraded.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-913', 'LEGION-913', 'LEGION', 'a running root', 'implementing', 1, 'in_progress', 'D', 6, true)`,
		// A root whose tree ran and finished, lingering when the daemon was upgraded.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over, linger_until)
			values ('LEGION-914', 'LEGION-914', 'LEGION', 'a finished tree', 'done', 1, 'done', 'E', 7, true, now() + interval '1 hour')`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-914', 'planner', 'planner-claim', 'plan-1', 0, '')`,
		// A root a person moved out of the waiting line, closed without ever running a phase: its
		// architect, which admission starts, is no phase of its own.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over, linger_until)
			values ('LEGION-915', 'LEGION-915', 'LEGION', 'a root that only waited', 'done', 1, 'backlog', 'F', 8, true, now() + interval '1 hour')`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-915', 'architect', 'architect-claim', '', 0, '')`,
		// A child that ran a phase under LEGION-914's tree and was handed over without its run, now
		// a root of its own waiting at admitted: its planner's claim still names the old tree.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-916', 'LEGION-916', 'LEGION', 'a handed-over child that ran', 'LEGION-914', 'admitted', 2, 'todo', 'G', 9, true)`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('legion-LEGION-916-planner', 'LEGION', 'LEGION-914', 'LEGION-916', 'planner', 1, '', '', 'retired', 0, 0, 0, 0)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-916', 'planner', 'legion-LEGION-916-planner', 'plan-2', 0, '')`,
		// A root whose own tree ran and finished, its implementer's claim naming that tree.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-917', 'LEGION-917', 'LEGION', 'a closed tree', 'done', 2, 'done', 'H', 10, true)`,
		`insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('legion-LEGION-917-implementer', 'LEGION', 'LEGION-917', 'LEGION-917', 'implementer', 1, '', '', 'retired', 0, 0, 0, 0)`,
		`insert into phases (issue, role, claim, handoff_commit, rounds, verdict)
			values ('LEGION-917', 'implementer', 'legion-LEGION-917-implementer', 'impl-1', 0, '')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before 0052: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The column takes the mark both ways, and a row written without it carries none either.
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over, taken_back)
		values ('LEGION-911', 'LEGION-911', 'LEGION', 'a tree taken back', 'admitted', 3, 'todo', 'B', 4, true, true)`); err != nil {
		t.Fatalf("record a taken-back root: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
		values ('LEGION-912', 'LEGION-912', 'LEGION', 'a handed-over child', 'admitted', 2, 'todo', 'C', 5, true)`); err != nil {
		t.Fatalf("record a handed-over child: %v", err)
	}
	for key, want := range map[string]bool{
		"LEGION-910": false, "LEGION-911": true, "LEGION-912": false,
		"LEGION-913": true, "LEGION-914": true, "LEGION-915": false,
		"LEGION-916": false, "LEGION-917": true,
	} {
		var got bool
		if err := store.Pool().QueryRow(ctx, `select taken_back from issues where key = $1`, key).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s taken_back = %t, want %t", key, got, want)
		}
	}
}
