// POST /v1/roles/set decides a soft claim on one read of the role's claim record, and
// store.Registry.SetRoleWithPrevious reads that record again and writes against the second read.
// The cases here land another claim between the two, which is the race the route's own
// precondition exists for: a soft claim that states no if_claim_revision is written against the
// revision the route decided on, so a record replaced under it refuses the write rather than
// letting it overwrite a claim the route never saw.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// roleClaimRetryRig is one listener's claim route over a bus reached through a relay, beside a
// registry on a connection of its own. Its one trick is landsDuringTheClaimsWrite: the relay
// holds the first protocol line naming the claimant's own interest key, which
// store.Registry.SetRoleWithPrevious sends after it has read the role's claim and before it
// writes one, and lands a claim of the second registry's there. So the record the route decided
// on is gone by the time the write reaches the bucket, with nothing timed and nothing slept.
type roleClaimRetryRig struct {
	t        *testing.T
	relay    *listenerRelay
	registry *store.Registry
	sessions *session.SessionRegistry
	// outside is the registry the interloping claim is made through, on a connection straight to
	// the server: the relay's goroutine is held while the hook runs, so a hook writing over the
	// listener's own connection would wait for itself.
	outside *store.Registry
	set     http.Handler
	raced   atomic.Bool
}

// newRoleClaimRetryRig registers every session in sessionIDs as live and returns the route over
// the stores they live in. A session the caller leaves out is one nothing is live under, which is
// a holder a soft claim may supersede.
func newRoleClaimRetryRig(t *testing.T, sessionIDs ...string) *roleClaimRetryRig {
	t.Helper()
	relay := startListenerRelay(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{relay.relayed}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus through the relay: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	waitForSessions(t, sessions)
	for _, sessionID := range sessionIDs {
		if err := sessions.Put(sessionID, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
	}
	outside, err := store.Open(setupPublishTestClient(t).Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry the interloping claim is made through: %v", err)
	}
	waitForRegistry(t, outside)
	deps := withRoleClaimDrain(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
	return &roleClaimRetryRig{
		t:        t,
		relay:    relay,
		registry: registry,
		sessions: sessions,
		outside:  outside,
		set:      roleSetHandler(deps, "test-machine", logging.New("test")),
	}
}

// landsDuringTheClaimsWrite makes winner the holder of role at the one moment that matters: after
// the claim claimant is about to make has been decided and read, and before its write reaches the
// bucket. The trigger is the claimant's own interest key, whose first line the route sends from
// inside store.Registry.SetRoleWithPrevious (UpsertWithin), which is past that call's read of the
// claim; the registration this route refreshes before it is a session bucket write and names no
// interest key, so nothing earlier in the request matches.
func (r *roleClaimRetryRig) landsDuringTheClaimsWrite(claimant, winner, role string) {
	r.t.Helper()
	r.relay.pause("$KV."+bus.InterestBucket+"."+claimant, 1, func() {
		if _, err := r.outside.SetRole(winner, "other-machine", role, false); err != nil {
			r.t.Errorf("the claim that lands under the one being made: %v", err)
		}
		r.raced.Store(true)
	})
}

// claim posts body to /v1/roles/set and returns the answer.
func (r *roleClaimRetryRig) claim(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	r.set.ServeHTTP(recorder, request)
	if !r.raced.Load() {
		r.t.Fatalf("no claim landed under this one, so it never raced: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	return recorder
}

// standingClaim is the claim the role bucket holds, read through the registry the route reads it
// through: its holder and the revision a conditional claim must name.
func (r *roleClaimRetryRig) standingClaim(role string) store.RoleClaim {
	r.t.Helper()
	claim, err := r.registry.RoleClaim(r.t.Context(), role)
	if err != nil {
		r.t.Fatalf("read role %s's claim: %v", role, err)
	}
	return claim
}

// A claim landing between the route's read and the write that follows it takes the role out from
// under the decision the route made: the record it weighed is gone, and its write is refused
// rather than overwriting a claim it never saw. The claimant asked nothing about revisions, so
// that refusal is no answer for it - the route reads the claim again, weighs the whole thing
// again on what stands now, and tries once more.
//
// Deciding again is what this case is about, and the two decisions differ: the first finds the
// role unheld and supersedes nobody, the second finds it held by a session nothing is live under
// and supersedes that holder. A route that kept its first decision and merely rewrote it against
// the new revision would be refused the role as held; one that never tried again would answer the
// revision mismatch. This claim lands, and the role ends up with the claimant.
func TestARoleClaimWhoseRecordIsReplacedUnderItIsDecidedAgainOnTheRecordThatStands(t *testing.T) {
	const (
		claimant = "ses_retry_claimant"
		lapsed   = "ses_retry_lapsed"
		role     = "legion-retry-decided-again"
	)
	rig := newRoleClaimRetryRig(t, claimant)
	rig.landsDuringTheClaimsWrite(claimant, lapsed, role)

	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"client_instance_id":%q}`, claimant, role, "ci_retry_claimant"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("a soft claim whose record was replaced under it: status %d, body %s; want the 200 of a claim decided again on the record that stands",
			recorder.Code, recorder.Body.String())
	}
	claim := rig.standingClaim(role)
	if claim.HolderSessionID != claimant {
		t.Fatalf("%s is held by %q after the claim answered 200, want %q", role, claim.HolderSessionID, claimant)
	}
	if claim.ClientInstanceID != "ci_retry_claimant" {
		t.Fatalf("%s's claim names the process %q, want ci_retry_claimant: the claim that landed is the claimant's own",
			role, claim.ClientInstanceID)
	}
}

// A claim that states its own if_claim_revision is a scheduler sequencing its lookup and its
// claim into one decision, and the revision it names is its own precondition. So the race above,
// made by such a claimant, ends the other way: the revision it sequenced on is gone, that is the
// answer it asked for, and the route neither reads the claim again nor weighs it again. Deciding
// again would take the role, since nothing is live under the session the winning claim names -
// which is exactly the answer this caller did not ask for.
func TestARoleClaimStatingItsOwnRevisionIsRefusedWhenItsRecordMovesAndIsNeverDecidedAgain(t *testing.T) {
	const (
		claimant = "ses_conditional_claimant"
		seeded   = "ses_conditional_seeded"
		winner   = "ses_conditional_winner"
		role     = "legion-retry-caller-precondition"
	)
	rig := newRoleClaimRetryRig(t, claimant)
	if _, err := rig.outside.SetRole(seeded, "other-machine", role, false); err != nil {
		t.Fatalf("seed %s's claim on %s: %v", seeded, role, err)
	}
	required := rig.standingClaim(role).Revision
	if required == 0 {
		t.Fatalf("the seeded claim on %s reports revision 0, so there is no revision to require", role)
	}
	rig.landsDuringTheClaimsWrite(claimant, winner, role)

	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true,"if_claim_revision":%d}`, claimant, role, required))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("a conditional claim whose revision was taken under it: status %d, body %s; want 409",
			recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Error         string `json:"error"`
		Role          string `json:"role"`
		Holder        string `json:"holder"`
		ClaimRevision uint64 `json:"claim_revision"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	standing := rig.standingClaim(role)
	if answer.Role != role || answer.Holder != winner || answer.ClaimRevision != standing.Revision {
		t.Fatalf("the refusal says role %q held by %q at revision %d; want %q held by %q at %d",
			answer.Role, answer.Holder, answer.ClaimRevision, role, winner, standing.Revision)
	}
	if standing.HolderSessionID != winner {
		t.Fatalf("%s is held by %q, want %q: a refused conditional claim is never decided again, and nothing is live under %q for it to supersede",
			role, standing.HolderSessionID, winner, winner)
	}
}
