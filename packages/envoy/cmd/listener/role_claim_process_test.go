package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
)

// The two processes one session id can be claimed under: the one that registered the session and
// holds the role, and a second that resumed that id - another Oh My Pi Envoy extension instance,
// another Claude Code channel-server process - and so sends the same session_id with a client
// instance of its own.
const (
	holdingProcess = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	secondProcess  = "9b8a7c6d5e4f3a2b9b8a7c6d5e4f3a2b"
)

// roleClaimProcessRig is one listener's role routes over one listener's stores: POST
// /v1/roles/set with the live window main gives it (withRoleClaimDrain reads it from the rig's
// own session registry, as main reads it from its own), the route's log, and the lookup every
// refused claimant reads to see which process holds the role.
type roleClaimProcessRig struct {
	deps *listenerDeps
	logs *bytes.Buffer
	set  http.Handler
}

// newRoleClaimProcessRig registers sessionID as the process that registered it (holdingProcess)
// and returns the routes over it. The registration is written once and never again by these
// tests: what each claim states is the process making it, and the record is the thing the claim
// rule stops trusting.
func newRoleClaimProcessRig(t *testing.T, sessionID string) *roleClaimProcessRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	deps := withRoleClaimDrain(t, &listenerDeps{client: setupPublishTestClient(t), registry: registry, sessions: sessions})
	registerInstance(t, sessions, sessionID, holdingProcess)
	logs := &bytes.Buffer{}
	return &roleClaimProcessRig{
		deps: deps,
		logs: logs,
		set:  roleSetHandler(deps, "test-machine", logging.NewWithWriter("test", logs)),
	}
}

// claim is a soft claim of role under sessionID by the process instance names, as a client makes
// it. An empty instance is a request that states none: a client predating the field, whose claim
// is weighed by the standing claim's renewal alone.
func (r *roleClaimProcessRig) claim(sessionID, role, instance string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, sessionID, role)
	if instance != "" {
		body = fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"client_instance_id":%q}`, sessionID, role, instance)
	}
	recorder := httptest.NewRecorder()
	r.set.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
	return recorder
}

// seed is the claim that puts role in the hands of instance, which every test here starts from.
func (r *roleClaimProcessRig) seed(t *testing.T, sessionID, role, instance string) {
	t.Helper()
	if recorder := r.claim(sessionID, role, instance); recorder.Code != http.StatusOK {
		t.Fatalf("seed claim of %s by %q: status %d %s, want 200", role, instance, recorder.Code, recorder.Body.String())
	}
}

// lookup is GET /v1/roles/<role>, the answer a refused claimant reads.
func (r *roleClaimProcessRig) lookup(role string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	roleGetHandler(r.deps).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	return recorder
}

// holdingProcessOf is the process the stored claim names, read from the bucket itself.
func (r *roleClaimProcessRig) holdingProcessOf(t *testing.T, role string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := r.deps.registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim.ClientInstanceID
}

// A claim from the very process the standing claim names is granted at once, however recently
// that claim was renewed: it is that process's own role, and a claim is how it says so again.
func TestARoleClaimUnderOneSessionIdIsGrantedToTheProcessTheClaimNames(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, holdingProcess)

	recorder := rig.claim(sessionID, role, holdingProcess)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the holding process's own claim: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := rig.holdingProcessOf(t, role); got != holdingProcess {
		t.Fatalf("the stored claim names the process %q, want %q", got, holdingProcess)
	}
}

// The incident this guards: a second process resumes a live session's id and claims the role its
// first process holds. The claim's own renewal is inside the live window, so the role stays where
// it is and the claimant reads the 409 that names the holder - and the route logs the refusal
// with both processes, so an operator sees the second one being held off.
func TestASecondProcessOfOneSessionIdIsRefusedTheRoleWhileTheHoldersClaimIsRenewed(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, holdingProcess)

	recorder := rig.claim(sessionID, role, secondProcess)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"holder":"`+sessionID+`"`) {
		t.Fatalf("a second process's claim of a renewed role: status %d %s, want 409 naming %s", recorder.Code, recorder.Body.String(), sessionID)
	}
	if got := rig.holdingProcessOf(t, role); got != holdingProcess {
		t.Fatalf("the stored claim names the process %q after the refusal, want %q: a refused claim writes nothing", got, holdingProcess)
	}
	logged := rig.logs.String()
	for _, want := range []string{`"role":"` + role + `"`, `"session_id":"` + sessionID + `"`, `"holder_client_instance_id":"` + holdingProcess + `"`, `"client_instance_id":"` + secondProcess + `"`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("refusal log = %q, want a line carrying %s", logged, want)
		}
	}
}

// Once nobody has answered for the claim inside the live window, the second process takes the
// role: the holder it names has stopped renewing it, and a claim stale by that much is the one
// thing that frees a role still claimed under the id the claimant is registering as its own.
func TestASecondProcessOfOneSessionIdTakesTheRoleOnceTheClaimsRenewalIsOlderThanTheWindow(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, holdingProcess)
	ageRoleClaim(t, rig.deps, role, 2*rig.deps.sessionLiveWindow)

	recorder := rig.claim(sessionID, role, secondProcess)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a second process's claim of a role nobody renews: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := rig.holdingProcessOf(t, role); got != secondProcess {
		t.Fatalf("the stored claim names the process %q, want the claimant's %q", got, secondProcess)
	}
}

// A claim record that names no process - one an earlier build wrote, or one whose claimant stated
// none - is no match for any claimant, so it is weighed by its renewal alone. It carries none
// either, so the moment it was claimed is what the window is measured from
// (store.RoleClaim.LastAnsweredAt): the role is kept while that moment is inside the window and
// granted once it is older, which is how a claim standing from before this build is taken over.
func TestARoleClaimRecordThatNamesNoProcessIsWeighedByTheMomentItWasClaimed(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, "")
	if got := rig.holdingProcessOf(t, role); got != "" {
		t.Fatalf("the seeded claim names the process %q, want none: its claimant stated none", got)
	}

	refused := rig.claim(sessionID, role, secondProcess)
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"holder":"`+sessionID+`"`) {
		t.Fatalf("a claim over a record claimed inside the window: status %d %s, want 409 naming %s", refused.Code, refused.Body.String(), sessionID)
	}

	ageRoleClaim(t, rig.deps, role, 2*rig.deps.sessionLiveWindow)
	granted := rig.claim(sessionID, role, secondProcess)
	if granted.Code != http.StatusOK {
		t.Fatalf("a claim over a record claimed a window ago: status %d %s, want 200", granted.Code, granted.Body.String())
	}
	if got := rig.holdingProcessOf(t, role); got != secondProcess {
		t.Fatalf("the stored claim names the process %q, want the claimant's %q", got, secondProcess)
	}
}

// A request that states no process is the same rule from the other side: it matches the claim's
// process no more than an absent claim matches a stated one, so it waits out the claim's renewal
// and takes the role once nobody answers for it. That is a client predating the field claiming a
// role held under its own session id.
func TestARoleClaimThatStatesNoProcessIsWeighedByTheStandingClaimsRenewal(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, holdingProcess)

	refused := rig.claim(sessionID, role, "")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"holder":"`+sessionID+`"`) {
		t.Fatalf("a claim that states no process against a renewed claim: status %d %s, want 409 naming %s", refused.Code, refused.Body.String(), sessionID)
	}
	if got := rig.holdingProcessOf(t, role); got != holdingProcess {
		t.Fatalf("the stored claim names the process %q after the refusal, want %q", got, holdingProcess)
	}

	ageRoleClaim(t, rig.deps, role, 2*rig.deps.sessionLiveWindow)
	granted := rig.claim(sessionID, role, "")
	if granted.Code != http.StatusOK {
		t.Fatalf("a claim that states no process against a claim nobody renews: status %d %s, want 200", granted.Code, granted.Body.String())
	}
	if got := rig.holdingProcessOf(t, role); got != "" {
		t.Fatalf("the stored claim names the process %q, want none: its claimant stated none", got)
	}
}

// The lookup answers the process the claim names beside the holder's session id, so a client
// refused the role can see that the holder is another process of its own session id - the 409 of
// POST /v1/roles/set names the session alone. A claim that names no process answers an empty
// instance rather than leaving the field out.
func TestTheRoleLookupAnswersTheProcessTheHoldersClaimNames(t *testing.T) {
	const sessionID = "ses_shared_id"
	rig := newRoleClaimProcessRig(t, sessionID)
	for name, tc := range map[string]struct{ role, instance string }{
		"a claim that names its process": {role: "legion-controller", instance: holdingProcess},
		"a claim that names none":        {role: "legion-delivery", instance: ""},
	} {
		t.Run(name, func(t *testing.T) {
			rig.seed(t, sessionID, tc.role, tc.instance)
			recorder := rig.lookup(tc.role)
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET /v1/roles/%s: status %d %s, want 200", tc.role, recorder.Code, recorder.Body.String())
			}
			var answer struct {
				Holder           string `json:"holder"`
				ClientInstanceID string `json:"client_instance_id"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
				t.Fatalf("decode %s: %v", recorder.Body.String(), err)
			}
			if answer.Holder != sessionID || answer.ClientInstanceID != tc.instance {
				t.Fatalf("the lookup answers holder %q of process %q, want %q of %q", answer.Holder, answer.ClientInstanceID, sessionID, tc.instance)
			}
		})
	}
}

// The 404 of a lapsed holder names its process too, on the same field: the claim stands until the
// role reaper takes it, so a client reading this answer still needs to know which process holds
// it. The holder here has left the session bucket, which is what makes the lookup a 404.
func TestTheRoleLookupAnswersALapsedHoldersProcess(t *testing.T) {
	const (
		sessionID = "ses_shared_id"
		role      = "legion-controller"
	)
	rig := newRoleClaimProcessRig(t, sessionID)
	rig.seed(t, sessionID, role, holdingProcess)
	if err := rig.deps.sessions.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("end %s's session: %v", sessionID, err)
	}

	recorder := rig.lookup(role)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/roles/%s once its holder's record has gone: status %d %s, want 404", role, recorder.Code, recorder.Body.String())
	}
	var answer roleHolderError
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if answer.Holder != sessionID || answer.ClientInstanceID != holdingProcess {
		t.Fatalf("the lapsed answer names holder %q of process %q, want %q of %q", answer.Holder, answer.ClientInstanceID, sessionID, holdingProcess)
	}
}
