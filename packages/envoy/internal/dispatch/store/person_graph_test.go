package store

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

func TestPersonAliasesAndAuthoredEdgesUseStoredActors(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'Graph', '{"kind":"user","id":" Alice "}', 'U');
		insert into people (email) values ('signed-in@example.com');
		insert into comments (id, issue_key, author, body)
		values ('00000000-0000-0000-0000-000000000001', 'CORE-1', '{"kind":"user","id":" ALICE "}', 'Person'),
		       ('00000000-0000-0000-0000-000000000002', 'CORE-1', '{"kind":"session","id":"s-owned","owner":"Alice","session_verified":true}', 'Owned'),
		       ('00000000-0000-0000-0000-000000000003', 'CORE-1', '{"kind":"session","id":"s-unowned"}', 'Unowned');
		insert into asks (id, issue_key, author, question)
		values ('00000000-0000-0000-0000-000000000004', 'CORE-1', '{"kind":"user","id":"Alice"}', 'Question?');
		insert into messages (id, issue_key, author, body)
		values ('00000000-0000-0000-0000-000000000005', 'CORE-1', '{"kind":"session","id":"s-owned","owner":"alice","session_verified":true}', 'Message');
	`); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Pool.Query(ctx, `select person || ':' || alias_kind || ':' || alias from person_aliases order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	var aliases []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		aliases = append(aliases, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	slices.Sort(aliases)
	want := []string{"alice:actor_id: ALICE ", "alice:actor_id: Alice ", "alice:actor_id:Alice", "alice:login:alice", "alice:session:s-owned", "signed-in@example.com:login:signed-in@example.com"}
	if !reflect.DeepEqual(aliases, want) {
		t.Fatalf("aliases = %#v, want %#v", aliases, want)
	}
	rows, err = database.Pool.Query(ctx, `select from_kind || ':' || from_id || '->' || to_kind || ':' || to_id from graph_person_edges where kind = 'authored_by' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	var edges []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	want = []string{
		"ask:00000000-0000-0000-0000-000000000004->person:alice",
		"comment:00000000-0000-0000-0000-000000000001->person:alice",
		"comment:00000000-0000-0000-0000-000000000002->session:s-owned",
		"comment:00000000-0000-0000-0000-000000000003->session:s-unowned",
		"message:00000000-0000-0000-0000-000000000005->session:s-owned",
	}
	if !reflect.DeepEqual(edges, want) {
		t.Fatalf("authored edges = %#v, want %#v", edges, want)
	}
}

func TestPersonAliasesIncludeEventClaimAndDocumentActorsWithoutInventingRoles(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, claimed_by, claimed_at, route, rank)
		values ('CORE-1', 'CORE', 1, 'Graph', '{"kind":"system","id":"seed"}', '{"kind":"user","id":" Claim "}', now(), 'role:reviewer', 'U');
		insert into events (issue_key, seq, type, actor, notify, payload) values
		('CORE-1', 1, 'issue.updated', '{"kind":"user","id":" Event "}', false, '{}'),
		('CORE-1', 2, 'issue.updated', '{"kind":"session","id":"token-session","owner":" Owner ","session_verified":true}', false, '{}');
		with document as (
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', '{"kind":"user","id":" Creator "}') returning id
		) insert into artifact_versions (artifact_id, number, markdown, authors)
		select id, 1, 'Words', '[{"kind":"user","id":" Credit "}]' from document;
	`); err != nil {
		t.Fatal(err)
	}
	rows, err := database.Pool.Query(ctx, `select person || ':' || alias_kind || ':' || alias from person_aliases`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	slices.Sort(got)
	want := []string{"claim:actor_id: Claim ", "claim:login:claim", "creator:actor_id: Creator ", "creator:login:creator", "credit:actor_id: Credit ", "credit:login:credit", "event:actor_id: Event ", "event:login:event", "owner:login:owner", "owner:session:token-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %#v, want %#v", got, want)
	}
}

func TestPersonMigrationLeavesExistingGraphViewUnchanged(t *testing.T) {
	ctx := context.Background()
	database := openEmptyTestStore(t)
	migrateThrough(t, database, 20261104074750)
	var before, after string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("person migration changed the existing graph view")
	}
	var sameColumns bool
	if err := database.Pool.QueryRow(ctx, `select
		array(select attname || ':' || atttypid::text from pg_attribute where attrelid = 'graph_edges'::regclass and attnum > 0 order by attnum)
		= array(select attname || ':' || atttypid::text from pg_attribute where attrelid = 'graph_person_edges'::regclass and attnum > 0 order by attnum)
	`).Scan(&sameColumns); err != nil {
		t.Fatal(err)
	}
	if !sameColumns {
		t.Fatal("the two derived graph relations must have the same columns and types")
	}
}
