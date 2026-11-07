package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// releaseKeptTopic is a topic of the holder's own, neither a role lane nor named by any release
// below: a record that survives a release must still carry it, which is how these cases read
// that the interest half did exactly what it always did.
const releaseKeptTopic = "notifications.team.release"

// roleClaimReleaseRig is one listener's interest routes over one listener's stores: POST
// /v1/interests/unsubscribe, the release a client makes for its own process, and DELETE
// /v1/interests/<session_id>, the administrator's forced one. The session it is built for holds
// role under holdingProcess and carries a topic of its own beside the role's lane topic, so each
// release below is read on the claim and on the record at once.
type roleClaimReleaseRig struct {
	registry *store.Registry
	routes   *http.ServeMux
	session  string
	role     string
}

// newRoleClaimReleaseRig registers sessionID as holdingProcess's, gives it role through that
// same process - the claim a client's own POST /v1/roles/set writes - and returns the routes
// over it.
func newRoleClaimReleaseRig(t *testing.T, sessionID, role, instance string) *roleClaimReleaseRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	registerInstance(t, sessions, sessionID, holdingProcess)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "test-machine"}, []string{releaseKeptTopic}); err != nil {
		t.Fatalf("register %s's interests: %v", sessionID, err)
	}
	claim := store.RoleClaimRequest{SessionID: sessionID, MachineID: "test-machine", ClientInstanceID: instance, Role: role}
	if _, err := registry.SetRoleWithPrevious(ctx, claim); err != nil {
		t.Fatalf("claim %s for %s as %q: %v", role, sessionID, instance, err)
	}
	rig := &roleClaimReleaseRig{
		registry: registry,
		routes:   http.NewServeMux(),
		session:  sessionID,
		role:     role,
	}
	registerV1Routes(rig.routes, &listenerDeps{client: setupPublishTestClient(t), registry: registry, sessions: sessions}, "test-machine", logging.New("test"))
	rig.assertHolder(t, sessionID)
	rig.assertTopics(t, releaseKeptTopic, contracts.RoleTopicPrefix+role)
	return rig
}

// unsubscribe is POST /v1/interests/unsubscribe for topics, by the process instance names. An
// empty instance is a request that states none, which is what a client predating the field
// sends; the topics are written as given, so an empty list is the shape that removes every
// interest the session has.
func (r *roleClaimReleaseRig) unsubscribe(instance string, topics ...string) *httptest.ResponseRecorder {
	quoted := make([]string, 0, len(topics))
	for _, topic := range topics {
		quoted = append(quoted, fmt.Sprintf("%q", topic))
	}
	body := fmt.Sprintf(`{"session_id":%q,"topics":[%s],"client_instance_id":%q}`, r.session, strings.Join(quoted, ","), instance)
	recorder := httptest.NewRecorder()
	r.routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/unsubscribe", strings.NewReader(body)))
	return recorder
}

// remove is DELETE /v1/interests/<session_id>, the administrator's removal of every interest the
// session has.
func (r *roleClaimReleaseRig) remove() *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	r.routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/interests/"+r.session, nil))
	return recorder
}

// assertHolder fails unless the role's stored claim names want as its holder; an empty want is a
// role no claim holds.
func (r *roleClaimReleaseRig) assertHolder(t *testing.T, want string) {
	t.Helper()
	holder, err := r.registry.RoleHolder(r.role)
	if err != nil {
		t.Fatalf("read %s's holder: %v", r.role, err)
	}
	if holder != want {
		t.Fatalf("%s's holder = %q, want %q", r.role, holder, want)
	}
}

// assertTopics fails unless the session's interest record carries exactly want.
func (r *roleClaimReleaseRig) assertTopics(t *testing.T, want ...string) {
	t.Helper()
	item, err := r.registry.Get(context.Background(), r.session)
	if err != nil {
		t.Fatalf("read %s's interest record: %v", r.session, err)
	}
	got := slices.Sorted(slices.Values(item.Topics))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s's topics = %v, want %v", r.session, got, want)
	}
}

// assertNoRecord fails unless the session's interest record is gone from the bucket.
func (r *roleClaimReleaseRig) assertNoRecord(t *testing.T) {
	t.Helper()
	if _, err := r.registry.Get(context.Background(), r.session); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("read %s's interest record after its removal: %v, want it gone", r.session, err)
	}
}

// An unsubscribe naming the process the claim names frees the role, and removes the topics it
// named from the record as it always has. The instance is trimmed on the way in, as session_id
// is, so a client that pads the field still hands its own role back.
func TestAnUnsubscribeNamingTheClaimsProcessFreesTheRoleAndRemovesItsTopics(t *testing.T) {
	const (
		sessionID = "ses_release_own_process"
		role      = "legion-controller"
	)
	rig := newRoleClaimReleaseRig(t, sessionID, role, holdingProcess)

	recorder := rig.unsubscribe("  "+holdingProcess+"  ", contracts.RoleTopicPrefix+role)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	rig.assertHolder(t, "")
	rig.assertTopics(t, releaseKeptTopic)
}

// The incident this guards, on the release side: a second process that resumed the holder's
// session id hands back a role it does not hold, and a client that cannot say which process it
// is does the same. Neither frees the live holder's claim, and both are answered 200 with their
// topics removed, exactly as before - the request is a session's unsubscribe, and only the claim
// is scoped to the process that asked.
func TestAnUnsubscribeThatDoesNotNameTheClaimsProcessFreesNoRole(t *testing.T) {
	const role = "legion-controller"
	for name, instance := range map[string]string{
		"a second process of that session": secondProcess,
		"a request that states no process": "",
		"an instance of nothing but space": "   ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("the role's own topic", func(t *testing.T) {
				rig := newRoleClaimReleaseRig(t, "ses_release_other_topic", role, holdingProcess)

				recorder := rig.unsubscribe(instance, contracts.RoleTopicPrefix+role)

				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, body %s; want 200", recorder.Code, recorder.Body.String())
				}
				if body := recorder.Body.String(); !strings.Contains(body, contracts.RoleTopicPrefix+role) {
					t.Fatalf("body = %s, want the topic it removed", body)
				}
				rig.assertHolder(t, "ses_release_other_topic")
				rig.assertTopics(t, releaseKeptTopic)
			})
			t.Run("every topic the session has", func(t *testing.T) {
				rig := newRoleClaimReleaseRig(t, "ses_release_other_all", role, holdingProcess)

				recorder := rig.unsubscribe(instance)

				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, body %s; want 200", recorder.Code, recorder.Body.String())
				}
				rig.assertHolder(t, "ses_release_other_all")
				rig.assertNoRecord(t)
			})
		})
	}
}

// The administrator's removal frees the role whoever holds it: it names no process and forces
// the release (store.RoleReleaseAuthority.Force), which is what clears the roles of a client
// that has gone away. It frees a claim naming a process and one naming none alike, and the
// session's interest record goes with it.
func TestTheAdministratorsDeleteFreesTheRoleClaimWhoeverHoldsIt(t *testing.T) {
	const role = "legion-controller"
	for name, tc := range map[string]struct {
		sessionID string
		instance  string
	}{
		"a claim naming the session's own process": {sessionID: "ses_release_admin_named", instance: holdingProcess},
		"a claim naming no process":                {sessionID: "ses_release_admin_unnamed"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newRoleClaimReleaseRig(t, tc.sessionID, role, tc.instance)

			recorder := rig.remove()

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body %s; want 204", recorder.Code, recorder.Body.String())
			}
			rig.assertHolder(t, "")
			rig.assertNoRecord(t)
		})
	}
}
