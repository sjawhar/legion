package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (populatedState) carries two step records: LEGION-208's architect,
// whose last call ended, and its implementer, whose call has a start and no end — the shape a
// turn that stopped moving leaves behind.

// stepsServed is the `step` of every claim view of a state body, by the call each names, and how
// many views the body holds: the two are read apart so a body that carries no claim view at all
// cannot pass for one whose records were stripped.
func stepsServed(t *testing.T, body []byte) (calls []string, views int) {
	t.Helper()
	for _, view := range claimViews(t, body) {
		views++
		step, carried := view["step"]
		if !carried {
			continue
		}
		step, ok := step.(map[string]any)
		if !ok {
			t.Fatalf("a claim view of %s carries a step that is no object: %v", body, view["step"])
		}
		call, _ := step.(map[string]any)["callId"].(string)
		calls = append(calls, call)
	}
	return calls, views
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below stallRecordContract is served claim views with no `step`,
// since that pod's plugin reads the state with a schema that knows no such key and refuses the
// whole body for it. A pod at that contract, a pod whose claims record no contract at all, and a
// caller the daemon cannot place in a pod — the operator's `legion state`, whose tool-call line
// reads these records — are served it. The claims are the state snapshot's own, so there is no
// readable state whose caller's contract is unknown: a snapshot without them is no state, and
// the route refuses it.
func TestTheStateRouteServesAPodItsPluginContractsStallRecord(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(stallRecordContract))
	wantCalls, wantViews := stepsServed(t, full)
	if len(wantCalls) == 0 || wantViews == 0 {
		t.Fatalf("the fixture's state %s carries no claim view with a step record; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		stall  bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", claims: podClaims(stallRecordContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(stallRecordContract - 1)},
		{name: "a pod at stallRecordContract", remote: statePodAddr + ":41000", claims: podClaims(stallRecordContract), stall: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), stall: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(stallRecordContract - 1), stall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)
			calls, views := stepsServed(t, body)
			if views != wantViews {
				t.Fatalf("the state %s carries %d claim views, want the fixture's %d", body, views, wantViews)
			}
			if !tc.stall {
				if len(calls) > 0 {
					t.Errorf("the state %s names the calls %v; the reader before stallRecordContract refuses the key", body, calls)
				}
				return
			}
			if len(calls) != len(wantCalls) {
				t.Errorf("the state %s names the calls %v, want the fixture's %v", body, calls, wantCalls)
			}
		})
	}
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// that contract, is served the state as the source answered it, step records and all.
func TestTheStateServedToAPodBeforeTheStallRecordContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(stallRecordContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	// Read through the claim views rather than as text: the settings block of this fixture
	// carries a deployment's own `step` key, which says nothing of a claim.
	if calls, _ := stepsServed(t, behind); len(calls) > 0 {
		t.Fatalf("the pod before the contract was served the calls %v, want no step record: %s", calls, behind)
	}
	if calls, _ := stepsServed(t, operator); len(calls) == 0 {
		t.Fatalf("the operator after it was served %s, want the step records the source answered", operator)
	}
}

// stallRecordContract is the number DaemonAPIVersion took at the bump this record forced, which
// was a task of its own: the two are equal now, so a pod at this daemon's contract is served the
// record and a pod still running an older plugin is not. A constant above DaemonAPIVersion would
// be a key no pod is ever served; one below would serve it to a plugin whose schema refuses the
// body for it.
func TestTheStallRecordContractIsTheNumberTheBumpTakes(t *testing.T) {
	const addedStallRecord = 56
	if stallRecordContract != addedStallRecord {
		t.Fatalf("stallRecordContract = %d, want %d, the contract whose entry in version.go adds the claim views' `step`",
			stallRecordContract, addedStallRecord)
	}
	if stallRecordContract > DaemonAPIVersion+1 {
		t.Fatalf("stallRecordContract = %d, more than one above DaemonAPIVersion %d: no caller could be served the key",
			stallRecordContract, DaemonAPIVersion)
	}
	if stallRecordContract < DaemonAPIVersion {
		t.Fatalf("stallRecordContract = %d, below DaemonAPIVersion %d: a pod of a plugin that refuses the key would be served it",
			stallRecordContract, DaemonAPIVersion)
	}
}

// The operator's view of a claim carries the record for every role, the controller's among them,
// and it is the only surface that does: internal/projection puts the controller's claim on no
// issue, so no claim view of the state can carry one, and `legion claims list` is where an
// operator reads what a stuck controller is doing.
func TestTheOperatorsControllerClaimCarriesItsStepRecord(t *testing.T) {
	startedAt := time.Date(2026, 9, 22, 9, 40, 2, 0, time.UTC)
	step := supervise.StepRecord{
		At:            startedAt,
		TurnStartedAt: startedAt.Add(-30 * time.Second),
		Tool:          "bash",
		CallID:        "toolu_ctl_9",
		ArgsBytes:     64,
		ArgsHash:      "1f05c8a3b6e2497d",
		StartedAt:     startedAt,
	}
	view := operatorView(supervise.Claim{
		Token: claim.ControllerToken("legion"), Role: claim.RoleController,
		State: supervise.StateWorking, Session: "ses_controller", Step: step,
	})

	if view.Step == nil {
		t.Fatalf("the operator's controller claim = %+v, want the record of the call it is in", view)
	}
	if view.Step.Tool != step.Tool || view.Step.CallID != step.CallID || view.Step.ArgsBytes != step.ArgsBytes ||
		view.Step.ArgsHash != step.ArgsHash || view.Step.EndedAt != nil {
		t.Fatalf("the controller's step = %+v, want the open %s call it names with its arguments measured", view.Step, step.Tool)
	}

	// The listing an operator reads carries it as one object under `step`.
	encoded, err := json.Marshal(OperatorClaims{Claims: []OperatorClaim{view}})
	if err != nil {
		t.Fatalf("marshal the operator claim list: %v", err)
	}
	for _, want := range []string{`"step":`, `"callId":"toolu_ctl_9"`, `"argsHash":"1f05c8a3b6e2497d"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the operator claim list %s carries no %s", encoded, want)
		}
	}
}

// A claim whose agent has neither begun a turn nor made a call the daemon saw carries no record
// at all, on either surface: the key is absent rather than an object of empty fields, which is
// what the schemas on the other side of this wire read it as.
func TestAClaimWhoseAgentHasNotSteppedCarriesNoRecord(t *testing.T) {
	if view := StepViewOf(supervise.StepRecord{}); view != nil {
		t.Fatalf("StepViewOf(the zero record) = %+v, want nothing", view)
	}
	encoded, err := json.Marshal(ClaimView{Session: "ses_planner", State: "queued"})
	if err != nil {
		t.Fatalf("marshal the claim view: %v", err)
	}
	if strings.Contains(string(encoded), "step") {
		t.Fatalf("the claim view of an agent that has not stepped = %s, want no step key", encoded)
	}
}

// The record names the call and measures its arguments; their text is in no field of it, because
// a shell command routinely holds a credential and this view reaches every surface an operator
// reads (supervise.StepRecord).
func TestTheStepViewCarriesNoArgumentText(t *testing.T) {
	const secret = "ghp_averyrealtokenvalue"
	view := StepViewOf(supervise.StepRecord{
		At: time.Date(2026, 9, 22, 9, 41, 0, 0, time.UTC), Tool: "bash", CallID: "toolu_7",
		ArgsBytes: len(secret), ArgsHash: "0a1b2c3d4e5f6071",
		StartedAt: time.Date(2026, 9, 22, 9, 41, 0, 0, time.UTC),
	})
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal the step view: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("the step view = %s, want no argument text", encoded)
	}
}
