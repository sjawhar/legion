package record

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

type unsupportedOutboxPayload struct{}

func (unsupportedOutboxPayload) OutboxKind() OutboxKind { return "unsupported" }

func TestOutboxPayloadsRoundTripThroughPostgres(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	payloads := []OutboxPayload{
		StatusWrite{Status: "testing", ObservedStatus: "in_progress"},
		StatusWrite{Status: "todo", ObservedStatus: "triage", PersonHandover: true},
		MessagePost{Body: "Pull request checks are blocked."},
		Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: "Tests passed", Verdict: "pass", Version: 4, Reason: ""},
		ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning, Reason: "escalated"},
		ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets", "resource-limits"}},
		ControllerNotice{Kind: UpdateStalledNotice, Reason: "the round has waited 4h0m0s", Update: &UpdateStalled{
			Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Reasons: "image ghcr.io/cybermonklabs/legion-worker:v1, now ghcr.io/cybermonklabs/legion-worker:v2",
			Waiting: []UpdateWaiting{{Claim: "legion-LEGION-208-implementer", Role: claim.RoleImplementer, State: "failed"}},
		}},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{Linger: 3},
		MergeQueuePublish{Role: "merge-queue", Packet: "READY #42 at head (approved at head) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"},
		GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepCriteria, DesignStepSpec}},
		IssueBranch{Generation: 2},
		SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Reason: "LEGION-209 is done"},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7},
		AutoMergeDisable{Number: 42},
		ReadyMerge{Number: 42, Head: "c0de"},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Task: "Production check of LEGION-208", CheckLook: 2, CheckOpenedAt: now},
		CheckWakeDue{Look: 2, OpenedAt: now, Reason: "the tester's own wake"},
		CheckWakeDue{Look: 3, Reason: "a look nobody reported"},
		CheckLook{Look: 2, OpenedAt: now},
		CheckLook{Look: 1},
		CheckBound{},
		PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing, Reason: "CI is red at head: pytest"},
		PhaseEnded{Role: claim.RoleImplementer, From: phase.Retro, To: phase.Implementing, Reason: "CI is red at head: pytest", TaskFollows: true},
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

// A person's handover is one dispatch_status row marked PersonHandover: while it is pending, the
// issue has a person's handover pending, and no second row is queued for it. The workflow's other
// status rows suppress nothing, so a hand-over row (HandOver, also a todo) on another issue, and
// an unmarked todo on the same issue, count for nothing; a finished row is deleted, so once the
// marked row is gone nothing is pending.
func TestPersonHandoverPendingCountsOnlyMarkedStatusRows(t *testing.T) {
	now := time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	enqueue := func(issue string, payload OutboxPayload) {
		t.Helper()
		row, err := NewOutboxRow(issue, payload, now)
		if err != nil {
			t.Fatalf("NewOutboxRow: %v", err)
		}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.Enqueue(ctx, tx, row))
		})
	}
	pending := func(issue string) bool {
		t.Helper()
		var got bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.PersonHandoverPending(ctx, tx, issue)
			must(t, err)
		})
		return got
	}

	enqueue("LEGION-208", StatusWrite{Status: "todo", ObservedStatus: "triage", PersonHandover: true})
	enqueue("LEGION-209", StatusWrite{Status: "todo", ObservedStatus: "backlog", HandOver: true, HandOverTree: "LEGION-200", HandOverLinger: 3})
	enqueue("LEGION-210", StatusWrite{Status: "todo", ObservedStatus: "triage"})
	enqueue("LEGION-210", ControllerNotice{Kind: TriageNotice})

	if !pending("LEGION-208") {
		t.Fatalf("PersonHandoverPending(LEGION-208) = false, want true while its marked row is in the outbox")
	}
	if pending("LEGION-209") {
		t.Fatalf("PersonHandoverPending(LEGION-209) = true, want false for an issue whose only pending row is a hand-over")
	}
	if pending("LEGION-210") {
		t.Fatalf("PersonHandoverPending(LEGION-210) = true, want false for an unmarked todo and a triage wake")
	}
	if pending("LEGION-211") {
		t.Fatalf("PersonHandoverPending(LEGION-211) = true, want false for an issue with no row")
	}

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.DueRows(ctx, tx, "LEGION", "LEGION-208", nil, now)
		must(t, err)
		if len(rows) != 1 || rows[0].Kind != OutboxKindDispatchStatus {
			t.Fatalf("DueRows of LEGION-208 = %#v, want the one marked row", rows)
		}
		claimed, ok, err := records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("ClaimRow of the marked row claimed nothing")
		}
		must(t, records.FinishOutbox(ctx, tx, claimed.ID, claimed.LeaseToken))
	})
	if pending("LEGION-208") {
		t.Fatalf("PersonHandoverPending(LEGION-208) = true after its row finished, want false")
	}
}

// Every kind the outbox stores runs on exactly one lane, and LaneOf names the lane whose kinds
// list it: a kind on neither lane would be queued and never run, one on both would run twice over.
func TestEveryOutboxKindIsInExactlyOneLane(t *testing.T) {
	every := []OutboxKind{OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindSupervise,
		OutboxKindGateSeed, OutboxKindLingerClose, OutboxKindIssueSuspend, OutboxKindWorkspaceRemove, OutboxKindMergeQueuePublish, OutboxKindIssueBranch, OutboxKindAutoMergeDisable, OutboxKindReadyMerge,
		OutboxKindCheckWake, OutboxKindCheckLook, OutboxKindCheckBound, OutboxKindPhaseEnded, OutboxKindIssueBundle, OutboxKindIssueRelease,
		OutboxKindGatedStepsPublish}
	lanes := map[OutboxKind][]OutboxLane{}
	for _, lane := range []OutboxLane{LaneLifecycle, LaneBoard, LaneRoleMessage} {
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
		{name: "a parked-decision notice for an architect", payload: Notice{Kind: ParkedDecisionNotice}, reason: "unknown notice kind"},
		{name: "an actor on a phase-finished notice", payload: Notice{Kind: "phase-finished", Actor: "user:a"}, reason: "only a spec decision's does"},
		{name: "a ref on a child-status notice", payload: Notice{Kind: "child-status", Ref: "dispatch://LEGION-1/spec"}, reason: "only a spec decision's does"},
		{name: "a decision on an architect's notice", payload: Notice{Kind: "design-approved", Decision: DecisionDesignApproved}, reason: "only a parked-decision controller notice does"},
		{name: "a root on an architect's notice", payload: Notice{Kind: "design-approved", Root: "LEGION-1"}, reason: "only a parked-decision controller notice does"},
		{name: "a parked decision with no root", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Ref: "dispatch://LEGION-1/ask/a", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision with no ref", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision by no person", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "session:s"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision by an empty user", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "user:"}, reason: "a parked-decision notice needs"},
		{name: "an unknown decision", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: "ask-ignored", Root: "LEGION-1", Ref: "dispatch://LEGION-1", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision with no decision", payload: ControllerNotice{Kind: ParkedDecisionNotice, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a decision on a slot-free wake", payload: ControllerNotice{Kind: SlotFreeNotice, Decision: DecisionAskAnswered}, reason: "only a parked-decision does"},
		{name: "an actor on a tick", payload: ControllerNotice{Kind: TickNotice, Actor: "user:a"}, reason: "only a parked-decision does"},
		{name: "a root on a held notice to the controller", payload: ControllerNotice{Kind: "held", Root: "LEGION-1"}, reason: "only a parked-decision does"},
		{name: "an update-stalled notice for an architect", payload: Notice{Kind: UpdateStalledNotice}, reason: "unknown notice kind"},
		{name: "an architect's notice carrying an update round", payload: Notice{Kind: "held", Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "only the controller's update-stalled notice does"},
		{name: "a tick notice carrying an update round", payload: ControllerNotice{Kind: TickNotice, Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "carries an update round only when it is one"},
		{name: "an update-stalled notice carrying no update round", payload: ControllerNotice{Kind: UpdateStalledNotice}, reason: "carries an update round only when it is one"},
		{name: "an update-stalled notice with no claims", payload: ControllerNotice{Kind: UpdateStalledNotice, Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "requires its round and the claims it waits on"},
		{name: "an update-stalled notice with no round", payload: ControllerNotice{Kind: UpdateStalledNotice, Update: &UpdateStalled{Waiting: []UpdateWaiting{{Claim: "legion-LEGION-208-tester", State: "working"}}}}, reason: "requires its round and the claims it waits on"},
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "a stop that is not a tree close", payload: SuperviseRequest{Op: "stop", Tree: "LEGION-208", Role: claim.RoleArchitect}, reason: "unknown supervise operation"},
		{name: "an issue close naming a linger", payload: SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Linger: 7}, reason: "only a tree close"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "a merge queue publish without a role", payload: MergeQueuePublish{Packet: "READY #42"}, reason: "merge queue publish requires role"},
		{name: "a merge queue publish without a packet", payload: MergeQueuePublish{Role: "merge-queue"}, reason: "merge queue publish requires packet"},
		{name: "a gated-steps publish without a project", payload: GatedStepsPublish{Steps: []DesignStep{DesignStepSpec}}, reason: "requires the project whose row it writes"},
		{name: "a gated-steps publish naming no step of the gate", payload: GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{"verification"}}, reason: `names "verification", which is no design-gate step`},
		{name: "a READY merge without a pull request", payload: ReadyMerge{Head: "c0de"}, reason: "READY merge requires its pull request number and head"},
		{name: "a READY merge without a head", payload: ReadyMerge{Number: 42}, reason: "READY merge requires its pull request number and head"},
		{name: "a check look on a suspend", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on a phase start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Phase: phase.Implementing, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on an architect's start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on a reviewer's start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a negative check look", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: -1}, reason: checkLookRefusal},
		{name: "a check wake without a look", payload: CheckWakeDue{Reason: "the tester's own wake"}, reason: "check wake requires the look it was named at"},
		{name: "a check wake without a reason", payload: CheckWakeDue{Look: 1}, reason: "check wake requires the reason it wakes the check"},
		{name: "a phase-ended message to an architect", payload: PhaseEnded{Role: claim.RoleArchitect, From: phase.Accepting, To: phase.Implementing, Reason: "CI is red at head: pytest"}, reason: "never to an architect"},
		{name: "a phase-ended message to no role", payload: PhaseEnded{From: phase.Testing, To: phase.Implementing, Reason: "CI is red at head: pytest"}, reason: "never to an architect"},
		{name: "a phase-ended message without the phase that ended", payload: PhaseEnded{Role: claim.RoleTester, To: phase.Implementing, Reason: "CI is red at head: pytest"}, reason: "names the phase that ended and the different phase"},
		{name: "a phase-ended message out of the phase the issue is in", payload: PhaseEnded{Role: claim.RoleImplementer, From: phase.Implementing, To: phase.Implementing, Reason: "CI is red at head: pytest"}, reason: "names the phase that ended and the different phase"},
		{name: "a phase-ended message without a reason", payload: PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing}, reason: "carries the reason the architect's notice carries"},
		{name: "a phase-ended task that follows for another role", payload: PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing, Reason: "CI is red at head: pytest", TaskFollows: true}, reason: "only the implementer is told its task follows"},
		{name: "a check look bound without a look", payload: CheckLook{}, reason: "check look bound requires the look it bounds"},
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
		{name: "a check wake without a look", row: OutboxRow{ID: 40, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"reason":"the tester's own wake"}`)}, reason: "check wake requires the look it was named at"},
		{name: "a check wake without a reason", row: OutboxRow{ID: 42, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"look":1}`)}, reason: "check wake requires the reason it wakes the check"},
		{name: "a check wake with an unknown field", row: OutboxRow{ID: 43, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"look":1,"reason":"the tester's own wake","build":"b00f"}`)}, reason: "unknown field"},
		{name: "a check bound with a field", row: OutboxRow{ID: 41, Kind: OutboxKindCheckBound, Payload: json.RawMessage(`{"look":1}`)}, reason: "unknown field"},
		{name: "a phase-ended message to an architect", row: OutboxRow{ID: 46, Kind: OutboxKindPhaseEnded, Payload: json.RawMessage(`{"role":"architect","from":"accepting","to":"implementing","reason":"CI is red at head: pytest"}`)}, reason: "never to an architect"},
		{name: "a phase-ended message with an unknown field", row: OutboxRow{ID: 47, Kind: OutboxKindPhaseEnded, Payload: json.RawMessage(`{"role":"tester","from":"testing","to":"implementing","reason":"CI is red at head: pytest","quiesce":"tester"}`)}, reason: "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeOutboxPayload(tc.row)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("row %d", tc.row.ID)) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("DecodeOutboxPayload error = %v, want row %d and %q", err, tc.row.ID, tc.reason)
			}
		})
	}
}

// checkLookRefusal is the validation's words for a supervise row whose check look is not a start
// of the implementer or the tester with no phase.
const checkLookRefusal = "a supervise check look names a start of the implementer or the tester with no phase"

// The release field is gone: an issue's close deletes its Sandbox in every case, so a suspension
// carries nothing to say which. A row the previous release queued with the field is stripped of
// it at the upgrade (migration 0051), since the payload decodes strictly and the key would stall
// its issue's lane; one written without it has always read as this row does, and nothing encodes
// the key again.
func TestAnIssueSuspensionCarriesNoReleaseField(t *testing.T) {
	row := OutboxRow{ID: 40, Kind: OutboxKindIssueSuspend, Payload: json.RawMessage(`{"tree":"LEGION-208","generation":3,"treeGeneration":7}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("DecodeOutboxPayload: %v", err)
	}
	want := IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7}
	if got != want {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "release") {
		t.Fatalf("a suspension encodes %s, want no release field", encoded)
	}
	stale := OutboxRow{ID: 41, Kind: OutboxKindIssueSuspend, Payload: json.RawMessage(`{"tree":"LEGION-208","generation":3,"treeGeneration":7,"release":true}`)}
	if _, err := DecodeOutboxPayload(stale); err == nil || !strings.Contains(err.Error(), `unknown field "release"`) {
		t.Fatalf("decoding a row the previous release wrote = %v, want the strict refusal migration 0051 clears", err)
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

// A parked-decision notice keeps every field through its outbox row, and a spec decision's
// architect notice keeps who made it and where.
func TestAParkedDecisionKeepsItsDecisionThroughTheRow(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, payload := range []OutboxPayload{
		ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionDesignChangesRequested, Root: "LEGION-1", Ref: "dispatch://LEGION-1/spec",
			Actor: "user:alice", Version: 3, Reason: "name the rollback path"},
		ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskReply, Root: "LEGION-1", Ref: "dispatch://LEGION-2/comment/c-1", Actor: "user:alice"},
		Notice{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
	} {
		row, err := NewOutboxRow("LEGION-1", payload, now)
		if err != nil {
			t.Fatalf("NewOutboxRow(%#v): %v", payload, err)
		}
		got, err := DecodeOutboxPayload(row)
		if err != nil || !reflect.DeepEqual(got, payload) {
			t.Fatalf("decoded %#v, %v; want %#v", got, err, payload)
		}
	}
}

// SpecDecision names the three notice kinds a person's spec decision writes, and no other.
func TestSpecDecisionNamesTheThreeKindsAPersonsSpecDecisionWrites(t *testing.T) {
	for kind, want := range map[NoticeKind]DecisionKind{
		"design-approved":          DecisionDesignApproved,
		"design-changes-requested": DecisionDesignChangesRequested,
		"carried-gate-opened":      DecisionCarriedGateOpened,
		"child-status":             "",
		"worker-died":              "",
		ParkedDecisionNotice:       "",
	} {
		got, ok := SpecDecision(kind)
		if got != want || ok != (want != "") {
			t.Errorf("SpecDecision(%q) = %q, %t; want %q, %t", kind, got, ok, want, want != "")
		}
	}
	for actor, want := range map[string]bool{"user:alice": true, "user:": false, "session:s": false, "": false, "agent:user:alice": false} {
		if got := PersonActor(actor); got != want {
			t.Errorf("PersonActor(%q) = %t, want %t", actor, got, want)
		}
	}
}

// The boot's publish of the deployment's design-gate steps is one outbox row addressed to the
// bare project key rather than to an issue. The record's part is three things: the row is due to
// the daemon of that project and to no other, which the same split_part scope answers for a key
// with no hyphen; a boot writes the project's one row of the kind, replacing what a pending row
// carries, so the list waiting for Dispatch is always the list the booting daemon holds; and a
// finished row leaves nothing behind, so the boot after it queues its own.
func TestGatedStepsPublishAtBoot(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// boot is what openWorkflow does with the list it was configured with: write the project's
	// one publish.
	boot := func(project string, steps []DesignStep) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.EnqueueGatedSteps(ctx, tx, GatedStepsPublish{Project: project, Steps: steps}, now))
		})
	}
	// queued is the one publish the outbox holds for project, by row id and by what it carries.
	queued := func(project string) (int64, GatedStepsPublish) {
		t.Helper()
		var held []OutboxRow
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			held, err = records.DueRows(ctx, tx, project, "", nil, now)
			must(t, err)
		})
		if len(held) != 1 || held[0].Issue != project {
			t.Fatalf("DueRows of %s = %#v, want the one publish addressed to the project key", project, held)
		}
		payload, err := DecodeOutboxPayload(held[0])
		must(t, err)
		publish, ok := payload.(GatedStepsPublish)
		if !ok {
			t.Fatalf("the row of %s decodes as %#v, want a gated-steps publish", project, payload)
		}
		return held[0].ID, publish
	}

	boot("LEGION", []DesignStep{DesignStepCriteria, DesignStepSpec})
	first, publish := queued("LEGION")
	want := GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepCriteria, DesignStepSpec}}
	if !reflect.DeepEqual(publish, want) {
		t.Fatalf("the publish carries %#v, want %#v, the configured list in the configured order", publish, want)
	}

	// A second boot on the same list leaves the row it finds where it is, so a restart loop
	// writes no second publish and takes nothing off the backoff the lane has set on this one.
	boot("LEGION", []DesignStep{DesignStepCriteria, DesignStepSpec})
	if id, again := queued("LEGION"); id != first || !reflect.DeepEqual(again, publish) {
		t.Fatalf("a boot on the unchanged list left row %d carrying %#v, want row %d carrying %#v untouched", id, again, first, publish)
	}

	// A boot on a changed list while that row is still pending replaces what it carries: an
	// operator who changes gates.design and restarts while Dispatch has yet to take the publish
	// gets the list this daemon booted on, not the one the boot before it held.
	boot("LEGION", []DesignStep{DesignStepSpec})
	changedRow, changed := queued("LEGION")
	if changedRow != first {
		t.Fatalf("the boot on the changed list wrote row %d beside %d, want the pending row carrying the new list", changedRow, first)
	}
	if want := (GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepSpec}}); !reflect.DeepEqual(changed, want) {
		t.Fatalf("the pending publish carries %#v after the boot on a changed list, want %#v", changed, want)
	}

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.DueRows(ctx, tx, "INFRA", "", nil, now)
		must(t, err)
		if len(rows) != 0 {
			t.Fatalf("DueRows of INFRA = %#v, want nothing: LEGION's publish is LEGION's daemon's", rows)
		}
	})

	var claimed OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		var ok bool
		var err error
		claimed, ok, err = records.ClaimRow(ctx, tx, changedRow, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("ClaimRow of the publish claimed nothing")
		}
		must(t, records.FinishOutbox(ctx, tx, claimed.ID, claimed.LeaseToken))
	})

	// The run deleted the row, so the next boot publishes whatever it is configured with now: a
	// deployment that gates nothing publishes the empty list rather than nothing at all.
	boot("LEGION", []DesignStep{})
	ran, published := queued("LEGION")
	if ran == changedRow {
		t.Fatalf("the boot after the publish ran reused row %d, want a row of its own", ran)
	}
	if want := (GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{}}); !reflect.DeepEqual(published, want) {
		t.Fatalf("the next boot published %#v, want the empty list of a deployment that gates nothing", published)
	}
}

// LongestPost is what a caller checks before a message is recorded, and Posted is what the runner
// posts, so no row's post may pass a bound the text its caller checked clears: the post of the
// smallest row id and of the largest are the same text but for the marker's digits, and the
// marker of math.MaxInt64 is the longest there is. Each body here stands on a bound — the body
// one block or one unit longer makes a post Dispatch refuses, which the case asserts — so a post
// of some row that measured above LongestPost is refused below rather than passing unnoticed.
// The shapes are the ones where the marker changes the measure: a body ending in a paragraph,
// where the marker is a block of its own, and one ending in an open code fence, where it is code
// inside the last block.
func TestNoRowsPostMeasuresAboveLongestPost(t *testing.T) {
	// The blank line and the longest marker LongestPost puts after a body, which the fenced body
	// below leaves room for inside its last block.
	marker := len(LongestPost(""))
	const fence = "see log\n\n```\n"
	for _, tc := range []struct{ name, body, longer string }{
		{"a body of paragraphs one block short of the bound on blocks",
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks-1), "\n\n"),
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n")},
		{"a last paragraph on the bound on units, which the marker stands beside",
			strings.Repeat("y", dispatchbody.BlockUnits),
			strings.Repeat("y", dispatchbody.BlockUnits+1)},
		{"an open code fence the marker falls inside, on the bound on units with it",
			fence + strings.Repeat("y", dispatchbody.BlockUnits-len("```\n")-marker),
			fence + strings.Repeat("y", dispatchbody.BlockUnits-len("```\n")-marker+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if over := dispatchbody.Check(LongestPost(tc.body)); over != nil {
				t.Fatalf("the longest post of this body = %v, want one Dispatch takes", over)
			}
			if dispatchbody.Check(LongestPost(tc.longer)) == nil {
				t.Fatalf("the longest post of this body one block or one unit longer is taken too, so the body is not on the bound and no post below could measure above it")
			}
			for _, id := range []int64{1, 9, 10, 99, 100, 1_000_000, math.MaxInt64 - 1, math.MaxInt64} {
				if over := dispatchbody.Check((MessagePost{Body: tc.body}).Posted(id)); over != nil {
					t.Errorf("the post of row %d = %v while the text its caller checked passes, so a body checked here is refused at the post", id, over)
				}
			}
		})
	}
}
