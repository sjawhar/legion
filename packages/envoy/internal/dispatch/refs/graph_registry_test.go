package refs

import (
	"context"
	"testing"
)

func TestEveryGraphEndpointKindHasANodeLoader(t *testing.T) {
	fixture := newBacklinkFixture(t)
	ctx := context.Background()
	// The shared fixture includes one synthetic newer-build reference to test rolling reads.
	// This registry covers kinds this build emits, not that deliberately unsupported row.
	if _, err := fixture.database.Pool.Exec(ctx, `delete from refs where from_kind = 'future' and from_id = 'x#y'`); err != nil {
		t.Fatal(err)
	}
	columnKinds := map[string]bool{
		"refs": true, "block_refs": true, "comment_block_refs": true,
		"message_block_refs": true, "ask_block_refs": true,
	}
	for _, view := range []string{"graph_edges", "graph_person_edges"} {
		var definition string
		if err := fixture.database.Pool.QueryRow(ctx, `select pg_get_viewdef($1::regclass, true)`, view).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		for _, arm := range parseGraphViewArms(t, definition) {
			for _, kind := range []string{arm.fromKind, arm.toKind} {
				if kind == "" {
					if !columnKinds[arm.table] {
						t.Errorf("%s has a new column-sourced endpoint kind in %s; declare and seed its producer", view, arm.table)
					}
				} else if nodeLoaders[kind] == nil {
					t.Errorf("%s emits endpoint kind %q without a node loader", view, kind)
				}
			}
		}
	}
	// The reference-table arms carry their endpoint kinds in columns. This fixture writes
	// every reference producer, so check those actual kinds alongside the views' literals.
	rows, err := fixture.database.Pool.Query(ctx, `
		select from_kind from `+AllEdgesSQL+` g
		union select to_kind from `+AllEdgesSQL+` g`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if nodeLoaders[kind] == nil {
			t.Errorf("stored graph endpoint kind %q has no node loader", kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
