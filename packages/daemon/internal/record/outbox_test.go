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
		ControllerNotice{Kind: AgentQuestionNotice, Role: claim.RoleTester, Reason: "not_answered", Question: &AgentQuestion{
			ID: "q-1", Issue: "LEGION-208", Session: "ses_tester", Asker: "user:person", Ref: "dispatch://LEGION-208/message/m-1", Text: "Is PR 7 green?",
		}},
		Notice{Kind: AgentDecisionNotice, Role: claim.RoleTester, AgentDecision: &AgentDecision{
			ID: "dispatch:34", Issue: "LEGION-209", Session: "ses_tester", Asker: "user:person", Ref: "dispatch://LEGION-209/ask/a-1", Text: "Selected: B",
		}},
		ControllerNotice{Kind: AgentDecisionNotice, Role: claim.RoleTester, AgentDecision: &AgentDecision{
			ID: "dispatch:c-9", Issue: "LEGION-209", Session: "ses_tester", Asker: "user:person", Ref: "dispatch://LEGION-209/comment/c-9", Text: "Use B.",
		}},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{Linger: 3},
		MergeQueuePublish{Role: "merge-queue", Packet: "READY #42 at head (approved at head) for LEGION-208 (https://github.com/sjawhar/legion/pull/42)"},
		IssueBranch{Generation: 2},
		SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Reason: "LEGION-209 is done"},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7},
		AutoMergeDisable{Number: 42},
		ReadyMerge{Number: 42, Head: "c0de"},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Task: "Production check of LEGION-208", CheckLook: 2, CheckOpenedAt: now},
		CheckCompare{Look: 2, Cause: "daemon boot", Build: "b00f", OpenedAt: now},
		CheckCompare{Look: 3, Cause: "deploy run", Deploy: &DeployRun{Run: 40203304600, Attempt: 2, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "main"}, OpenedAt: now},
		CheckLook{Look: 2, OpenedAt: now},
		CheckLook{Look: 1},
		CheckBound{},
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
		OutboxKindGateSeed, OutboxKindLingerClose, OutboxKindIssueSuspend, OutboxKindWorkspaceRemove, OutboxKindMergeQueuePublish, OutboxKindIssueBranch, OutboxKindAutoMergeDisable, OutboxKindReadyMerge,
		OutboxKindCheckCompare, OutboxKindCheckLook, OutboxKindCheckBound}
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
		{name: "an agent-question notice for an architect", payload: Notice{Kind: AgentQuestionNotice, Question: &AgentQuestion{ID: "q-1"}}, reason: "unknown notice kind"},
		{name: "an architect's notice carrying a question", payload: Notice{Kind: "held", Question: &AgentQuestion{ID: "q-1"}}, reason: "only a controller's agent-question notice does"},
		{name: "an agent-question notice without its question", payload: ControllerNotice{Kind: AgentQuestionNotice, Reason: "not_answered"}, reason: "carries a question only when"},
		{name: "another controller notice carrying a question", payload: ControllerNotice{Kind: TickNotice, Question: &AgentQuestion{ID: "q-1"}}, reason: "carries a question only when"},
		{name: "an agent-question notice naming no question", payload: ControllerNotice{Kind: AgentQuestionNotice, Question: &AgentQuestion{Issue: "LEGION-208"}}, reason: "names its question's id"},
		{name: "an agent-decision notice without its decision", payload: Notice{Kind: AgentDecisionNotice, Role: claim.RoleTester}, reason: "carries a decision only when"},
		{name: "an agent-decision controller notice without its decision", payload: ControllerNotice{Kind: AgentDecisionNotice}, reason: "carries a decision only when"},
		{name: "another notice carrying a decision", payload: Notice{Kind: "held", AgentDecision: &AgentDecision{ID: "dispatch:34"}}, reason: "carries a decision only when"},
		{name: "another controller notice carrying a decision", payload: ControllerNotice{Kind: AgentQuestionNotice, Question: &AgentQuestion{ID: "q-1"},
			AgentDecision: &AgentDecision{ID: "dispatch:34"}}, reason: "carries a decision only when"},
		{name: "an agent-decision notice naming no decision", payload: ControllerNotice{Kind: AgentDecisionNotice, AgentDecision: &AgentDecision{Issue: "LEGION-209"}}, reason: "names its decision's id"},
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "a stop that is not a tree close", payload: SuperviseRequest{Op: "stop", Tree: "LEGION-208", Role: claim.RoleArchitect}, reason: "unknown supervise operation"},
		{name: "an issue close naming a linger", payload: SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Linger: 7}, reason: "only a tree close"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "a merge queue publish without a role", payload: MergeQueuePublish{Packet: "READY #42"}, reason: "merge queue publish requires role"},
		{name: "a merge queue publish without a packet", payload: MergeQueuePublish{Role: "merge-queue"}, reason: "merge queue publish requires packet"},
		{name: "a READY merge without a pull request", payload: ReadyMerge{Head: "c0de"}, reason: "READY merge requires its pull request number and head"},
		{name: "a READY merge without a head", payload: ReadyMerge{Number: 42}, reason: "READY merge requires its pull request number and head"},
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

// An agent-question notice carries a question of up to AgentQuestionTextLimit characters whole,
// and a longer one cut at that many characters, counted as characters rather than bytes, with a
// note naming its length and where the whole question is.
func TestAnAgentQuestionsTextIsCutAtItsLimitWithANote(t *testing.T) {
	const ref = "dispatch://LEGION-208/message/m-1"
	whole := strings.Repeat("é", AgentQuestionTextLimit)
	if got := AgentQuestionText(whole, ref); got != whole {
		t.Fatalf("a question of exactly the limit = %d characters, want it whole", len([]rune(got)))
	}
	long := whole + "?!"
	got := AgentQuestionText(long, ref)
	want := whole + "\n\n[cut: the question is 2002 characters; the first 2000 are shown, the whole question is at " + ref + "]"
	if got != want {
		t.Fatalf("a question over the limit = %q, want its first %d characters and the note", got[len(whole):], AgentQuestionTextLimit)
	}
}

// An agent-decision notice cuts what a person decided as an agent-question notice cuts a question,
// and its note names a decision.
func TestAnAgentDecisionsTextIsCutAtTheSameLimitWithANote(t *testing.T) {
	const ref = "dispatch://LEGION-208/ask/a-1"
	whole := strings.Repeat("é", AgentQuestionTextLimit)
	if got := AgentDecisionText(whole, ref); got != whole {
		t.Fatalf("a decision of exactly the limit = %d characters, want it whole", len([]rune(got)))
	}
	want := whole + "\n\n[cut: the decision is 2001 characters; the first 2000 are shown, the whole decision is at " + ref + "]"
	if got := AgentDecisionText(whole+"!", ref); got != want {
		t.Fatalf("a decision over the limit = %q, want its first %d characters and the note", got[len(whole):], AgentQuestionTextLimit)
	}
}
