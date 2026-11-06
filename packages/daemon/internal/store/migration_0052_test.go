package store

import (
	"context"
	"strings"
	"testing"
)

// The daemon mirrors what Dispatch says about every issue of its project, because the transaction
// that decides whether to admit a root or start a child performs no I/O. 0052 gives that mirror
// its table: a row per key, the keys it is blocked by as an array, the Dispatch sequence it was
// observed at, and an index on the project each consumer reads whole, in the key order it reads
// it in.
func TestTheDispatchMirrorArrivesWith0052(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 51)
	var before int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.tables
		where table_name = 'dispatch_issues'`).Scan(&before); err != nil {
		t.Fatalf("look for dispatch_issues before 0052: %v", err)
	}
	if before != 0 {
		t.Fatalf("dispatch_issues exists at 51, want it to arrive with 0052")
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The index carries the key as well as the project, because every read of the mirror is
	// `where project = $1 order by key`: the ordering comes off the index rather than a sort of
	// the project's rows.
	var indexdef string
	if err := store.Pool().QueryRow(ctx, `select indexdef from pg_indexes
		where tablename = 'dispatch_issues' and indexname = 'dispatch_issues_project'`).Scan(&indexdef); err != nil {
		t.Fatalf("look for the project index: %v", err)
	}
	if !strings.Contains(indexdef, "(project, key)") {
		t.Fatalf("dispatch_issues_project = %q, want it on (project, key)", indexdef)
	}

	if _, err := store.Pool().Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq)
		values ('LEGION-900', 'LEGION', 'todo', array['LEGION-901', 'LEGION-902'], 7)`); err != nil {
		t.Fatalf("insert a mirrored issue: %v", err)
	}
	// An issue waiting on nothing takes the default, so a row written without blockers reads back
	// as an empty list rather than null.
	if _, err := store.Pool().Exec(ctx, `insert into dispatch_issues (key, project, status, dispatch_seq)
		values ('LEGION-901', 'LEGION', 'done', 3)`); err != nil {
		t.Fatalf("insert a mirrored issue with no blockers: %v", err)
	}
	for _, want := range []struct {
		key      string
		status   string
		blockers []string
		seq      int64
	}{
		{key: "LEGION-900", status: "todo", blockers: []string{"LEGION-901", "LEGION-902"}, seq: 7},
		{key: "LEGION-901", status: "done", blockers: []string{}, seq: 3},
	} {
		var status string
		var blockers []string
		var seq int64
		if err := store.Pool().QueryRow(ctx, `select status, blockers, dispatch_seq from dispatch_issues where key = $1`,
			want.key).Scan(&status, &blockers, &seq); err != nil {
			t.Fatalf("read %s: %v", want.key, err)
		}
		if status != want.status || seq != want.seq {
			t.Errorf("%s = status %q at %d, want %q at %d", want.key, status, seq, want.status, want.seq)
		}
		if len(blockers) != len(want.blockers) {
			t.Fatalf("%s blockers = %#v, want %#v", want.key, blockers, want.blockers)
		}
		for i, blocker := range want.blockers {
			if blockers[i] != blocker {
				t.Errorf("%s blocker %d = %q, want %q", want.key, i, blockers[i], blocker)
			}
		}
	}
}
