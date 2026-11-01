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
		StatusWrite{Status: "todo", ObservedStatus: "triage", PersonHandover: true},
		MessagePost{Body: "Pull request checks are blocked."},
		Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing, Summary: "Tests passed", Verdict: "pass", Version: 4, Reason: ""},
		ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning, Reason: "escalated"},
		ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets", "resource-limits"}},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{Linger: 3},
		IssueBranch{Generation: 2},
		SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Reason: "LEGION-209 is done"},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Task: "Production check of LEGION-208", CheckLook: 2, CheckOpenedAt: now},
		CheckCompare{Look: 2, Cause: "daemon boot", Build: "b00f", OpenedAt: now},
		CheckCompare{Look: 3, Cause: "deploy run", Deploy: &DeployRun{Run: 40203304600, Attempt: 2, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "main"}, OpenedAt: now},
		CheckLook{Look: 2, OpenedAt: now},
		CheckLook{Look: 1},
		CheckBound{},
		ApprovalCarry{Repo: "sjawhar/legion", Number: 42, Approved: "c0de", Head: "handoff"},
		RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.implementer", Body: "Pull request #42: CI settled at c0de: unit failed."},
		PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing, Reason: "the head conflicts with main"},
		PhaseEnded{Role: claim.RoleImplementer, From: phase.Retro, To: phase.Implementing, Reason: "the head conflicts with main", TaskFollows: true},
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
		OutboxKindGateSeed, OutboxKindLingerClose, OutboxKindIssueSuspend, OutboxKindWorkspaceRemove, OutboxKindIssueBranch,
		OutboxKindCheckCompare, OutboxKindCheckLook, OutboxKindCheckBound, OutboxKindApprovalCarry, OutboxKindRoleMessage, OutboxKindPhaseEnded}
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
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "a stop that is not a tree close", payload: SuperviseRequest{Op: "stop", Tree: "LEGION-208", Role: claim.RoleArchitect}, reason: "unknown supervise operation"},
		{name: "an issue close naming a linger", payload: SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Linger: 7}, reason: "only a tree close"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "a role message without a role", payload: RoleMessage{Body: "Pull request #42 settled."}, reason: "role message requires role"},
		{name: "a role message without a body", payload: RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.architect"}, reason: "role message requires body"},
		{name: "a check look on a suspend", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: 1}, reason: "a supervise check look names a start of the implementer with no phase"},
		{name: "a check look on a phase start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Phase: phase.Implementing, CheckLook: 1}, reason: "a supervise check look names a start of the implementer with no phase"},
		{name: "a check look on another role's start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, CheckLook: 1}, reason: "a supervise check look names a start of the implementer with no phase"},
		{name: "a negative check look", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: -1}, reason: "a supervise check look names a start of the implementer with no phase"},
		{name: "a check compare without a look", payload: CheckCompare{Cause: "daemon boot", Build: "b00f"}, reason: "check compare requires the look it starts"},
		{name: "a check compare without a build", payload: CheckCompare{Look: 1, Cause: "daemon boot"}, reason: "check compare requires the build commit and what asked for it"},
		{name: "a check compare without a cause", payload: CheckCompare{Look: 1, Build: "b00f"}, reason: "check compare requires the build commit and what asked for it"},
		{name: "a check compare with both a build and a deploy run", payload: CheckCompare{Look: 1, Cause: "deploy run", Build: "b00f", Deploy: &validDeployRun}, reason: "check compare carries both a build commit and a deploy run; it compares exactly one"},
		{name: "a check compare with neither a build nor a deploy run", payload: CheckCompare{Look: 1, Cause: "deploy run"}, reason: "or a deploy run in place of the build commit"},
		{name: "a deploy run check compare without a cause", payload: CheckCompare{Look: 1, Deploy: &validDeployRun}, reason: "or a deploy run in place of the build commit"},
		{name: "a deploy run without a run", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Run = 0 })}, reason: deployRunRefusal},
		{name: "a deploy run with a negative run", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Run = -1 })}, reason: deployRunRefusal},
		{name: "a deploy run without an attempt", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Attempt = 0 })}, reason: deployRunRefusal},
		{name: "a deploy run without a workflow", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Workflow = "" })}, reason: deployRunRefusal},
		{name: "a deploy run without a step", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Step = "" })}, reason: deployRunRefusal},
		{name: "a deploy run without a branch", payload: CheckCompare{Look: 1, Cause: "deploy run", Deploy: deployRunWith(func(d *DeployRun) { d.Branch = "" })}, reason: deployRunRefusal},
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
		{name: "a check compare without a look", row: OutboxRow{ID: 40, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"cause":"daemon boot","build":"b00f"}`)}, reason: "check compare requires the look it starts"},
		{name: "a check compare with both a build and a deploy run", row: OutboxRow{ID: 42, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"look":1,"cause":"deploy run","build":"b00f","deploy":{"run":7,"attempt":1,"workflow":".github/workflows/envoy.yaml","step":"Apply","branch":"main"}}`)}, reason: "it compares exactly one"},
		{name: "a check compare with neither a build nor a deploy run", row: OutboxRow{ID: 43, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"look":1,"cause":"deploy run"}`)}, reason: "or a deploy run in place of the build commit"},
		{name: "a deploy run without a step", row: OutboxRow{ID: 44, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"look":1,"cause":"deploy run","deploy":{"run":7,"attempt":1,"workflow":".github/workflows/envoy.yaml","branch":"main"}}`)}, reason: deployRunRefusal},
		{name: "a deploy run with an unknown field", row: OutboxRow{ID: 45, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"look":1,"cause":"deploy run","deploy":{"run":7,"attempt":1,"workflow":".github/workflows/envoy.yaml","step":"Apply","branch":"main","job":3}}`)}, reason: "unknown field"},
		{name: "a check bound with a field", row: OutboxRow{ID: 41, Kind: OutboxKindCheckBound, Payload: json.RawMessage(`{"look":1}`)}, reason: "unknown field"},
		{name: "an approval carry without its pull request", row: OutboxRow{ID: 46, Kind: OutboxKindApprovalCarry, Payload: json.RawMessage(`{"repo":"sjawhar/legion","approved":"c0de","head":"handoff"}`)}, reason: "requires its repository and pull request number"},
		{name: "an approval carry of one head", row: OutboxRow{ID: 47, Kind: OutboxKindApprovalCarry, Payload: json.RawMessage(`{"repo":"sjawhar/legion","number":42,"approved":"c0de","head":"c0de"}`)}, reason: "an approval of the head needs no carry"},
		{name: "a role message with no body", row: OutboxRow{ID: 48, Kind: OutboxKindRoleMessage, Payload: json.RawMessage(`{"role":"legion.sjawhar-legion.LEGION-208.architect"}`)}, reason: "role message requires body"},
		{name: "a phase-ended message to an architect", row: OutboxRow{ID: 49, Kind: OutboxKindPhaseEnded, Payload: json.RawMessage(`{"role":"architect","from":"accepting","to":"implementing","reason":"the head conflicts with main"}`)}, reason: "never to an architect"},
		{name: "a phase-ended message with an unknown field", row: OutboxRow{ID: 50, Kind: OutboxKindPhaseEnded, Payload: json.RawMessage(`{"role":"tester","from":"testing","to":"implementing","reason":"the head conflicts with main","quiesce":"tester"}`)}, reason: "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeOutboxPayload(tc.row)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("row %d", tc.row.ID)) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("DecodeOutboxPayload error = %v, want row %d and %q", err, tc.row.ID, tc.reason)
			}
		})
	}
}

// validDeployRun is a deploy run CheckCompare accepts, which deployRunWith changes one field of.
var validDeployRun = DeployRun{Run: 40203304600, Attempt: 1, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "main"}

// deployRunRefusal is the validation's words for a deploy run missing one of its fields.
const deployRunRefusal = "check compare's deploy run requires a positive run and attempt and its workflow, step and branch"

// deployRunWith is a copy of validDeployRun that change has altered.
func deployRunWith(change func(*DeployRun)) *DeployRun {
	deploy := validDeployRun
	change(&deploy)
	return &deploy
}

// A check_compare row a daemon queued before deploy runs existed carries a build and no deploy:
// it decodes to the build compare it was, and a build compare encodes no deploy field, so a daemon
// rolled back past this change still reads the build rows this one writes.
func TestABuildCheckCompareRowReadsAsItDidBeforeDeployRuns(t *testing.T) {
	opened := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	row := OutboxRow{ID: 46, Kind: OutboxKindCheckCompare, Payload: json.RawMessage(`{"look":2,"cause":"daemon boot","build":"b00f","openedAt":"2026-09-23T01:00:00Z"}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("DecodeOutboxPayload: %v", err)
	}
	want := CheckCompare{Look: 2, Cause: "daemon boot", Build: "b00f", OpenedAt: opened}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
	encoded, err := NewOutboxRow("LEGION-208", want, opened)
	if err != nil {
		t.Fatalf("NewOutboxRow: %v", err)
	}
	if string(encoded.Payload) != string(row.Payload) {
		t.Fatalf("a build compare encodes %s, want %s with no deploy field", encoded.Payload, row.Payload)
	}
}

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
