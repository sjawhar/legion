package store

import (
	"context"
	"testing"
)

// A root an older daemon already made of a member of another tree stays its own tree after the
// upgrade, as one this daemon makes does: the older daemon adopted nothing, and the first admission
// sweep after 0055 would otherwise take it back into the tree it left once that tree is live. 0055
// marks the rows that still show the tree they left: an outbox hand-over the daemon processed, or a
// phase worker's claim still naming another tree. A root that only waited, a root that re-admitted
// itself after a trip to backlog (INFRA-214's case), and a member still in its tree keep the
// default, so each stays adoptable or unaffected as before.
func TestTheTreeARootLeftIsBackfilledBy0055(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 54)
	claim := func(token, tree, issue string) string {
		return `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('` + token + `', 'LEGION', '` + tree + `', '` + issue + `', 'planner', 1, '', '', 'retired', 0, 0, 0, 0)`
	}
	for _, seed := range []string{
		// The old tree a child was handed over from.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-920', 'LEGION-920', 'LEGION', 'a finished tree', 'done', 1, 'done', 'A', 1, true)`,
		// A child the outbox handed over without its run, never started: no claim, no mark of a run.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-921', 'LEGION-921', 'LEGION', 'an uncarried hand-over', 'LEGION-920', 'admitted', 2, 'todo', 'B', 2, true)`,
		`insert into processed_events (source, event_id) values ('outbox', 'handover:LEGION-921:77')`,
		// Another outbox row's event whose key only starts like the first: not a hand-over of LEGION-92.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-92', 'LEGION-92', 'LEGION', 'a root whose key prefixes another', 'LEGION-920', 'admitted', 1, 'todo', 'C', 3, true)`,
		// A child that ran a worker under the old tree, admitted as an orphan by a person, since gone
		// to backlog: its planner's claim still names the old tree.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-922', 'LEGION-922', 'LEGION', 'an orphan that ran', 'LEGION-920', 'done', 2, 'backlog', 'D', 4, true)`,
		claim("legion-LEGION-922-planner", "LEGION-920", "LEGION-922"),
		// A root that re-admitted itself after a trip to backlog, its own worker's claim on its own tree.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-923', 'LEGION-923', 'LEGION', 'a root back from backlog', 'LEGION-930', 'admitted', 3, 'todo', 'E', 5, true)`,
		claim("legion-LEGION-923-planner", "LEGION-923", "LEGION-923"),
		// A member still in its tree, with a hand-over event the reopened root fenced off.
		`insert into issues (key, tree, project, title, parent, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-924', 'LEGION-920', 'LEGION', 'a member still in its tree', 'LEGION-920', 'planning', 1, 'in_progress', 'F', 6, false)`,
		`insert into processed_events (source, event_id) values ('outbox', 'handover:LEGION-924:78')`,
		// A hand-over event recorded by another source does not count.
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, handed_over)
			values ('LEGION-925', 'LEGION-925', 'LEGION', 'a root that only waited', 'admitted', 1, 'todo', 'G', 7, true)`,
		`insert into processed_events (source, event_id) values ('api', 'handover:LEGION-925:79')`,
	} {
		if _, err := store.Pool().Exec(ctx, seed); err != nil {
			t.Fatalf("record before 0055: %v", err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for key, want := range map[string]string{
		"LEGION-920": "",
		"LEGION-921": "LEGION-920",
		"LEGION-92":  "",
		"LEGION-922": "LEGION-920",
		"LEGION-923": "",
		"LEGION-924": "",
		"LEGION-925": "",
	} {
		var got string
		if err := store.Pool().QueryRow(ctx, `select handed_over_from from issues where key = $1`, key).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s handed_over_from = %q, want %q", key, got, want)
		}
	}
}
