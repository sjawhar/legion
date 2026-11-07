package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimsOnly is a Supervisor that answers Claims and nothing else: the state route reads the
// claims of the pod that asked, and decides nothing about any claim.
type claimsOnly struct {
	claims []supervise.Claim
	err    error
}

func (c claimsOnly) Machine(claim.Token) (*supervise.Machine, bool) { return nil, false }
func (c claimsOnly) Create(context.Context, supervise.Claim, string) (*supervise.Machine, bool, error) {
	return nil, false, errors.New("unexpected create")
}
func (c claimsOnly) Claims(context.Context) ([]supervise.Claim, error) { return c.claims, c.err }

const (
	// statePodUID is the pod populatedState's LEGION-208 workers run in, and statePodAddr its
	// address, from which that pod's plugin asks for the state.
	statePodUID  = "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"
	statePodAddr = "10.1.24.6"
)

// contract45ClaimView is every key the claim view of contract 45's strict reader knows
// (packages/contracts/src/legion-api.ts's legionClaimView at pi-legion v17.1.2, the last release
// declaring 45): one key more and it refuses the whole state.
var contract45ClaimView = []string{"session", "state", "locator", "capabilities", "pluginContract"}

// stateServer serves state, one value for every request, as a daemon whose pod at statePodAddr is
// statePodUID and whose claims sup answers.
func stateServer(t *testing.T, state State, sup Supervisor) http.Handler {
	t.Helper()
	return NewServer("127.0.0.1", 8437, Options{
		State: fakeSource{state: state}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		Supervisor: sup,
		CallerPod: func(addr netip.Addr) (string, bool) {
			if addr == netip.MustParseAddr(statePodAddr) {
				return statePodUID, true
			}
			return "", false
		},
	}).Handler
}

// stateAsked is GET /legion/v1/state on handler, asked from remote.
func stateAsked(t *testing.T, handler http.Handler, remote string) []byte {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
	request.RemoteAddr = remote
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// stateFrom is GET /legion/v1/state asked from remote of a server of its own.
func stateFrom(t *testing.T, state State, remote string, sup Supervisor) []byte {
	t.Helper()
	return stateAsked(t, stateServer(t, state, sup), remote)
}

// podClaims is the claims of the pod at statePodAddr, each recording contract, beside a claim in
// another pod at this daemon's contract, which says nothing of the caller.
func podClaims(contract int) claimsOnly {
	inPod := func(token claim.Token, pod string, contract int) supervise.Claim {
		return supervise.Claim{Token: token, State: supervise.StateReady, PluginContract: contract, Locator: &runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: pod + "/1",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-208", PodUID: pod, Container: "roles", Generation: 1},
		}}
	}
	return claimsOnly{claims: []supervise.Claim{
		inPod("legion-legion-legion-208-planner", statePodUID, contract),
		inPod("legion-legion-legion-208-implementer", statePodUID, contract),
		inPod("legion-legion-legion-210-planner", "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34", DaemonAPIVersion),
	}}
}

// claimViews is every claim view of a state body, decoded as the plugin's reader sees it.
func claimViews(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var state struct {
		Issues map[string]struct {
			Architect map[string]any `json:"architect"`
			Workers   map[string]struct {
				Claim map[string]any `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	var views []map[string]any
	for _, issue := range state.Issues {
		if issue.Architect != nil {
			views = append(views, issue.Architect)
		}
		for _, worker := range issue.Workers {
			views = append(views, worker.Claim)
		}
	}
	return views
}

// A plugin one contract behind the daemon reads the state with a schema that knows no
// `updateRound` or `updateHead` and refuses the whole body for either, which is every legion
// tool call of a worker the update round is still serving (read_record, handoff_complete). The
// route authenticates nobody, so it knows that plugin by the pod the request came from: a pod
// whose claims record a contract below supervise.PodUpdateContract is served claim views with
// only the keys contract 45 knows, and the rest of the body as it is. A pod at this contract, and
// a caller in no pod — the operator's `legion state`, whose open-rounds line reads both fields —
// are served both; a pod whose claims cannot be read is served the body every reader since 45
// takes.
func TestTheStateRouteServesAPodItsPluginContractsClaimViews(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(supervise.PodUpdateContract-1))
	if !strings.Contains(string(full), `"updateRound"`) || !strings.Contains(string(full), `"updateHead"`) {
		t.Fatalf("the fixture's state %s carries no update round; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		sup    Supervisor
		rounds bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", sup: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", sup: podClaims(supervise.PodUpdateContract - 1)},
		{name: "a pod whose claims cannot be read", remote: statePodAddr + ":41000", sup: claimsOnly{err: errors.New("the store is gone")}},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", sup: podClaims(DaemonAPIVersion), rounds: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", sup: podClaims(0), rounds: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", sup: podClaims(supervise.PodUpdateContract - 1), rounds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.sup)

			if tc.rounds {
				if string(body) != string(full) {
					t.Fatalf("state = %s, want the state as this contract writes it, %s", body, full)
				}
				return
			}
			for _, view := range claimViews(t, body) {
				for key := range view {
					if !slices.Contains(contract45ClaimView, key) {
						t.Errorf("claim view %v carries %q, which contract 45's strict reader refuses", view, key)
					}
				}
			}
			// Nothing else of the body differs: it is the full state with the two claim keys gone,
			// and with the three memory keys, contract 51's two tree keys — `admission.trees` and
			// each issue's `tree` — and contract 53's retired entries under `strandedClaims` gone
			// too, since a pod below this contract's own reader refuses those as well
			// (memory_test.go and tree_test.go hold two of those halves, and
			// strandedWithoutRetired below the third).
			var want, got map[string]any
			if err := json.Unmarshal(full, &want); err != nil {
				t.Fatal(err)
			}
			delete(want["admission"].(map[string]any), "trees")
			want["strandedClaims"] = strandedWithoutRetired(t, want["strandedClaims"])
			for _, issue := range want["issues"].(map[string]any) {
				issue := issue.(map[string]any)
				views := []any{issue["architect"]}
				for _, worker := range issue["workers"].(map[string]any) {
					views = append(views, worker.(map[string]any)["claim"])
				}
				for _, view := range views {
					if view, ok := view.(map[string]any); ok {
						delete(view, "updateRound")
						delete(view, "updateHead")
					}
				}
				delete(issue, "memoryGroup")
				delete(issue, "memoryDrawMode")
				delete(issue, "memoryDrawnAt")
				delete(issue, "tree")
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("state = %s, want the full state without those keys, %s", gotJSON, wantJSON)
			}
		})
	}
}

// strandedWithoutRetired is the `strandedClaims` of a baseline body with its retired entries gone,
// the report a pod below retiredStrandedContract is served (api's withoutRetiredStranded). The key
// is never null, so a body every entry of which is retired keeps it as an empty array.
func strandedWithoutRetired(t *testing.T, stranded any) []any {
	t.Helper()
	rows, ok := stranded.([]any)
	if !ok {
		t.Fatalf("the state's strandedClaims is %T, want the array the route always writes", stranded)
	}
	kept := []any{}
	for _, row := range rows {
		if row.(map[string]any)["state"] == string(supervise.StateRetired) {
			continue
		}
		kept = append(kept, row)
	}
	return kept
}

// strandedServed is the report of claims nothing will wake of a state body, entry for entry as
// the plugin's reader sees it, and whether `strandedClaims` is on the wire at all: the key is
// never absent and never null, an emptied report included.
func strandedServed(t *testing.T, body []byte) (stranded []map[string]string, carried bool) {
	t.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	raw, carried := state["strandedClaims"]
	if !carried {
		return nil, false
	}
	if err := json.Unmarshal(raw, &stranded); err != nil {
		t.Fatalf("decode strandedClaims %s: %v", raw, err)
	}
	return stranded, stranded != nil
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below retiredStrandedContract is served the report of claims
// nothing will wake without its retired entries, since that pod's plugin parses
// `strandedClaims[].state` with a strict enum of `suspended` and `failed` and refuses the whole
// state body for a third value; every entry whose state it knows stays, in the order the report
// named it. A pod whose claims cannot be read is served that same report. A pod at this contract,
// a pod whose claims record no contract at all, and a caller the daemon cannot place in a pod —
// the operator's `legion state`, which names a retired claim under the workflow start it needs —
// are served every entry.
func TestTheStateRouteServesAPodItsPluginContractsStrandedClaims(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(retiredStrandedContract-1))
	every, carried := strandedServed(t, full)
	var kept []map[string]string
	for _, entry := range every {
		if entry["state"] != string(supervise.StateRetired) {
			kept = append(kept, entry)
		}
	}
	if !carried || len(kept) == len(every) || len(kept) == 0 {
		t.Fatalf("the fixture's state %s names no retired claim nothing will wake beside claims in other states; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name    string
		remote  string
		sup     Supervisor
		retired bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", sup: podClaims(retiredStrandedContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", sup: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", sup: podClaims(retiredStrandedContract - 1)},
		{name: "a pod whose claims cannot be read", remote: statePodAddr + ":41000", sup: claimsOnly{err: errors.New("the claims could not be read")}},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", sup: podClaims(DaemonAPIVersion), retired: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", sup: podClaims(0), retired: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", sup: podClaims(retiredStrandedContract - 1), retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.sup)
			stranded, carried := strandedServed(t, body)

			want := kept
			if tc.retired {
				want = every
			}
			if !carried {
				t.Fatalf("the state %s carries no strandedClaims; the key is never absent and never null", body)
			}
			if !reflect.DeepEqual(stranded, want) {
				t.Fatalf("the claims nothing will wake = %v, want %v", stranded, want)
			}
		})
	}
}

// retiredStrandedContract is the contract whose entry in version.go added `retired` to the states
// a stranded claim may be in, and this daemon's own contract is never below it: a caller at this
// contract is served the retired entries.
func TestTheRetiredStrandedContractIsTheContractThatAddedTheState(t *testing.T) {
	const addedRetiredStranded = 53
	if retiredStrandedContract != addedRetiredStranded {
		t.Fatalf("retiredStrandedContract = %d, want %d, the contract whose entry in version.go added `retired` to a stranded claim's state",
			retiredStrandedContract, addedRetiredStranded)
	}
	if retiredStrandedContract > DaemonAPIVersion {
		t.Fatalf("retiredStrandedContract = %d, above DaemonAPIVersion %d: no caller could be served a retired claim",
			retiredStrandedContract, DaemonAPIVersion)
	}
}

// The body a pod a contract behind is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, update rounds and all.
func TestTheStateServedToAPodAContractBehindLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(supervise.PodUpdateContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if strings.Contains(string(behind), `"updateRound"`) {
		t.Fatalf("the pod a contract behind was served %s, want no updateRound", behind)
	}
	if !strings.Contains(string(operator), `"updateRound"`) || !strings.Contains(string(operator), `"updateHead"`) {
		t.Fatalf("the operator after it was served %s, want the update rounds the source answered", operator)
	}
}
