package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// catchUpGatesVersion is the version of the migration this file tests, so the test migrates
// through everything before it whatever lands beside it.
const catchUpGatesVersion = 20261106143638

// A database can hold catch-up notices the previous release queued with a `gate` key when this
// migration arrives. The design gate is a list of steps now, so a catch-up states one entry for
// each step and record.CatchUp has no such field; the payload decodes strictly
// (record.DecodeOutboxPayload), so a row still carrying the key would fail to decode on every
// attempt, telling its architect nothing of its tree and releasing no notice waiting behind it.
// The negative control is that failure read before the migration.
//
// After it, each catch-up decodes as the gate it already stated, read as this release reads a
// stored gate row: the one step spec, gated under `root-issues` and not under `off`, and an open
// entry whose approved version is the version the old payload called open. A root that had
// registered no gate reads as the unregistered step its architect has still to register, and a
// notice that is no catch-up is untouched.
func TestTheCatchUpMigrationRewritesTheGateKeyAsTheSpecStepsEntry(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, catchUpGatesVersion-1)
	insert := `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('notice', $1, $2, 0, now(), '') returning id`
	var registered, unregistered, plain int64
	if err := store.pool.QueryRow(ctx, insert, "LEGION-208",
		`{"kind":"catch-up","role":"architect","reason":"the tree of LEGION-208","catch_up":{"generation":3,"launch":2,
			"gate":{"policy":"root-issues","artifact":"7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26","version":4,"open":true},
			"issues":[{"key":"LEGION-208","title":"a gated root","phase":"implementing","status":"in_progress"}]}}`).Scan(&registered); err != nil {
		t.Fatalf("queue the catch-up of a registered gate the previous release wrote: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, "LEGION-300",
		`{"kind":"catch-up","role":"architect","reason":"the tree of LEGION-300","catch_up":{"generation":1,"launch":1,
			"gate":{"policy":"off","open":false},
			"issues":[{"key":"LEGION-300","title":"a root with no gate","phase":"planning","status":"in_progress"}]}}`).Scan(&unregistered); err != nil {
		t.Fatalf("queue the catch-up of a root that registered nothing: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, "LEGION-209",
		`{"kind":"phase-finished","role":"architect","phase":"testing","summary":"the tests are green"}`).Scan(&plain); err != nil {
		t.Fatalf("queue the notice that carries no catch-up: %v", err)
	}

	before, err := decodeNotice(t, store, registered)
	if err == nil || !strings.Contains(err.Error(), `unknown field "gate"`) {
		t.Fatalf("the catch-up carrying gate decoded as %#v, %v before the migration, want the refusal that names the field: it is the row the migration exists for", before, err)
	}
	t.Logf("before the migration the catch-up carrying gate is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the catch-up gate rewrite: %v", err)
	}

	for _, want := range []struct {
		id      int64
		payload record.Notice
	}{
		{registered, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "the tree of LEGION-208",
			CatchUp: &record.CatchUp{Generation: 3, Launch: 2,
				Gates: []record.CatchUpGate{{Step: record.DesignStepSpec, Gated: true,
					Artifact: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", Version: 4, Approved: 4, Open: true}},
				Issues: []record.CatchUpIssue{{Key: "LEGION-208", Title: "a gated root", Phase: phase.Implementing, Status: "in_progress"}}}}},
		{unregistered, record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "the tree of LEGION-300",
			CatchUp: &record.CatchUp{Generation: 1, Launch: 1,
				Gates:  []record.CatchUpGate{{Step: record.DesignStepSpec, Unregistered: true}},
				Issues: []record.CatchUpIssue{{Key: "LEGION-300", Title: "a root with no gate", Phase: phase.Planning, Status: "in_progress"}}}}},
		{plain, record.Notice{Kind: "phase-finished", Role: claim.RoleArchitect, Phase: phase.Testing, Summary: "the tests are green"}},
	} {
		got, err := decodeNotice(t, store, want.id)
		if err != nil {
			t.Fatalf("decode row %d after the migration: %v", want.id, err)
		}
		if !reflect.DeepEqual(got, want.payload) {
			t.Fatalf("row %d decodes as %#v, want %#v", want.id, got, want.payload)
		}
		told, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal row %d back: %v", want.id, err)
		}
		t.Logf("after the migration row %d is told as %s", want.id, told)
	}
}

// decodeNotice decodes the notice row id holds, as the outbox runner does.
func decodeNotice(t *testing.T, store *Store, id int64) (any, error) {
	t.Helper()
	var payload []byte
	if err := store.pool.QueryRow(context.Background(), "select payload from outbox where id = $1", id).Scan(&payload); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return record.DecodeOutboxPayload(record.OutboxRow{ID: id, Kind: record.OutboxKindNotice, Payload: payload})
}
