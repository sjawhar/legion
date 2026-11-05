package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// turnOutcome posts one report as it arrives on the wire: a map, so a case can name exactly the
// members its body carries and leave the rest off, which is what the two caller shapes are about.
func (h *harness) turnOutcome(body map[string]any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/turn-outcome", body, nil)
}

// turnFields are the members of a report that are the turn rather than the caller.
func turnFields(generation uint64) map[string]any {
	return map[string]any{"generation": generation, "sequence": 1, "outcome": string(claim.TurnClean)}
}

// The Claude bridge holds no registration secret — its pane's registration is claude-envoy's,
// inside Claude Code — so it reports under the launch's own boot token, which the daemon resolves
// to the same claim a registration's does. The claim is registered first because only a
// registered claim keeps a report (supervise's onTurnOutcome row), which is the order a pane
// runs in: claude-envoy registers inside Claude Code before the bridge's first turn.
func TestTurnOutcomeTakesTheLaunchesBootToken(t *testing.T) {
	h := newHarness(t)
	_, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")

	body := turnFields(registered.Generation)
	body["bootToken"] = bootToken
	body["outcome"] = string(claim.TurnProviderError)
	if recorder := h.turnOutcome(body); recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("a boot-token report = %d with body %s, want 204 and nothing", recorder.Code, recorder.Body)
	}
}

// The agent's own plugin reports as it always has, with the secret its registration was issued.
func TestTurnOutcomeTakesTheRegistrationsSecret(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")

	body := turnFields(registered.Generation)
	body["claimToken"] = string(token)
	body["sessionId"] = "ses_implementer"
	body["secret"] = registered.Secret
	body["outcome"] = string(claim.TurnRefused)
	body["category"] = "reasoning_extraction"
	if recorder := h.turnOutcome(body); recorder.Code != http.StatusNoContent {
		t.Fatalf("an agent report = %d with body %s, want 204", recorder.Code, recorder.Body)
	}
}

// A body that gave the agent's identity in part meant that identity and gave it incompletely:
// it is refused by the field it left out, never read as the boot caller it is not.
func TestTurnOutcomeRefusesAnAgentCallerGivenInPart(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")

	for _, tc := range []struct {
		left  string
		clear string
	}{
		{"claimToken", "claimToken"},
		{"sessionId", "sessionId"},
		{"secret", "secret"},
	} {
		t.Run("without "+tc.left, func(t *testing.T) {
			body := turnFields(registered.Generation)
			body["claimToken"] = string(token)
			body["sessionId"] = "ses_implementer"
			body["secret"] = registered.Secret
			delete(body, tc.clear)
			wantRefusal(t, h.turnOutcome(body), http.StatusBadRequest, tc.left+" is required")
		})
	}
}

// Each caller says who is calling, so a body that names both says it twice over and a body that
// names neither says it not at all; the route answers each with the one sentence that names both
// shapes.
func TestTurnOutcomeRefusesABodyNamingBothCallersOrNeither(t *testing.T) {
	h := newHarness(t)
	token, bootToken := h.launch("LEGION-208", claim.RoleImplementer)
	registered := h.registered(bootToken, "ses_implementer")
	sentence := "a turn outcome names its caller with claimToken, sessionId and secret, or with bootToken; give one"

	both := turnFields(registered.Generation)
	both["claimToken"] = string(token)
	both["sessionId"] = "ses_implementer"
	both["secret"] = registered.Secret
	both["bootToken"] = bootToken
	wantRefusal(t, h.turnOutcome(both), http.StatusBadRequest, sentence)

	wantRefusal(t, h.turnOutcome(turnFields(registered.Generation)), http.StatusBadRequest, sentence)
}

// A boot token no launch minted is an invalid token, not an unknown claim: the daemon says the
// same of it here as it says of a registration presenting one.
func TestTurnOutcomeRefusesABootTokenNoLaunchMinted(t *testing.T) {
	h := newHarness(t)
	h.launch("LEGION-208", claim.RoleImplementer)

	body := turnFields(1)
	body["bootToken"] = "a token nothing minted"
	wantRefusal(t, h.turnOutcome(body), http.StatusForbidden, claim.InvalidBootToken.Message)
}

// A pane the daemon has already replaced reports for a launch the claim has left: its token is
// refused, and so is a report that names any generation but the live launch's. Either would
// otherwise put another pane's turn on the claim the daemon counts this generation's turns for.
func TestTurnOutcomeHoldsABootTokenToItsOwnLaunch(t *testing.T) {
	h := newHarness(t)
	token, first := h.launch("LEGION-208", claim.RoleImplementer)
	replaced := h.stored(token).Generation
	current := h.relaunch(token)

	stale := turnFields(replaced)
	stale["bootToken"] = first
	wantRefusal(t, h.turnOutcome(stale), http.StatusForbidden, claim.InvalidBootToken.Message)

	wrongGeneration := turnFields(replaced)
	wrongGeneration["bootToken"] = current
	wantRefusal(t, h.turnOutcome(wrongGeneration), http.StatusConflict, claim.StaleGeneration.Message)
}
