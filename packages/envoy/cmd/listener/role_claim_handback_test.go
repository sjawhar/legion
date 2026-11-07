// A client hands a role back through POST /v1/interests/unsubscribe, and a client built before a
// release named the process making it hands it back naming none: the listener frees no claim for
// such a release (store.RoleReleaseAuthority), and the role was then renewed by every later
// registration of the process that gave it up, because the renewal followed the claim view alone.
// What stops it is the interest record the hand-back left: these cases drive the real routes over
// a real role bucket, so what they prove is what a hand-back and the heartbeats after it do to the
// claim the bucket holds.

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// roleClaimHandBackRig is one listener's whole set of v1 routes over one listener's stores: a
// hand-back is three routes in sequence - the registration, the claim, and the unsubscribe that
// gives the role back - and the heartbeats that follow are the first route again, so the rig is
// the mux rather than one handler.
type roleClaimHandBackRig struct {
	t       *testing.T
	deps    *listenerDeps
	routes  *http.ServeMux
	session string
	role    string
	topic   string
}

func newRoleClaimHandBackRig(t *testing.T, sessionID, role string) *roleClaimHandBackRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	deps := withRoleClaimDrain(t, &listenerDeps{client: setupPublishTestClient(t), registry: registry, sessions: sessions})
	routes := http.NewServeMux()
	registerV1Routes(routes, deps, "test-machine", logging.New("test"))
	return &roleClaimHandBackRig{
		t:       t,
		deps:    deps,
		routes:  routes,
		session: sessionID,
		role:    role,
		topic:   "notifications.team.handback",
	}
}

// register is the heartbeat POST /v1/interests/subscribe a running client repeats, carrying the
// topics it names and the process making it. Every client this listener serves sends a topic
// list on it, and none of them names the role's own lane topic there: the lane topic reaches the
// session's interest record from the claim (POST /v1/roles/set) and stays on it because a
// registration merges its topics into the record rather than replacing them.
func (r *roleClaimHandBackRig) register(instance string, topics string) {
	r.t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,%s"self_subscribed":true,"client_instance_id":%q}`, r.session, topics, instance)
	recorder := r.send(http.MethodPost, "/v1/interests/subscribe", body)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("registration of %s by process %q: status %d %s, want 200", r.session, instance, recorder.Code, recorder.Body.String())
	}
}

// ownTopics is the topic list every registration below carries but the two that carry none: one
// topic of the session's own, as a client registers the subjects it has open.
func (r *roleClaimHandBackRig) ownTopics() string {
	return fmt.Sprintf(`"topics":[%q],`, r.topic)
}

// claim takes the role for this session through POST /v1/roles/set, which writes the lane topic
// onto the session's interest record beside the claim it stores.
func (r *roleClaimHandBackRig) claim(instance string) {
	r.t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"role":%q,"client_instance_id":%q}`, r.session, r.role, instance)
	if recorder := r.send(http.MethodPost, "/v1/roles/set", body); recorder.Code != http.StatusOK {
		r.t.Fatalf("the claim of %s by process %q: status %d %s, want 200", r.role, instance, recorder.Code, recorder.Body.String())
	}
}

// handBack is the role's hand-back as a client built before a release named its process sends
// it: the role's lane topic and no client_instance_id at all. The route answers 200 and frees no
// claim, which is the whole of what this case starts from.
func (r *roleClaimHandBackRig) handBack() {
	r.t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"topics":[%q]}`, r.session, contracts.RoleTopicPrefix+r.role)
	if recorder := r.send(http.MethodPost, "/v1/interests/unsubscribe", body); recorder.Code != http.StatusOK {
		r.t.Fatalf("the hand-back of %s: status %d %s, want 200", r.role, recorder.Code, recorder.Body.String())
	}
}

func (r *roleClaimHandBackRig) send(method, target, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	r.routes.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
	return recorder
}

// stored is the role's claim as the bucket holds it now.
func (r *roleClaimHandBackRig) stored() store.RoleClaim {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claim, err := r.deps.registry.RoleClaim(ctx, r.role)
	if err != nil {
		r.t.Fatalf("read %s's claim: %v", r.role, err)
	}
	return claim
}

// topics is the session's interest record as the routes have left it.
func (r *roleClaimHandBackRig) topics() []string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	item, err := r.deps.registry.Get(ctx, r.session)
	if err != nil {
		r.t.Fatalf("read %s's interest record: %v", r.session, err)
	}
	return item.Topics
}

// The defect this guards: a client that sends its process on every registration but not on its
// hand-back frees no claim when it gives a role up, and went on renewing that claim for as long
// as its process ran - so the role stayed with a process that no longer served it, and the role's
// messages reached a session that had given it back. The hand-back removes the role's lane topic
// from the session's interest record, and that record is what the registration's renewal now
// reads: the heartbeats after the hand-back leave the claim exactly as it stands, and the role
// claim reaper frees the role once nobody has answered for it inside the live window.
//
// The control is the heartbeat before the hand-back, which renews the very same claim through the
// very same route: without it a case that proved nothing - a route that renewed no claim at all -
// would read as this defect fixed.
func TestARegistrationAfterAHandBackThatFreedNoClaimRenewsTheRoleNoFurther(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_handing_role_back"
	)
	rig := newRoleClaimHandBackRig(t, sessionID, role)
	rig.register(holdingProcess, rig.ownTopics())
	rig.claim(holdingProcess)
	claimed := rig.stored()

	// The control: while the record still names the lane topic, the heartbeat renews the claim.
	// The renewal is read as the revision it wrote rather than as a moment, since a claim taken
	// and renewed inside one millisecond carries the same stamp twice.
	rig.register(holdingProcess, rig.ownTopics())
	renewed := rig.stored()
	if renewed.Revision == claimed.Revision {
		t.Fatalf("the claim still stands at revision %d after the heartbeat before the hand-back: that registration renewed nothing", renewed.Revision)
	}

	rig.handBack()

	if held := rig.stored(); held.HolderSessionID != sessionID || held.ClientInstanceID != holdingProcess {
		t.Fatalf("the claim after the hand-back is held by %q of process %q, want %q of %q: a release naming no process frees nothing",
			held.HolderSessionID, held.ClientInstanceID, sessionID, holdingProcess)
	}
	if topics := rig.topics(); slices.Contains(topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("the record after the hand-back is %v, want the role's lane topic gone from it", topics)
	}
	stood := rig.stored()

	for beat := range 2 {
		rig.register(holdingProcess, rig.ownTopics())
		if after := rig.stored(); after != stood {
			t.Fatalf("the claim after heartbeat %d is %+v, want it exactly as the hand-back left it, %+v: the record no longer names the role's lane",
				beat+1, after, stood)
		}
	}
}

// A registration that carried no topics at all says nothing about the lanes its session still
// follows, so it renews every claim its process holds, as every registration did before: the
// record it leaves is not read as a hand-back. Without this arm a client that registers no topics
// would lose every role it holds one live window after it started.
//
// The control is the same session, the same claim and the same hand-back, registering with a
// topic list: that registration renews nothing, so what this case reads is the arm and not a
// renewal that would have happened anyway.
func TestARegistrationCarryingNoTopicsRenewsEveryClaimItHolds(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_registering_no_topics"
	)
	for name, topics := range map[string]string{
		"a registration that sent no topic list": "",
		"a registration that sent an empty one":  `"topics":[],`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newRoleClaimHandBackRig(t, sessionID, role)
			rig.register(holdingProcess, rig.ownTopics())
			rig.claim(holdingProcess)
			rig.handBack()
			stood := rig.stored()

			// The control: a registration that states its topics renews nothing here.
			rig.register(holdingProcess, rig.ownTopics())
			if after := rig.stored(); after != stood {
				t.Fatalf("the claim after a registration naming its topics is %+v, want it as the hand-back left it, %+v", after, stood)
			}

			rig.register(holdingProcess, topics)

			if after := rig.stored(); after.Revision == stood.Revision {
				t.Fatalf("the claim still stands at revision %d, want the renewal this registration made: a registration carrying no topics renews every claim it holds",
					after.Revision)
			}
		})
	}
}
