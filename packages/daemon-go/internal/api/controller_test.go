package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

func (h *harness) controllerSecret(bearer string) *httptest.ResponseRecorder {
	h.t.Helper()
	header := http.Header{}
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	return h.request(http.MethodPost, "/legion/v1/controller/secret", "{}", header)
}

// mintedSecret is what `legion controller start` fetches with the operator's bearer.
func (h *harness) mintedSecret() string {
	h.t.Helper()
	recorder := h.controllerSecret(testOperatorToken)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("controller secret = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var answer ControllerSecretResponse
	decodeInto(h.t, recorder, &answer)
	if answer.Secret == "" {
		h.t.Fatal("the controller secret route answered an empty secret")
	}
	return answer.Secret
}

// registeredController registers session with the controller capability and returns what the
// controller learned.
func (h *harness) registeredController(capability, session string) ControllerRegisterResponse {
	h.t.Helper()
	recorder := h.register(capability, session)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("register %s with the controller capability = %d, want 200; body %s", session, recorder.Code, recorder.Body)
	}
	var response ControllerRegisterResponse
	decodeInto(h.t, recorder, &response)
	return response
}

func (h *harness) controllerSessionGrant(session, secret string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{SessionID: session, Secret: secret}, nil)
}

// The route that mints a controller capability answers only the operator's bearer, and a refusal
// mints nothing: the incumbent controller keeps working.
func TestControllerSecretRefusesWithoutTheOperatorBearer(t *testing.T) {
	h := newHarness(t)
	for _, bearer := range []string{"", "not-the-operator-token"} {
		wantRefusal(t, h.controllerSecret(bearer), http.StatusForbidden, invalidOperatorToken)
	}
	if _, found, err := h.store.Controller(context.Background(), testProject); err != nil || found {
		t.Fatalf("controller record after two refusals = %v, %v, want none minted", found, err)
	}
}

// The capability `legion controller start` fetches is what its Oh My Pi registers with, through
// the claim registration route: the answer names the project's controller role token, the role
// `controller`, the capability's generation, and a secret of the registration's own — no tree
// and no issue, which a controller has neither of.
func TestAControllerRegistersWithTheMintedCapability(t *testing.T) {
	h := newHarness(t)
	capability := h.mintedSecret()

	recorder := h.register(capability, "ses_controller")
	if recorder.Code != http.StatusOK {
		t.Fatalf("register with the controller capability = %d; body %s", recorder.Code, recorder.Body)
	}
	var registration ControllerRegisterResponse
	decodeInto(t, recorder, &registration)
	if registration.ClaimToken != claim.ControllerToken(testProject) || registration.Role != ControllerRole ||
		registration.Generation != 1 || registration.Secret == "" || registration.Secret == capability {
		t.Fatalf("registration = %+v, want %s, role controller, generation 1, and a secret of its own",
			registration, claim.ControllerToken(testProject))
	}

	record, found, err := h.store.Controller(context.Background(), testProject)
	if err != nil || !found || record.Session != "ses_controller" {
		t.Fatalf("controller record = %+v, %v, %v, want ses_controller registered", record, found, err)
	}
}

// The operator's Oh My Pi is checked by no boot gate — the daemon gates only its own panes — so the
// registration holds the controller's plugin to this daemon's contract, naming both, and records
// no registration for a plugin that speaks another.
func TestAControllerSpeakingAnotherContractIsRefusedNamingBoth(t *testing.T) {
	h := newHarness(t)
	capability := h.mintedSecret()
	recorder := h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability, SessionID: "ses_controller", OmpSessionFile: "/sessions/ses_controller.jsonl",
		AgentID: "agent-ses_controller", PluginContract: GoDaemonAPIVersion + 1,
	}, nil)
	wantRefusal(t, recorder, http.StatusConflict, fmt.Sprintf(
		"pi-legion-envoy speaks Go daemon API contract %d; this daemon requires %d", GoDaemonAPIVersion+1, GoDaemonAPIVersion))
	record, found, err := h.store.Controller(context.Background(), testProject)
	if err != nil || !found || record.Registered() {
		t.Fatalf("controller record = %+v, %v, %v, want the capability minted and no session registered", record, found, err)
	}
}

// The registered session mints controller grants with its registration secret, and nothing else
// does: another session, or another secret, is refused as the claim form refuses them.
func TestTheRegisteredControllerMintsControllerGrants(t *testing.T) {
	h := newCredentialHarness(t, &tokenSource{})
	registration := h.registeredController(h.mintedSecret(), "ses_controller")

	recorder := h.controllerSessionGrant("ses_controller", registration.Secret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("controller-session grant = %d; body %s", recorder.Code, recorder.Body)
	}
	var grant GrantResponse
	decodeInto(t, recorder, &grant)
	// A controller grant, by what it may redeem: no repository credential.
	answer := h.request(http.MethodPost, "/legion/v1/gh-token", GrantCredentialRequest{GrantID: grant.GrantID}, nil)
	wantFailure(t, answer, http.StatusForbidden, "CONTROLLER_HAS_NO_REPOSITORY")

	for _, attempt := range []struct{ session, secret string }{
		{"ses_other", registration.Secret},
		{"ses_controller", "not-the-registration-secret"},
	} {
		wantFailure(t, h.controllerSessionGrant(attempt.session, attempt.secret), http.StatusForbidden, "INVALID_SESSION_SECRET")
	}
}

// A second `legion controller start` replaces the first: the new mint moves the generation on,
// the first capability no longer registers, the first registration no longer mints grants, and a
// grant the first minted before the rotation no longer redeems.
func TestASecondControllerSecretRevokesTheFirst(t *testing.T) {
	h, _, statuses := newArchitectHarness(t, nil, nil)
	first := h.mintedSecret()
	registration := h.registeredController(first, "ses_first")
	recorder := h.controllerSessionGrant("ses_first", registration.Secret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("first controller's grant = %d; body %s", recorder.Code, recorder.Body)
	}
	var earlier GrantResponse
	decodeInto(t, recorder, &earlier)

	second := h.mintedSecret()

	wantRefusal(t, h.register(first, "ses_first"), claim.InvalidBootToken.Status, claim.InvalidBootToken.Message)
	wantFailure(t, h.controllerSessionGrant("ses_first", registration.Secret), http.StatusForbidden, "INVALID_SESSION_SECRET")
	status := h.request(http.MethodPost, "/legion/v1/issues/status",
		IssueStatusRequest{GrantID: earlier.GrantID, Issue: "LEGION-208", Status: "backlog"}, nil)
	wantFailure(t, status, http.StatusForbidden, "GRANT_UNAVAILABLE")
	if len(statuses.writes) != 0 {
		t.Fatalf("Dispatch writes = %+v, want none from a revoked controller grant", statuses.writes)
	}

	replacement := h.registeredController(second, "ses_second")
	if replacement.Generation != 2 {
		t.Fatalf("second registration generation = %d, want 2", replacement.Generation)
	}
	recorder = h.controllerSessionGrant("ses_second", replacement.Secret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("second controller's grant = %d; body %s", recorder.Code, recorder.Body)
	}
	var current GrantResponse
	decodeInto(t, recorder, &current)
	status = h.request(http.MethodPost, "/legion/v1/issues/status",
		IssueStatusRequest{GrantID: current.GrantID, Issue: "LEGION-208", Status: "backlog"}, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("issue status with the second controller's grant = %d; body %s", status.Code, status.Body)
	}
	if len(statuses.writes) != 1 || statuses.writes[0] != (statusWrite{issue: "LEGION-208", status: "backlog"}) {
		t.Fatalf("Dispatch writes = %+v, want LEGION-208 backlog", statuses.writes)
	}
}

// A token that is neither a launch's boot token nor the current controller capability is the one
// refusal a registration has for an unknown token, whether or not a capability was ever minted.
func TestAnUnknownTokenIsRefusedWithOrWithoutAController(t *testing.T) {
	h := newHarness(t)
	wantRefusal(t, h.register("never-minted", "ses_unknown"), claim.InvalidBootToken.Status, claim.InvalidBootToken.Message)
	h.mintedSecret()
	wantRefusal(t, h.register("never-minted", "ses_unknown"), claim.InvalidBootToken.Status, claim.InvalidBootToken.Message)
}

// wantFailure holds a response to the status and the stable code of one credential or workflow
// refusal.
func wantFailure(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != code || failure.Error == "" {
		t.Fatalf("failure = %+v, want code %s and a sentence", failure, code)
	}
}

// Every credential and workflow route refuses a missing field in the one shape its other
// refusals take — a stable code beside the sentence — while the claim routes keep their
// sentence alone.
func TestCredentialAndWorkflowRoutesRefuseAMissingFieldWithACode(t *testing.T) {
	h, _, _ := newArchitectHarness(t, nil, nil)
	for _, route := range []struct{ path, body, field string }{
		{"/legion/v1/grants", `{"sessionId":"s","tree":"LEGION-208","issue":"LEGION-208"}`, "secret"},
		{"/legion/v1/grants", `{"sessionId":"s"}`, "secret"},
		{"/legion/v1/gh-token", `{}`, "grantId"},
		{"/legion/v1/git-credential", `{}`, "grantId"},
		{"/legion/v1/provisioning-credential", `{}`, "grantId"},
		{"/legion/v1/handoff/complete", `{"grantId":"g","commit":"c"}`, "summary"},
		{"/legion/v1/issues/status", `{"grantId":"g","issue":"LEGION-208"}`, "status"},
		{"/legion/v1/signoff", `{"grantId":"g"}`, "issue"},
		{"/legion/v1/children/park", `{"grantId":"g"}`, "issue"},
		{"/legion/v1/children/rerun", `{"grantId":"g"}`, "issue"},
	} {
		recorder := h.request(http.MethodPost, route.path, route.body, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s %s = %d, want 400; body %s", route.path, route.body, recorder.Code, recorder.Body)
		}
		var failure Failure
		decodeInto(t, recorder, &failure)
		if failure.Code != "MISSING_FIELD" || failure.Error != route.field+" is required" {
			t.Fatalf("%s %s = %+v, want MISSING_FIELD naming %s", route.path, route.body, failure, route.field)
		}
	}
	wantRefusal(t, h.request(http.MethodPost, "/legion/v1/claims/ready", `{"claimToken":"c","sessionId":"s","generation":1}`, nil),
		http.StatusBadRequest, "secret is required")
}
