package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// newStaleHarness is a harness with its log captured so a test can read the Warn line a
// stale-caller or an undecodable-body refusal writes (LEGION-164). Its supervise deps can
// relaunch a stale-plugin process (newHarness's default).
func newStaleHarness(t *testing.T) (*harness, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t)
	logs := &bytes.Buffer{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
		PromptAgents: testPromptAgents, PluginRelaunchable: true,
		State: fakeSource{}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		Log: slog.New(slog.NewJSONHandler(logs, nil)),
	}).Handler
	return h, logs
}

// A claim whose last registration the daemon relaunched for this contract — the stale-registration
// 409 pi-legion exits on — is refused on ready and on the grants route's claim form, naming both
// contracts, once the route has decoded its caller; each refusal logs a Warn naming the route,
// the claim, and both contracts.
func TestStaleClaimIsRefusedOnReadyAndGrantsAndLogsAWarn(t *testing.T) {
	h, logs := newStaleHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	current := h.registerContract(boot, session, DaemonAPIVersion, true)
	stale := DaemonAPIVersion - 1
	h.makeStale(t, token, session, current.Secret, stale)
	sentence := claim.StalePluginContract(stale, DaemonAPIVersion).Message

	wantRefusal(t,
		h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
			ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation,
		}, nil),
		http.StatusConflict, sentence)

	grantsRecorder := h.request(http.MethodPost, "/legion/v1/grants", GrantRequest{
		SessionID: session, Secret: current.Secret, Tree: "LEGION-208", Issue: "LEGION-208",
	}, nil)
	if grantsRecorder.Code != http.StatusConflict {
		t.Fatalf("stale grants status = %d, want 409; body %s", grantsRecorder.Code, grantsRecorder.Body)
	}
	var failure Failure
	decodeInto(t, grantsRecorder, &failure)
	if failure.Code != "STALE_PLUGIN_CONTRACT" {
		t.Fatalf("stale grants code = %q, want STALE_PLUGIN_CONTRACT", failure.Code)
	}
	if failure.Error != sentence {
		t.Fatalf("grants refusal sentence = %q, want %q", failure.Error, sentence)
	}

	logged := logs.String()
	if !strings.Contains(logged, `"msg":"api: refused a call from a process on another plugin contract"`) {
		t.Fatalf("logs %s do not carry the refusal's Warn line", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf(`"pluginContract":%d`, stale)) ||
		!strings.Contains(logged, fmt.Sprintf(`"daemonApiVersion":%d`, DaemonAPIVersion)) {
		t.Fatalf("logs %s do not name both contracts", logged)
	}
	if strings.Contains(logged, current.Secret) {
		t.Fatalf("logs %s carry the claim's secret", logged)
	}
}

// A claim's grant, minted while it was current, is refused on handoff/complete once the claim has
// gone stale — a daemon restart that recorded no relaunch for it in this harness (see
// harness.makeStale) — with the same sentence and code the grants route answers with.
func TestStaleClaimIsRefusedOnHandoffComplete(t *testing.T) {
	github := newHandoffGitHub(t)
	h, _ := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	grantID := implementer.grant(t)
	stale := DaemonAPIVersion - 1
	h.makeStale(t, implementer.token, implementer.session, implementer.secret, stale)

	recorder := h.complete(HandoffCompleteRequest{GrantID: grantID, Summary: "implemented", Commit: "aabbcc"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("handoff/complete status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != "STALE_PLUGIN_CONTRACT" {
		t.Fatalf("handoff/complete code = %q, want STALE_PLUGIN_CONTRACT", failure.Code)
	}
	if want := claim.StalePluginContract(stale, DaemonAPIVersion).Message; failure.Error != want {
		t.Fatalf("handoff/complete refusal sentence = %q, want %q", failure.Error, want)
	}
}

// An undecodable grants body (an unknown field, as 2026-10-12's "push" was) from a stale claim is
// refused the stale-caller 409; the identical body from a current claim gets today's 400 naming
// the field, logged with the route and the decoder's error.
func TestUndecodableGrantsBodyRefusesAStaleClaimButNamesTheFieldForACurrentOne(t *testing.T) {
	h, logs := newStaleHarness(t)
	body := func(session, secret string) string {
		return fmt.Sprintf(`{"sessionId":%q,"secret":%q,"tree":"LEGION-208","issue":"LEGION-208","push":true}`, session, secret)
	}

	staleToken, staleBoot := h.launch("LEGION-208", claim.RoleImplementer)
	staleSession := "ses_stale"
	staleCurrent := h.registerContract(staleBoot, staleSession, DaemonAPIVersion, true)
	stale := DaemonAPIVersion - 1
	h.makeStale(t, staleToken, staleSession, staleCurrent.Secret, stale)
	staleRecorder := h.request(http.MethodPost, "/legion/v1/grants", body(staleSession, staleCurrent.Secret), nil)
	wantFailure(t, staleRecorder, http.StatusConflict, "STALE_PLUGIN_CONTRACT")

	_, currentBoot := h.launch("LEGION-209", claim.RoleImplementer)
	currentSession := "ses_current"
	currentRegistered := h.registerContract(currentBoot, currentSession, DaemonAPIVersion, true)
	currentBody := fmt.Sprintf(`{"sessionId":%q,"secret":%q,"tree":"LEGION-209","issue":"LEGION-209","push":true}`, currentSession, currentRegistered.Secret)
	currentRecorder := h.request(http.MethodPost, "/legion/v1/grants", currentBody, nil)
	if currentRecorder.Code != http.StatusBadRequest {
		t.Fatalf("a current claim's unknown-field body = %d, want 400; body %s", currentRecorder.Code, currentRecorder.Body)
	}
	if !strings.Contains(currentRecorder.Body.String(), "push") {
		t.Fatalf("400 body %s does not name the unknown field", currentRecorder.Body)
	}

	logged := logs.String()
	if !strings.Contains(logged, `"msg":"api: refused an undecodable request body"`) ||
		!strings.Contains(logged, `"route":"/legion/v1/grants"`) || !strings.Contains(logged, `push`) {
		t.Fatalf("logs %s do not name the route and the decoder's error for the undecodable body", logged)
	}
}

// An undecodable claims/ready body (an unknown field) from a stale claim is refused the
// stale-caller 409, in the claim-route shape (claim.StalePluginContract, {"error": …}) rather than
// the grants route's Failure{Code}; the identical body from a current claim gets today's 400
// naming the field, exactly as the grants family's own undecodable body does
// (claimSecretCheck, the identity check ready and exit share).
func TestUndecodableReadyBodyRefusesAStaleClaimButNamesTheFieldForACurrentOne(t *testing.T) {
	h, _ := newStaleHarness(t)
	withClaim := func(token claim.Token, session, secret string) string {
		return fmt.Sprintf(`{"claimToken":%q,"sessionId":%q,"secret":%q,"generation":1,"push":true}`, token, session, secret)
	}

	staleToken, staleBoot := h.launch("LEGION-208", claim.RoleImplementer)
	staleSession := "ses_stale_ready"
	staleCurrent := h.registerContract(staleBoot, staleSession, DaemonAPIVersion, true)
	stale := DaemonAPIVersion - 1
	h.makeStale(t, staleToken, staleSession, staleCurrent.Secret, stale)
	staleRecorder := h.request(http.MethodPost, "/legion/v1/claims/ready", withClaim(staleToken, staleSession, staleCurrent.Secret), nil)
	wantRefusal(t, staleRecorder, http.StatusConflict, claim.StalePluginContract(stale, DaemonAPIVersion).Message)

	currentToken, currentBoot := h.launch("LEGION-209", claim.RoleImplementer)
	currentSession := "ses_current_ready"
	currentRegistered := h.registerContract(currentBoot, currentSession, DaemonAPIVersion, true)
	currentRecorder := h.request(http.MethodPost, "/legion/v1/claims/ready", withClaim(currentToken, currentSession, currentRegistered.Secret), nil)
	if currentRecorder.Code != http.StatusBadRequest {
		t.Fatalf("a current claim's unknown-field ready body = %d, want 400; body %s", currentRecorder.Code, currentRecorder.Body)
	}
	if !strings.Contains(currentRecorder.Body.String(), "push") {
		t.Fatalf("400 body %s does not name the unknown field", currentRecorder.Body)
	}
}

// An undecodable handoff/complete body (an unknown field), naming a grant minted while the claim
// was current and still valid now that it has gone stale, is refused the stale-caller 409 in the
// grant-bearing shape (Failure{Code: "STALE_PLUGIN_CONTRACT"}); the identical body naming a
// current claim's grant gets today's 400 naming the field — redeemIdentityCheck, the identity
// check every grant-bearing route shares with the grants route's own claim form.
func TestUndecodableHandoffCompleteBodyRefusesAStaleClaimButNamesTheFieldForACurrentOne(t *testing.T) {
	github := newHandoffGitHub(t)
	h, _ := newHandoffHarness(t, github, nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Implementing)
	stale := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	staleGrant := stale.grant(t)
	h.makeStale(t, stale.token, stale.session, stale.secret, DaemonAPIVersion-1)
	staleBody := fmt.Sprintf(`{"grantId":%q,"summary":"implemented","commit":"aabbcc","push":true}`, staleGrant)

	staleRecorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", staleBody, nil)
	wantFailure(t, staleRecorder, http.StatusConflict, "STALE_PLUGIN_CONTRACT")

	seedIssueAt(t, h, "LEGION-209", phase.Implementing)
	current := newLiveClaim(t, h, "LEGION-209", claim.RoleImplementer)
	currentGrant := current.grant(t)
	currentBody := fmt.Sprintf(`{"grantId":%q,"summary":"implemented","commit":"aabbcc","push":true}`, currentGrant)

	currentRecorder := h.request(http.MethodPost, "/legion/v1/handoff/complete", currentBody, nil)
	if currentRecorder.Code != http.StatusBadRequest {
		t.Fatalf("a current claim's unknown-field handoff/complete body = %d, want 400; body %s", currentRecorder.Code, currentRecorder.Body)
	}
	if !strings.Contains(currentRecorder.Body.String(), "push") {
		t.Fatalf("400 body %s does not name the unknown field", currentRecorder.Body)
	}
}

// The register route decodes leniently from the start: its body carries the contract the
// record-and-relaunch path needs, so an unknown field reaches registration rather than a 400.
func TestUndecodableRegisterBodyReachesRegistration(t *testing.T) {
	h := newHarnessUnrelaunchable(t)
	token, boot := h.launch("LEGION-208", claim.RoleArchitect)
	body := fmt.Sprintf(`{"bootToken":%q,"sessionId":"ses_architect","ompSessionFile":"/sessions/ses_architect.jsonl","agentId":"agent-ses_architect","pluginContract":23,"workerSecret":"x"}`, boot)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/register", body, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("register with an unknown field = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if stored := h.stored(token); stored.PluginContract != 23 {
		t.Fatalf("stored pluginContract = %d, want 23 recorded despite the unknown field", stored.PluginContract)
	}
}

// GET /legion/v1/state authenticates nobody, so a stale claim's presence never refuses it.
func TestStateRouteUntouchedForAStaleClaim(t *testing.T) {
	h, _ := newStaleHarness(t)
	_, boot := h.launch("LEGION-208", claim.RoleImplementer)
	h.registerContract(boot, "ses_implementer", DaemonAPIVersion-1, true)

	recorder := h.request(http.MethodGet, "/legion/v1/state", nil, nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

// A daemon that can relaunch nothing (PluginRelaunchable false, LEGION-164's degraded Kubernetes
// mode) refuses no call on account of a claim's stale contract: the same stale seed that gets a
// relaunchable daemon's ready refused (TestStaleClaimIsRefusedOnReadyAndGrantsAndLogsAWarn)
// succeeds here, since nothing relaunches the process and refusing it would stall an agent that
// works today.
func TestUnrelaunchableDaemonRefusesNoCallForAStaleClaim(t *testing.T) {
	h := newHarnessUnrelaunchable(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	session := "ses_implementer"
	current := h.registerContract(boot, session, DaemonAPIVersion, false)
	h.makeStale(t, token, session, current.Secret, DaemonAPIVersion-1)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: current.Secret, Generation: current.Generation,
	}, nil)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("ready from a stale claim on an unrelaunchable daemon = %d, want 204; body %s", recorder.Code, recorder.Body)
	}
}

// The operator's controller becomes stale only across a daemon restart (it cannot itself start on
// the wrong contract): a grant after the restart is refused 409 naming `legion controller start`,
// since the daemon is not the operator's controller's to relaunch.
func TestOperatorControllersStaleGrantIsRefused(t *testing.T) {
	h := newControllerHarness(t, false)
	capability := h.mintedSecret()
	registration := h.registeredController(capability, "ses_operator")
	stale := DaemonAPIVersion - 1
	controllerRecord, found, err := h.store.Controller(context.Background(), testProject)
	if err != nil || !found {
		t.Fatalf("read the controller record: found=%v err=%v", found, err)
	}
	if ok, err := h.store.RegisterController(context.Background(), testProject, controllerRecord.Generation,
		"ses_operator", secretHash(registration.Secret), time.Now().UTC(), stale); err != nil || !ok {
		t.Fatalf("seed the controller's stale contract: ok=%v err=%v", ok, err)
	}

	recorder := h.controllerSessionGrant("ses_operator", registration.Secret)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale controller grant status = %d, want 409; body %s", recorder.Code, recorder.Body)
	}
	var failure Failure
	decodeInto(t, recorder, &failure)
	if failure.Code != "STALE_PLUGIN_CONTRACT" {
		t.Fatalf("stale controller grant code = %q, want STALE_PLUGIN_CONTRACT", failure.Code)
	}
	if want := claim.StaleControllerContract(stale, DaemonAPIVersion).Message; failure.Error != want {
		t.Fatalf("controller grant refusal sentence = %q, want %q", failure.Error, want)
	}
}

// An operator-bearer grant (the Controller: true grant `legion status` redeems on
// POST /legion/v1/issues/status with --operator-token-file) is returned by redeem without
// consulting the controller record at all (LEGION-164): a mint for it checks nothing about the
// record either (mintControllerGrant, the grant route's operator form), so its own staleness is
// never the record's to answer for — only the controller session's own grant mint is
// (controllerSessionGrant). The record's stale contract that refuses the session's own mint
// (TestOperatorControllersStaleGrantIsRefused) leaves the operator-bearer grant's redeem
// untouched.
func TestOperatorBearerGrantRedeemsDespiteTheControllerRecordsStaleContract(t *testing.T) {
	h := newControllerHarness(t, false)
	capability := h.mintedSecret()
	registration := h.registeredController(capability, "ses_operator")
	stale := DaemonAPIVersion - 1
	controllerRecord, found, err := h.store.Controller(context.Background(), testProject)
	if err != nil || !found {
		t.Fatalf("read the controller record: found=%v err=%v", found, err)
	}
	if ok, err := h.store.RegisterController(context.Background(), testProject, controllerRecord.Generation,
		"ses_operator", secretHash(registration.Secret), time.Now().UTC(), stale); err != nil || !ok {
		t.Fatalf("seed the controller's stale contract: ok=%v err=%v", ok, err)
	}

	grant := controllerGrant(t, h)

	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{
		GrantID: grant.GrantID, Issue: "LEGION-208", Status: "todo",
	}, nil)
	if recorder.Code == http.StatusConflict {
		t.Fatalf("operator-bearer grant redeem = 409, want no stale refusal; body %s", recorder.Body)
	}

	sessionRecorder := h.controllerSessionGrant("ses_operator", registration.Secret)
	if sessionRecorder.Code != http.StatusConflict {
		t.Fatalf("controller session's own grant mint = %d, want 409; body %s", sessionRecorder.Code, sessionRecorder.Body)
	}
	var sessionFailure Failure
	decodeInto(t, sessionRecorder, &sessionFailure)
	if want := claim.StaleControllerContract(stale, DaemonAPIVersion).Message; sessionFailure.Error != want {
		t.Fatalf("controller session grant refusal sentence = %q, want %q", sessionFailure.Error, want)
	}
}

// Wrong secret is still refused 403, exactly as today, regardless of the claim's recorded
// contract: the stale-caller refusal only ever replaces a 400 or runs after authentication, never
// before it.
func TestWrongSecretStillRefusedBeforeAnyStaleCheck(t *testing.T) {
	h, _ := newStaleHarness(t)
	token, boot := h.launch("LEGION-208", claim.RoleImplementer)
	h.registerContract(boot, "ses_implementer", DaemonAPIVersion-1, true)

	recorder := h.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_implementer", Secret: "the wrong secret", Generation: 1,
	}, nil)

	wantRefusal(t, recorder, http.StatusForbidden, "Invalid session secret")
}
