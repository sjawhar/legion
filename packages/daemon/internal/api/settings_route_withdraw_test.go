package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/oidc"
	"github.com/sjawhar/envoy/oidc/oidctest"
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
