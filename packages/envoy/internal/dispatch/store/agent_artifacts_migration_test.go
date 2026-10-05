package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 0070 makes an agent's session a third artifact owner. Every artifact written before it keeps its
// ref_key; a session's artifact is addressed agent/<session>/<slug>; ref_key is the trigger's to
// write, as it was the generated column's; and a row with no owner, or two, is refused.
func TestMigrate0070AddressesAnAgentsArtifactsAndKeepsEveryOtherRefKey(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 69)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) values
			('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			(null, 'CORE', 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed artifacts at 0069: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0070: %v", err)
	}

	refKeys := func() map[string]string {
		t.Helper()
		rows, err := store.Pool.Query(ctx, `select name, ref_key from artifacts`)
		if err != nil {
			t.Fatalf("read ref keys: %v", err)
		}
		defer rows.Close()
		keys := map[string]string{}
		for rows.Next() {
			var name, refKey string
			if err := rows.Scan(&name, &refKey); err != nil {
				t.Fatalf("scan ref key: %v", err)
			}
			keys[name] = refKey
		}
		return keys
	}
	if keys := refKeys(); keys["spec.md"] != "CORE-1/spec" || keys["notes.md"] != "CORE/notes" {
		t.Fatalf("ref keys after 0070 = %v, want CORE-1/spec and CORE/notes kept", keys)
	}

	var refKey string
	if err := store.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('01a1058e-f14f', 'shot-png', 'shot.png', 'image', '{"kind":"user","id":"alice"}')
		returning ref_key
	`).Scan(&refKey); err != nil {
		t.Fatalf("insert a session's artifact: %v", err)
	}
	if refKey != "agent/01a1058e-f14f/shot-png" {
		t.Fatalf("session artifact ref_key = %q, want agent/01a1058e-f14f/shot-png", refKey)
	}
	if _, err := store.Pool.Exec(ctx, `update artifacts set ref_key = 'CORE/other' where name = 'notes.md'`); err != nil {
		t.Fatalf("write ref_key directly: %v", err)
	}
	if keys := refKeys(); keys["notes.md"] != "CORE/notes" {
		t.Fatalf("ref_key after a direct write = %q, want CORE/notes from the row", keys["notes.md"])
	}
	if _, err := store.Pool.Exec(ctx, `update artifacts set slug = 'notes-2' where name = 'notes.md'`); err != nil {
		t.Fatalf("rename a slug: %v", err)
	}
	if keys := refKeys(); keys["notes.md"] != "CORE/notes-2" {
		t.Fatalf("ref_key after a slug change = %q, want CORE/notes-2 from the row", keys["notes.md"])
	}

	for name, insert := range map[string]string{
		"no owner":                `insert into artifacts (slug, name, kind, created_by) values ('a', 'a', 'file', '{}')`,
		"a session and a project": `insert into artifacts (session_id, project_key, slug, name, kind, created_by) values ('s', 'CORE', 'b', 'b', 'file', '{}')`,
		"a session and an issue":  `insert into artifacts (session_id, issue_key, slug, name, kind, created_by) values ('s', 'CORE-1', 'c', 'c', 'file', '{}')`,
		"an empty session":        `insert into artifacts (session_id, slug, name, kind, created_by) values ('', 'd', 'd', 'file', '{}')`,
	} {
		_, err := store.Pool.Exec(ctx, insert)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "artifacts_one_owner" {
			t.Errorf("%s: %v, want artifacts_one_owner to refuse it", name, err)
		}
	}
	_, err := store.Pool.Exec(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ('01a1058e-f14f', 'shot-png', 'other.png', 'image', '{}')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "artifacts_ref_key_key" {
		t.Errorf("a second agent/01a1058e-f14f/shot-png: %v, want artifacts_ref_key_key to refuse it", err)
	}
}
