package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// roleClaimRelaunchRig is one listener's whole set of v1 routes over one listener's stores, with
// the live window main gives them (withRoleClaimDrain reads it from the rig's own session
// registry, as main reads it from its own). A relaunch is three routes in sequence - the
// registration, the daemon's clean stop of the dead process's session, and the claim - so the
// rig is the mux rather than one handler.
type roleClaimRelaunchRig struct {
	t       *testing.T
	deps    *listenerDeps
	routes  *http.ServeMux
	session string
	role    string
	topic   string
}

// newRoleClaimRelaunchRig builds the routes over an empty pair of buckets. Nothing is registered
// and no role is claimed: each stage below goes through the route a client or the daemon uses,
// because what this case is about is what those routes leave in the bucket.
func newRoleClaimRelaunchRig(t *testing.T, sessionID, role string) *roleClaimRelaunchRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	deps := withRoleClaimDrain(t, &listenerDeps{client: setupPublishTestClient(t), registry: registry, sessions: sessions})
	routes := http.NewServeMux()
	registerV1Routes(routes, deps, "test-machine", logging.New("test"))
	return &roleClaimRelaunchRig{
		t:       t,
		deps:    deps,
		routes:  routes,
		session: sessionID,
		role:    role,
		topic:   "notifications.team.relaunch",
	}
}

// register is the heartbeat POST /v1/interests/subscribe a running client repeats, stating the
// process making it: it writes the session's registration - self_subscribed, as a client that
// receives on its own subject sends - and renews the claims that process holds
// (store.Registry.RenewRoleClaims).
func (r *roleClaimRelaunchRig) register(instance string) {
	r.t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"topics":[%q],"self_subscribed":true,"client_instance_id":%q}`, r.session, r.topic, instance)
	recorder := r.send(http.MethodPost, "/v1/interests/subscribe", body)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("registration of %s by process %q: status %d %s, want 200", r.session, instance, recorder.Code, recorder.Body.String())
	}
}

// claim is the soft claim the lane makes when the lookup does not name this session and this
// process (packages/pi-envoy/src/registration-state.ts, reassertRole).
func (r *roleClaimRelaunchRig) claim(instance string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"client_instance_id":%q}`, r.session, r.role, instance)
	return r.send(http.MethodPost, "/v1/roles/set", body)
}

// cleanStop is the daemon's clean stop of a dead role's session: DELETE
// /v1/sessions/<session_id>?if_not_registered_after=<the moment the probe judged the process
// gone> (packages/daemon/internal/notify.HTTPPublisher.DeleteSession).
func (r *roleClaimRelaunchRig) cleanStop(goneAt time.Time) *httptest.ResponseRecorder {
	target := fmt.Sprintf("/v1/sessions/%s?if_not_registered_after=%d", r.session, goneAt.UnixMilli())
	return r.send(http.MethodDelete, target, "")
}

// unsubscribe is POST /v1/interests/unsubscribe with no topics at all: the client's own clean
// stop, which hands back every interest the session has and the roles its process holds.
func (r *roleClaimRelaunchRig) unsubscribe(instance string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"session_id":%q,"topics":[],"client_instance_id":%q}`, r.session, instance)
	return r.send(http.MethodPost, "/v1/interests/unsubscribe", body)
}

func (r *roleClaimRelaunchRig) send(method, target, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	r.routes.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
	return recorder
}

// stored is the role's claim as the bucket holds it now.
func (r *roleClaimRelaunchRig) stored() store.RoleClaim {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := r.deps.registry.RoleClaim(ctx, r.role)
	if err != nil {
		r.t.Fatalf("read %s's claim: %v", r.role, err)
	}
	return claim
}

// A Legion role whose agent process a probe judges gone is stopped cleanly and relaunched under
// the same session id in a new process: the daemon deletes the dead process's registration
// (DELETE /v1/sessions/<session_id>?if_not_registered_after=…, dispatch://INFRA-392) and the
// relaunched process registers that id again with a client instance of its own. The claim the
// dead process left still names the dead process, and a release frees only the claims naming the
// process that asks (store.RoleReleaseAuthority), so the whole path from the clean stop to the
// relaunched holder's own stop is held here:
//
//   - the relaunch's registration renews nothing, because a registration renews only the claims
//     its own process holds (store.Registry.renewRoleClaim); the dead process's claim is left
//     exactly as it stands, renewal included;
//   - while that renewal is inside the live window the lane's re-claim is refused the role, which
//     is the refusal that keeps a second process that resumed a live lane's session id from
//     taking the role under the id it shares;
//   - once nobody has answered for the claim inside that window - which is what the dead process
//     not answering means - the next re-claim is granted and the stored claim names the
//     relaunched process. That is the step that moves the claim's instance, and it is the lane's
//     own heartbeat that makes it (packages/pi-envoy/src/registration-state.ts, reassertRole);
//   - and the relaunched holder's own clean stop then frees the role at once, rather than leaving
//     it to the role claim reaper, because by then the claim names the process handing it back.
func TestARelaunchedRoleHolderReclaimsItsRoleAndItsOwnStopFreesIt(t *testing.T) {
	const (
		sessionID = "ses_relaunched_holder"
		role      = "legion-controller"
	)
	rig := newRoleClaimRelaunchRig(t, sessionID, role)

	// The process that holds the role registers and claims it.
	rig.register(holdingProcess)
	if recorder := rig.claim(holdingProcess); recorder.Code != http.StatusOK {
		t.Fatalf("the first process's claim of %s: status %d %s, want 200", role, recorder.Code, recorder.Body.String())
	}
	claimed := rig.stored()
	if claimed.ClientInstanceID != holdingProcess {
		t.Fatalf("the stored claim names the process %q, want %q", claimed.ClientInstanceID, holdingProcess)
	}

	// The daemon's clean stop of the dead process's session. The moment it belongs to is after
	// the registration it is removing, as a stop that lands before the relaunch registers is.
	recorder := rig.cleanStop(time.Now().Add(time.Minute))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"deleted":true`) {
		t.Fatalf("the clean stop of %s: status %d %s, want 200 reporting the registration deleted", sessionID, recorder.Code, recorder.Body.String())
	}
	if after := rig.stored(); after != claimed {
		t.Fatalf("the claim after the clean stop = %+v, want %+v: the stop removes the registration and touches no claim", after, claimed)
	}

	// The relaunch registers the same session id as a process of its own. It renews nothing:
	// the claim names the process that died.
	rig.register(secondProcess)
	if after := rig.stored(); after != claimed {
		t.Fatalf("the claim after the relaunch's registration = %+v, want %+v: a registration renews only the claims its own process holds", after, claimed)
	}

	// And until that claim's renewal is older than the live window, the relaunched process is
	// refused its own role, exactly as a second process that resumed a live lane's id is.
	if refused := rig.claim(secondProcess); refused.Code != http.StatusConflict {
		t.Fatalf("the relaunch's re-claim while the dead process's claim is still renewed: status %d %s, want 409", refused.Code, refused.Body.String())
	}
	if after := rig.stored(); after != claimed {
		t.Fatalf("the claim after the refused re-claim = %+v, want %+v: a refused claim writes nothing", after, claimed)
	}

	// The dead process answers for nothing, so its claim's renewal ages out of the window, and
	// the next heartbeat's re-claim takes the role and moves the claim to this process.
	ageRoleClaim(t, rig.deps, role, 2*roleClaimLiveWindow(rig.deps))
	if granted := rig.claim(secondProcess); granted.Code != http.StatusOK {
		t.Fatalf("the relaunch's re-claim once nobody answers for the claim: status %d %s, want 200", granted.Code, granted.Body.String())
	}
	reclaimed := rig.stored()
	if reclaimed.HolderSessionID != sessionID || reclaimed.ClientInstanceID != secondProcess {
		t.Fatalf("the re-claimed claim is held by %q of process %q, want %q of %q", reclaimed.HolderSessionID, reclaimed.ClientInstanceID, sessionID, secondProcess)
	}

	// The relaunched holder's own clean stop, through the route every client hands a role back
	// on: the role is free the moment it answers.
	if stopped := rig.unsubscribe(secondProcess); stopped.Code != http.StatusOK {
		t.Fatalf("the relaunched holder's unsubscribe: status %d %s, want 200", stopped.Code, stopped.Body.String())
	}
	holder, err := rig.deps.registry.RoleHolder(role)
	if err != nil {
		t.Fatalf("read %s's holder after the relaunched holder's stop: %v", role, err)
	}
	if holder != "" {
		t.Fatalf("%s is still held by %q after its holder's own clean stop, want it freed at the deregistration", role, holder)
	}
}
