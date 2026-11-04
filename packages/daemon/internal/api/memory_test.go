package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// memoryDrawnAt is the instant LEGION-208's tree was drawn into its memory group, which its root
// and every issue under it carry.
var memoryDrawnAt = time.Date(2026, 9, 20, 11, 4, 5, 0, time.UTC)

// The state the fixture builds carries one drawn tree (populatedState): the root LEGION-208 and
// LEGION-209, a child of it, both showing the root's draw as internal/projection fills them,
// beside the issues no draw has reached. Its claim views carry the pod's update round, so one
// state proves both downgrades and that they compose.

// issueViews is every issue of a state body, decoded as the plugin's reader sees it, by key.
func issueViews(t *testing.T, body []byte) map[string]map[string]any {
	t.Helper()
	var state struct {
		Issues map[string]map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	return state.Issues
}

// An issue's memory group is its tree's: the root that was drawn and every issue under it carry
// the group, the mode it was drawn under and the instant of the draw, and an issue of a tree no
// draw has reached carries none of the three, which `omitempty` leaves off the wire rather than
// writing as empty strings a strict reader would refuse.
func TestTheStateShowsEveryIssueItsTreesMemoryDraw(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion))
	issues := issueViews(t, body)

	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		view, shown := issues[key]
		if !shown {
			t.Fatalf("the state %s carries no issue %s", body, key)
		}
		want := map[string]any{
			"memoryGroup":    "memory",
			"memoryDrawMode": "random",
			"memoryDrawnAt":  "2026-09-20T11:04:05Z",
		}
		for field, value := range want {
			if view[field] != value {
				t.Errorf("%s's %s = %v, want its tree's %v", key, field, view[field], value)
			}
		}
	}
	// LEGION-210 is a tree of its own that no draw has reached.
	for _, field := range []string{"memoryGroup", "memoryDrawMode", "memoryDrawnAt"} {
		if value, carried := issues["LEGION-210"][field]; carried {
			t.Errorf("LEGION-210, which no draw reached, carries %s = %v", field, value)
		}
	}
}

// contract46Issue is every key the issue record of contract 46's strict reader knows
// (packages/contracts/src/legion-api.ts's legionIssue at the last release declaring 46): one key
// more and it refuses the whole state, which is every `read_record` of every worker in a pod one
// release behind.
var contract46Issue = []string{
	"key", "generation", "phase", "status", "repo", "defaultBranch", "holdReason", "architect",
	"workers", "pullRequest", "designGate", "slot", "waitingOn", "check",
}

// The route authenticates nobody, so it knows a plugin by the pod the request came from: a pod
// whose claims record a contract below memoryContract is served issues with only the keys
// contract 46 knows, and the rest of the body as it is. The two downgrades compose: a pod at 46 is
// served the update rounds it reads and no memory, a pod below 46 neither, and a pod at this
// contract, a pod whose claims record none and a caller in no pod — the operator's `legion state`,
// whose memory line reads the draws — are served both.
func TestTheStateRouteServesAPodItsPluginContractsIssues(t *testing.T) {
	full := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(memoryContract-1))
	if !strings.Contains(string(full), `"memoryGroup"`) || !strings.Contains(string(full), `"updateRound"`) {
		t.Fatalf("the fixture's state %s carries no memory draw or no update round; this test proves nothing", full)
	}

	for _, tc := range []struct {
		name   string
		remote string
		sup    Supervisor
		memory bool
		rounds bool
	}{
		{name: "a pod whose plugin is a contract behind", remote: statePodAddr + ":41000", sup: podClaims(memoryContract - 1), rounds: true},
		{name: "a pod two contracts behind, below both", remote: statePodAddr + ":41000", sup: podClaims(supervise.PodUpdateContract - 1)},
		{name: "the same pod asking over IPv6", remote: "[::ffff:" + statePodAddr + "]:41000", sup: podClaims(memoryContract - 1), rounds: true},
		{name: "a pod at this contract", remote: statePodAddr + ":41000", sup: podClaims(DaemonAPIVersion), memory: true, rounds: true},
		{name: "a pod whose claims record no contract", remote: statePodAddr + ":41000", sup: podClaims(0), memory: true, rounds: true},
		{name: "a caller in no pod", remote: "192.0.2.1:41000", sup: podClaims(memoryContract - 1), memory: true, rounds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := stateFrom(t, populatedState(t), tc.remote, tc.sup)

			// Only LEGION-208's tree was drawn: its root and its child carry the draw when the
			// caller is served it, and no other issue carries one whoever asks.
			drawn := map[string]bool{"LEGION-208": true, "LEGION-209": true}
			for key, view := range issueViews(t, body) {
				carries := view["memoryGroup"] != nil || view["memoryDrawMode"] != nil || view["memoryDrawnAt"] != nil
				if want := tc.memory && drawn[key]; carries != want {
					t.Errorf("issue %s of %s carries a memory draw = %v, want %v", key, body, carries, want)
				}
				if tc.memory {
					continue
				}
				for field := range view {
					if !slices.Contains(contract46Issue, field) {
						t.Errorf("issue view %v carries %q, which contract 46's strict reader refuses", view, field)
					}
				}
			}
			if rounds := strings.Contains(string(body), `"updateRound"`); rounds != tc.rounds {
				t.Errorf("the state %s carries an update round = %v, want %v", body, rounds, tc.rounds)
			}

			// Nothing else of the body differs: it is the full state with the stripped keys gone.
			var want, got map[string]any
			if err := json.Unmarshal(full, &want); err != nil {
				t.Fatal(err)
			}
			for _, issue := range want["issues"].(map[string]any) {
				issue := issue.(map[string]any)
				if !tc.memory {
					delete(issue, "memoryGroup")
					delete(issue, "memoryDrawMode")
					delete(issue, "memoryDrawnAt")
				}
				if tc.rounds {
					continue
				}
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
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("state = %s, want the full state without the stripped keys, %s", gotJSON, wantJSON)
			}
		})
	}
}

// The body a pod a contract behind is served is a copy: the next caller of the same daemon, at
// this contract, is served the state as the source answered it, memory draws and all.
func TestTheStateServedToAPodBeforeTheMemoryContractLeavesTheNextCallersStateWhole(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(memoryContract-1))

	behind := stateAsked(t, handler, statePodAddr+":41000")
	operator := stateAsked(t, handler, "192.0.2.1:41000")

	if strings.Contains(string(behind), `"memoryGroup"`) {
		t.Fatalf("the pod a contract behind was served %s, want no memoryGroup", behind)
	}
	if !strings.Contains(string(operator), `"memoryGroup"`) {
		t.Fatalf("the operator was served %s, want the memory draw the state carries", operator)
	}
}

// memoryContract is the contract whose entry in version.go added the three fields, and this
// daemon's own contract is never below it: a caller at this contract is served them.
func TestTheMemoryContractIsTheContractThatAddedTheFields(t *testing.T) {
	const addedMemory = 47
	if memoryContract != addedMemory {
		t.Fatalf("memoryContract = %d, want %d, the contract whose entry in version.go added the issues' memory group",
			memoryContract, addedMemory)
	}
	if memoryContract > DaemonAPIVersion {
		t.Fatalf("memoryContract = %d, above DaemonAPIVersion %d: no caller could be served the fields",
			memoryContract, DaemonAPIVersion)
	}
}
