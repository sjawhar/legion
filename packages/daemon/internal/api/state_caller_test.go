package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// podClaims, stateServer and stateFrom carry the claims the state's own snapshot holds: the route
// reads the caller's plugin contract from those, as the daemon's source answers them beside the
// state it projected from them (api.StateSource).

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
// statePodUID and whose state snapshot holds claims.
func stateServer(t *testing.T, state State, claims []supervise.Claim) http.Handler {
	t.Helper()
	return NewServer("127.0.0.1", 8437, Options{
		Steps: config.DesignGateRootIssues.Steps(),
		State: fakeSource{state: state, claims: claims}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
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
func stateFrom(t *testing.T, state State, remote string, claims []supervise.Claim) []byte {
	t.Helper()
	return stateAsked(t, stateServer(t, state, claims), remote)
}

// podClaims is the claims of the pod at statePodAddr, each recording contract, beside a claim in
// another pod at this daemon's contract, which says nothing of the caller.
func podClaims(contract int) []supervise.Claim {
	inPod := func(token claim.Token, pod string, contract int) supervise.Claim {
		return supervise.Claim{Token: token, State: supervise.StateReady, PluginContract: contract, Locator: &runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: pod + "/1",
			Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-legion-208", PodUID: pod, Container: "roles", Generation: 1},
		}}
	}
	return []supervise.Claim{
		inPod("legion-legion-legion-208-planner", statePodUID, contract),
		inPod("legion-legion-legion-208-implementer", statePodUID, contract),
		inPod("legion-legion-legion-210-planner", "4b8e1d60-3c92-4f7a-ae15-8d0c2b5f6a34", DaemonAPIVersion),
	}
}

// wakesServed is the `wakesWaiting` of a state body — the wakes it names, and whether the key is
// on the wire at all, which an empty array is not. The two are read apart because the reader
// before wakesWaitingContract refuses the key whether it names a wake or nothing, so an emptied
// key is as bad as a filled one and withoutWakesWaiting leaves it off.
func wakesServed(t *testing.T, body []byte) (wakes []map[string]any, carried bool) {
	t.Helper()
	var state struct {
		WakesWaiting json.RawMessage `json:"wakesWaiting"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	if state.WakesWaiting == nil {
		return nil, false
	}
	if err := json.Unmarshal(state.WakesWaiting, &wakes); err != nil {
		t.Fatalf("decode wakesWaiting %s: %v", state.WakesWaiting, err)
	}
	return wakes, true
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
// only the keys contract 45 knows, and the rest of the body as it is — the trees, the wakes the
// pacing holds and the gated steps among the keys it is served without, since such a pod is
// below treeContract, wakesWaitingContract and gatedStepsContract too. A pod at this contract,
// and a caller in no pod — the operator's `legion state`, whose open-rounds line reads both
// fields and whose queue line reads those wakes — are served everything. The claims are the
// state snapshot's own, so there is no readable state whose caller's contract is unknown: a
// snapshot without them is no state, and the route refuses it.
func TestTheStateRouteServesAPodItsPluginContractsClaimViews(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(supervise.PodUpdateContract-1))
	fullWakes, fullCarried := wakesServed(t, full)
	if !strings.Contains(string(full), `"updateRound"`) || !strings.Contains(string(full), `"updateHead"`) || !fullCarried || len(fullWakes) == 0 {
		t.Fatalf("the fixture's state %s carries no update round or no waiting wake; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		claims []supervise.Claim
		rounds bool
		wakes  bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", claims: podClaims(supervise.PodUpdateContract - 1)},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", claims: podClaims(DaemonAPIVersion), rounds: true, wakes: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", claims: podClaims(0), rounds: true, wakes: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", claims: podClaims(supervise.PodUpdateContract - 1), rounds: true, wakes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.claims)

			wakes, carried := wakesServed(t, body)
			if carried != tc.wakes {
				t.Errorf("the state %s carries the `wakesWaiting` key = %v, want %v; the reader before wakesWaitingContract refuses it, empty as much as filled", body, carried, tc.wakes)
			}
			if tc.wakes && len(wakes) != len(fullWakes) {
				t.Errorf("the state %s names %d of the wakes the pacing holds, want the fixture's %d", body, len(wakes), len(fullWakes))
			}
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
			// with the three memory keys gone too, since a pod below this contract's own reader
			// refuses those as well (memory_test.go holds that half), with contract 51's two tree
			// keys — `admission.trees` and each issue's `tree` — gone (tree_test.go holds that
			// half), with `wakesWaiting` gone, the key a reader before contract 53 refuses empty
			// as surely as filled (withoutWakesWaiting), and with the gated steps gone, the
			// field contract 55 added.
			var want, got map[string]any
			if err := json.Unmarshal(full, &want); err != nil {
				t.Fatal(err)
			}
			if !tc.wakes {
				delete(want, "wakesWaiting")
			}
			delete(want["admission"].(map[string]any), "trees")
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
				delete(issue, "gatedSteps")
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
