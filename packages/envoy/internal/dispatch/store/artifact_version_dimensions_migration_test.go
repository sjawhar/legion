package store

import (
	"context"
	"testing"
)

// artifactVersionDimensionsVersion is the migration that gives an artifact version the two
// nullable columns a picture's own width and height are recorded in (INFRA-760 R12).
const artifactVersionDimensionsVersion = 20261106013613

// The pair starts null on every stored version, and nothing backfills it: a version stored before
// this migration keeps no shape, which is the state every reader already answers for. The release
// before this one goes on writing versions by the columns it names, so its insert still commits,
// and the new columns take a width and a height where the new build records them.
func TestMigrationLeavesEveryStoredVersionWithNoRecordedShape(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, artifactVersionDimensionsVersion-1)); err != nil {
		t.Fatalf("migrate through the migration before the shape: %v", err)
	}
	for _, seed := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('TEST-1', 'TEST', 1, 'Shaped', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('TEST-1', 'TEST', 'shot-png', 'shot.png', 'image', '{"kind":"user","id":"alice"}')`,
		`insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		 select id, 1, '\x89504e47'::bytea, 'image/png', 4, 'abc', '[]'::jsonb from artifacts where ref_key = 'TEST-1/shot-png'`,
	} {
		if _, err := store.Pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed before the migration: %v", err)
		}
	}
	if err := store.migrate(ctx, migrationsThrough(t, artifactVersionDimensionsVersion)); err != nil {
		t.Fatalf("migrate to the shape: %v", err)
	}
	var stored int
	if err := store.Pool.QueryRow(ctx, `
		select count(*) from artifact_versions where width is null and height is null
	`).Scan(&stored); err != nil {
		t.Fatalf("read the stored version's shape: %v", err)
	}
	if stored != 1 {
		t.Fatalf("versions with no recorded shape = %d, want the one stored before the migration", stored)
	}
	// The release before this one names neither column in its insert, and the columns have no
	// default, so that write still commits and records no shape.
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		select id, 2, '\x89504e47'::bytea, 'image/png', 4, 'def', '[]'::jsonb from artifacts where ref_key = 'TEST-1/shot-png'
	`); err != nil {
		t.Fatalf("the previous release's version write: %v", err)
	}
	// The new build records the pair it read from the picture's header.
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, width, height)
		select id, 3, '\x89504e47'::bytea, 'image/png', 4, 'ghi', '[]'::jsonb, 1600, 900 from artifacts where ref_key = 'TEST-1/shot-png'
	`); err != nil {
		t.Fatalf("the new build's version write: %v", err)
	}
	var shape string
	if err := store.Pool.QueryRow(ctx, `
		select string_agg(concat_ws('x', coalesce(width::text, 'null'), coalesce(height::text, 'null')), ' ' order by number)
		from artifact_versions
	`).Scan(&shape); err != nil {
		t.Fatalf("read every version's shape: %v", err)
	}
	if shape != "nullxnull nullxnull 1600x900" {
		t.Fatalf("the versions' shapes = %q, want %q", shape, "nullxnull nullxnull 1600x900")
	}
}
