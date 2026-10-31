// A conditional role claim: GET /v1/roles/<role> reports the role's claim revision, and POST
// /v1/roles/set takes the role only while the bucket still holds the revision the claimant read.
// Every case here drives the two HTTP handlers against a real role bucket, so what it proves is
// the wire shape a scheduler sees, not a store call's signature.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// conditionalClaimRig is the two routes a scheduler uses, over one role bucket, with the sessions
// the cases claim as already registered.
type conditionalClaimRig struct {
	t        *testing.T
	registry *store.Registry
	sessions *session.SessionRegistry
	roleKV   nats.KeyValue
	set      http.Handler
	get      http.Handler
}

func newConditionalClaimRig(t *testing.T, sessionIDs ...string) *conditionalClaimRig {
	t.Helper()
	registry, sessions := setupSessionsTest(t, nil, nil)
	deps := &listenerDeps{registry: registry, sessions: sessions}
	for _, sessionID := range sessionIDs {
		if err := sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine"}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
	}
	// The role bucket itself, read beside the routes: the revision a case builds its request
	// from comes from the bucket, so each case still reaches the claim it is about on a build
	// whose GET reports no revision at all, and fails there too rather than only here.
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open jetstream: %v", err)
	}
	roleKV, err := js.KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	return &conditionalClaimRig{
		t:        t,
		registry: registry,
		sessions: sessions,
		roleKV:   roleKV,
		set:      roleSetHandler(deps, "test-machine", logging.New("test")),
		get:      roleGetHandler(deps),
	}
}

// bucketRevision is the revision the role bucket holds role's claim at, 0 for a role with no
// claim record. It is the number a conditional claim must name.
func (r *conditionalClaimRig) bucketRevision(role string) uint64 {
	r.t.Helper()
	entry, err := r.roleKV.Get(role)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0
	}
	if err != nil {
		r.t.Fatalf("read role %s's claim entry: %v", role, err)
	}
	return entry.Revision()
}

// claim posts body to /v1/roles/set and returns the answer.
func (r *conditionalClaimRig) claim(body string) *httptest.ResponseRecorder {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	r.set.ServeHTTP(recorder, request)
	return recorder
}

// lookup reads GET /v1/roles/<role> and returns its status and decoded body.
func (r *conditionalClaimRig) lookup(role string) (int, map[string]any) {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	r.get.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	body := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		r.t.Fatalf("decode GET /v1/roles/%s (%d): %v; body %s", role, recorder.Code, err, recorder.Body.String())
	}
	return recorder.Code, body
}

// reportsClaimRevision checks that GET /v1/roles/<role> reports want as claim_revision. It
// reports rather than fails, so a case goes on to the claim it is about even where the lookup
// carries no such field at all.
func (r *conditionalClaimRig) reportsClaimRevision(role string, want uint64) {
	r.t.Helper()
	status, body := r.lookup(role)
	revision, ok := body["claim_revision"].(float64)
	if !ok {
		r.t.Errorf("GET /v1/roles/%s (%d) carries no numeric claim_revision: %v", role, status, body)
		return
	}
	if uint64(revision) != want {
		r.t.Errorf("GET /v1/roles/%s (%d) reports claim_revision %v, want %d", role, status, body["claim_revision"], want)
	}
}

// holder is the session the role's claim names in the bucket itself.
func (r *conditionalClaimRig) holder(role string) string {
	r.t.Helper()
	holder, err := r.registry.RoleHolder(role)
	if err != nil {
		r.t.Fatalf("read role %s's holder: %v", role, err)
	}
	return holder
}

// holdsRoleTopic says whether sessionID's interest record carries the role's topic. A refused
// claim must leave it untouched: the claimant's interest write is the one write that happens
// before the claim itself, so this is where a precondition weighed too late would show. A
// claimant whose every claim was refused has no interest record at all, which is that same
// answer in its strongest form.
func (r *conditionalClaimRig) holdsRoleTopic(sessionID, role string) bool {
	r.t.Helper()
	item, err := r.registry.Get(context.Background(), sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false
	}
	if err != nil {
		r.t.Fatalf("read %s's interest record: %v", sessionID, err)
	}
	return slices.Contains(item.Topics, contracts.RoleTopicPrefix+role)
}

// A claim naming a revision the role's claim no longer sits at is refused, and the refusal writes
// nothing: the role keeps its holder and the claimant's record never takes the role's topic.
func TestAConditionalRoleClaimNamingAnotherRevisionIsRefusedAndWritesNothing(t *testing.T) {
	const (
		role     = "conditional-stale"
		holder   = "ses_cond_holder"
		claimant = "ses_cond_claimant"
	)
	rig := newConditionalClaimRig(t, holder, claimant)

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)); recorder.Code != http.StatusOK {
		t.Fatalf("the first claim: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	revision := rig.bucketRevision(role)
	if revision == 0 {
		t.Fatalf("the role bucket holds no claim for %s after %s claimed it", role, holder)
	}
	rig.reportsClaimRevision(role, revision)
	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":%d}`, claimant, role, revision+1))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("a claim at revision %d while the bucket holds %d: status %d, body %s; want 409",
			revision+1, revision, recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Error         string `json:"error"`
		Role          string `json:"role"`
		Holder        string `json:"holder"`
		ClaimRevision uint64 `json:"claim_revision"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the 409: %v; body %s", err, recorder.Body.String())
	}
	if answer.Role != role || answer.Holder != holder || answer.ClaimRevision != revision {
		t.Fatalf("the 409 says role %q, holder %q, claim_revision %d; want %q, %q, %d",
			answer.Role, answer.Holder, answer.ClaimRevision, role, holder, revision)
	}
	if !strings.Contains(answer.Error, fmt.Sprintf("revision %d", revision)) {
		t.Fatalf("the 409's error does not name the revision the bucket holds: %q", answer.Error)
	}

	if got := rig.holder(role); got != holder {
		t.Fatalf("after the refused claim role %s is held by %q, want %q", role, got, holder)
	}
	if got := rig.bucketRevision(role); got != revision {
		t.Fatalf("after the refused claim the bucket holds %s's claim at revision %d, want the untouched %d", role, got, revision)
	}
	rig.reportsClaimRevision(role, revision)
	if rig.holdsRoleTopic(claimant, role) {
		t.Fatalf("the refused claimant %s took role %s's topic into its interest record", claimant, role)
	}
}

// A claim naming the revision the bucket holds takes the role, and the role's claim moves to a
// revision of its own, which the next conditional claim must name instead.
func TestAConditionalRoleClaimNamingTheCurrentRevisionTakesTheRole(t *testing.T) {
	const (
		role     = "conditional-fresh"
		holder   = "ses_cond_first"
		claimant = "ses_cond_second"
	)
	rig := newConditionalClaimRig(t, holder, claimant)

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)); recorder.Code != http.StatusOK {
		t.Fatalf("the first claim: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	revision := rig.bucketRevision(role)
	rig.reportsClaimRevision(role, revision)

	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":%d}`, claimant, role, revision))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a claim at the revision the bucket holds (%d): status %d, body %s; want 200",
			revision, recorder.Code, recorder.Body.String())
	}
	if got := rig.holder(role); got != claimant {
		t.Fatalf("role %s is held by %q after the conditional claim, want %q", role, got, claimant)
	}
	after := rig.bucketRevision(role)
	if after == revision || after == 0 {
		t.Fatalf("the bucket holds %s's claim at revision %d after the conditional claim, want a revision of its own past %d", role, after, revision)
	}
	rig.reportsClaimRevision(role, after)
	if !rig.holdsRoleTopic(claimant, role) {
		t.Fatalf("the claimant %s holds role %s but not its topic", claimant, role)
	}
}

// if_claim_revision 0 is the claimant's word that the role has no claim record. It takes an
// unclaimed role, and is refused the moment one exists - the case a scheduler racing another
// scheduler over a role nobody held must lose rather than overwrite.
func TestAConditionalRoleClaimAtRevisionZeroTakesOnlyARoleWithNoClaimRecord(t *testing.T) {
	const (
		unclaimed = "conditional-unclaimed"
		taken     = "conditional-taken"
		first     = "ses_cond_zero_first"
		second    = "ses_cond_zero_second"
	)
	rig := newConditionalClaimRig(t, first, second)

	status, body := rig.lookup(unclaimed)
	if status != http.StatusNotFound {
		t.Fatalf("GET /v1/roles/%s: status %d, body %v; want 404", unclaimed, status, body)
	}
	if revision, ok := body["claim_revision"].(float64); !ok || revision != 0 {
		t.Errorf("GET /v1/roles/%s reports claim_revision %v, want 0 for a role no record claims", unclaimed, body["claim_revision"])
	}
	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, first, unclaimed)); recorder.Code != http.StatusOK {
		t.Fatalf("a claim at revision 0 of a role no record claims: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	if got := rig.holder(unclaimed); got != first {
		t.Fatalf("role %s is held by %q, want %q", unclaimed, got, first)
	}

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, first, taken)); recorder.Code != http.StatusOK {
		t.Fatalf("the first claim of %s: status %d, body %s; want 200", taken, recorder.Code, recorder.Body.String())
	}
	revision := rig.bucketRevision(taken)
	rig.reportsClaimRevision(taken, revision)
	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, second, taken))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("a claim at revision 0 of a role %s already claims: status %d, body %s; want 409",
			first, recorder.Code, recorder.Body.String())
	}
	var answer struct {
		Holder        string `json:"holder"`
		ClaimRevision uint64 `json:"claim_revision"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the 409: %v; body %s", err, recorder.Body.String())
	}
	if answer.Holder != first || answer.ClaimRevision != revision {
		t.Fatalf("the 409 says holder %q at claim_revision %d; want %q at %d", answer.Holder, answer.ClaimRevision, first, revision)
	}
	if got := rig.holder(taken); got != first {
		t.Fatalf("after the refused claim role %s is held by %q, want %q", taken, got, first)
	}
	if rig.holdsRoleTopic(second, taken) {
		t.Fatalf("the refused claimant %s took role %s's topic into its interest record", second, taken)
	}
}

// A claim that names no if_claim_revision is the claim this listener always answered: hard claims
// stay last-claim-wins over a role another session holds, and a soft claim over a live holder
// still answers the 409 that names it.
func TestARoleClaimWithNoIfClaimRevisionIsUnchanged(t *testing.T) {
	const (
		hard   = "conditional-absent-hard"
		soft   = "conditional-absent-soft"
		holder = "ses_cond_absent_holder"
		other  = "ses_cond_absent_other"
	)
	rig := newConditionalClaimRig(t, holder, other)

	for _, role := range []string{hard, soft} {
		if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, holder, role)); recorder.Code != http.StatusOK {
			t.Fatalf("the first claim of %s: status %d, body %s; want 200", role, recorder.Code, recorder.Body.String())
		}
	}

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, other, hard)); recorder.Code != http.StatusOK {
		t.Fatalf("a hard claim with no if_claim_revision over a held role: status %d, body %s; want 200",
			recorder.Code, recorder.Body.String())
	}
	if got := rig.holder(hard); got != other {
		t.Fatalf("role %s is held by %q after a last-claim-wins claim, want %q", hard, got, other)
	}

	recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"soft":true}`, other, soft))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`"holder":%q`, holder)) {
		t.Fatalf("a soft claim with no if_claim_revision over a live holder: status %d, body %s; want 409 naming %s",
			recorder.Code, recorder.Body.String(), holder)
	}
	if got := rig.holder(soft); got != holder {
		t.Fatalf("role %s is held by %q after the refused soft claim, want %q", soft, got, holder)
	}
}

// The lapsed holder, which is what a scheduler meets after a session exits cleanly and its
// registration is gone: the lookup releases the claim and reports no revision, and a claim at
// `if_claim_revision: 0` then takes the role, because that is the state the lookup described.
func TestALookupThatReleasesALapsedClaimReportsNoRevisionAndZeroThenClaims(t *testing.T) {
	const (
		role     = "conditional-lapsed"
		lapsed   = "ses_cond_lapsed"
		claimant = "ses_cond_successor"
	)
	rig := newConditionalClaimRig(t, lapsed, claimant)

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q}`, lapsed, role)); recorder.Code != http.StatusOK {
		t.Fatalf("the lapsing holder's claim: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	if err := rig.sessions.Delete(context.Background(), lapsed); err != nil {
		t.Fatalf("deregister %s: %v", lapsed, err)
	}

	status, body := rig.lookup(role)
	if status != http.StatusNotFound || body["reason"] != "holder_lapsed" {
		t.Fatalf("GET /v1/roles/%s with its holder deregistered: status %d, body %v; want 404 holder_lapsed", role, status, body)
	}
	if revision, ok := body["claim_revision"].(float64); !ok || revision != 0 {
		t.Errorf("the lapsed answer reports claim_revision %v, want 0 for the claim it released", body["claim_revision"])
	}
	if got := rig.bucketRevision(role); got != 0 {
		t.Fatalf("the role bucket still holds %s's claim at revision %d after the lookup released it", role, got)
	}

	if recorder := rig.claim(fmt.Sprintf(`{"session_id":%q,"role":%q,"if_claim_revision":0}`, claimant, role)); recorder.Code != http.StatusOK {
		t.Fatalf("a claim at revision 0 of the released role: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	if got := rig.holder(role); got != claimant {
		t.Fatalf("role %s is held by %q, want %q", role, got, claimant)
	}
}
