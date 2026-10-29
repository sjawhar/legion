package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// storedRoleClaim is the role bucket's own record of a claim: the bytes it holds and the revision
// it holds them at. A release deletes the key, so a read that released one leaves no record at
// all, and a claim written again afterwards is at a higher revision; comparing both is therefore
// the whole question "is this claim as it was?".
type storedRoleClaim struct {
	value    string
	revision uint64
}

func readStoredRoleClaim(t *testing.T, registry *store.Registry, role string) storedRoleClaim {
	t.Helper()
	entry, err := registryRoleKV(t, registry).Get(role)
	if err != nil {
		t.Fatalf("read the stored claim of role %s: %v", role, err)
	}
	return storedRoleClaim{value: string(entry.Value()), revision: entry.Revision()}
}

// seedLapsedRoleClaim gives role a claim whose holder registered and is gone, the state a role
// read is asked about: a session that stopped without releasing its role. It returns the holder's
// last heartbeat, which the listener keeps after the session leaves its cache.
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
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	if err := sessions.Delete(context.Background(), holder); err != nil {
		t.Fatalf("end %s's session: %v", holder, err)
	}
	return entry.UpdatedAt
}

// A lookup of a role whose holder is gone answers what it found and changes nothing: the claim is
// in the bucket at the same revision with the same bytes afterwards, and a second lookup answers
// the same, because the first took nothing. That is the whole point of the route: a monitor that
// watches a role, or an agent asking who holds one, must be able to read a lapsed claim without
// ending it. Reaping it is the role reaper's and the next claimant's (keepLapsedRoleClaim), and
// the reaper still ends exactly this claim once the read has answered.
func TestARoleLookupLeavesALapsedHoldersClaimAsItIs(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_lapsed_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	lastSeen := seedLapsedRoleClaim(t, registry, sessions, role, holder)
	seeded := readStoredRoleClaim(t, registry, role)

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	for _, read := range []string{"first", "second"} {
		recorder := httptest.NewRecorder()
		roleGetHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s lookup: status %d %s, want 404", read, recorder.Code, recorder.Body.String())
		}
		// The claim first: that it is still there, at the revision and bytes it was seeded with,
		// is the property. What the answer says about it follows.
		if stored := readStoredRoleClaim(t, registry, role); stored != seeded {
			t.Fatalf("the claim after the %s lookup = %+v, want %+v: the lookup changed it", read, stored, seeded)
		}
		var answer struct {
			Error         string `json:"error"`
			Reason        string `json:"reason"`
			Holder        string `json:"holder"`
			LastSeen      int64  `json:"last_seen"`
			ClaimReleased bool   `json:"claim_released"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&answer); err != nil {
			t.Fatalf("decode the %s lookup's answer: %v", read, err)
		}
		if answer.Reason != "holder_lapsed" || answer.Holder != holder || answer.LastSeen != lastSeen {
			t.Fatalf("the %s lookup answered %+v, want %s's lapsed claim with its last heartbeat", read, answer, holder)
		}
		if answer.ClaimReleased {
			t.Fatalf("the %s lookup answered claim_released: a read releases nothing", read)
		}
		t.Logf("%s lookup: %s", read, answer.Error)
	}

	// The claim is still the reaper's to end, so leaving it is not leaving it for ever.
	reaped, err := registry.ReapRoleClaims(func(string) bool { return false }, 0)
	if err != nil || reaped != 1 {
		t.Fatalf("reap role claims = %d, %v; want the one claim the lookups left", reaped, err)
	}
	if got, err := registry.RoleHolder(role); err != nil || got != "" {
		t.Fatalf("the claim after the reaper = %q, %v; want it released", got, err)
	}
}

// GET /v1/roles answers every claim at once - which is what a monitor of the roles reads, and what
// GET /v1/sessions cannot give, since a session that is gone lists nothing - and changes no claim
// either: a lapsed holder's claim is in the bucket at the same revision with the same bytes once
// the list has answered.
func TestTheRoleListReadsEveryClaimAndTouchesNone(t *testing.T) {
	const (
		liveRole     = "planner"
		liveHolder   = "ses_live_holder"
		lapsedRole   = "reviewer"
		lapsedHolder = "ses_gone_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put(liveHolder, session.SessionEntry{
		MachineID:      "test-machine",
		Dir:            "/test/live",
		Title:          "the planner",
		Capabilities:   []string{"steer"},
		SelfSubscribed: true,
	}); err != nil {
		t.Fatalf("register %s: %v", liveHolder, err)
	}
	claimRoles(t, registry, liveHolder, liveRole)
	lastSeen := seedLapsedRoleClaim(t, registry, sessions, lapsedRole, lapsedHolder)
	seeded := map[string]storedRoleClaim{
		liveRole:   readStoredRoleClaim(t, registry, liveRole),
		lapsedRole: readStoredRoleClaim(t, registry, lapsedRole),
	}
	waitFor(t, 10*time.Second, "both claims to reach the registry's view", func() bool {
		claims, err := registry.RoleHolders()
		return err == nil && claims[liveRole] == liveHolder && claims[lapsedRole] == lapsedHolder
	})

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	recorder := httptest.NewRecorder()
	roleListHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list the roles: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	var rows []roleHolderInfo
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatalf("decode the list: %v", err)
	}
	// Sorted by role, so the lapsed claim's row is the planner's second.
	want := []roleHolderInfo{
		{
			Role: liveRole, State: roleHolderLive, Holder: liveHolder,
			Title: "the planner", Dir: "/test/live", MachineID: "test-machine", Host: "test-machine",
			Capabilities: []string{"steer"}, LastSeen: rowLastSeen(rows, liveRole),
		},
		{Role: lapsedRole, State: roleHolderLapsed, Holder: lapsedHolder, LastSeen: lastSeen},
	}
	if len(rows) != len(want) {
		t.Fatalf("the list holds %d rows (%+v), want %d", len(rows), rows, len(want))
	}
	for i, row := range rows {
		if row.Role != want[i].Role || row.State != want[i].State || row.Holder != want[i].Holder ||
			row.Title != want[i].Title || row.Dir != want[i].Dir || row.MachineID != want[i].MachineID ||
			row.Host != want[i].Host || row.LastSeen != want[i].LastSeen ||
			len(row.Capabilities) != len(want[i].Capabilities) {
			t.Fatalf("row %d = %+v, want %+v", i, row, want[i])
		}
	}
	if rows[0].LastSeen == 0 {
		t.Fatalf("the live row names no heartbeat: %+v", rows[0])
	}
	for role, stored := range seeded {
		if got := readStoredRoleClaim(t, registry, role); got != stored {
			t.Fatalf("the claim of role %s after the list = %+v, want %+v: the list changed it", role, got, stored)
		}
	}
	t.Logf("GET /v1/roles: %s", recorder.Body.String())
}

// rowLastSeen is the heartbeat the list reported for role, which the live holder's row carries
// from its session entry; the test asserts separately that it is not zero.
func rowLastSeen(rows []roleHolderInfo, role string) int64 {
	for _, row := range rows {
		if row.Role == role {
			return row.LastSeen
		}
	}
	return 0
}

// The list answers a GET and nothing else: a write never arrives at a read route by mistake.
func TestTheRoleListRefusesEveryOtherMethod(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	recorder := httptest.NewRecorder()
	roleListHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/v1/roles", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/roles: status %d %s, want 405", recorder.Code, recorder.Body.String())
	}
}

// /v1/roles is registered as its own exact route, ahead of the /v1/roles/ subtree the lookup
// answers: without that registration http.ServeMux redirects the path to the trailing slash and
// the lookup answers it "role is required". This drives the registered routes rather than the
// handler, so a registration deleted by mistake fails here.
func TestTheRoleListAnswersItsPathThroughTheRegisteredRoutes(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	claimRoles(t, registry, "ses_route_holder", "planner")
	waitFor(t, 10*time.Second, "the claim to reach the registry's view", func() bool {
		claims, err := registry.RoleHolders()
		return err == nil && claims["planner"] == "ses_route_holder"
	})
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{client: client, registry: registry, sessions: sessions}, "test-machine", logging.New("test"))

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/roles through the registered routes: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	var rows []roleHolderInfo
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatalf("decode the list: %v", err)
	}
	if len(rows) != 1 || rows[0].Role != "planner" || rows[0].Holder != "ses_route_holder" {
		t.Fatalf("the list = %+v, want the planner's claim", rows)
	}
}

// The registry's view of the claims answers RoleHolders with every claim and no bucket call, and a
// claim this build cannot read (store.ErrMalformedRoleClaim: here a record that names no holder)
// names nobody, so it is in no entry rather than one naming nobody - and in no row of the list
// the route builds from it.
func TestRoleHoldersAnswersEveryReadableClaim(t *testing.T) {
	registry, _ := setupSessionsTest(t, nil, nil)
	claimRoles(t, registry, "ses_holder", "planner", "reviewer")
	roleKV := registryRoleKV(t, registry)
	if _, err := roleKV.Put("unreadable", []byte(`{"holder_session_id":""}`)); err != nil {
		t.Fatalf("store a claim this build cannot read: %v", err)
	}
	// Written straight to the bucket after it, and only the watcher can bring it in: the view
	// holding it proves the watcher has applied the unreadable record before it, where
	// claimRoles' own claims are in the view the moment SetRole returns and prove nothing of the
	// watcher.
	if _, err := roleKV.Put("watched", []byte(`{"holder_session_id":"ses_watched"}`)); err != nil {
		t.Fatalf("store the claim the watcher must deliver: %v", err)
	}
	waitFor(t, 10*time.Second, "the watcher to deliver the claims written past the unreadable one", func() bool {
		claims, err := registry.RoleHolders()
		return err == nil && claims["watched"] == "ses_watched"
	})
	claims, err := registry.RoleHolders()
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	if claims["planner"] != "ses_holder" || claims["reviewer"] != "ses_holder" {
		t.Fatalf("the claims = %v, want both roles held by ses_holder", claims)
	}
	if holder, ok := claims["unreadable"]; ok {
		t.Fatalf("a claim this build cannot read names %q; want no entry at all", holder)
	}
	// The map is the caller's own: writing it changes no later read.
	claims["planner"] = "ses_someone_else"
	if again, err := registry.RoleHolders(); err != nil || again["planner"] != "ses_holder" {
		t.Fatalf("the claims after a caller wrote its map = %v, %v; want ses_holder's", again, err)
	}
}
