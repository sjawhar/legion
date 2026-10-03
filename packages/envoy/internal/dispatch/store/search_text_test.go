package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// seedSearchDocument makes a project, an issue and a document artifact to hang versions on, and
// returns the artifact id.
func seedSearchDocument(t *testing.T, ctx context.Context, store *Store) string {
	t.Helper()
	var artifactID string
	if err := store.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		i as (insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-1', 'CORE', 1, 'Search', '{"kind":"user","id":"alice"}', 'A') returning key),
		a as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select 'CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id)
		select id::text from a
	`).Scan(&artifactID); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return artifactID
}

// Search indexing takes time linear in the text (LEGION-465). Postgres's parser rescans a run
// such as `a_a_a_…` from every letter in it: to_tsvector('english', …) alone took 5.0 s, 21 s and
// 84 s for 25, 50 and 100 KB of `a_` on Postgres 16.15, so a 1 MiB body took hours and this
// statement timeout cancelled every shape below on the generated columns 0010 declared. Through
// search_text each takes under a second (0.85 s for 1 MiB of `a_`).
func TestSearchIndexesAPathologicalBodyInBoundedTime(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	artifactID := seedSearchDocument(t, ctx, store)
	for number, shape := range []string{"a_", "1_", "a-b_", "1.2_", "a_1_"} {
		body := strings.Repeat(shape, (1<<20)/len(shape))
		started := time.Now()
		// The insert runs in its own transaction so a statement_timeout bounds it; the rollback
		// releases the connection whatever happened, since Pool.Close waits for every connection.
		err := func() error {
			tx, err := store.Pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "set local statement_timeout = '20s'"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, $3, '[]')`, artifactID, number+1, body); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "57014" {
			t.Fatalf("indexing 1 MiB of %q ran past 20 s: search indexing is not linear in the text", shape)
		}
		if err != nil {
			t.Fatalf("insert 1 MiB of %q: %v", shape, err)
		}
		t.Logf("1 MiB of %q indexed in %s", shape, time.Since(started).Round(time.Millisecond))
	}
}

// search_text changes no lexeme of ordinary text, and none of a pathological body it breaks: a
// run split at an underscore gives the words the parser read between the underscores anyway.
// The corpus is every markdown file of this module (packages/envoy), technical prose with
// identifiers, paths and URLs; of 3,336 documents (the repository's and production's), none
// changed its vector when the function was chosen.
func TestSearchTextKeepsEveryLexeme(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	same := func(name, body string) {
		t.Helper()
		var equal bool
		var lost, gained []string
		if err := store.Pool.QueryRow(ctx, `
			with raw as (select to_tsvector('english', $1) as v), broken as (select to_tsvector('english', search_text($1)) as v)
			select raw.v = broken.v,
			       array(select unnest(tsvector_to_array(raw.v)) except select unnest(tsvector_to_array(broken.v))),
			       array(select unnest(tsvector_to_array(broken.v)) except select unnest(tsvector_to_array(raw.v)))
			  from raw, broken
		`, body).Scan(&equal, &lost, &gained); err != nil {
			t.Fatalf("%s: compare vectors: %v", name, err)
		}
		if !equal {
			t.Errorf("%s: search_text changed the lexemes: lost %q, gained %q", name, lost, gained)
		}
	}
	moduleRoot := filepath.Join("..", "..", "..")
	files := 0
	if err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "out") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		same(path, string(body))
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}
	if files < 10 {
		t.Fatalf("found %d markdown files under %s, want the module's documentation", files, moduleRoot)
	}
	// A pathological body the function breaks, small enough for the raw parse to finish; an empty
	// text (a binary artifact version's, after its coalesce); and snake-case identifiers.
	same("x_ run", strings.Repeat("x_", 5000))
	same("empty", "")
	same("snake runs", strings.Repeat("alpha_beta_gamma_delta_epsilon_zeta_eta_theta_iota_kappa_lambda_mu_nu_xi_omicron_pi_rho_sigma_tau ", 50))
}
