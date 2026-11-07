// A client hands a role back through POST /v1/interests/unsubscribe, and a client built before a
// release named the process making it hands it back naming none: the listener frees no claim for
// such a release (store.RoleReleaseAuthority), and the role was then renewed by every later
// registration of the process that gave it up, because nothing on the claim said it had been
// handed back. The mark that release now leaves is what stops it: these cases drive the real
// routes over a real role bucket, so what they prove is what a hand-back, the heartbeats after
// it, a claim that takes the role again and the lookup a client reads all do to the claim the
// bucket holds.

package main

import (
	"context"
	"encoding/json"
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
		topic:   handBackOwnTopic,
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

// handBackOwnTopic is a topic of the holder's own, named by every registration below that names
// any: a client registers the subjects it has open, and opens none for a role lane.
const handBackOwnTopic = "notifications.team.handback"

// ownTopics is the topic list a registration carries where it carries one.
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
// claim, since the claim names a process this release cannot be; what it leaves is the mark that
// says the role was given up (store.RoleClaim.HandedBack).
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

// decodeLookup reads GET /v1/roles/<role>, the answer a client weighing the role reads, into
// answer, and fails the test unless the route answered status.
func (r *roleClaimHandBackRig) decodeLookup(status int, answer any) {
	r.t.Helper()
	recorder := r.send(http.MethodGet, "/v1/roles/"+r.role, "")
	if recorder.Code != status {
		r.t.Fatalf("GET /v1/roles/%s: status %d %s, want %d", r.role, recorder.Code, recorder.Body.String(), status)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), answer); err != nil {
		r.t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
}

// The defect this guards: a client that sends its process on every registration but not on its
// hand-back frees no claim when it gives a role up, and went on renewing that claim for as long
// as its process ran - so the role stayed with a process that no longer served it, and the
// role's messages reached a session that had given it back. The hand-back now marks the claim it
// could not free (store.RoleClaim.HandedBack), and no registration renews a marked claim,
// whoever sends it: the claim's renewal stands where the hand-back left it and the role claim
// reaper frees the role once nobody has answered for it inside the live window.
//
// The control is the heartbeat before the hand-back, which renews the very same claim through
// the very same route: without it a case that proved nothing - a route that renewed no claim at
// all - would read as this defect fixed.
func TestARegistrationAfterAHandBackThatFreedNoClaimRenewsTheRoleNoFurther(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_handing_role_back"
	)
	rig := newRoleClaimHandBackRig(t, sessionID, role)
	rig.register(holdingProcess, rig.ownTopics())
	rig.claim(holdingProcess)
	claimed := rig.stored()

	// The control: before the hand-back the heartbeat renews the claim. The renewal is read as
	// the revision it wrote rather than as a moment, since a claim taken and renewed inside one
	// millisecond carries the same stamp twice.
	rig.register(holdingProcess, rig.ownTopics())
	renewed := rig.stored()
	if renewed.Revision == claimed.Revision {
		t.Fatalf("the claim still stands at revision %d after the heartbeat before the hand-back: that registration renewed nothing", renewed.Revision)
	}

	rig.handBack()

	stood := rig.stored()
	if stood.HolderSessionID != sessionID || stood.ClientInstanceID != holdingProcess {
		t.Fatalf("the claim after the hand-back is held by %q of process %q, want %q of %q: a release naming no process frees nothing",
			stood.HolderSessionID, stood.ClientInstanceID, sessionID, holdingProcess)
	}
	if !stood.HandedBack {
		t.Fatalf("the claim after the hand-back is %+v, want it marked handed back: the release could free no claim it did not name the process of", stood)
	}
	// The claim stands, so the role is still this session's and the interest half of that claim
	// stays in the record: a release that freed no claim leaves that claim's lane topic where it
	// is, and the reaper frees the role at its first pass past the live window.
	if topics := rig.topics(); !slices.Contains(topics, contracts.RoleTopicPrefix+role) {
		t.Fatalf("the record after the hand-back is %v, want the role's lane topic still on it: the claim it belongs to stands", topics)
	}

	for beat := range 2 {
		for _, instance := range []string{holdingProcess, ""} {
			rig.register(instance, rig.ownTopics())
			if after := rig.stored(); after != stood {
				t.Fatalf("the claim after heartbeat %d by process %q is %+v, want it exactly as the hand-back left it, %+v: no registration renews a marked claim",
					beat+1, instance, after, stood)
			}
		}
	}
}

// A registration by the process a claim names renews it whatever topics the registration
// carries: no client registers a role's lane topic - a client registers the subjects it opened,
// and opens none for a role lane - and a registration that carries no list at all says nothing
// about the lanes its session follows. What says a role was given up is the mark a hand-back
// leaves on the claim, and nothing here has handed anything back.
func TestARegistrationRenewsTheRoleItHoldsWhateverTopicsItCarries(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_registering_any_topics"
	)
	for name, topics := range map[string]string{
		"a registration naming topics of its own": fmt.Sprintf(`"topics":[%q],`, handBackOwnTopic),
		"a registration that sent no topic list":  "",
		"a registration that sent an empty one":   `"topics":[],`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newRoleClaimHandBackRig(t, sessionID, role)
			rig.register(holdingProcess, rig.ownTopics())
			rig.claim(holdingProcess)
			claimed := rig.stored()

			rig.register(holdingProcess, topics)

			renewed := rig.stored()
			if renewed.Revision == claimed.Revision {
				t.Fatalf("the claim still stands at revision %d, want the renewal this registration made: a renewal is weighed on the claim and not on a topic list",
					renewed.Revision)
			}
			if renewed.HolderSessionID != sessionID || renewed.ClientInstanceID != holdingProcess || renewed.HandedBack {
				t.Fatalf("the renewed claim is %+v, want %q's claim of process %q, unmarked", renewed, sessionID, holdingProcess)
			}
		})
	}
}

// A role granted again to the process that handed it back is that process's role from the write
// on: POST /v1/roles/set writes a fresh claim record, so the mark goes with the claim it was
// written on, and the heartbeats that follow renew the new claim. It is how a holder takes back
// a role it gave up without waiting out the role claim reaper.
func TestAClaimGrantedAgainToTheProcessThatHandedTheRoleBackClearsTheMark(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_reclaiming_its_role"
	)
	rig := newRoleClaimHandBackRig(t, sessionID, role)
	rig.register(holdingProcess, rig.ownTopics())
	rig.claim(holdingProcess)
	rig.handBack()
	if marked := rig.stored(); !marked.HandedBack {
		t.Fatalf("the claim after the hand-back is %+v, want it marked handed back", marked)
	}

	rig.claim(holdingProcess)

	granted := rig.stored()
	if granted.HandedBack {
		t.Fatalf("the claim granted again is %+v, want the mark gone: every claim writes a fresh record", granted)
	}
	rig.register(holdingProcess, rig.ownTopics())
	if renewed := rig.stored(); renewed.Revision == granted.Revision {
		t.Fatalf("the claim still stands at revision %d after the heartbeat that followed the claim: the role is this process's to renew again", renewed.Revision)
	}
}

// The lookup reports the mark, on the 200 of a holder still answering and on the 404 of one that
// has stopped: the claim stands until the role reaper takes it, so a client reading either
// answer sees that its holder gave the role back and that no renewal is keeping it. The key is
// left out where the claim carries no mark, as client_instance_id is left out where the claim
// names no process, so an absent field reads as "this claim is still its holder's".
func TestTheRoleLookupReportsTheHandBackMark(t *testing.T) {
	const (
		role      = "legion-controller"
		sessionID = "ses_looked_up_after_handback"
	)
	rig := newRoleClaimHandBackRig(t, sessionID, role)
	rig.register(holdingProcess, rig.ownTopics())
	rig.claim(holdingProcess)

	// A pointer tells the field left out from the field answered false, which is what a client
	// reads this answer by.
	var held struct {
		Holder     string `json:"holder"`
		HandedBack *bool  `json:"handed_back"`
	}
	rig.decodeLookup(http.StatusOK, &held)
	if held.Holder != sessionID || held.HandedBack != nil {
		t.Fatalf("the lookup of a claim nobody handed back answers holder %q and handed_back %v, want %q and the field left out", held.Holder, held.HandedBack, sessionID)
	}

	rig.handBack()

	rig.decodeLookup(http.StatusOK, &held)
	if held.HandedBack == nil || !*held.HandedBack {
		t.Fatalf("the lookup of a claim its holder handed back answers handed_back %v, want true", held.HandedBack)
	}

	if err := rig.deps.sessions.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("end %s's session: %v", sessionID, err)
	}
	var lapsed roleHolderError
	rig.decodeLookup(http.StatusNotFound, &lapsed)
	if lapsed.Holder != sessionID || !lapsed.HandedBack {
		t.Fatalf("the 404 of a lapsed holder that handed the role back is %+v, want %q's claim marked handed back", lapsed, sessionID)
	}
}
