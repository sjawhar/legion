package store

import (
	"context"
	"testing"
)

// 0069-0070 on a database that already holds issues: every stored title gets the lexemes the
// duplicate check reads (api/duplicates.go), the title's own and never its key's, including a title
// whose whole vector passes Postgres's limit on one tsvector, which 0068 let a creation store and
// which a backfill through to_tsvector would fail the boot on (SQLSTATE 54000). The backfill leaves
// search as it was, and the trigger keeps the column for every issue created or retitled after it.
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
		"CORE-5": distinctWords(100_000),
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
	var opening bool
	if err := store.Pool.QueryRow(ctx, `select 'w000001' = any(title_lexemes) and not 'w099999' = any(title_lexemes) from issues where key = 'CORE-5'`).Scan(&opening); err != nil {
		t.Fatalf("read CORE-5: %v", err)
	}
	if !opening {
		t.Error("CORE-5's title_lexemes are not the opening its bounded vector indexes")
	}

	if _, err := store.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-6', 'CORE', 6, 'Launch window settlement', '{"kind":"user","id":"alice"}', 'CORE-6')`); err != nil {
		t.Fatalf("insert after the migration: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update issues set title = 'Recalibrated windows' where key = 'CORE-1'`); err != nil {
		t.Fatalf("retitle: %v", err)
	}
	var stale []string
	rows, err := store.Pool.Query(ctx, `
		select key from issues
		 where title_lexemes is distinct from tsvector_to_array(search_vector('', search_text(title)))
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
