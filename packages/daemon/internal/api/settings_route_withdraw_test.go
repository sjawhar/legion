package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/oidc"
	"github.com/sjawhar/envoy/oidc/oidctest"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// TestAProjectedTokenWithdrawsNoKeyItMayNotSet: the settings route's body maps a path to `null`
// to withdraw it — "A path mapped to `null` is withdrawn: the route change the daemon kept for
// it is over and the file's own value takes the key back" (settings_route.go). A withdrawal is a
// change to that path like any other, so the caller rule has to hold for it too: a caller that
// may not set a path must not be able to decide its value by withdrawing the operator's change
// and sending the key back to the file's.
//
// `instructions` is the key here because it is not plain data — it reaches a worker at its next
// launch, so `Row.Plain` is false for it and a projected caller may not set it — and because
// the fixture's configuration resolves with it changed, which a credential key's would not.
// This is the boundary this round adds, driven as the party it must refuse.
func TestAProjectedTokenWithdrawsNoKeyItMayNotSet(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	const audience = "legion-daemon"
	const dispatch = "system:serviceaccount:dispatch:dispatch"
	handler, live := settingsRouteServer(t, SettingsRouteAuth{
		Verifier:        oidc.NewLazyVerifier(issuer.URL(), audience),
		ServiceAccounts: []string{dispatch},
	})
	projected := issuer.Mint(t, key, issuer.Claims(dispatch, audience))

	// The operator sets the launch key, which is its own to set.
	const operators = "/etc/legion/operators.md"
	recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"instructions": json.RawMessage(`"` + operators + `"`)},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("the operator's call = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := settingsAnswer(t, recorder)["instructions"]; got.Refused != "" {
		t.Fatalf("the operator's own change was refused: %+v", got)
	}
	if held := live.Snapshot().InstructionsPath; held != operators {
		t.Fatalf("the daemon reads instructions from %q, want the operator's %q", held, operators)
	}

	// The projected caller withdraws it, which would hand the key back to the file.
	recorder = settingsCall(t, handler, projected, SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"instructions": nil},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("the projected call = %d; body %s", recorder.Code, recorder.Body)
	}
	result := settingsAnswer(t, recorder)["instructions"]
	if result.Refused == "" || result.Applied != "" {
		t.Errorf("the answer for the withdrawal = %+v, want it refused by name and nothing applied", result)
	}
	if held := live.Snapshot().InstructionsPath; held != operators {
		t.Errorf("after the withdrawal the daemon reads instructions from %q, want the operator's %q still", held, operators)
	}
}

// The design gate is the one key the daemon reads at each decision that a projected
// ServiceAccount token may not set (INFRA-1077's spec, version 17): the gate is the rule that
// says a person decides a tree's specification, and Dispatch's path — any deployment-scoped
// Dispatch user, a person or an agent — is not the party that may switch it off. The operator
// bearer's own change of the same key applies, and the projected caller's withdrawal of it is
// refused as the withdrawal of any other key it may not set.
//
// `gates.design` takes three spellings since INFRA-1272 — the word `off`, the word
// `root-issues`, and the list of steps the deployment gates — and the refusal is the row's, not
// the value's, so each spelling is refused and each is a row here. A deployment that gates no
// step and one that gates every step are the two ends the gate moves between, and neither end
// is Dispatch's to choose.
func TestAProjectedTokenDoesNotMoveTheDesignGate(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	const audience = "legion-daemon"
	const dispatch = "system:serviceaccount:dispatch:dispatch"
	for _, tc := range []struct {
		name  string
		value json.RawMessage
	}{
		{name: "the word off", value: json.RawMessage(`"off"`)},
		{name: "the word root-issues", value: json.RawMessage(`"root-issues"`)},
		{name: "a list of steps", value: json.RawMessage(`["decisions","criteria","spec"]`)},
		{name: "the empty list", value: json.RawMessage(`[]`)},
		{name: "the withdrawal", value: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, live := settingsRouteServer(t, SettingsRouteAuth{
				Verifier:        oidc.NewLazyVerifier(issuer.URL(), audience),
				ServiceAccounts: []string{dispatch},
			})
			projected := issuer.Mint(t, key, issuer.Claims(dispatch, audience))
			steps := live.Snapshot().Gates.Steps()

			recorder := settingsCall(t, handler, projected, SettingsChangeRequest{
				By:      "ana@example.com",
				Changes: map[string]json.RawMessage{"gates.design": tc.value},
			})

			if recorder.Code != http.StatusOK {
				t.Fatalf("the projected call = %d; body %s", recorder.Code, recorder.Body)
			}
			if got := settingsAnswer(t, recorder)["gates.design"]; got.Refused == "" || got.Applied != "" {
				t.Fatalf("the answer for gates.design = %+v, want it refused by name and nothing applied", got)
			}
			if held := live.Snapshot().Gates.Steps(); !slices.Equal(held, steps) {
				t.Fatalf("the daemon's design gate is %v, want the %v it was running on", held, steps)
			}
		})
	}
}

// The operator bearer's change of the same key applies, in each spelling the key takes: the key
// is no more fixed than it was, and only the projected arm is refused it.
func TestTheOperatorMovesTheDesignGateInEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value json.RawMessage
		want  []record.DesignStep
	}{
		{name: "the word off", value: json.RawMessage(`"off"`), want: config.DesignGateOff.Steps()},
		{name: "a list of steps", value: json.RawMessage(`["decisions","spec"]`),
			want: []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}},
		{name: "the empty list", value: json.RawMessage(`[]`), want: []record.DesignStep{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, live := settingsRouteServer(t, SettingsRouteAuth{})

			recorder := settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
				By:      "ana@example.com",
				Changes: map[string]json.RawMessage{"gates.design": tc.value},
			})

			if got := settingsAnswer(t, recorder)["gates.design"]; got.Refused != "" || got.Applied == "" {
				t.Fatalf("the operator's change of gates.design = %+v, want it applied", got)
			}
			if held := live.Snapshot().Gates.Steps(); !slices.Equal(held, tc.want) {
				t.Fatalf("the daemon's design gate is %v, want the operator's %v", held, tc.want)
			}
		})
	}
}
