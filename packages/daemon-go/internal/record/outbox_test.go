package record

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

type unsupportedOutboxPayload struct{}

func (unsupportedOutboxPayload) OutboxKind() OutboxKind { return "unsupported" }

func TestOutboxPayloadsRoundTripThroughPostgres(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	payloads := []OutboxPayload{
		StatusWrite{Status: "testing", ObservedStatus: "in_progress"},
		MessagePost{Body: "Pull request checks are blocked."},
		Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: "Tests passed", Version: 4, Reason: ""},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{},
	}
	for _, payload := range payloads {
		t.Run(string(payload.OutboxKind()), func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			records := NewStore()
			row, err := NewOutboxRow("LEGION-208", payload, now)
			if err != nil {
				t.Fatalf("NewOutboxRow: %v", err)
			}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.Enqueue(ctx, tx, row))
			})
			var claimed OutboxRow
			inTx(t, st, func(tx pgx.Tx) {
				rows, err := records.ClaimDue(ctx, tx, now, 1, time.Minute)
				must(t, err)
				if len(rows) != 1 {
					t.Fatalf("ClaimDue = %#v, want one row", rows)
				}
				claimed = rows[0]
			})
			got, err := DecodeOutboxPayload(claimed)
			if err != nil {
				t.Fatalf("DecodeOutboxPayload: %v", err)
			}
			if !reflect.DeepEqual(got, payload) {
				t.Fatalf("payload = %#v, want %#v", got, payload)
			}
		})
	}
}

func TestNewOutboxRowRefusesInvalidPayloads(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		payload OutboxPayload
		reason  string
	}{
		{name: "empty status", payload: StatusWrite{}, reason: "empty status"},
		{name: "unknown notice", payload: Notice{Kind: "unknown"}, reason: "unknown notice kind"},
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "unsupported payload", payload: unsupportedOutboxPayload{}, reason: "unknown outbox payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOutboxRow("LEGION-208", tc.payload, now)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow error = %v, want %q", err, tc.reason)
			}
		})
	}
}

func TestDecodeOutboxPayloadRefusesInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    OutboxRow
		reason string
	}{
		{name: "unknown kind", row: OutboxRow{ID: 31, Kind: "unknown", Payload: json.RawMessage(`{}`)}, reason: "unknown kind"},
		{name: "invalid JSON", row: OutboxRow{ID: 32, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{`)}, reason: "unexpected EOF"},
		{name: "unknown field", row: OutboxRow{ID: 33, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"status":"testing","observedStatus":"todo","surprise":true}`)}, reason: "unknown field"},
		{name: "kind payload mismatch", row: OutboxRow{ID: 34, Kind: OutboxKindDispatchMessage, Payload: json.RawMessage(`{}`)}, reason: "no body"},
		{name: "empty status", row: OutboxRow{ID: 35, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"observedStatus":"todo"}`)}, reason: "empty status"},
		{name: "unknown notice", row: OutboxRow{ID: 36, Kind: OutboxKindNotice, Payload: json.RawMessage(`{"kind":"unknown"}`)}, reason: "unknown notice kind"},
		{name: "unknown supervise operation", row: OutboxRow{ID: 37, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"unknown","tree":"LEGION-208","role":"architect"}`)}, reason: "unknown supervise operation"},
		{name: "start without tree", row: OutboxRow{ID: 38, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"start","role":"architect"}`)}, reason: "start requires tree"},
		{name: "start without role", row: OutboxRow{ID: 39, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"start","tree":"LEGION-208"}`)}, reason: "start requires role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeOutboxPayload(tc.row)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("row %d", tc.row.ID)) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("DecodeOutboxPayload error = %v, want row %d and %q", err, tc.row.ID, tc.reason)
			}
		})
	}
}
