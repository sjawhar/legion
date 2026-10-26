package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// publishedSender sends one message from source to target through the send route and returns the
// sender the published envelope carries, as the target's own subscription reads it, after checking
// the route's answer carries the same.
func publishedSender(t *testing.T, state *listenerDeps, source, target string) *contracts.EnvelopeSender {
	t.Helper()
	probe, err := state.client.Conn.SubscribeSync(contracts.AgentSubject(target))
	if err != nil {
		t.Fatalf("subscribe to %s: %v", target, err)
	}
	defer func() { _ = probe.Unsubscribe() }()
	if err := state.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	recorder := httptest.NewRecorder()
	sendHandler(state, logging.New("test")).ServeHTTP(recorder, postJSON("/v1/messages/send", sendBody(map[string]any{
		"source": "agent", "source_session": source, "target_session": target, "message": "who holds what?",
	})))
	if recorder.Code != http.StatusOK {
		t.Fatalf("send from %s: status %d, body %s; want 200", source, recorder.Code, recorder.Body.String())
	}
	var answered contracts.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode the send answer %s: %v", recorder.Body.String(), err)
	}
	msg, err := probe.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("read the envelope %s sent: %v", source, err)
	}
	var published contracts.Envelope
	if err := json.Unmarshal(msg.Data, &published); err != nil {
		t.Fatalf("decode the published envelope: %v", err)
	}
	if published.Sender == nil || published.Sender.SessionID != source {
		t.Fatalf("the published envelope's sender = %+v, want %s", published.Sender, source)
	}
	if answered.Sender == nil || !slices.Equal(answered.Sender.Roles, published.Sender.Roles) {
		t.Fatalf("the send answered sender %+v, but published %+v", answered.Sender, published.Sender)
	}
	return published.Sender
}

// sessionRows is GET /v1/sessions by session id.
func sessionRows(t *testing.T, registry *store.Registry, state *listenerDeps) map[string]sessionInfo {
	t.Helper()
	recorder := httptest.NewRecorder()
	sessionsHandler(registry, state.sessions).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
	}
	var rows []sessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode GET /v1/sessions: %v", err)
	}
	byID := map[string]sessionInfo{}
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	return byID
}

// A session holds a role exactly when the role's claim names it, and every place that says who
// holds a role says so from the claims: a row of GET /v1/sessions, GET /v1/registry/<session_id>
// (which Dispatch's route roster reads) and the sender a send stamps. A session whose record keeps
// the role's topic from a claim another session now holds is reported holding nothing, and the
// claim's holder holds the role though its record lacks the topic, or though it has no interest
// record at all.
func TestARoleIsHeldByTheSessionItsClaimNamesWhateverTheRecordsTopics(t *testing.T) {
	const (
		stale  = "ses_claims_stale_topic"
		holder = "ses_claims_holder_without_topic"
		bare   = "ses_claims_holder_without_record"
		target = "ses_claims_target"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t,
		map[string][]string{stale: {contracts.RoleTopicPrefix + "reviewer"}, holder: nil, target: nil},
		map[string]int{stale: 1, holder: 1, bare: 1, target: 1})
	// The claims go in as another listener's claim would, straight to the role bucket, so no record
	// gains a topic on the way.
	roles, err := client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	for role, session := range map[string]string{"reviewer": holder, "planner": bare} {
		value, err := json.Marshal(store.RoleClaim{HolderSessionID: session, ClaimedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode %s's claim: %v", role, err)
		}
		if _, err := roles.Put(role, value); err != nil {
			t.Fatalf("store %s's claim naming %s: %v", role, session, err)
		}
	}
	waitFor(t, 5*time.Second, "the claims to reach the registry's view", func() bool {
		a, errA := registry.RolesHeldBy(holder)
		b, errB := registry.RolesHeldBy(bare)
		return errA == nil && errB == nil && slices.Equal(a, []string{"reviewer"}) && slices.Equal(b, []string{"planner"})
	})
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	want := map[string][]string{stale: {}, holder: {"reviewer"}, bare: {"planner"}}

	rows := sessionRows(t, registry, state)
	if topics := rows[stale].Topics; !slices.Contains(topics, contracts.RoleTopicPrefix+"reviewer") {
		t.Fatalf("%s's row has topics %v; the test needs its stale role topic there", stale, topics)
	}
	if topics := rows[bare].Topics; len(topics) != 0 {
		t.Fatalf("%s's row has topics %v; the test needs it to have no interest record", bare, topics)
	}
	for session, roles := range want {
		if got := rows[session].Roles; !slices.Equal(got, roles) {
			t.Errorf("GET /v1/sessions reports %s holding %v, want %v", session, got, roles)
		}
		answer := sessionEntryRoute(t, state, session)
		if answer.Code != http.StatusOK {
			t.Fatalf("GET /v1/registry/%s: status %d, body %s; want 200", session, answer.Code, answer.Body.String())
		}
		if got := decodeSessionInfo(t, answer).Roles; !slices.Equal(got, roles) {
			t.Errorf("GET /v1/registry/%s reports roles %v, want %v", session, got, roles)
		}
		if got := publishedSender(t, state, session, target).Roles; !slices.Equal(got, roles) {
			t.Errorf("a send from %s stamps sender roles %v, want %v", session, got, roles)
		}
	}
}

// A claims view that cannot answer - here the role claim watcher ended when its bucket went away,
// and nothing replaced it - is not a session holding no roles. The two session routes answer the
// bus-unavailable 503 with Retry-After, never a row a roster would take for a role nobody holds. A
// send only stamps roles as a hint, so it still goes out, its sender carrying no roles.
func TestAClaimsViewThatCannotAnswerIsTheBusNotAnsweringOnTheSessionRoutes(t *testing.T) {
	const (
		sender = "ses_claims_view_sender"
		target = "ses_claims_view_target"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t,
		map[string][]string{sender: nil, target: nil},
		map[string]int{sender: 1, target: 1})
	claimRoles(t, registry, sender, "reviewer")
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	if got := publishedSender(t, state, sender, target).Roles; !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("control: a send from the claim's holder stamps roles %v, want [reviewer]", got)
	}

	if err := client.JS().DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	waitFor(t, 30*time.Second, "the role claim view to stop answering", func() bool {
		_, err := registry.RolesHeldBy(sender)
		return err != nil
	})

	for _, route := range []struct {
		path    string
		handler http.Handler
	}{
		{"/v1/sessions", sessionsHandler(registry, sessions)},
		{"/v1/registry/" + sender, sessionEntryHandler(state)},
	} {
		recorder := httptest.NewRecorder()
		route.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, route.path, nil))
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" ||
			!strings.Contains(recorder.Body.String(), busUnansweredPrefix) {
			t.Errorf("GET %s with a claims view that cannot answer: status %d, Retry-After %q, body %s; want the bus-unavailable 503",
				route.path, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
	}
	if got := publishedSender(t, state, sender, target).Roles; len(got) != 0 {
		t.Fatalf("a send whose claims view cannot answer stamps roles %v, want none", got)
	}
}

// timeOutRoleClaimScan puts registry's role claim view where a listener start leaves it when
// nats.go's idle timer ends the view's first scan of raw before the scan delivers any claim: the
// view is emptied, and the watcher feeding it is replaced by one whose first scan times out at
// once and that applies nothing. Package store's own tests drive the registry's real watcher
// through the same timeout; from here the registry's fields are reached the way api_test.go
// reaches the session registry's watcher.
func timeOutRoleClaimScan(t *testing.T, registry *store.Registry, raw natsgo.KeyValue) {
	t.Helper()
	fields := reflect.ValueOf(registry).Elem()
	field := fields.FieldByName("roleWatcher")
	watcher := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	watcher.Interface().(*kvwatch.Watcher).Stop()
	holders := fields.FieldByName("holders")
	reflect.NewAt(holders.Type(), unsafe.Pointer(holders.UnsafeAddr())).Elem().Set(reflect.ValueOf(map[string]string{}))
	timedOut := kvwatch.New("role claims", bus.KeyValue{KeyValue: kvwatchtest.TimingOut(raw, 0)}, func(natsgo.KeyValueEntry) {}, func() {})
	watcher.Set(reflect.ValueOf(timedOut))
	t.Cleanup(timedOut.Stop)
	timedOut.Start()
	waitFor(t, 5*time.Second, "the role claim scan to time out", timedOut.Ready)
}

// requireRolesOrUnanswered fails the test unless recorder answers 200 with the session's rows
// (rolesOf reads them) holding exactly want, or the bus-unavailable 503 with Retry-After: 1. It
// reports whether the answer was the 200.
func requireRolesOrUnanswered(t *testing.T, what string, recorder *httptest.ResponseRecorder, want []string, rolesOf func([]byte) []string) bool {
	t.Helper()
	switch recorder.Code {
	case http.StatusOK:
		if got := rolesOf(recorder.Body.Bytes()); !slices.Equal(got, want) {
			t.Fatalf("%s answered 200 with roles %v, want %v or the bus-unavailable 503: a holder reported without its role", what, got, want)
		}
		return true
	case http.StatusServiceUnavailable:
		if recorder.Header().Get("Retry-After") != "1" || !strings.Contains(recorder.Body.String(), busUnansweredPrefix) {
			t.Fatalf("%s answered 503 with Retry-After %q and body %s, want the bus-unavailable 503", what, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
		return false
	default:
		t.Fatalf("%s answered %d %s, want 200 with roles %v or the bus-unavailable 503", what, recorder.Code, recorder.Body.String(), want)
		return false
	}
}

// A role claim view whose first scan nats.go's idle timer ended is not complete, so it says
// nothing about who holds a role. The two session routes answer the bus-unavailable 503 with
// Retry-After: 1, never the claim's holder listed with no roles, and a send still goes out, its
// sender carrying no roles.
func TestARoleViewWhoseFirstScanTimedOutIsTheBusNotAnsweringOnTheSessionRoutes(t *testing.T) {
	const (
		holder = "ses_timed_out_view_holder"
		target = "ses_timed_out_view_target"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t,
		map[string][]string{holder: nil, target: nil},
		map[string]int{holder: 1, target: 1})
	claimRoles(t, registry, holder, "reviewer")
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	if got := decodeSessionInfo(t, sessionEntryRoute(t, state, holder)).Roles; !slices.Equal(got, []string{"reviewer"}) {
		t.Fatalf("control: GET /v1/registry/%s reports roles %v, want [reviewer]", holder, got)
	}
	raw, err := client.JS().KeyValue(bus.RoleClaimBucket)
	if err != nil {
		t.Fatalf("open the role bucket: %v", err)
	}
	timeOutRoleClaimScan(t, registry, raw)

	for _, route := range []struct {
		path    string
		handler http.Handler
	}{
		{"/v1/sessions", sessionsHandler(registry, sessions)},
		{"/v1/registry/" + holder, sessionEntryHandler(state)},
	} {
		recorder := httptest.NewRecorder()
		route.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, route.path, nil))
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" ||
			!strings.Contains(recorder.Body.String(), busUnansweredPrefix) {
			t.Errorf("GET %s with a role claim view whose first scan timed out: status %d, Retry-After %q, body %s; want the bus-unavailable 503",
				route.path, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
	}
	if got := publishedSender(t, state, holder, target).Roles; len(got) != 0 {
		t.Fatalf("a send whose role claim view timed out stamps roles %v, want none", got)
	}
}

// A recreated role bucket empties the role claim view, and the refill that follows delivers the
// claims one at a time. Until it has delivered them all, GET /v1/registry/<session_id> and GET
// /v1/sessions answer the holder's roles or the bus-unavailable 503 with Retry-After: 1, never the
// holder with no roles. The holder's claim is the last of thousands, so the routes are asked while
// the refill runs.
func TestARecreatedRoleBucketIsNeverAnsweredAsAHolderWithoutItsRole(t *testing.T) {
	const (
		holder = "ses_recreated_roles_holder"
		filler = "ses_recreated_roles_filler"
		target = "ses_recreated_roles_target"
		claims = 3000
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t,
		map[string][]string{holder: nil, target: nil},
		map[string]int{holder: 1, target: 1})
	claimRoles(t, registry, holder, "reviewer")
	state := &listenerDeps{client: client, registry: registry, sessions: sessions}

	js := client.JS()
	if err := js.DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	recreated := testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	put := func(role, session string) {
		value, err := json.Marshal(store.RoleClaim{HolderSessionID: session, ClaimedAt: time.Now().UnixMilli()})
		if err != nil {
			t.Fatalf("encode %s's claim: %v", role, err)
		}
		if _, err := recreated.Put(role, value); err != nil {
			t.Fatalf("store %s's claim: %v", role, err)
		}
	}
	for i := range claims {
		put(fmt.Sprintf("filler-%04d", i), filler)
	}
	put("reviewer", holder)
	if err := registry.Rewatch(client.Conn); err != nil {
		t.Fatalf("Rewatch: %v", err)
	}

	want := []string{"reviewer"}
	entryRoles := func(body []byte) []string {
		var info sessionInfo
		if err := json.Unmarshal(body, &info); err != nil {
			t.Fatalf("decode GET /v1/registry/%s: %v", holder, err)
		}
		return info.Roles
	}
	listedRoles := func(body []byte) []string {
		var rows []sessionInfo
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode GET /v1/sessions: %v", err)
		}
		for _, row := range rows {
			if row.SessionID == holder {
				return row.Roles
			}
		}
		t.Fatalf("GET /v1/sessions answered 200 without %s: %s", holder, body)
		return nil
	}
	unanswered := 0
	for deadline := time.Now().Add(30 * time.Second); ; {
		entry := sessionEntryRoute(t, state, holder)
		entryAnswered := requireRolesOrUnanswered(t, "GET /v1/registry/"+holder, entry, want, entryRoles)
		list := httptest.NewRecorder()
		sessionsHandler(registry, sessions).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
		listAnswered := requireRolesOrUnanswered(t, "GET /v1/sessions", list, want, listedRoles)
		if entryAnswered && listAnswered {
			break
		}
		unanswered++
		if time.Now().After(deadline) {
			t.Fatal("the session routes still answer 503 30 s after the rewatch")
		}
		time.Sleep(time.Millisecond)
	}
	t.Logf("the session routes answered the bus-unavailable 503 %d times while the refill ran", unanswered)
	if got := publishedSender(t, state, holder, target).Roles; !slices.Equal(got, want) {
		t.Fatalf("after the refill a send from %s stamps roles %v, want %v", holder, got, want)
	}
}
