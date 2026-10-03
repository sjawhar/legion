package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/searchtest"
)

// 0069-0070 on a database that already holds issues: every stored title gets the lexemes the
// duplicate check reads (api/duplicates.go), the title's own and never its key's. The backfill
// leaves search as it was, and the trigger keeps the column for every issue created or retitled
// after it, a title whose whole vector passes Postgres's limit on one tsvector included, whose
// lexemes are those of the opening its bounded vector indexes.
func TestTitleLexemesFillEveryStoredTitleAndFollowEveryRetitle(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 68)
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name) values ('CORE', 'Core')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for key, title := range map[string]string{
		"CORE-1": "Calibrate the astrolabes",
		"CORE-2": "the and of",
		"CORE-3": "Core 3 notes",
		"CORE-4": "see /srv/a_b_c_d_e_f_g_h_i_j_k_l_m_n_o_p_q_r.txt",
	} {
		if _, err := store.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'CORE', substr($1, 6)::int, $2, '{"kind":"user","id":"alice"}', $1)`, key, title); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	searchBefore := issueSearchVectors(t, ctx, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply 0069-0070: %v", err)
	}

	if searchAfter := issueSearchVectors(t, ctx, store); len(searchAfter) != len(searchBefore) {
		t.Fatalf("search vectors: %d before, %d after", len(searchBefore), len(searchAfter))
	} else {
		for key, vector := range searchBefore {
			if searchAfter[key] != vector {
				t.Errorf("%s: the backfill changed search", key)
			}
		}
	}
	for key, want := range map[string]string{
		"CORE-1": "{astrolab,calibr}",
		"CORE-2": "{}",
		"CORE-3": "{3,core,note}",
	} {
		var got string
		if err := store.Pool.QueryRow(ctx, `select title_lexemes::text from issues where key = $1`, key).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Errorf("%s title_lexemes = %s, want %s", key, got, want)
		}
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-5', 'CORE', 5, $1, '{"kind":"user","id":"alice"}', 'CORE-5'),
		       ('CORE-6', 'CORE', 6, 'Launch window settlement', '{"kind":"user","id":"alice"}', 'CORE-6')`,
		searchtest.DistinctWords(100_000, "\n")); err != nil {
		t.Fatalf("insert after the migration: %v", err)
	}
	var opening bool
	if err := store.Pool.QueryRow(ctx, `select 'w000001' = any(title_lexemes) and not 'w099999' = any(title_lexemes) from issues where key = 'CORE-5'`).Scan(&opening); err != nil {
		t.Fatalf("read CORE-5: %v", err)
	}
	if !opening {
		t.Error("CORE-5's title_lexemes are not the opening its bounded vector indexes")
	}
	if _, err := store.Pool.Exec(ctx, `update issues set title = 'Recalibrated windows' where key = 'CORE-1'`); err != nil {
		t.Fatalf("retitle: %v", err)
	}
	var stale []string
	rows, err := store.Pool.Query(ctx, `
		select key from issues
		 where title_lexemes is distinct from title_lexemes(title)
		 order by key`)
	if err != nil {
		t.Fatalf("compare title_lexemes with the titles: %v", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stale = append(stale, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("compare title_lexemes with the titles: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("title_lexemes do not hold their titles' lexemes on %v", stale)
	}
}

// 0070 holds no issue's row while it waits for a write that holds one. Its update locks every
// issue's row in the order the rows lie on disk, and a reparent locks the issue and its new parent
// in key order (lockIssueAndParent, api/issue_parent.go:54-61), which is text order: reparenting
// CORE-9 under CORE-10 locks CORE-10, the later row on disk, first. A reparent that held CORE-10
// while the update, already holding CORE-9, waited for it, and then asked for CORE-9, closed a
// cycle, and Postgres's deadlock check killed one side with 40P01: the boot, or the reparent with
// a 500. 0070 takes issues EXCLUSIVE before it writes, so it waits for the reparent's table lock
// holding nothing, the reparent's second row lock is granted, it commits, and 0070 then runs.
func TestMigrate0070WaitsForAReparentWithoutDeadlockingIt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 69)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'CORE-' || n, 'CORE', n, 'Calibration window ' || n, '{"kind":"user","id":"alice"}', lpad(n::text, 3, '0')
		  from generate_series(1, 10) as n;`); err != nil {
		t.Fatalf("seed issues: %v", err)
	}
	reparent, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reparent: %v", err)
	}
	defer reparent.Rollback(ctx)
	lock := func(key string) error {
		_, err := reparent.Exec(ctx, `select project_key from issues where key = $1 for no key update`, key)
		return err
	}
	if err := lock("CORE-10"); err != nil {
		t.Fatalf("lock CORE-10: %v", err)
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.Migrate(ctx) }()
	waitForSessionBlockedBy(t, store, reparent.Conn().PgConn().PID())
	if err := lock("CORE-9"); err != nil {
		t.Fatalf("lock CORE-9 while 0070 waited for the reparent: %v", err)
	}
	if _, err := reparent.Exec(ctx, `update issues set parent_key = 'CORE-10' where key = 'CORE-9'`); err != nil {
		t.Fatalf("reparent CORE-9: %v", err)
	}
	if err := reparent.Commit(ctx); err != nil {
		t.Fatalf("commit the reparent: %v", err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("0070 failed while a reparent held an issue's row: %v", err)
	}
}

// waitForSessionBlockedBy waits until some session waits for a lock pid holds. It fails t after
// 20 s.
func waitForSessionBlockedBy(t *testing.T, store *Store, pid uint32) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var blocked bool
		if err := store.Pool.QueryRow(ctx, `
			select exists(select 1 from pg_stat_activity where $1 = any(pg_blocking_pids(pid)))
		`, int32(pid)).Scan(&blocked); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session was seen waiting for backend %d", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func issueSearchVectors(t *testing.T, ctx context.Context, store *Store) map[string]string {
	t.Helper()
	rows, err := store.Pool.Query(ctx, `select key, search::text from issues`)
	if err != nil {
		t.Fatalf("read search vectors: %v", err)
	}
	defer rows.Close()
	vectors := map[string]string{}
	for rows.Next() {
		var key, vector string
		if err := rows.Scan(&key, &vector); err != nil {
			t.Fatalf("scan search vector: %v", err)
		}
		vectors[key] = vector
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read search vectors: %v", err)
	}
	return vectors
}
