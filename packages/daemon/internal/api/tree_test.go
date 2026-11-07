package api

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The state the fixture builds (populatedState) names three live trees and gives every issue it
// records the root of its own, so one state proves both keys treeContract added and that they
// come off the wire together.

// treesServed is the `admission.trees` of a state body — the roots it names, and whether the key
// is on the wire at all, which an empty array is not — and the key of every issue whose record
// carries a `tree`, sorted. The two are read apart because the reader before the contract refuses
// `trees` whether it names a tree or nothing, so an emptied key is as bad as a filled one.
func treesServed(t *testing.T, body []byte) (roots []string, carried bool, issues []string) {
	t.Helper()
	var state struct {
		Admission map[string]json.RawMessage `json:"admission"`
		Issues    map[string]struct {
			Tree string `json:"tree"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	if raw, ok := state.Admission["trees"]; ok {
		carried = true
		if err := json.Unmarshal(raw, &roots); err != nil {
			t.Fatalf("decode admission.trees %s: %v", raw, err)
		}
	}
	for key, issue := range state.Issues {
		if issue.Tree != "" {
			issues = append(issues, key)
		}
	}
	slices.Sort(issues)
	return roots, carried, issues
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below treeContract is served a state with no `admission.trees`
// and no issue carrying `tree`, since that pod's plugin reads the state with a strict schema that
// refuses the whole body for either key, and refuses `trees` emptied as surely as filled. A pod
// whose claims cannot be read is served the body every reader since 45 takes, which is that one.
// A pod at this contract, a pod whose claims record no contract at all, and a caller the daemon
// cannot place in a pod — the operator's `legion state`, which names the live trees — are served
// both keys.
func TestTheStateRouteServesAPodItsPluginContractsTrees(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(treeContract-1))
	wantRoots, wantCarried, wantIssues := treesServed(t, full)
	if !wantCarried || len(wantRoots) == 0 || len(wantIssues) == 0 {
		t.Fatalf("the fixture's state %s names no live tree or gives no issue a tree of its own; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		sup    Supervisor
		trees  bool
	}{
		{name: "a pod whose plugin is the contract before this one", remote: statePodAddr + ":41000", sup: podClaims(treeContract - 1)},
		{name: "a pod below PodUpdateContract, below every one of them", remote: statePodAddr + ":41000", sup: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", sup: podClaims(treeContract - 1)},
		{name: "a pod whose claims cannot be read", remote: statePodAddr + ":41000", sup: claimsOnly{err: errors.New("the claims could not be read")}},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", sup: podClaims(DaemonAPIVersion), trees: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", sup: podClaims(0), trees: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", sup: podClaims(treeContract - 1), trees: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.sup)
			roots, carried, issues := treesServed(t, body)

			if !tc.trees {
				if carried {
					t.Errorf("the state %s carries admission.trees = %v; the reader before treeContract refuses the key, empty as much as filled", body, roots)
				}
				if len(issues) > 0 {
					t.Errorf("the state %s gives %v a tree of its own; the reader before treeContract refuses that key too", body, issues)
				}
				return
			}
			if !carried || !slices.Equal(roots, wantRoots) {
				t.Errorf("the state %s names the live trees %v (the key is on the wire = %v), want %v", body, roots, carried, wantRoots)
			}
			if !slices.Equal(issues, wantIssues) {
				t.Errorf("the state %s gives %v a tree of its own, want %v", body, issues, wantIssues)
			}
		})
	}
}

// The body a pod before the contract is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, live trees and all.
func TestTheStateServedToAPodBeforeTheTreeContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(treeContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if strings.Contains(string(behind), `"trees":`) || strings.Contains(string(behind), `"tree":`) {
		t.Fatalf("the pod before the contract was served %s, want neither the live trees nor an issue's own", behind)
	}
	if !strings.Contains(string(operator), `"trees":`) || !strings.Contains(string(operator), `"tree":`) {
		t.Fatalf("the operator was served %s, want the live trees and each issue's own", operator)
	}
}

// treeContract is the contract whose entry in version.go added the two keys, and this daemon's own
// contract is never below it: a caller at this contract is served them.
func TestTheTreeContractIsTheContractThatAddedTheKeys(t *testing.T) {
	const addedTrees = 51
	if treeContract != addedTrees {
		t.Fatalf("treeContract = %d, want %d, the contract whose entry in version.go added `admission.trees` and the issues' `tree`",
			treeContract, addedTrees)
	}
	if treeContract > DaemonAPIVersion {
		t.Fatalf("treeContract = %d, above DaemonAPIVersion %d: no caller could be served the keys",
			treeContract, DaemonAPIVersion)
	}
}
