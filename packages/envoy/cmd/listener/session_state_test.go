package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// stateClock is the registry's clock for the tests below: the real time moved on by what the test
// advanced it, so the creation times the server stamps and the registry's own time agree until a
// test moves a session past a window without waiting it out.
type stateClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *stateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *stateClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// stalledSessionsRig opens the two registries and the clock these tests cross the session windows
// with: a live window of a minute, a gone horizon of two, and a bucket whose maximum age is far
// longer than either, which is the deployed shape (ENVOY_SESSION_MAX_AGE past the gone horizon)
// where a stalled session's record still stands to be read as unresponsive.
func stalledSessionsRig(t *testing.T) (*listenerDeps, *stateClock) {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	now := &stateClock{}
	sessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(1),
		session.WithSessionTTL(time.Hour),
		session.WithSessionClock(now.Now),
		session.WithSessionLiveWindow(time.Minute),
		session.WithSessionGoneHorizon(2*time.Minute),
	)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	waitForSessions(t, sessions)
	return &listenerDeps{client: client, registry: registry, sessions: sessions}, now
}

// register writes sessionID's registration as its plugin's heartbeat does, and answers the
// registration time the record now carries.
func register(t *testing.T, d *listenerDeps, sessionID string, entry session.SessionEntry) int64 {
	t.Helper()
	if err := d.sessions.Put(sessionID, entry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	stored, state := d.sessions.Lookup(sessionID)
	if state != session.Live {
		t.Fatalf("%s just registered reads %s, want live", sessionID, state)
	}
	return stored.UpdatedAt
}

// A session that stopped heartbeating is not dropped from the registry's answers: it is left out
// of the default listing, as it always was, and listed with state "unresponsive" and its last
// registration when the caller asks for the unresponsive rows. Before this change the same
// session was simply absent, and every caller reading that absence - a Legion controller looking
// for a worker to replace among them - read it as dead.
func TestAStalledSessionIsListedOnlyWhenTheCallerAsksForTheUnresponsiveOnes(t *testing.T) {
	d, now := stalledSessionsRig(t)
	stalledAt := register(t, d, "ses_stalled", session.SessionEntry{MachineID: "test-machine", Dir: "/test/stalled", SelfSubscribed: true})
	now.advance(90 * time.Second)
	register(t, d, "ses_live", session.SessionEntry{MachineID: "test-machine", Dir: "/test/live", SelfSubscribed: true})

	listed := func(query string) []sessionInfo {
		t.Helper()
		recorder := httptest.NewRecorder()
		sessionsHandler(d.registry, d.sessions).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/sessions%s: status %d, body %s", query, recorder.Code, recorder.Body.String())
		}
		var rows []sessionInfo
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode GET /v1/sessions%s: %v", query, err)
		}
		return rows
	}

	live := listed("")
	if len(live) != 1 || live[0].SessionID != "ses_live" || live[0].State != session.Live {
		t.Fatalf("the default listing = %+v, want only ses_live, live", live)
	}
	both := listed("?include=unresponsive")
	if len(both) != 2 {
		t.Fatalf("the listing with the unresponsive rows = %+v, want both sessions", both)
	}
	stalled := both[1]
	if both[0].SessionID == "ses_stalled" {
		stalled = both[0]
	}
	if stalled.SessionID != "ses_stalled" || stalled.State != session.Unresponsive || stalled.LastSeen != stalledAt {
		t.Fatalf("the stalled row = %+v, want ses_stalled unresponsive, last seen %d", stalled, stalledAt)
	}
}

// GET /v1/registry/<session_id> keeps its 404 for a session that stopped answering, so a caller
// that reads only the "no live session " prefix - an older Dispatch classifying a delivery
// failure - is unaffected, and adds the state and the last registration to that body, so a caller
// that reads them can tell "stalled" from "gone". With include=unresponsive it answers the
// session itself, which is how a caller that must still address it gets its port and its machine.
func TestTheRegistryRouteAnswersAStalledSessionOnlyWhenAsked(t *testing.T) {
	d, now := stalledSessionsRig(t)
	stalledAt := register(t, d, "ses_stalled", session.SessionEntry{MachineID: "test-machine", Dir: "/test/stalled", SelfSubscribed: true})
	now.advance(90 * time.Second)

	refused := httptest.NewRecorder()
	sessionEntryHandler(d).ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "/v1/registry/ses_stalled", nil))
	if refused.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/registry/ses_stalled: status %d, body %s; want 404", refused.Code, refused.Body.String())
	}
	var refusal noLiveSessionRefusal
	if err := json.Unmarshal(refused.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", refused.Body.String(), err)
	}
	if refusal.Error != "no live session ses_stalled" {
		t.Fatalf("the refusal reads %q, want the kept prefix and nothing after it", refusal.Error)
	}
	if refusal.State != session.Unresponsive || refusal.LastSeen != stalledAt {
		t.Fatalf("the refusal = %+v, want state unresponsive and last seen %d", refusal, stalledAt)
	}

	asked := httptest.NewRecorder()
	sessionEntryHandler(d).ServeHTTP(asked, httptest.NewRequest(http.MethodGet, "/v1/registry/ses_stalled?include=unresponsive", nil))
	if asked.Code != http.StatusOK {
		t.Fatalf("GET /v1/registry/ses_stalled?include=unresponsive: status %d, body %s; want 200", asked.Code, asked.Body.String())
	}
	var info sessionInfo
	if err := json.Unmarshal(asked.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode the answer %s: %v", asked.Body.String(), err)
	}
	if info.SessionID != "ses_stalled" || info.State != session.Unresponsive || info.LastSeen != stalledAt {
		t.Fatalf("the answer = %+v, want ses_stalled unresponsive, last seen %d", info, stalledAt)
	}
}

// A role whose holder stopped answering keeps that holder's claim, however many times the role is
// looked up and whatever the reapers do, until the holder is gone. Each lookup answers the 404 it
// always did, now naming the holder's state and saying in so many words that it released nothing;
// the claim and its claimed_at are untouched afterwards, so the holder's own relaunch can take its
// role back with a soft claim rather than finding it unclaimed by whoever asked for it first.
//
// Five lookups and a role-reaper pass, because the release this replaces happened inside the
// lookup: one lookup used to be enough to lose the role, and a pass of the reaper used to end it
// on the session cache's word alone.
func TestLookupsAndAReaperPassLeaveAStalledHoldersRoleClaimExactlyAsItWas(t *testing.T) {
	d, now := stalledSessionsRig(t)
	const (
		role   = "reviewer"
		holder = "ses_stalled_holder"
	)
	stalledAt := register(t, d, holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	claimRoles(t, d.registry, holder, role)
	claimed, err := d.registry.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read the seeded claim: %v", err)
	}
	now.advance(90 * time.Second)

	for attempt := range 5 {
		recorder := httptest.NewRecorder()
		roleGetHandler(d).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("lookup %d: status %d, body %s; want 404", attempt, recorder.Code, recorder.Body.String())
		}
		var answer roleHolderError
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode lookup %d's answer %s: %v", attempt, recorder.Body.String(), err)
		}
		if answer.Reason != roleHolderLapsed || answer.Holder != holder || answer.HolderState != session.Unresponsive {
			t.Fatalf("lookup %d answered %+v, want holder_lapsed naming %s as unresponsive", attempt, answer, holder)
		}
		if answer.ClaimReleased == nil || *answer.ClaimReleased {
			t.Fatalf("lookup %d answered claim_released %v, want false: a lookup releases nothing", attempt, answer.ClaimReleased)
		}
		if answer.LastSeen == nil || *answer.LastSeen != stalledAt {
			t.Fatalf("lookup %d answered last_seen %v, want the holder's last registration %d", attempt, answer.LastSeen, stalledAt)
		}
	}

	live, _ := d.sessions.Windows()
	logger := logging.New("test")
	reaped, err := d.registry.ReapRoleClaims(func(sessionID string) bool {
		return roleHolderMayBeLive(logger, d.sessions, sessionID)
	}, live)
	if err != nil || reaped != 0 {
		t.Fatalf("the role reaper released %d claims, %v; want none while the holder is only unresponsive", reaped, err)
	}
	after, err := d.registry.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read the claim after the lookups and the pass: %v", err)
	}
	if after.HolderSessionID != holder || after.ClaimedAt != claimed.ClaimedAt {
		t.Fatalf("the claim after the lookups and the pass = %+v, want %+v, untouched", after, claimed)
	}
}

// A send to a session that stopped answering but receives on its own NATS subscription is taken
// and stored for it, naming the state it was taken for, because that is exactly what the session
// reads back when it comes round (POST /v1/replay). A send to a session past the gone horizon is
// refused, keeping the "no live session " prefix a sender routes around the pane on, and names
// the state and the registration the refusal was decided on.
func TestASendIsStoredForAStalledSessionAndRefusedForAGoneOne(t *testing.T) {
	d, now := stalledSessionsRig(t)
	const sessionID = "ses_stalled_recipient"
	registeredAt := register(t, d, sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	now.advance(90 * time.Second)

	send := func() *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		body := fmt.Sprintf(`{"target_session":%q,"source":"agent","source_session":"ses_sender","message":"are you there?"}`, sessionID)
		sendHandler(d, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(body)))
		return recorder
	}

	accepted := send()
	if accepted.Code != http.StatusOK {
		t.Fatalf("send to a stalled self-subscribed session: status %d, body %s; want 200", accepted.Code, accepted.Body.String())
	}
	var answer sendResponse
	if err := json.Unmarshal(accepted.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the send answer %s: %v", accepted.Body.String(), err)
	}
	if answer.RecipientState != session.Unresponsive || answer.LastSeen != registeredAt {
		t.Fatalf("the send answer = %+v, want recipient_state unresponsive and last_seen %d", answer, registeredAt)
	}

	now.advance(2 * time.Minute)
	refused := send()
	if refused.Code != http.StatusNotFound {
		t.Fatalf("send to a gone session: status %d, body %s; want 404", refused.Code, refused.Body.String())
	}
	var refusal noLiveSessionRefusal
	if err := json.Unmarshal(refused.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", refused.Body.String(), err)
	}
	if refusal.Error != "no live session "+sessionID {
		t.Fatalf("the refusal reads %q, want the kept prefix naming the session", refusal.Error)
	}
	if refusal.State != session.Gone || refusal.LastSeen != registeredAt {
		t.Fatalf("the refusal = %+v, want state gone and the session's last registration %d", refusal, registeredAt)
	}
}

// A listener that restarted after its reaper deleted a session's record remembers no last
// registration for it, so its refusal carries no `last_seen` at all. A sender then cannot tell a
// session this listener watched reach the gone horizon from one it has never heard of, and the
// two call for different acts: the first is dead, the second may be alive on another listener.
// The refusal says in its own text that the listener cannot name that time (dispatch://INFRA-392,
// A6). A session this rig holds no record and no remembered registration for is exactly the state
// a restart leaves.
func TestARefusalThatCannotNameTheLastRegistrationSaysSo(t *testing.T) {
	d, _ := stalledSessionsRig(t)
	const sessionID = "ses_reaped_before_the_restart"

	recorder := httptest.NewRecorder()
	body := fmt.Sprintf(`{"target_session":%q,"source":"agent","source_session":"ses_sender","message":"are you there?"}`, sessionID)
	sendHandler(d, logging.New("test")).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(body)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("send to a session the listener holds nothing for: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	var refusal noLiveSessionRefusal
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if !strings.HasPrefix(refusal.Error, "no live session "+sessionID) {
		t.Fatalf("the refusal reads %q, want the kept prefix naming the session", refusal.Error)
	}
	if refusal.LastSeen != 0 {
		t.Fatalf("the refusal names last_seen %d, so this listener can name the time; this test is about the refusal that cannot", refusal.LastSeen)
	}
	said := strings.ToLower(refusal.Error)
	names := strings.Contains(said, "last seen") || strings.Contains(said, "last registration") || strings.Contains(said, "last registered")
	unknown := strings.Contains(said, "unknown") || strings.Contains(said, "not know") || strings.Contains(said, "no record")
	if !names || !unknown {
		t.Fatalf("the refusal reads %q and says nothing about this listener not knowing when %s last registered", refusal.Error, sessionID)
	}
}

// /healthz says which maximum age the session bucket is keeping records for, and says it again
// after another process changed it: the registry settles the age on every reaper pass, so an
// operator who raises it past the gone horizon can read, from a running listener and with no
// restart, that the raise took. Without the field, a deployment whose bucket still drops records
// after five minutes looks exactly like one that keeps them for ninety.
func TestHealthzReportsTheSessionMaxAgeInUseAndFollowsAChangeWithinOnePass(t *testing.T) {
	d, _ := stalledSessionsRig(t)
	maxAge := func() string {
		t.Helper()
		value, ok := sessionHealthFields(d.sessions)["session_max_age"].(string)
		if !ok {
			t.Fatalf("/healthz carries no session_max_age: %+v", sessionHealthFields(d.sessions))
		}
		return value
	}
	if got := maxAge(); got != time.Hour.String() {
		t.Fatalf("session_max_age = %s, want the %s the bucket was opened with", got, time.Hour)
	}

	js, err := d.client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.SetKeyValueMaxAge(js, bus.SessionBucket, 90*time.Minute); err != nil {
		t.Fatalf("another process raising the bucket's maximum age: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.sessions.ReapSessions(ctx, session.SessionReaper{})
	if got, want := maxAge(), (90 * time.Minute).String(); got != want {
		t.Fatalf("session_max_age after one pass = %s, want the %s another process set", got, want)
	}
}
