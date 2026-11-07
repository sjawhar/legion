package api

import (
	"encoding/json"
	"testing"
)

// The three fields settingSourcesContract added — `settingSources`, the `daemon` block's
// `commit` and each pull request's `requiresChecks` — are on every state this daemon writes,
// and every schema of `packages/contracts/src/legion-api.ts` was strict until INFRA-1048, so a
// plugin older than that refuses the whole body for one key it does not know. A caller in a pod
// whose claims record a contract below settingSourcesContract is therefore served the body
// without the three, as a caller below memoryContract is served it without the memory draw; a
// pod at this contract, a pod whose claims record none, and a caller in no pod are served all
// three.
func TestTheStateRouteWithholdsWhatTheSettingSourcesContractAddedFromAnOlderPlugin(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion))
	if !carriesSettingSources(t, full) || !carriesCommit(t, full) || !carriesRequiresChecks(t, full) {
		t.Fatalf("the fixture's state %s carries none of the three fields; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name    string
		remote  string
		sup     Supervisor
		carries bool
		// whole is whether the rest of the body is compared with the full one: a pod at
		// settingSourcesContract - 1 is above both other contracts, so the three fields are the
		// only difference, where a pod further behind is served no memory draw and no update
		// round either (TestTheStateRouteServesAPodItsPluginContractsIssues covers those two).
		whole bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", sup: podClaims(settingSourcesContract - 1), whole: true},
		{name: "a pod whose plugin is three contracts behind", remote: statePodAddr + ":41000", sup: podClaims(memoryContract - 2)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", sup: podClaims(settingSourcesContract - 1), whole: true},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", sup: podClaims(DaemonAPIVersion), carries: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", sup: podClaims(0), carries: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", sup: podClaims(settingSourcesContract - 1), carries: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.sup)

			if got := carriesSettingSources(t, body); got != tc.carries {
				t.Errorf("the state carries settingSources = %v, want %v: %s", got, tc.carries, body)
			}
			if got := carriesCommit(t, body); got != tc.carries {
				t.Errorf("the state carries daemon.commit = %v, want %v: %s", got, tc.carries, body)
			}
			if got := carriesRequiresChecks(t, body); got != tc.carries {
				t.Errorf("a pull request carries requiresChecks = %v, want %v: %s", got, tc.carries, body)
			}
			if tc.whole {
				// Nothing else of the body moves: the withheld state is the full one with the
				// three fields cleared, which is what the contract before 53 describes.
				if want := bodyWithout(t, populatedState(t)); want != string(normalized(t, body)) {
					t.Errorf("the withheld state differs from the full one with the three fields cleared:\n got %s\nwant %s", body, want)
				}
			}
		})
	}
}

// carriesSettingSources, carriesCommit and carriesRequiresChecks read the three fields off a state
// body as the plugin's reader does.
func carriesSettingSources(t *testing.T, body []byte) bool {
	t.Helper()
	_, held := decodedState(t, body)["settingSources"]
	return held
}

func carriesCommit(t *testing.T, body []byte) bool {
	t.Helper()
	var daemon map[string]any
	if err := json.Unmarshal(decodedState(t, body)["daemon"], &daemon); err != nil {
		t.Fatalf("decode the daemon block of %s: %v", body, err)
	}
	_, held := daemon["commit"]
	return held
}

func carriesRequiresChecks(t *testing.T, body []byte) bool {
	t.Helper()
	for key, view := range issueViews(t, body) {
		pullRequest, held := view["pullRequest"]
		if !held {
			continue
		}
		fields, object := pullRequest.(map[string]any)
		if !object {
			t.Fatalf("%s's pullRequest is no object: %v", key, pullRequest)
		}
		if _, carries := fields["requiresChecks"]; carries {
			return true
		}
	}
	return false
}

func decodedState(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	return state
}

// bodyWithout is the full state with the three fields settingSourcesContract added cleared,
// encoded and normalized, which is what the withheld body must equal. It reads the answer the
// daemon's own withholding gives on the typed state, rather than deleting the three keys from
// the full body by name: a rename of one of their tags moves this and the daemon together, and
// the three readers above, which name the keys the contract names, are what would catch it.
func bodyWithout(t *testing.T, state State) string {
	t.Helper()
	encoded, err := json.Marshal(withoutSettingSources(state))
	if err != nil {
		t.Fatalf("marshal the withheld state: %v", err)
	}
	return string(normalized(t, encoded))
}

// normalized is body re-encoded through a map, so two bodies are compared by their content and
// not by the order their keys were written in.
func normalized(t *testing.T, body []byte) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
