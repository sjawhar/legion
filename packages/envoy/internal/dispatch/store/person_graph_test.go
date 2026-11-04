package store

import (
	"context"
	"reflect"
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
		       ('00000000-0000-0000-0000-000000000002', 'CORE-1', '{"kind":"session","id":"s-owned","owner":"Alice"}', 'Owned'),
		       ('00000000-0000-0000-0000-000000000003', 'CORE-1', '{"kind":"session","id":"s-unowned"}', 'Unowned');
		insert into asks (id, issue_key, author, question)
		values ('00000000-0000-0000-0000-000000000004', 'CORE-1', '{"kind":"user","id":"Alice"}', 'Question?');
		insert into messages (id, issue_key, author, body)
		values ('00000000-0000-0000-0000-000000000005', 'CORE-1', '{"kind":"session","id":"s-owned","owner":"alice"}', 'Message');
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
		if err := rows.Scan(&value); err != nil { t.Fatal(err) }
		aliases = append(aliases, value)
	}
	if err := rows.Err(); err != nil { t.Fatal(err) }
	rows.Close()
	want := []string{"alice:actor_id: ALICE ", "alice:actor_id: Alice ", "alice:actor_id:Alice", "alice:login:alice", "alice:session:s-owned", "signed-in@example.com:login:signed-in@example.com"}
	if !reflect.DeepEqual(aliases, want) { t.Fatalf("aliases = %#v, want %#v", aliases, want) }
	rows, err = database.Pool.Query(ctx, `select from_kind || ':' || from_id || '->' || to_kind || ':' || to_id from graph_edges where kind = 'authored_by' order by 1`)
	if err != nil { t.Fatal(err) }
	var edges []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil { t.Fatal(err) }
		edges = append(edges, value)
	}
	if err := rows.Err(); err != nil { t.Fatal(err) }
	rows.Close()
	want = []string{
		"ask:00000000-0000-0000-0000-000000000004->person:alice",
		"comment:00000000-0000-0000-0000-000000000001->person:alice",
		"comment:00000000-0000-0000-0000-000000000002->session:s-owned",
		"comment:00000000-0000-0000-0000-000000000003->session:s-unowned",
		"message:00000000-0000-0000-0000-000000000005->session:s-owned",
	}
	if !reflect.DeepEqual(edges, want) { t.Fatalf("authored edges = %#v, want %#v", edges, want) }
}
