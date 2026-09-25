package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The two shapes a locator is ever written in. They are goldens rather than assertions about
// fields because the bytes are what survives: a locator is persisted as jsonb on the claim row
// and projected into the state the plugin reads, so a Go-side rename that the tests only read
// back through Go would be invisible until a daemon restart failed to find its own panes.
//
// A sandbox is named for its claim: the token lowercased to a DNS-1123 name, and past 63
// characters a readable prefix and an 8-hex hash of the whole token. The name is the same for
// every generation of the claim; this token is short, so its name is the token lowercased.
const (
	tmuxLocatorJSON = `{"runtime":"tmux","claim":"legion-omp-LEGION-208-tester",` +
		`"incarnation":"31847:918273","tmux":{"window":"@3","pane":"%41"}}`
	sandboxLocatorJSON = `{"runtime":"sandbox","claim":"legion-omp-LEGION-208-tester",` +
		`"incarnation":"3f2b1c7e-9a4d-4f1b-8c2e-7d6a5b4c3e2f",` +
		`"sandbox":{"namespace":"legion","name":"legion-omp-legion-208-tester"}}`
)

func tmuxLocator() Locator {
	return Locator{
		Runtime:     RuntimeTmux,
		Claim:       claim.Token("legion-omp-LEGION-208-tester"),
		Incarnation: "31847:918273",
		Tmux:        &TmuxLocator{Window: "@3", Pane: "%41"},
	}
}

func sandboxLocator() Locator {
	return Locator{
		Runtime:     RuntimeSandbox,
		Claim:       claim.Token("legion-omp-LEGION-208-tester"),
		Incarnation: "3f2b1c7e-9a4d-4f1b-8c2e-7d6a5b4c3e2f",
		Sandbox:     &SandboxLocator{Namespace: "legion", Name: "legion-omp-legion-208-tester"},
	}
}

func TestLocatorRoundTripsByteForByte(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		locator Locator
		want    string
	}{
		{name: "tmux", locator: tmuxLocator(), want: tmuxLocatorJSON},
		{name: "sandbox", locator: sandboxLocator(), want: sandboxLocatorJSON},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(testCase.locator)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != testCase.want {
				t.Fatalf("locator json\n got: %s\nwant: %s", encoded, testCase.want)
			}

			var read Locator
			if err := json.Unmarshal([]byte(testCase.want), &read); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := read.Validate(); err != nil {
				t.Fatalf("validate a locator this package wrote: %v", err)
			}
			again, err := json.Marshal(read)
			if err != nil {
				t.Fatalf("marshal the locator read back: %v", err)
			}
			if string(again) != testCase.want {
				t.Fatalf("round trip\n got: %s\nwant: %s", again, testCase.want)
			}
		})
	}
}

// The backend member the runtime did not write is absent from the bytes, not `null`: the state
// the plugin reads parses strictly, and a `"sandbox":null` on a tmux locator is a member it has
// no schema for.
func TestTheUnusedBackendMemberIsAbsent(t *testing.T) {
	encoded, err := json.Marshal(tmuxLocator())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "sandbox") {
		t.Fatalf("a tmux locator carried a sandbox member: %s", encoded)
	}
}

// `encoding/json` cannot refuse an absent member, so validation is a separate step run wherever a
// locator is read back — the store load and the API projection. Everything it refuses is a record
// nothing could act on: there is no process to probe, stop, or resume behind it.
func TestValidateRefusesALocatorNothingCouldBeActedOnThrough(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		locator      Locator
		wantFragment string
	}{
		{
			name:         "no runtime",
			locator:      Locator{Claim: "legion-omp-LEGION-208-tester", Incarnation: "1:2", Tmux: &TmuxLocator{Window: "@3", Pane: "%41"}},
			wantFragment: "runtime",
		},
		{
			name:         "no claim",
			locator:      Locator{Runtime: RuntimeTmux, Incarnation: "1:2", Tmux: &TmuxLocator{Window: "@3", Pane: "%41"}},
			wantFragment: "claim",
		},
		{
			name:         "no incarnation",
			locator:      Locator{Runtime: RuntimeTmux, Claim: "legion-omp-LEGION-208-tester", Tmux: &TmuxLocator{Window: "@3", Pane: "%41"}},
			wantFragment: "incarnation",
		},
		{
			name: "a sandbox member under runtime tmux",
			locator: Locator{
				Runtime:     RuntimeTmux,
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "1:2",
				Sandbox:     &SandboxLocator{Namespace: "legion", Name: "pod"},
			},
			wantFragment: `runtime "tmux"`,
		},
		{
			name: "a tmux member under runtime sandbox",
			locator: Locator{
				Runtime:     RuntimeSandbox,
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "1:2",
				Tmux:        &TmuxLocator{Window: "@3", Pane: "%41"},
			},
			wantFragment: `runtime "sandbox"`,
		},
		{
			name: "both backend members",
			locator: Locator{
				Runtime:     RuntimeTmux,
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "1:2",
				Tmux:        &TmuxLocator{Window: "@3", Pane: "%41"},
				Sandbox:     &SandboxLocator{Namespace: "legion", Name: "pod"},
			},
			wantFragment: "exactly one",
		},
		{
			name:         "no backend member at all",
			locator:      Locator{Runtime: RuntimeTmux, Claim: "legion-omp-LEGION-208-tester", Incarnation: "1:2"},
			wantFragment: `runtime "tmux"`,
		},
		{
			name: "a runtime no backend answers to",
			locator: Locator{
				Runtime:     "kubernetes",
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "1:2",
				Sandbox:     &SandboxLocator{Namespace: "legion", Name: "pod"},
			},
			wantFragment: `"kubernetes"`,
		},
		{
			name: "a tmux member with no pane",
			locator: Locator{
				Runtime:     RuntimeTmux,
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "1:2",
				Tmux:        &TmuxLocator{Window: "@3"},
			},
			wantFragment: "pane",
		},
		{
			name: "a sandbox member with no name",
			locator: Locator{
				Runtime:     RuntimeSandbox,
				Claim:       "legion-omp-LEGION-208-tester",
				Incarnation: "3f2b1c7e",
				Sandbox:     &SandboxLocator{Namespace: "legion"},
			},
			wantFragment: "name",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.locator.Validate()
			if err == nil {
				t.Fatalf("validate: got nil, want a refusal naming %q", testCase.wantFragment)
			}
			if !strings.Contains(err.Error(), testCase.wantFragment) {
				t.Fatalf("refusal %q does not name %q", err, testCase.wantFragment)
			}
		})
	}
}

// A Known is one claim: every runtime refuses one whose locator is another claim's process, or not
// a locator at all, before it releases or sweeps anything. A claim with no process is known with
// no locator.
func TestKnownRefusesALocatorThatIsNotItsClaims(t *testing.T) {
	loc := tmuxLocator()
	unaddressable := tmuxLocator()
	unaddressable.Tmux = nil
	for _, testCase := range []struct {
		name         string
		known        Known
		wantFragment string
	}{
		{name: "another claim's locator", known: Known{Claim: "legion-omp-LEGION-208-reviewer", Locator: &loc}, wantFragment: "legion-omp-LEGION-208-tester"},
		{name: "a locator nothing could be acted on through", known: Known{Claim: loc.Claim, Locator: &unaddressable}, wantFragment: "no tmux member"},
		{name: "no claim", known: Known{Locator: &loc}, wantFragment: "no claim token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.known.Validate()
			if err == nil || !strings.Contains(err.Error(), testCase.wantFragment) {
				t.Fatalf("validate: got %v, want a refusal naming %q", err, testCase.wantFragment)
			}
		})
	}
	for _, known := range []Known{{Claim: loc.Claim, Locator: &loc}, {Claim: loc.Claim}} {
		if err := known.Validate(); err != nil {
			t.Errorf("validate %+v: %v", known, err)
		}
	}
}
