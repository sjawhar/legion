package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A database can hold supervise start rows the previous release queued with a `quiesce` key when
// 0065 arrives. A start waits for no turn of the role it takes the phase from, so the payload has
// no such field and decodes strictly (record.DecodeOutboxPayload), so a row still carrying
// the key would fail to decode on every attempt and stall its issue's lifecycle lane. The negative
// control is that failure read before the migration; the migration strips the key and leaves
// everything else of the row as it is, so the start then runs as the start it already was. A row
// that never had it is untouched.
func TestTheSuperviseMigrationStripsTheQuiesceKeyFromQueuedRows(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 64)
	insert := `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ('supervise', $1, $2, 0, now(), '') returning id`
	var takingOver, plain int64
	if err := store.pool.QueryRow(ctx, insert, "LEGION-208",
		`{"op":"start","tree":"LEGION-208","role":"implementer","task":"fix the red","generation":3,"phase":"implementing","quiesce":"tester"}`).Scan(&takingOver); err != nil {
		t.Fatalf("queue the taking-over start the previous release wrote: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, "LEGION-209",
		`{"op":"start","tree":"LEGION-208","role":"tester","task":"test it","generation":4,"phase":"testing"}`).Scan(&plain); err != nil {
		t.Fatalf("queue the start written without the field: %v", err)
	}
	before, err := decodeRow(t, store, takingOver)
	if err == nil || !strings.Contains(err.Error(), `unknown field "quiesce"`) {
		t.Fatalf("the start carrying quiesce decoded as %#v, %v before the migration, want the refusal that names the field: it is the row the migration exists for", before, err)
	}
	t.Logf("before the migration the start carrying quiesce is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the quiesce-key strip: %v", err)
	}
	for _, want := range []struct {
		id      int64
		payload record.SuperviseRequest
	}{
		{takingOver, record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: "implementer", Task: "fix the red", Generation: 3, Phase: "implementing"}},
		{plain, record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: "tester", Task: "test it", Generation: 4, Phase: "testing"}},
	} {
		got, err := decodeRow(t, store, want.id)
		if err != nil {
			t.Fatalf("decode row %d after the migration: %v", want.id, err)
		}
		if got != want.payload {
			t.Fatalf("row %d decodes as %#v, want %#v", want.id, got, want.payload)
		}
		t.Logf("after the migration row %d runs as %#v", want.id, got)
	}
}

// decodeRow decodes the supervise row id holds, as the outbox runner does.
func decodeRow(t *testing.T, store *Store, id int64) (any, error) {
	t.Helper()
	var payload []byte
	if err := store.pool.QueryRow(context.Background(), "select payload from outbox where id = $1", id).Scan(&payload); err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return record.DecodeOutboxPayload(record.OutboxRow{ID: id, Kind: record.OutboxKindSupervise, Payload: payload})
}
