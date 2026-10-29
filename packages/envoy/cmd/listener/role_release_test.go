// What a role lookup and the explicit release each do to a role's claim: the lookup reads and
// changes nothing, the release ends a lapsed claim and records the caller that asked for it, and
// a live holder's claim survives both.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// seedLapsedRoleClaim gives role to holder and then takes holder out of the session bucket: the
// durable claim with no registered holder. That is the same state whether the holder died or its
// machine was too loaded for it to heartbeat inside the bucket's TTL while its process worked,
// which is why nothing may be taken from it by a read. It answers the holder's last heartbeat.
func seedLapsedRoleClaim(t *testing.T, registry *store.Registry, sessions *session.SessionRegistry, role, holder string) int64 {
	t.Helper()
	if err := sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", holder, err)
	}
	entry, err := sessions.Get(holder)
	if err != nil {
		t.Fatalf("read %s's heartbeat: %v", holder, err)
	}
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("give %s to %s: %v", role, holder, err)
	}
	if err := sessions.Delete(context.Background(), holder); err != nil {
		t.Fatalf("lapse %s's registration: %v", holder, err)
	}
	return entry.UpdatedAt
}

func releaseRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/roles/release", strings.NewReader(body))
}

// lookupRole serves GET /v1/roles/<role> and answers the recorder.
func lookupRole(d *listenerDeps, role string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	roleGetHandler(d).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	return recorder
}

// A lookup of a role whose holder has left the session bucket reports the lapse and leaves the
// claim where it is, so a tool that only looks a role up cannot end that holder's claim: the
// holder may be a live process that missed one heartbeat on a loaded machine, and no peer and no
// record would say who ended it. Two lookups in a row answer the same thing, which is the whole
// property: before this, the first answered `claim_released: true` and the second `unclaimed`.
func TestARoleLookupLeavesALapsedClaimWhereItIs(t *testing.T) {
	const (
		role   = "bet64-eta-arity"
		holder = "ses_busy_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	lastSeen := seedLapsedRoleClaim(t, registry, sessions, role, holder)
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	for _, attempt := range []string{"first", "second"} {
		recorder := lookupRole(state, role)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("the %s lookup: status %d, body %s; want 404", attempt, recorder.Code, recorder.Body.String())
		}
		var answer roleHolderError
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the %s lookup's answer %s: %v", attempt, recorder.Body.String(), err)
		}
		if answer.Reason != roleHolderLapsed || answer.Holder != holder {
			t.Fatalf("the %s lookup answered %+v, want %s's lapse", attempt, answer, holder)
		}
		if answer.LastSeen == nil || *answer.LastSeen != lastSeen {
			t.Fatalf("the %s lookup's last_seen = %v, want %d", attempt, answer.LastSeen, lastSeen)
		}
		if answer.ClaimReleased == nil || *answer.ClaimReleased {
			t.Fatalf("the %s lookup answered claim_released %v: a lookup releases nothing", attempt, answer.ClaimReleased)
		}
		if got, err := registry.RoleHolder(role); err != nil || got != holder {
			t.Fatalf("the claim after the %s lookup = %q, %v; want %s's, untouched", attempt, got, err, holder)
		}
		t.Logf("%s lookup: %s", attempt, strings.TrimSpace(recorder.Body.String()))
	}
}

// Releasing is a call of its own, and it names its caller: the answer and the listener's one
// record line both carry released_by, so a claim that ends can be traced to whoever ended it
// rather than inferred from what happened next.
func TestAnExplicitReleaseEndsALapsedClaimAndRecordsItsCaller(t *testing.T) {
	const (
		role   = "bet64-eta-arity"
		holder = "ses_busy_holder"
		caller = "envoy-role-holder-watch"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	lastSeen := seedLapsedRoleClaim(t, registry, sessions, role, holder)
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	recorder, record := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return roleReleaseHandler(state, logger) },
		releaseRequest(`{"role":"`+role+`","holder":"`+holder+`","released_by":"`+caller+`"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("release: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	var answer roleReleaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the release's answer %s: %v", recorder.Body.String(), err)
	}
	if !answer.Released || answer.Role != role || answer.Holder != holder || answer.Reason != roleHolderLapsed {
		t.Fatalf("the release answered %+v, want %s's lapsed claim released", answer, holder)
	}
	if answer.ReleasedBy != caller || !strings.Contains(answer.Detail, caller) {
		t.Fatalf("the release answered %+v, want the caller %s named", answer, caller)
	}
	if answer.LastSeen == nil || *answer.LastSeen != lastSeen {
		t.Fatalf("the release's last_seen = %v, want %d", answer.LastSeen, lastSeen)
	}
	if got, err := registry.RoleHolder(role); err != nil || got != "" {
		t.Fatalf("the claim after the release = %q, %v; want it released", got, err)
	}
	assertRecordFields(t, record, map[string]any{
		"msg":         "listener role release",
		"level":       "INFO",
		"role":        role,
		"holder":      holder,
		"released_by": caller,
		"released":    true,
	})
	t.Logf("release: %s", strings.TrimSpace(recorder.Body.String()))

	// The role reaper, or another caller, may have taken the claim first; a second release is
	// then an answer, not a failure, so a caller needs no special case for the race it lost.
	repeat := httptest.NewRecorder()
	roleReleaseHandler(state, logging.New("test")).ServeHTTP(repeat,
		releaseRequest(`{"role":"`+role+`","released_by":"`+caller+`"}`))
	if repeat.Code != http.StatusOK {
		t.Fatalf("release of an unclaimed role: status %d, body %s; want 200", repeat.Code, repeat.Body.String())
	}
	var again roleReleaseResponse
	if err := json.Unmarshal(repeat.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode the second release's answer %s: %v", repeat.Body.String(), err)
	}
	if again.Released || again.Reason != roleHolderUnclaimed {
		t.Fatalf("the second release answered %+v, want an unclaimed role and nothing released", again)
	}
}

// Neither route may touch a live holder's claim: the lookup answers it, and the release refuses
// with the holder's id, as a soft claim of a held role does.
func TestALiveHoldersClaimSurvivesBothTheLookupAndARelease(t *testing.T) {
	const (
		role   = "bet64-eta-arity"
		holder = "ses_live_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", holder, err)
	}
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("give %s to %s: %v", role, holder, err)
	}
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	lookup := lookupRole(state, role)
	if lookup.Code != http.StatusOK || !strings.Contains(lookup.Body.String(), `"holder":"`+holder+`"`) {
		t.Fatalf("lookup of a live holder's role: status %d, body %s; want 200 naming %s", lookup.Code, lookup.Body.String(), holder)
	}
	if got, err := registry.RoleHolder(role); err != nil || got != holder {
		t.Fatalf("the claim after the lookup = %q, %v; want %s's", got, err, holder)
	}

	recorder, record := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return roleReleaseHandler(state, logger) },
		releaseRequest(`{"role":"`+role+`","released_by":"a caller that should be refused"}`))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("release of a live holder's role: status %d, body %s; want 409", recorder.Code, recorder.Body.String())
	}
	var refusal struct {
		Error  string `json:"error"`
		Role   string `json:"role"`
		Holder string `json:"holder"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if refusal.Holder != holder || refusal.Role != role {
		t.Fatalf("the refusal reads %+v, want %s named as %s's holder", refusal, holder, role)
	}
	assertRecordFields(t, record, map[string]any{
		"msg":     "listener role release refused",
		"level":   "WARN",
		"outcome": "live_holder",
		"holder":  holder,
	})
	if got, err := registry.RoleHolder(role); err != nil || got != holder {
		t.Fatalf("the claim after the refused release = %q, %v; want %s's, untouched", got, err, holder)
	}
	t.Logf("refused release: %s", strings.TrimSpace(recorder.Body.String()))
}

// A release written for one incident names the holder it was written for, so a claim that has
// moved to another session since is left alone rather than ended by a stale remedy.
func TestAReleaseThatNamesAnotherHolderChangesNothing(t *testing.T) {
	const (
		role     = "bet64-eta-arity"
		holder   = "ses_current_holder"
		departed = "ses_the_incidents_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	seedLapsedRoleClaim(t, registry, sessions, role, holder)
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	recorder := httptest.NewRecorder()
	roleReleaseHandler(state, logging.New("test")).ServeHTTP(recorder,
		releaseRequest(`{"role":"`+role+`","holder":"`+departed+`","released_by":"a stale remedy"}`))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("release naming another holder: status %d, body %s; want 409", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), holder) {
		t.Fatalf("the refusal reads %s, want %s named as the current holder", recorder.Body.String(), holder)
	}
	if got, err := registry.RoleHolder(role); err != nil || got != holder {
		t.Fatalf("the claim after the refused release = %q, %v; want %s's, untouched", got, err, holder)
	}
}

// Every release is recorded against a caller, so the route refuses one that names none rather
// than writing a line nobody can be traced through.
func TestAReleaseWithoutACallerIsRefused(t *testing.T) {
	const role = "bet64-eta-arity"
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	seedLapsedRoleClaim(t, registry, sessions, role, "ses_busy_holder")
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	for _, body := range []string{
		`{"role":"` + role + `"}`,
		`{"role":"` + role + `","released_by":"   "}`,
		`{"released_by":"a caller"}`,
	} {
		recorder := httptest.NewRecorder()
		roleReleaseHandler(state, logging.New("test")).ServeHTTP(recorder, releaseRequest(body))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("release %s: status %d, body %s; want 400", body, recorder.Code, recorder.Body.String())
		}
	}
	if got, err := registry.RoleHolder(role); err != nil || got != "ses_busy_holder" {
		t.Fatalf("the claim after the refused releases = %q, %v; want it untouched", got, err)
	}
}
