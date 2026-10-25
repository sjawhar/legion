package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// architectureSourcesPerRepoVersion is the migration that lets a project hold one architecture
// source per repository and records which source projects each component (INFRA-391).
const architectureSourcesPerRepoVersion = 117

// seedSingleSourceProject seeds, at the schema before the migration, project CORE as the earlier
// release left it: one imported source, its snapshot, components api and store with api depending
// on store, and CORE-1 attached to api.
func seedSingleSourceProject(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by, last_sync_at, last_commit, last_tree_sha)
			values ('CORE', 'legion/arch', 'main', 42, '{"kind":"user","id":"alice"}', '2026-10-01T12:00:00Z', 'commit-one', 'tree-one');
		insert into architecture_snapshots (id, project_key, commit, files) values (500, 'CORE', 'commit-one', '{"api.md": "x"}');
		insert into components (project_key, id, title, prose, parent, external, paths, snapshot_id) values
			('CORE', 'store', 'Store', 'The store.', null, true, '{}', 500),
			('CORE', 'api', 'HTTP API', 'The API.', 'store', false, '{internal/api}', 500);
		insert into component_depends (project_key, from_id, to_id) values ('CORE', 'api', 'store');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
		insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit');
		insert into issue_component_members (issue_key, project_key, component_id) values ('CORE-1', 'CORE', 'api');
	`); err != nil {
		t.Fatalf("seed before the migration: %v", err)
	}
}

// architectureState reads every row the migration must leave as it found it, as text: the source
// without its new id, each component without its new source_id, the dependency edges, the
// snapshots, the attachments and the component arms of graph_edges.
func architectureState(t *testing.T, store *Store) string {
	t.Helper()
	var state string
	if err := store.Pool.QueryRow(context.Background(), `
		select concat_ws(E'\n',
			(select string_agg((to_jsonb(s) - 'id')::text, E'\n' order by s.project_key) from architecture_sources s),
			(select string_agg((to_jsonb(c) - 'source_id')::text, E'\n' order by c.project_key, c.id) from components c),
			(select string_agg(to_jsonb(d)::text, E'\n' order by d.project_key, d.from_id, d.to_id) from component_depends d),
			(select string_agg(to_jsonb(a)::text, E'\n' order by a.id) from architecture_snapshots a),
			(select string_agg(to_jsonb(m)::text, E'\n' order by m.issue_key, m.component_id) from issue_component_members m),
			(select string_agg(concat_ws(' ', g.from_id, g.kind, g.to_id), E'\n' order by g.from_id, g.kind, g.to_id)
			   from graph_edges g where g.to_kind = 'component'))
	`).Scan(&state); err != nil {
		t.Fatalf("read the architecture rows: %v", err)
	}
	return state
}

// A project with one source keeps it, its components, their edges, its snapshot and every
// attachment through the migration, with nothing re-imported: the source reads id 0 with every
// bookkeeping column as it was (so the next sync takes the unchanged-head shortcut), each component
// reads source_id 0, and a second repository of the same project becomes a source with an id above
// 0. A repository is a source of a project once, every component names a source of its project, and
// deleting a source deletes its components and their edges.
func TestMigrationKeepsAProjectsOneSourceAndItsComponents(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, architectureSourcesPerRepoVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", architectureSourcesPerRepoVersion-1, err)
	}
	seedSingleSourceProject(t, store)
	before := architectureState(t, store)
	if err := store.migrate(ctx, migrationsThrough(t, architectureSourcesPerRepoVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", architectureSourcesPerRepoVersion, err)
	}
	if after := architectureState(t, store); after != before {
		t.Fatalf("the migration changed the architecture rows:\nbefore\n%s\nafter\n%s", before, after)
	}
	if !strings.Contains(before, `"last_commit": "commit-one"`) || !strings.Contains(before, `"last_tree_sha": "tree-one"`) ||
		!strings.Contains(before, `"installation_id": 42`) || !strings.Contains(before, "CORE/api depends_on CORE/store") {
		t.Fatalf("the seeded state lacks what it is meant to hold:\n%s", before)
	}
	var sourceIDs, componentSources string
	if err := store.Pool.QueryRow(ctx, `
		select (select string_agg(id::text, ',') from architecture_sources),
		       (select string_agg(id || '=' || source_id, ',' order by id) from components)
	`).Scan(&sourceIDs, &componentSources); err != nil {
		t.Fatalf("read the new columns: %v", err)
	}
	if sourceIDs != "0" || componentSources != "api=0,store=0" {
		t.Fatalf("source ids %q, component sources %q; want the existing source at 0 and both components on it", sourceIDs, componentSources)
	}

	var second int64
	if err := store.Pool.QueryRow(ctx, `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'acme/infra', 'main', 7, '{"kind":"user","id":"alice"}') returning id
	`).Scan(&second); err != nil {
		t.Fatalf("add a second source to CORE: %v", err)
	}
	if second <= 0 {
		t.Fatalf("a new source of CORE got id %d, want one above 0", second)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'acme/infra', 'release', 7, '{"kind":"user","id":"alice"}')
	`); !isSQLState(err, "23505") {
		t.Fatalf("acme/infra a second time in CORE: %v, want unique_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into components (project_key, id, title, prose, snapshot_id, source_id) values ('CORE', 'ghost', 'Ghost', '', 500, 99)
	`); !isSQLState(err, "23503") {
		t.Fatalf("a component naming no source of CORE: %v, want foreign_key_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into components (project_key, id, title, prose, snapshot_id) values ('CORE', 'unnamed', 'Unnamed', '', 500)
	`); !isSQLState(err, "23502") {
		t.Fatalf("a component naming no source_id: %v, want not_null_violation (no default stands in for it)", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into components (project_key, id, title, prose, snapshot_id, source_id) values ('CORE', 'infra', 'Infra', '', 500, $1)
	`, second); err != nil {
		t.Fatalf("a component of the second source: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `delete from architecture_sources where project_key = 'CORE' and id = 0`); err != nil {
		t.Fatalf("delete the legacy source: %v", err)
	}
	var components string
	var edges int
	if err := store.Pool.QueryRow(ctx, `
		select (select string_agg(id, ',' order by id) from components), (select count(*) from component_depends)
	`).Scan(&components, &edges); err != nil {
		t.Fatalf("read what the delete left: %v", err)
	}
	if components != "infra" || edges != 0 {
		t.Fatalf("after deleting source 0: components %q, edges %d; want only the second source's infra and no edge", components, edges)
	}
}

// The census counts the components the new foreign key refuses, those of a project with no source,
// at the schema before the migration: such a component counts 1 while a project's own source's
// components count nothing, and the migration then refuses to apply over it.
func TestCensusCountsAComponentWhoseProjectHasNoSource(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, architectureSourcesPerRepoVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", architectureSourcesPerRepoVersion-1, err)
	}
	seedSingleSourceProject(t, store)
	if got := architectureSourcesCensus(t, store); got != 0 {
		t.Fatalf("census over a project whose components have their source = %d, want 0", got)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('LONE', 'Lone');
		insert into architecture_snapshots (id, project_key, commit, files) values (600, 'LONE', 'c1', '{}');
		insert into components (project_key, id, title, prose, snapshot_id) values ('LONE', 'orphan', 'Orphan', '', 600);
	`); err != nil {
		t.Fatalf("seed a component with no source: %v", err)
	}
	if got := architectureSourcesCensus(t, store); got != 1 {
		t.Fatalf("census = %d, want 1: LONE/orphan alone has no source", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, architectureSourcesPerRepoVersion)); err == nil {
		t.Fatal("the migration applied over a component whose project has no source")
	}
	if got := schemaVersion(t, store); got != architectureSourcesPerRepoVersion-1 {
		t.Fatalf("schema version after the refusal = %d, want %d", got, architectureSourcesPerRepoVersion-1)
	}
}

// architectureSourcesCensus takes the pre-deploy census of the migration and answers its count.
func architectureSourcesCensus(t *testing.T, store *Store) int64 {
	t.Helper()
	report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationsThrough(t, architectureSourcesPerRepoVersion), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Pending) != 1 || report.Pending[0].Count == nil {
		t.Fatalf("census did not answer one count:\n%s", reportText(report))
	}
	return *report.Pending[0].Count
}
