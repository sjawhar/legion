package store

import (
	"context"
	"testing"
)

// searchRowShapes writes one row of every shape the five search columns index, under the suffix,
// and returns a label and the query reading each row's vector as text (NULL reads as "<null>").
// Shapes: an issue; a markdown document version, a binary version (markdown NULL) and an empty
// markdown version; a comment; an ask with no options or answer, and one answered with options
// and a selection; a message.
func searchRowShapes(t *testing.T, ctx context.Context, store *Store, suffix string) map[string]string {
	t.Helper()
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core') on conflict do nothing
	`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	key := "CORE-" + suffix
	if _, err := store.Pool.Exec(ctx, `
		with i as (insert into issues (key, project_key, number, title, created_by, rank) values ($1, 'CORE', $2, 'Navigation instruments ' || $3, '{"kind":"user","id":"alice"}', $3) returning key),
		a as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select $1, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id),
		b as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select $1, 'CORE', 'photo', 'photo.png', 'image', false, '{"kind":"user","id":"alice"}' from i returning id),
		v1 as (insert into artifact_versions (artifact_id, number, markdown, authors) select id, 1, 'The astrolabe measures altitude.', '[]' from a),
		v2 as (insert into artifact_versions (artifact_id, number, markdown, authors) select id, 2, '', '[]' from a),
		v3 as (insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors) select id, 1, '\x89504e47'::bytea, 'image/png', 4, 'abc', '[]' from b),
		c as (insert into comments (issue_key, author, body) select $1, '{"kind":"user","id":"alice"}', 'Replace the sextant diagram.' from i),
		k1 as (insert into asks (issue_key, author, question) select $1, '{"kind":"user","id":"alice"}', 'Keep the quadrant?' from i),
		k2 as (insert into asks (issue_key, author, question, options, state, answer) select $1, '{"kind":"user","id":"alice"}', 'Which alidade?', '[{"label":"Brass","description":"the heavy one"},{"label":"Steel"}]', 'answered', '{"text":"Use the brass one.","selected":["Brass"]}' from i),
		m as (insert into messages (issue_key, author, body) select $1, '{"kind":"session","id":"s1"}', 'Compass calibration done.' from i)
		select 1
	`, key, len(suffix), suffix); err != nil {
		t.Fatalf("seed rows %s: %v", suffix, err)
	}
	return map[string]string{
		"issue":                  `select coalesce(search::text, '<null>') from issues where key = '` + key + `'`,
		"document version":       `select coalesce(v.search::text, '<null>') from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '` + key + `' and a.slug = 'spec' and v.number = 1`,
		"empty markdown version": `select coalesce(v.search::text, '<null>') from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '` + key + `' and a.slug = 'spec' and v.number = 2`,
		"binary version":         `select coalesce(v.search::text, '<null>') from artifact_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '` + key + `' and a.slug = 'photo'`,
		"comment":                `select coalesce(search::text, '<null>') from comments where issue_key = '` + key + `'`,
		"open ask":               `select coalesce(search::text, '<null>') from asks where issue_key = '` + key + `' and state = 'open'`,
		"answered ask":           `select coalesce(search::text, '<null>') from asks where issue_key = '` + key + `' and state = 'answered'`,
		"message":                `select coalesce(search::text, '<null>') from messages where issue_key = '` + key + `'`,
	}
}

func readVectors(t *testing.T, ctx context.Context, store *Store, queries map[string]string) map[string]string {
	t.Helper()
	vectors := map[string]string{}
	for shape, query := range queries {
		var vector string
		if err := store.Pool.QueryRow(ctx, query).Scan(&vector); err != nil {
			t.Fatalf("read %s vector: %v", shape, err)
		}
		vectors[shape] = vector
	}
	return vectors
}

// 0056-0062 on a database that already holds rows: every stored vector of every row shape survives
// the conversion from a generated column to a trigger-filled one (a binary version's stays NULL, as
// 0019's expression left it), rows written after the migration are indexed by the triggers
// exactly as the generated columns indexed the same shapes before it, and the one row whose text
// holds a run of sixteen underscore-joined segments is re-indexed by the new expression.
func TestSearchTextMigrationKeepsEveryRowShapesVector(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 55)
	generated := searchRowShapes(t, ctx, store, "a")
	before := readVectors(t, ctx, store, generated)
	// What the generated columns gave each shape on main: a binary version's vector is NULL (0019
	// dropped 0010's coalesce; every function in the expression is STRICT), an empty markdown's is
	// the empty vector, and the trigger reproduces both.
	if before["binary version"] != "<null>" || before["empty markdown version"] != "" {
		t.Fatalf("the generated column gave a binary version %q and an empty markdown %q; the test expects NULL and the empty vector", before["binary version"], before["empty markdown version"])
	}
	// Sixteen underscore-joined segments inside a path the parser keeps whole as one lexeme: the
	// one shape whose vector the new expression changes, so 0062 must re-index it.
	const candidate = "see /srv/a_b_c_d_e_f_g_h_i_j_k_l_m_n_o_p_q_r.txt today"
	if _, err := store.Pool.Exec(ctx, `insert into messages (issue_key, author, body) values ('CORE-a', '{"kind":"user","id":"alice"}', $1)`, candidate); err != nil {
		t.Fatalf("insert candidate: %v", err)
	}
	var candidateBefore string
	if err := store.Pool.QueryRow(ctx, `select search::text from messages where body = $1`, candidate).Scan(&candidateBefore); err != nil {
		t.Fatalf("read candidate before: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("apply 0056-0062: %v", err)
	}

	after := readVectors(t, ctx, store, generated)
	for shape, vector := range before {
		if after[shape] != vector {
			t.Errorf("%s: its stored vector changed across the migration:\n before %s\n after  %s", shape, vector, after[shape])
		}
	}
	triggered := readVectors(t, ctx, store, searchRowShapes(t, ctx, store, "bb"))
	for shape, vector := range before {
		// The second seed's issue title and rank carry the suffix; everything else is the same text.
		if shape == "issue" {
			continue
		}
		if triggered[shape] != vector {
			t.Errorf("%s: the trigger indexed a new row otherwise than the generated column indexed the same text:\n generated %s\n trigger   %s", shape, vector, triggered[shape])
		}
	}
	var candidateAfter, candidateExpected string
	if err := store.Pool.QueryRow(ctx, `select search::text, to_tsvector('english', search_text($1))::text from messages where body = $1`, candidate).Scan(&candidateAfter, &candidateExpected); err != nil {
		t.Fatalf("read candidate after: %v", err)
	}
	if candidateAfter == candidateBefore || candidateAfter != candidateExpected {
		t.Errorf("the candidate row was not re-indexed by the new expression:\n before   %s\n after    %s\n expected %s", candidateBefore, candidateAfter, candidateExpected)
	}
}
