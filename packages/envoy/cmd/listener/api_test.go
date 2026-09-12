package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

type fakeStreamInfo struct {
	info           *nats.StreamInfo
	err            error
	stream         string
	subjectsFilter string
	calls          int
}

func (f *fakeStreamInfo) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	f.calls++
	f.stream = stream
	if len(opts) > 0 {
		if request, ok := opts[0].(*nats.StreamInfoRequest); ok {
			f.subjectsFilter = request.SubjectsFilter
		}
	}
	return f.info, f.err
}

type delayedStreamInfo struct {
	delay time.Duration
}

func (f delayedStreamInfo) StreamInfo(_ string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	time.Sleep(f.delay)
	return &nats.StreamInfo{}, nil
}

func TestSendHandler_StampsSenderAndReturnsRecipient(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, map[string][]string{
		"ses_source": {
			"notifications.role.reviewer",
			"notifications.role.maintainer",
		},
	}, map[string]int{
		"ses_source": 1,
		"ses_target": 1,
	})
	if err := sessions.Put("ses_source", session.SessionEntry{
		Port:      1,
		MachineID: "test-machine",
		Dir:       "/test/ses_source",
		Title:     "Release sender",
	}); err != nil {
		t.Fatalf("register source session: %v", err)
	}

	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	message := strings.Repeat("a", 161) + "\n\nsecond paragraph\n\nthird paragraph"
	requestBody, err := json.Marshal(map[string]interface{}{
		"source_session": "ses_source",
		"target_session": "ses_target",
		"message":        message,
		"in_reply_to":    "evt-parent",
		"supersedes":     "evt-old",
		"urgency":        "high",
		"expects_reply":  "required",
		"expires_at":     1757221200000,
	})
	if err != nil {
		t.Fatalf("marshal send request: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(string(requestBody)))

	sendHandler(&state).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("send status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	t.Logf("send success: %s", recorder.Body.String())
	responseBody := recorder.Body.Bytes()
	var response contracts.Envelope
	if err := json.Unmarshal(responseBody, &response); err != nil {
		t.Fatalf("decode send envelope: %v", err)
	}
	var metadata struct {
		Recipient string `json:"recipient"`
	}
	if err := json.Unmarshal(responseBody, &metadata); err != nil {
		t.Fatalf("decode send recipient: %v", err)
	}
	if metadata.Recipient != "ses_target" {
		t.Fatalf("recipient = %q, want ses_target", metadata.Recipient)
	}
	if response.PayloadSummary != strings.Repeat("a", 159)+"…" {
		t.Fatalf("payload_summary = %q, want 160-rune summary with ellipsis", response.PayloadSummary)
	}
	if response.Payload != message {
		t.Fatalf("payload = %q, want original message", response.Payload)
	}
	if response.InReplyTo != "evt-parent" || response.Supersedes != "evt-old" || response.Urgency != "high" || response.ExpectsReply != "required" {
		t.Fatalf("correlation fields = %+v", response)
	}
	if response.ExpiresAt == nil || *response.ExpiresAt != 1757221200000 {
		t.Fatalf("expires_at = %v, want 1757221200000", response.ExpiresAt)
	}
	if response.Sender == nil {
		t.Fatal("sender stamp is missing")
	}
	if response.Sender.SessionID != "ses_source" || response.Sender.Machine != "test-machine" || response.Sender.Cwd != "/test/ses_source" || response.Sender.Title != "Release sender" {
		t.Fatalf("sender = %+v", response.Sender)
	}
	if got := strings.Join(response.Sender.Roles, ","); got != "maintainer,reviewer" {
		t.Fatalf("sender roles = %q, want maintainer,reviewer", got)
	}
}

func TestMessageHandlersRejectInvalidEnums(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})

	cases := []struct {
		name  string
		path  string
		body  string
		want  string
		field string
	}{
		{
			name:  "send urgency",
			path:  "/v1/messages/send",
			body:  `{"target_session":"ses_target","message":"hello","urgency":"critical"}`,
			want:  "urgency must be one of low, med, high, blocking",
			field: "urgency",
		},
		{
			name:  "publish expects reply",
			path:  "/v1/messages/publish",
			body:  `{"topic":"notifications.github.example-org.example-repo.pr.1","message":"hello","expects_reply":"soon"}`,
			want:  "expects_reply must be one of none, optional, required",
			field: "expects_reply",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.path == "/v1/messages/send" {
				sendHandler(&state).ServeHTTP(recorder, request)
			} else {
				publishHandler(&state).ServeHTTP(recorder, request)
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Error    string   `json:"error"`
				Expected []string `json:"expected"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if response.Error != tc.want || len(response.Expected) != 1 || response.Expected[0] != tc.field {
				t.Fatalf("error response = %+v, want field %q", response, tc.field)
			}
		})
	}
}

func TestMessageHandlersRejectBlankMessage(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})

	cases := []struct {
		name string
		path string
		body string
	}{
		{
			name: "send",
			path: "/v1/messages/send",
			body: `{"target_session":"ses_target","message":"   "}`,
		},
		{
			name: "publish",
			path: "/v1/messages/publish",
			body: `{"topic":"notifications.github.example-org.example-repo.pr.1","message":"   "}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.path == "/v1/messages/send" {
				sendHandler(&state).ServeHTTP(recorder, request)
			} else {
				publishHandler(&state).ServeHTTP(recorder, request)
			}

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			t.Logf("%s blank message: %s", tc.name, recorder.Body.String())
			if body := recorder.Body.String(); body != "{\"error\":\"message is required\",\"expected\":[\"message\"]}\n" {
				t.Fatalf("body = %q", body)
			}
		})
	}
}

func TestPublishHandler_ReportsRoleHolderOnlyWhenLive(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_holder", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	handler := publishHandler(&state)

	t.Run("unheld role returns 404", func(t *testing.T) {
		roleProbe, err := client.Conn.SubscribeSync("notifications.role.unheld")
		if err != nil {
			t.Fatalf("subscribe to unheld role: %v", err)
		}
		t.Cleanup(func() { _ = roleProbe.Unsubscribe() })
		exceptionProbe, err := client.Conn.SubscribeSync("notifications.envoy.exceptions.notifications.role.unheld")
		if err != nil {
			t.Fatalf("subscribe to unheld-role exception lane: %v", err)
		}
		t.Cleanup(func() { _ = exceptionProbe.Unsubscribe() })
		if err := client.Conn.Flush(); err != nil {
			t.Fatalf("flush unheld-role subscriptions: %v", err)
		}

		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.role.unheld","message":"please review"}`))
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
		}
		t.Logf("publish unheld role: %s", recorder.Body.String())
		if body := recorder.Body.String(); body != "{\"error\":\"no holder for role unheld\"}\n" {
			t.Fatalf("body = %q", body)
		}
		assertNoMessage := func(name string, probe *nats.Subscription) {
			t.Helper()
			message, err := probe.NextMsg(100 * time.Millisecond)
			if err == nil {
				t.Fatalf("unexpected %s message: %s", name, message.Data)
			}
			if !errors.Is(err, nats.ErrTimeout) {
				t.Fatalf("wait for %s message: %v", name, err)
			}
		}
		assertNoMessage("unheld role", roleProbe)
		assertNoMessage("unheld-role exception", exceptionProbe)
	})

	t.Run("live holder is returned", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"notifications.role.reviewer","message":"please review"}`))
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
		}
		t.Logf("publish live-held role: %s", recorder.Body.String())
		var response struct {
			Holder string `json:"holder"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatalf("decode publish holder: %v", err)
		}
		if response.Holder != "ses_holder" {
			t.Fatalf("holder = %q, want ses_holder", response.Holder)
		}
	})

	t.Run("stale holder returns 404 from publish and lookup", func(t *testing.T) {
		if err := sessions.Delete("ses_holder"); err != nil {
			t.Fatalf("delete holder session: %v", err)
		}
		publishRecorder := httptest.NewRecorder()
		handler.ServeHTTP(publishRecorder, httptest.NewRequest(
			http.MethodPost,
			"/v1/messages/publish",
			strings.NewReader(`{"topic":"notifications.role.reviewer","message":"please review"}`),
		))
		if publishRecorder.Code != http.StatusNotFound {
			t.Fatalf("publish status = %d, want 404; body = %s", publishRecorder.Code, publishRecorder.Body.String())
		}
		roleRecorder := httptest.NewRecorder()
		roleGetHandler(&state).ServeHTTP(roleRecorder, httptest.NewRequest(http.MethodGet, "/v1/roles/reviewer", nil))
		if roleRecorder.Code != http.StatusNotFound {
			t.Fatalf("role lookup status = %d, want 404; body = %s", roleRecorder.Code, roleRecorder.Body.String())
		}
	})
}

func TestRoleGetHandlerReturnsLiveHolder(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_holder", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	mux := http.NewServeMux()
	registerV1Routes(mux, &state, "test-machine", logging.New("test"))

	t.Run("live", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/reviewer", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Role     string `json:"role"`
			Holder   string `json:"holder"`
			LastSeen int64  `json:"last_seen"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatalf("decode role response: %v", err)
		}
		if response.Role != "reviewer" || response.Holder != "ses_holder" || response.LastSeen == 0 {
			t.Fatalf("role response = %+v", response)
		}
	})

	t.Run("unheld", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/unheld", nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); body != "{\"error\":\"no holder for role unheld\"}\n" {
			t.Fatalf("body = %q", body)
		}
	})
}
func TestRoleClaimRestoresWhileHolderIsLiveAndDropsAfterTTL(t *testing.T) {
	client := setupPublishTestClient(t)
	if err := client.JS().DeleteKeyValue(session.SessionBucket); err != nil &&
		!errors.Is(err, nats.ErrBucketNotFound) && !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("reset session bucket TTL: %v", err)
	}
	const (
		sessionID = "ses_role_restart"
		role      = "restart-survivor"
	)
	firstRegistry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open first role registry: %v", err)
	}
	firstSessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(1),
		session.WithSessionTTL(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("open first session registry: %v", err)
	}
	if err := firstSessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register role holder: %v", err)
	}
	var firstState atomic.Pointer[listenerDeps]
	firstState.Store(&listenerDeps{client: client, registry: firstRegistry, sessions: firstSessions})
	claim := httptest.NewRecorder()
	roleSetHandler(&firstState, "test-machine").ServeHTTP(
		claim,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/roles/set",
			strings.NewReader(`{"session_id":"ses_role_restart","role":"restart-survivor","soft":true,"previous_session_id":"ses_previous"}`),
		),
	)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim role: status = %d, body = %s", claim.Code, claim.Body.String())
	}
	persisted, err := firstRegistry.RoleClaim(role)
	if err != nil {
		t.Fatalf("read persisted role claim: %v", err)
	}
	if persisted.HolderSessionID != sessionID || persisted.ClaimedAt <= 0 ||
		persisted.PreviousSessionID != "ses_previous" {
		t.Fatalf("persisted claim = %+v", persisted)
	}

	restoredRegistry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("restore role registry: %v", err)
	}
	restoredSessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("restore session registry: %v", err)
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restoredSessions.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("restore session cache: %v", err)
	}
	var restoredState atomic.Pointer[listenerDeps]
	restoredState.Store(&listenerDeps{client: client, registry: restoredRegistry, sessions: restoredSessions})
	get := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		roleGetHandler(&restoredState).ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil),
		)
		return recorder
	}

	if response := get(); response.Code != http.StatusOK {
		t.Fatalf("restored live role: status = %d, body = %s", response.Code, response.Body.String())
	}

	if err := restoredSessions.Delete(sessionID); err != nil {
		t.Fatalf("expire restored holder: %v", err)
	}
	if response := get(); response.Code != http.StatusNotFound {
		t.Fatalf("unregistered restored role: status = %d, want 404; body = %s", response.Code, response.Body.String())
	}
	if holder, err := restoredRegistry.RoleHolder(role); err != nil || holder != sessionID {
		t.Fatalf("restored grace claim = %q, %v; want %q", holder, err, sessionID)
	}

	deadline := time.Now().Add(time.Second)
	for {
		if response := get(); response.Code != http.StatusNotFound {
			t.Fatalf("expired role status = %d, want 404; body = %s", response.Code, response.Body.String())
		}
		holder, err := restoredRegistry.RoleHolder(role)
		if err != nil {
			t.Fatalf("read expired role claim: %v", err)
		}
		if holder == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired role claim = %q, want removed", holder)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRoleSetHandlerSoftClaim(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	for _, id := range []string{"ses_live", "ses_resumer", "ses_heir"} {
		if err := sessions.Put(id, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	mux := http.NewServeMux()
	registerV1Routes(mux, &state, "test-machine", logging.New("test"))
	post := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
		return recorder
	}

	t.Run("live holder is refused with 409 naming the holder", func(t *testing.T) {
		if rec := post(`{"session_id":"ses_live","role":"sre"}`); rec.Code != http.StatusOK {
			t.Fatalf("seed hard claim: status = %d, body = %s", rec.Code, rec.Body.String())
		}
		rec := post(`{"session_id":"ses_resumer","role":"sre","soft":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
		var response struct {
			Error  string `json:"error"`
			Role   string `json:"role"`
			Holder string `json:"holder"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decode 409 body: %v", err)
		}
		if response.Role != "sre" || response.Holder != "ses_live" || response.Error == "" {
			t.Fatalf("409 body = %+v", response)
		}
		holder, err := registry.RoleHolder("sre")
		if err != nil || holder != "ses_live" {
			t.Fatalf("holder after refused soft claim = %q, %v; want ses_live", holder, err)
		}
	})

	t.Run("dead holder is superseded", func(t *testing.T) {
		// ses_live's session entry ages out (5m TTL in production); here it is
		// deleted, which is what the listener observes either way.
		if err := sessions.Delete("ses_live"); err != nil {
			t.Fatalf("expire holder session: %v", err)
		}
		rec := post(`{"session_id":"ses_heir","role":"sre","soft":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		holder, err := registry.RoleHolder("sre")
		if err != nil || holder != "ses_heir" {
			t.Fatalf("holder after soft claim over dead holder = %q, %v; want ses_heir", holder, err)
		}
	})

	t.Run("hard claim still takes a live holder's role", func(t *testing.T) {
		rec := post(`{"session_id":"ses_resumer","role":"sre"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		holder, err := registry.RoleHolder("sre")
		if err != nil || holder != "ses_resumer" {
			t.Fatalf("holder after hard claim = %q, %v; want ses_resumer", holder, err)
		}
	})

	t.Run("a live predecessor is superseded when the claimant declares it", func(t *testing.T) {
		// ses_resumer holds the role and is still live (a fork's parent keeps
		// heartbeating). The forked child names it as its predecessor.
		if err := sessions.Put("ses_fork_child", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
			t.Fatalf("register child: %v", err)
		}
		rec := post(`{"session_id":"ses_fork_child","role":"sre","soft":true,"previous_session_id":"ses_resumer"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		holder, err := registry.RoleHolder("sre")
		if err != nil || holder != "ses_fork_child" {
			t.Fatalf("holder after child's soft claim = %q, %v; want ses_fork_child", holder, err)
		}
	})

	t.Run("a live holder that is not the declared predecessor is still refused", func(t *testing.T) {
		// ses_resumer now resumes its own transcript, which still records the
		// role, but the live child holds it and ses_resumer continues nobody.
		rec := post(`{"session_id":"ses_resumer","role":"sre","soft":true,"previous_session_id":"ses_heir"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
		}
		holder, err := registry.RoleHolder("sre")
		if err != nil || holder != "ses_fork_child" {
			t.Fatalf("holder after refused claim = %q, %v; want ses_fork_child", holder, err)
		}
	})
}

func TestSubscribeHandlerWarnsWhenGitHubRepositoryIsUnwired(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	inspector := &fakeStreamInfo{info: &nats.StreamInfo{}}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		streamName: "notifications",
		streamInfo: inspector,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(`{
		"session_id":"ses_subscriber",
		"self_subscribed":true,
		"topics":["notifications.github.example-org.example-repo.pr","notifications.github.example-org.example-repo.pr.>"]
	}`))
	subscribeHandler(&state, "test-machine", logging.New("test")).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	t.Logf("subscribe unwired repository: %s", recorder.Body.String())
	var response struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode subscribe response: %v", err)
	}
	const want = "no GitHub event for example-org/example-repo in the stream's retention window; is the App installed there?"
	if len(response.Warnings) != 1 || response.Warnings[0] != want {
		t.Fatalf("warnings = %#v, want [%q]", response.Warnings, want)
	}
	if inspector.stream != "notifications" || inspector.subjectsFilter != "notifications.github.example-org.example-repo.>" {
		t.Fatalf("stream inspection = stream %q subjects_filter %q", inspector.stream, inspector.subjectsFilter)
	}
	if inspector.calls != 1 {
		t.Fatalf("stream inspector calls = %d, want one per repository", inspector.calls)
	}
}

func TestSubscribeHandlerWarnsWhenGitHubWiringLookupFails(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	inspector := &fakeStreamInfo{err: errors.New("nats unavailable")}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		streamName: "notifications",
		streamInfo: inspector,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(`{
		"session_id":"ses_subscriber",
		"self_subscribed":true,
		"topics":["notifications.github.example-org.example-repo.pr.>"]
	}`))
	subscribeHandler(&state, "test-machine", logging.New("test")).ServeHTTP(recorder, request)

	var response struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode subscribe response: %v", err)
	}
	const want = "could not verify GitHub wiring for example-org/example-repo: nats unavailable"
	if len(response.Warnings) != 1 || response.Warnings[0] != want {
		t.Fatalf("warnings = %#v, want [%q]", response.Warnings, want)
	}
}

func TestUnwiredRepositoryWarningSkipsWildcardRepositorySegment(t *testing.T) {
	inspector := &fakeStreamInfo{info: &nats.StreamInfo{}}
	deps := &listenerDeps{streamName: "notifications", streamInfo: inspector}
	if warning := unwiredRepositoryWarning(context.Background(), deps, "notifications.github.*.example-repo.pr.>", logging.New("test")); warning != "" {
		t.Fatalf("warning = %q, want no warning for invalid repository segment", warning)
	}
	if inspector.calls != 0 {
		t.Fatalf("stream inspector calls = %d, want zero for invalid repository segment", inspector.calls)
	}
}

func TestSubscribeHandlerDoesNotBlockOnUnwiredRepositoryCheck(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		streamName: "notifications",
		streamInfo: delayedStreamInfo{delay: 2 * time.Second},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(`{
		"session_id":"ses_subscriber",
		"self_subscribed":true,
		"topics":["notifications.github.example-org.example-repo.pr.>"]
	}`))
	start := time.Now()
	subscribeHandler(&state, "test-machine", logging.New("test")).ServeHTTP(recorder, request)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("subscribe took %s, want the advisory check to finish within one second", elapsed)
	}
	t.Logf("timed unwired-repository response after %s: %s", elapsed, recorder.Body.String())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode subscribe response: %v", err)
	}
	const want = "could not verify GitHub wiring for example-org/example-repo: context deadline exceeded"
	if len(response.Warnings) != 1 || response.Warnings[0] != want {
		t.Fatalf("warnings = %#v, want [%q]", response.Warnings, want)
	}
}

func TestSubscribeHandlerFailsWhenSessionRegistryPutFails(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	routeClient, err := bus.Connect([]string{sharedListenerTestNATSURI(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("open session registry connection: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(routeClient.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	routeClient.Conn.Close()

	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(`{
		"session_id":"ses_unroutable",
		"port":34751,
		"topics":["notifications.github.example-org.example-repo.pr.>"]
	}`))
	subscribeHandler(&state, "test-machine", logging.New("test")).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"error\":\"session registry unavailable\"}\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestUnsubscribeRequiresSessionIDAndReturnsRemovedTopics(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, map[string][]string{
		"ses_subscriber": {"notifications.github.example-org.example-repo.pr.>"},
	}, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	mux := http.NewServeMux()
	registerV1Routes(mux, &state, "test-machine", logging.New("test"))

	t.Run("missing session id", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/unsubscribe", strings.NewReader(`{"topics":["notifications.github.example-org.example-repo.pr.>"]}`)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
		}
		t.Logf("unsubscribe missing session_id: %s", recorder.Body.String())
		if body := recorder.Body.String(); body != "{\"error\":\"session_id is required\",\"expected\":[\"session_id\"]}\n" {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("success reports removed topics", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/unsubscribe", strings.NewReader(`{"session_id":"ses_subscriber","topics":["notifications.github.example-org.example-repo.pr.>"]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); body != "{\"removed\":[\"notifications.github.example-org.example-repo.pr.\\u003e\"]}\n" {
			t.Fatalf("body = %q", body)
		}
	})
}
func TestUnsubscribeAllReleasesRoleClaims(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	const (
		sessionID = "ses_unsubscribe_role"
		role      = "legion-controller"
	)
	if _, err := registry.SetRole(sessionID, "test-machine", role, false); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	mux := http.NewServeMux()
	registerV1Routes(mux, &state, "test-machine", logging.New("test"))

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost,
		"/v1/interests/unsubscribe",
		strings.NewReader(`{"session_id":"ses_unsubscribe_role","topics":[]}`),
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	holder, err := registry.RoleHolder(role)
	if err != nil {
		t.Fatalf("RoleHolder: %v", err)
	}
	if holder != "" {
		t.Fatalf("role holder = %q, want removed session", holder)
	}
}

func TestSessionsHandlerFiltersAndReportsRoles(t *testing.T) {
	registry, sessions := setupSessionsTest(t, map[string][]string{
		"ses_match": {"notifications.role.reviewer", "notifications.role.maintainer"},
		"ses_other": {"notifications.role.observer"},
	}, map[string]int{
		"ses_match": 1,
		"ses_other": 1,
	})
	if err := sessions.Put("ses_match", session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/work/example", Title: "Release work"}); err != nil {
		t.Fatalf("set matching session: %v", err)
	}
	if err := sessions.Put("ses_other", session.SessionEntry{Port: 1, MachineID: "test-machine", Dir: "/work/other", Title: "Other work"}); err != nil {
		t.Fatalf("set nonmatching session: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions?dir=example&title=Release", nil)
	sessionsHandler(registry, sessions).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response []struct {
		SessionID string   `json:"session_id"`
		Roles     []string `json:"roles"`
		LastSeen  int64    `json:"last_seen"`
		UpdatedAt int64    `json:"updated_at"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode sessions response: %v", err)
	}
	if len(response) != 1 || response[0].SessionID != "ses_match" {
		t.Fatalf("sessions = %+v", response)
	}
	if got := strings.Join(response[0].Roles, ","); got != "maintainer,reviewer" {
		t.Fatalf("roles = %q, want maintainer,reviewer", got)
	}
	if response[0].LastSeen == 0 || response[0].LastSeen != response[0].UpdatedAt {
		t.Fatalf("last_seen = %d, updated_at = %d", response[0].LastSeen, response[0].UpdatedAt)
	}
}

func TestSendHandler_StampsInterestSenderWithoutSessionEntry(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, map[string][]string{
		"ses_source": {
			"notifications.role.reviewer",
			"notifications.role.maintainer",
		},
	}, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(`{
		"source_session":"ses_source",
		"target_session":"ses_target",
		"message":"review this"
	}`))
	sendHandler(&state).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response contracts.Envelope
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	if response.Sender == nil || response.Sender.Title != "" {
		t.Fatalf("sender = %+v, want stamp without title", response.Sender)
	}
	if response.Sender.Machine != "test-machine" || response.Sender.Cwd != "/test/ses_source" {
		t.Fatalf("sender = %+v", response.Sender)
	}
	if got := strings.Join(response.Sender.Roles, ","); got != "maintainer,reviewer" {
		t.Fatalf("roles = %q, want maintainer,reviewer", got)
	}
}

func TestPublishHandler_SuppliedPayloadWins(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	payload := `{"kind":"structured"}`
	requestBody, err := json.Marshal(map[string]string{
		"topic":   "notifications.github.example-org.example-repo.pr.1",
		"message": "first line\nsecond line",
		"payload": payload,
	})
	if err != nil {
		t.Fatalf("marshal publish request: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(string(requestBody)))
	publishHandler(&state).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response contracts.Envelope
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if response.PayloadSummary != "first line" || response.Payload != payload {
		t.Fatalf("published payload = summary %q, payload %q", response.PayloadSummary, response.Payload)
	}
}

func TestPublishHandlerPreservesAllOptionalMessageFields(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	recorder := httptest.NewRecorder()
	publishHandler(&state).ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPost,
		"/v1/messages/publish",
		strings.NewReader(`{
			"topic":"notifications.github.example-org.example-repo.pr.1",
			"message":"first paragraph\n\nsecond paragraph",
			"in_reply_to":"evt_parent",
			"supersedes":"evt_old",
			"urgency":"blocking",
			"expects_reply":"required"
		}`),
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response contracts.Envelope
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if response.InReplyTo != "evt_parent" ||
		response.Supersedes != "evt_old" ||
		response.Urgency != "blocking" ||
		response.ExpectsReply != "required" {
		t.Fatalf("optional fields = %+v", response)
	}
}

func TestRegisterV1Routes_UnknownRouteReturnsJSONError(t *testing.T) {
	var state atomic.Pointer[listenerDeps]
	mux := http.NewServeMux()
	registerV1Routes(mux, &state, "test-machine", logging.New("test"))

	for _, path := range []string{"/v1", "/v1/not-a-route"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}
			if body := recorder.Body.String(); body != "{\"error\":\"not found\"}\n" {
				t.Fatalf("body = %q", body)
			}
		})
	}
}
func TestMessageHandlersRejectPresentEmptyOptionalFields(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	for _, tc := range []struct {
		path, body, field string
	}{
		{"/v1/messages/send", `{"target_session":"ses_target","message":"hello","in_reply_to":""}`, "in_reply_to"},
		{"/v1/messages/send", `{"target_session":"ses_target","message":"hello","supersedes":""}`, "supersedes"},
		{"/v1/messages/publish", `{"topic":"notifications.github.example-org.example-repo.pr.1","message":"hello","urgency":""}`, "urgency"},
		{"/v1/messages/publish", `{"topic":"notifications.github.example-org.example-repo.pr.1","message":"hello","expects_reply":""}`, "expects_reply"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.path == "/v1/messages/send" {
				sendHandler(&state).ServeHTTP(rr, req)
			} else {
				publishHandler(&state).ServeHTTP(rr, req)
			}
			var response apiError
			_ = json.NewDecoder(rr.Body).Decode(&response)
			if rr.Code != http.StatusBadRequest || len(response.Expected) != 1 || response.Expected[0] != tc.field {
				t.Fatalf("status=%d response=%+v, want expected %q", rr.Code, response, tc.field)
			}
		})
	}
}

func TestMessageHandlersUseFirstNonEmptyLine(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages/send", `{"target_session":"ses_target","message":"\n  \nFirst paragraph.\n\nSecond paragraph."}`},
		{"/v1/messages/publish", `{"topic":"notifications.github.example-org.example-repo.pr.1","message":"\n  \nFirst paragraph.\n\nSecond paragraph."}`},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		if tc.path == "/v1/messages/send" {
			sendHandler(&state).ServeHTTP(rr, req)
		} else {
			publishHandler(&state).ServeHTTP(rr, req)
		}
		var envelope contracts.Envelope
		_ = json.NewDecoder(rr.Body).Decode(&envelope)
		if rr.Code != http.StatusOK || envelope.PayloadSummary != "First paragraph." || envelope.Payload == "" {
			t.Fatalf("%s response = %d %+v", tc.path, rr.Code, envelope)
		}
	}
}
