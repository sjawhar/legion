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
		Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: "Tests passed", Verdict: "pass", Version: 4, Reason: ""},
		ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning, Reason: "escalated"},
		ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets", "resource-limits"}},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{Linger: 3},
		MergeQueuePublish{Role: "merge-queue", Packet: "READY #42 at head (approved at head) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"},
		IssueBranch{Generation: 2},
		SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Reason: "LEGION-209 is done"},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7, Release: true},
		AutoMergeDisable{Number: 42},
		ReadyMerge{Number: 42, Head: "c0de"},
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
				rows, err := records.DueRows(ctx, tx, "LEGION", "", nil, now)
				must(t, err)
				if len(rows) != 1 {
					t.Fatalf("DueRows = %#v, want one row", rows)
				}
				var ok bool
				claimed, ok, err = records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
				must(t, err)
				if !ok {
					t.Fatalf("ClaimRow of the one due row claimed nothing")
				}
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

// Every kind the outbox stores runs on exactly one lane, and LaneOf names the lane whose kinds
// list it: a kind on neither lane would be queued and never run, one on both would run twice over.
func TestEveryOutboxKindIsInExactlyOneLane(t *testing.T) {
	every := []OutboxKind{OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindSupervise,
		OutboxKindGateSeed, OutboxKindLingerClose, OutboxKindIssueSuspend, OutboxKindWorkspaceRemove, OutboxKindMergeQueuePublish, OutboxKindIssueBranch, OutboxKindAutoMergeDisable, OutboxKindReadyMerge}
	lanes := map[OutboxKind][]OutboxLane{}
	for _, lane := range []OutboxLane{LaneLifecycle, LaneBoard} {
		for _, kind := range LaneKinds(lane) {
			lanes[kind] = append(lanes[kind], lane)
		}
	}
	for _, kind := range every {
		if len(lanes[kind]) != 1 {
			t.Errorf("%s is on lanes %v, want exactly one", kind, lanes[kind])
			continue
		}
		if got := LaneOf(kind); got != lanes[kind][0] {
			t.Errorf("LaneOf(%s) = %s, want %s, the lane that lists it", kind, got, lanes[kind][0])
		}
	}
	if len(lanes) != len(every) {
		t.Errorf("the lanes list %d kinds, want the %d the outbox stores", len(lanes), len(every))
	}
	if lane := LaneOf("unsupported"); lane != "" {
		t.Errorf("LaneOf of a kind that is not one = %q, want none", lane)
	}
	if kinds := LaneKinds("fast"); kinds != nil {
		t.Errorf("LaneKinds of a lane that is not one = %v, want nil", kinds)
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
		{name: "a triage notice for an architect", payload: Notice{Kind: TriageNotice}, reason: "unknown notice kind"},
		{name: "a slot-free notice for an architect", payload: Notice{Kind: SlotFreeNotice}, reason: "unknown notice kind"},
		{name: "a todo notice for an architect", payload: Notice{Kind: TodoNotice}, reason: "unknown notice kind"},
		{name: "a tick notice for an architect", payload: Notice{Kind: TickNotice}, reason: "unknown notice kind"},
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "a stop that is not a tree close", payload: SuperviseRequest{Op: "stop", Tree: "LEGION-208", Role: claim.RoleArchitect}, reason: "unknown supervise operation"},
		{name: "an issue close naming a linger", payload: SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Linger: 7}, reason: "only a tree close"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "a merge queue publish without a role", payload: MergeQueuePublish{Packet: "READY #42"}, reason: "merge queue publish requires role"},
		{name: "a merge queue publish without a packet", payload: MergeQueuePublish{Role: "merge-queue"}, reason: "merge queue publish requires packet"},
		{name: "a READY merge without a pull request", payload: ReadyMerge{Head: "c0de"}, reason: "READY merge requires its pull request number and head"},
		{name: "a READY merge without a head", payload: ReadyMerge{Number: 42}, reason: "READY merge requires its pull request number and head"},
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

// A releasing suspension is the field's own: an issue_suspend row written before the field existed
// reads as the close that keeps, so no migration rewrites it.
func TestAnIssueSuspensionWithoutTheReleaseFieldKeepsTheSandbox(t *testing.T) {
	row := OutboxRow{ID: 40, Kind: OutboxKindIssueSuspend, Payload: json.RawMessage(`{"tree":"LEGION-208","generation":3,"treeGeneration":7}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("DecodeOutboxPayload: %v", err)
	}
	want := IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7}
	if got != want {
		t.Fatalf("payload = %#v, want %#v with Release false", got, want)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "release") {
		t.Fatalf("a keeping suspension encodes %s, want no release field", encoded)
	}
}

// A tick notice carries the names of the deployment capabilities with no decision through the
// outbox row and back, and a notice with none carries no such field: the controller's payload
// reads `openCapabilities` only when there is a gap to name.
func TestATickNoticeCarriesItsOpenCapabilitiesThroughTheRow(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		payload ControllerNotice
		field   bool
	}{
		{"a tick naming two gaps", ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets", "resource-limits"}}, true},
		{"a tick with no gap", ControllerNotice{Kind: TickNotice}, false},
		{"a slot-free wake", ControllerNotice{Kind: SlotFreeNotice}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := NewOutboxRow("LEGION", tc.payload, now)
			if err != nil {
				t.Fatalf("NewOutboxRow: %v", err)
			}
			if got := strings.Contains(string(row.Payload), `"openCapabilities"`); got != tc.field {
				t.Fatalf("payload %s carries openCapabilities: %t, want %t", row.Payload, got, tc.field)
			}
			got, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("DecodeOutboxPayload: %v", err)
			}
			if !reflect.DeepEqual(got, tc.payload) {
				t.Fatalf("payload = %#v, want %#v", got, tc.payload)
			}
		})
	}
}
