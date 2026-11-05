package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/oidc"
	"github.com/sjawhar/envoy/oidc/oidctest"

	"github.com/sjawhar/legion/daemon/internal/config"
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
func TestAProjectedTokenDoesNotMoveTheDesignGate(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "cluster-key")
	const audience = "legion-daemon"
	const dispatch = "system:serviceaccount:dispatch:dispatch"
	handler, live := settingsRouteServer(t, SettingsRouteAuth{
		Verifier:        oidc.NewLazyVerifier(issuer.URL(), audience),
		ServiceAccounts: []string{dispatch},
	})
	projected := issuer.Mint(t, key, issuer.Claims(dispatch, audience))
	gate := live.Snapshot().Gates.Design

	recorder := settingsCall(t, handler, projected, SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"gates.design": json.RawMessage(`"off"`)},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("the projected call = %d; body %s", recorder.Code, recorder.Body)
	}
	if got := settingsAnswer(t, recorder)["gates.design"]; got.Refused == "" || got.Applied != "" {
		t.Fatalf("the answer for gates.design = %+v, want it refused by name and nothing applied", got)
	}
	if held := live.Snapshot().Gates.Design; held != gate {
		t.Fatalf("the daemon's design gate is %q, want the %q it was running on", held, gate)
	}

	// The withdrawal of a gate key is a change to it like any other, and is refused too.
	recorder = settingsCall(t, handler, projected, SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"gates.design": nil},
	})
	if got := settingsAnswer(t, recorder)["gates.design"]; got.Refused == "" || got.Applied != "" {
		t.Fatalf("the answer for the withdrawal = %+v, want it refused by name and nothing applied", got)
	}
	if held := live.Snapshot().Gates.Design; held != gate {
		t.Fatalf("after the withdrawal the design gate is %q, want the %q it was running on", held, gate)
	}

	// The operator bearer's same change applies: the key is no more fixed than it was.
	recorder = settingsCall(t, handler, testOperatorToken, SettingsChangeRequest{
		By:      "ana@example.com",
		Changes: map[string]json.RawMessage{"gates.design": json.RawMessage(`"off"`)},
	})
	if got := settingsAnswer(t, recorder)["gates.design"]; got.Refused != "" || got.Applied == "" {
		t.Fatalf("the operator's change of gates.design = %+v, want it applied", got)
	}
	if held := live.Snapshot().Gates.Design; held != config.DesignGateOff {
		t.Fatalf("the daemon's design gate is %q, want the operator's off", held)
	}
}
