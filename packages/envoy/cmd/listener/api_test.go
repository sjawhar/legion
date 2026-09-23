package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
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

type racingRoleClaimResolver struct {
	*store.Registry

	beforeFirstRelease func()
	releaseCalls       int
}

func (r *racingRoleClaimResolver) ReleaseExpiredRoleClaim(role, sessionID string, ttl time.Duration) (store.ExpiredRoleClaimRelease, error) {
	r.releaseCalls++
	if r.releaseCalls == 1 {
		r.beforeFirstRelease()
	}
	return r.Registry.ReleaseExpiredRoleClaim(role, sessionID, ttl)
}

// stalledStreamInfo is a stream-info lookup that never answers until the test ends, like a
// JetStream API that has stopped responding. It records how far away each lookup's deadline was
// when the lookup was issued.
type stalledStreamInfo struct {
	deadlines chan time.Duration
	release   chan struct{}
}

func (f stalledStreamInfo) StreamInfo(_ string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	for _, opt := range opts {
		if ctx, ok := opt.(nats.ContextOpt); ok {
			if deadline, ok := ctx.Deadline(); ok {
				f.deadlines <- time.Until(deadline)
			}
		}
	}
	<-f.release
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

// TestDispatchClientSendUsesListenerWireContract keeps Dispatch's client and the
// listener's real send handler on the same JSON boundary. In particular, payload
// is an opaque JSON string on this HTTP API, rather than an embedded JSON value.
func TestDispatchClientSendUsesListenerWireContract(t *testing.T) {
	publisher := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/ses_target",
		Title: "planner", Capabilities: []string{"btw"},
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: publisher, registry: registry, sessions: sessions})

	var wire string
	handler := sendHandler(&state)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("capture Dispatch send: %v", err)
		}
		wire = string(body)
		r.Body = io.NopCloser(strings.NewReader(wire))
		handler.ServeHTTP(w, r)
	}))
	defer listener.Close()
	payloadBytes, err := os.ReadFile("../../../contracts/fixtures/dispatch-targeted-delivery.json")
	if err != nil {
		t.Fatalf("read targeted delivery fixture: %v", err)
	}
	payload := string(payloadBytes)

	result, err := dispatchenvoy.New(listener.URL).Send(context.Background(), dispatchenvoy.SendInput{
		TargetSession:  "ses_target",
		Message:        "Can this ship?",
		Payload:        json.RawMessage(payload),
		IdempotencyKey: "message-1:1",
		ExpectsReply:   "required",
	})
	if err != nil {
		t.Fatalf("Dispatch send through listener: %v", err)
	}
	if result.Recipient != "ses_target" || result.EnvelopeID == "" {
		t.Fatalf("send result = %#v, want listener envelope for ses_target", result)
	}
	var request struct {
		Payload      string  `json:"payload"`
		Urgency      *string `json:"urgency"`
		ExpectsReply string  `json:"expects_reply"`
	}
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatalf("decode captured Dispatch wire request: %v", err)
	}
	if request.Payload != payload || request.Urgency != nil || request.ExpectsReply != "required" {
		t.Fatalf("captured Dispatch wire request = %s", wire)
	}
	t.Logf("targeted Dispatch send wire: %s", wire)
}

// TestSendHandlerRefusesTargetThatDoesNotAdvertiseFrameMode proves the listener refuses a
// send when the TARGETED FRAME embedded in payload's own delivery.mode names a mode the
// target session does not advertise -- reading the mode a caller actually asks the receiver
// to execute, not a separate, omittable top-level hint a caller could leave off (or set to a
// different, still-advertised mode) to bypass the guard. There is no top-level delivery field
// on the wire at all; both of a review reproducer's bypass cases -- an omitted hint, and a
// hint that disagrees with the frame -- are closed by construction because the hint does not
// exist to omit or disagree: only the frame's own mode is ever read.
func TestSendHandlerRefusesTargetThatDoesNotAdvertiseFrameMode(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port:         1,
		MachineID:    "test-machine",
		Dir:          "/test/ses_target",
		Title:        "planner",
		Capabilities: []string{"aside", "btw"},
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	handler := sendHandler(&state)
	steerFrame := `{"event":{"id":0,"issue_key":"CORE-1"},"delivery":{"attempt":1,"mode":"steer"}}`

	for name, requestJSON := range map[string]string{
		"no top-level delivery field on the wire at all": fmt.Sprintf(
			`{"target_session":"ses_target","message":"Can this ship?","payload":%s}`, mustJSONString(t, steerFrame),
		),
		"a stray unrelated field cannot override the frame": fmt.Sprintf(
			`{"target_session":"ses_target","message":"Can this ship?","payload":%s,"urgency":"high"}`, mustJSONString(t, steerFrame),
		),
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(requestJSON))
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
			}
			var response struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Error != "session ses_target (planner) does not advertise steer" {
				t.Fatalf("error = %q, want the standard does-not-advertise shape", response.Error)
			}
		})
	}
}

// TestSendHandlerAllowsAdvertisedFrameModeAndUntaggedSends proves the guard only refuses a
// genuine mismatch: a frame naming a mode the target does advertise still sends, and a
// genuinely untagged send -- no payload, or a payload whose exact "delivery" key is entirely
// absent, including a payload that is not even valid JSON -- is never subjected to an
// inferred check. This is deliberately distinct from a payload that DOES carry the exact
// "delivery" key but cannot be read unambiguously (TestSendHandlerRefusesUnreadableOr
// AmbiguousDeliveryFrames): presence of that key is a claim of a targeted send, and an
// unreadable claim is refused, never defaulted open the way genuine absence is here.
func TestSendHandlerAllowsAdvertisedFrameModeAndUntaggedSends(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port:         1,
		MachineID:    "test-machine",
		Dir:          "/test/ses_target",
		Title:        "planner",
		Capabilities: []string{"aside", "btw"},
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	handler := sendHandler(&state)
	btwFrame := `{"event":{"id":0,"issue_key":"CORE-1"},"delivery":{"attempt":1,"mode":"btw"}}`

	for name, requestJSON := range map[string]string{
		"advertised frame mode": fmt.Sprintf(
			`{"target_session":"ses_target","message":"Can this ship?","payload":%s}`, mustJSONString(t, btwFrame),
		),
		"no payload at all": `{"target_session":"ses_target","message":"Can this ship?"}`,
		"delivery key entirely absent from a valid JSON payload": fmt.Sprintf(
			`{"target_session":"ses_target","message":"Can this ship?","payload":%s}`, mustJSONString(t, `{"note":"plain interest notification"}`),
		),
		"payload is not valid JSON at all, so it has no delivery key either": fmt.Sprintf(
			`{"target_session":"ses_target","message":"Can this ship?","payload":%s}`, mustJSONString(t, "not json at all"),
		),
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(requestJSON))
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestSendHandlerRefusesUnreadableOrAmbiguousDeliveryFrames proves the listener refuses --
// rather than silently allowing, as it once did -- a send whose targeted frame claims
// delivery (the exact "delivery" key is present) but whose mode cannot be read as a single,
// unambiguous, non-empty string: an empty delivery object, a non-string mode, an empty mode
// string, a delivery value that is not even an object, and a same-key-different-case sibling
// key at either level. Presence of the exact "delivery" key is itself a claim that this send
// is targeted; an unreadable or ambiguous claim is refused, never defaulted open the way a
// send with no "delivery" key at all is (see TestSendHandlerAllowsAdvertisedFrameModeAnd
// UntaggedSends). The case-variant case reproduces a second independent review's finding: the
// guard used to decode the payload into a Go struct, whose case-insensitive key matching could
// read a same-key-different-case sibling ("Delivery") instead of the exact "delivery" key the
// receiving client actually executes, so an attacker could hide an unadvertised real mode
// ("btw", exact key) behind an advertised decoy in the wrong-cased key ("aside", sibling key).
func TestSendHandlerRefusesUnreadableOrAmbiguousDeliveryFrames(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port:         1,
		MachineID:    "test-machine",
		Dir:          "/test/ses_target",
		Title:        "planner",
		Capabilities: []string{"aside"},
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	handler := sendHandler(&state)

	for name, payload := range map[string]string{
		"delivery present with no mode key at all":  `{"delivery":{}}`,
		"delivery.mode is not a JSON string":        `{"delivery":{"mode":123}}`,
		"delivery.mode is an empty string":          `{"delivery":{"mode":""}}`,
		"delivery is present but not a JSON object": `{"delivery":"btw"}`,
		"case-variant duplicate delivery key hides an unadvertised real mode behind an advertised decoy (the second reviewer's reproducer)": `{"event":{"id":0,"issue_key":"CORE-1"},"delivery":{"attempt":1,"mode":"btw"},"Delivery":{"mode":"aside"}}`,
		"case-variant duplicate mode key inside an otherwise well-formed delivery object":                                                   `{"delivery":{"mode":"btw","Mode":"aside"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			requestJSON := fmt.Sprintf(
				`{"target_session":"ses_target","message":"Can this ship?","payload":%s}`, mustJSONString(t, payload),
			)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(requestJSON))
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
			}
			var response struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Error != "session ses_target (planner) sent a delivery mode that cannot be read unambiguously" {
				t.Fatalf("error = %q, want the unreadable-delivery refusal shape", response.Error)
			}
		})
	}
}

// TestSendHandlerRefusesUnadvertisedModeFromDerivedPayload proves the guard
// checks messageEnvelope's effective payload, not only the optional HTTP
// payload field. A long structured message becomes the receiver's payload.
func TestSendHandlerRefusesUnadvertisedModeFromDerivedPayload(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/ses_target", Title: "planner",
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	frame, err := os.ReadFile("../../../contracts/fixtures/dispatch-targeted-delivery.json")
	if err != nil {
		t.Fatalf("read targeted delivery fixture: %v", err)
	}
	if string(frame) == contracts.OneLineSummary(string(frame)) {
		t.Fatal("targeted delivery fixture must derive an effective payload from message")
	}

	rr := httptest.NewRecorder()
	request := fmt.Sprintf(
		`{"target_session":"ses_target","source":"dispatch","message":%s}`,
		mustJSONString(t, string(frame)),
	)
	sendHandler(&state).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(request)))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rr.Code, rr.Body.String())
	}
	var response struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if response.Error != "session ses_target (planner) does not advertise btw" {
		t.Fatalf("error = %q, want derived-payload capability refusal", response.Error)
	}
}

// TestDispatchClientSendFrameModeIsGuardedEndToEnd proves the guard reads the mode from a
// real Dispatch client Send call, using the same targeted-delivery fixture the existing
// wire-contract test uses (delivery.mode "btw"), through the real listener handler -- not a
// synthetic minimal payload -- so the parsing is proven against production shape.
func TestDispatchClientSendFrameModeIsGuardedEndToEnd(t *testing.T) {
	publisher := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if err := sessions.Put("ses_target", session.SessionEntry{
		Port: 1, MachineID: "test-machine", Dir: "/test/ses_target",
		Title: "planner", Capabilities: []string{"aside"},
	}); err != nil {
		t.Fatalf("register target session: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: publisher, registry: registry, sessions: sessions})
	handler := sendHandler(&state)
	payloadBytes, err := os.ReadFile("../../../contracts/fixtures/dispatch-targeted-delivery.json")
	if err != nil {
		t.Fatalf("read targeted delivery fixture: %v", err)
	}

	listener := httptest.NewServer(handler)
	defer listener.Close()
	result, sendErr := dispatchenvoy.New(listener.URL).Send(context.Background(), dispatchenvoy.SendInput{
		TargetSession: "ses_target",
		Message:       "Can this ship?",
		Payload:       payloadBytes,
	})
	if sendErr == nil {
		t.Fatalf("Dispatch send through listener = %#v, want a refusal: the fixture's mode is btw and ses_target only advertises aside", result)
	}
	if sendErr.Error() != "session ses_target (planner) does not advertise btw" {
		t.Fatalf("send error = %q, want the standard does-not-advertise shape", sendErr.Error())
	}
}

func mustJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode JSON string literal: %v", err)
	}
	return string(encoded)
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

func TestRoleHolderNotFoundResponsesIdentifyState(t *testing.T) {
	type roleError struct {
		Error         string `json:"error"`
		Reason        string `json:"reason"`
		Holder        string `json:"holder"`
		LastSeen      int64  `json:"last_seen"`
		ClaimReleased bool   `json:"claim_released"`
	}
	tests := []struct {
		name    string
		request func() *http.Request
		handler func(*atomic.Pointer[listenerDeps]) http.Handler
	}{
		{
			name: "publish",
			request: func() *http.Request {
				return httptest.NewRequest(
					http.MethodPost,
					"/v1/messages/publish",
					strings.NewReader(`{"topic":"notifications.role.reviewer","message":"please review"}`),
				)
			},
			handler: func(state *atomic.Pointer[listenerDeps]) http.Handler {
				return publishHandler(state)
			},
		},
		{
			name: "role lookup",
			request: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/v1/roles/reviewer", nil)
			},
			handler: func(state *atomic.Pointer[listenerDeps]) http.Handler {
				return roleGetHandler(state)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			registry, sessions := setupSessionsTest(t, nil, nil)
			var state atomic.Pointer[listenerDeps]
			state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
			handler := tc.handler(&state)

			unclaimedRecorder := httptest.NewRecorder()
			handler.ServeHTTP(unclaimedRecorder, tc.request())
			if unclaimedRecorder.Code != http.StatusNotFound {
				t.Fatalf("unclaimed status = %d, want 404; body = %s", unclaimedRecorder.Code, unclaimedRecorder.Body.String())
			}
			t.Logf("unclaimed role response: %s", unclaimedRecorder.Body.String())
			var unclaimed roleError
			if err := json.NewDecoder(unclaimedRecorder.Body).Decode(&unclaimed); err != nil {
				t.Fatalf("decode unclaimed response: %v", err)
			}
			if unclaimed.Error != "no holder for role reviewer" || unclaimed.Reason != "unclaimed" ||
				unclaimed.Holder != "" || unclaimed.LastSeen != 0 || unclaimed.ClaimReleased {
				t.Fatalf("unclaimed response = %+v", unclaimed)
			}

			if err := sessions.Put("ses_holder", session.SessionEntry{
				MachineID:      "test-machine",
				SelfSubscribed: true,
			}); err != nil {
				t.Fatalf("register holder: %v", err)
			}
			entry, err := sessions.Get("ses_holder")
			if err != nil {
				t.Fatalf("read holder last seen: %v", err)
			}
			if _, err := registry.SetRole("ses_holder", "test-machine", "reviewer", false); err != nil {
				t.Fatalf("set holder: %v", err)
			}
			if err := sessions.Delete("ses_holder"); err != nil {
				t.Fatalf("expire holder session: %v", err)
			}

			lapsedRecorder := httptest.NewRecorder()
			handler.ServeHTTP(lapsedRecorder, tc.request())
			if lapsedRecorder.Code != http.StatusNotFound {
				t.Fatalf("lapsed status = %d, want 404; body = %s", lapsedRecorder.Code, lapsedRecorder.Body.String())
			}
			t.Logf("lapsed role response: %s", lapsedRecorder.Body.String())
			var lapsed roleError
			if err := json.NewDecoder(lapsedRecorder.Body).Decode(&lapsed); err != nil {
				t.Fatalf("decode lapsed response: %v", err)
			}
			if lapsed.Error != fmt.Sprintf(
				"role reviewer holder ses_holder lapsed; claim released (last seen %d)",
				entry.UpdatedAt,
			) || lapsed.Reason != "holder_lapsed" || lapsed.Holder != "ses_holder" ||
				lapsed.LastSeen != entry.UpdatedAt || !lapsed.ClaimReleased {
				t.Fatalf("lapsed response = %+v", lapsed)
			}
			holder, err := registry.RoleHolder("reviewer")
			if err != nil {
				t.Fatalf("read released claim: %v", err)
			}
			if holder != "" {
				t.Fatalf("released claim holder = %q, want empty", holder)
			}
		})
	}
}

func TestResolveLiveRoleHolderReresolvesSupersededClaim(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	if _, err := registry.SetRole("ses_lapsed", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("set lapsed holder: %v", err)
	}
	if err := sessions.Put("ses_replacement", session.SessionEntry{
		MachineID:      "test-machine",
		SelfSubscribed: true,
	}); err != nil {
		t.Fatalf("register replacement holder: %v", err)
	}
	resolver := &racingRoleClaimResolver{
		Registry: registry,
		beforeFirstRelease: func() {
			if _, err := registry.SetRole("ses_replacement", "test-machine", "reviewer", false); err != nil {
				t.Fatalf("replace lapsed holder: %v", err)
			}
		},
	}

	result, err := resolveLiveRoleHolder(resolver, sessions, "reviewer")
	if err != nil {
		t.Fatalf("resolve superseded role holder: %v", err)
	}
	if result.state != roleHolderLive || result.holder != "ses_replacement" || resolver.releaseCalls != 1 {
		t.Fatalf("resolved role = %+v after %d releases, want live replacement", result, resolver.releaseCalls)
	}
}

func TestRoleHolderLapsedResponseDoesNotClaimConcurrentRemoval(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	const (
		role      = "reviewer"
		sessionID = "ses_lapsed"
	)
	if _, err := registry.SetRole(sessionID, "test-machine", role, false); err != nil {
		t.Fatalf("set lapsed holder: %v", err)
	}
	resolver := &racingRoleClaimResolver{
		Registry: registry,
		beforeFirstRelease: func() {
			if err := registry.Remove(sessionID, nil); err != nil {
				t.Fatalf("remove lapsed role claim: %v", err)
			}
		},
	}

	result, err := resolveLiveRoleHolder(resolver, sessions, role)
	if err != nil {
		t.Fatalf("resolve concurrently removed role holder: %v", err)
	}
	if result.state != roleHolderLapsed || result.claimRelease != store.ExpiredRoleClaimMissing {
		t.Fatalf("resolved role = %+v, want missing lapsed claim", result)
	}
	recorder := httptest.NewRecorder()
	writeRoleHolderError(recorder, role, result)
	var response struct {
		Reason        string `json:"reason"`
		ClaimReleased bool   `json:"claim_released"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode lapsed response: %v", err)
	}
	if response.Reason != "holder_lapsed" || response.ClaimReleased {
		t.Fatalf("lapsed response = %+v, want missing claim without release", response)
	}
}

func TestRoleHolderLapsedResponseDoesNotInventLastSeen(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if _, err := registry.SetRole("ses_lapsed", "test-machine", "reviewer", false); err != nil {
		t.Fatalf("set lapsed holder: %v", err)
	}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})

	recorder := httptest.NewRecorder()
	roleGetHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/reviewer", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		LastSeen *int64 `json:"last_seen"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode lapsed response: %v", err)
	}
	if response.LastSeen != nil {
		t.Fatalf("last_seen = %d, want omitted when no session heartbeat is available", *response.LastSeen)
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

	t.Run("unheld role returns 404 without publishing", func(t *testing.T) {
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
		if body := recorder.Body.String(); body != "{\"error\":\"no holder for role unheld\",\"reason\":\"unclaimed\"}\n" {
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
		if body := recorder.Body.String(); body != "{\"error\":\"no holder for role unheld\",\"reason\":\"unclaimed\"}\n" {
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
	lookup := stalledStreamInfo{deadlines: make(chan time.Duration, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(lookup.release) })
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{
		client:     client,
		registry:   registry,
		sessions:   sessions,
		streamName: "notifications",
		streamInfo: lookup,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(`{
		"session_id":"ses_subscriber",
		"self_subscribed":true,
		"topics":["notifications.github.example-org.example-repo.pr.>"]
	}`))
	responded := make(chan struct{})
	go func() {
		defer close(responded)
		subscribeHandler(&state, "test-machine", logging.New("test")).ServeHTTP(recorder, request)
	}()
	// The lookup never answers, so the handler can only respond by abandoning it at the check's
	// own deadline. The wait below only turns a blocked handler into a failure instead of a hang.
	select {
	case <-responded:
	case <-time.After(30 * time.Second):
		t.Fatal("subscribe is still waiting on a stream-info lookup that never answers")
	}
	// The lookup goroutine records its deadline when it runs, which need not be before the handler
	// abandons it, so wait for the record rather than expecting it to be there already.
	select {
	case remaining := <-lookup.deadlines:
		if remaining > unwiredRepositoryWarningTimeout {
			t.Fatalf("stream-info lookup was issued with a deadline %s away, want at most the %s advisory bound", remaining, unwiredRepositoryWarningTimeout)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no stream-info lookup was issued with a deadline")
	}
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
	routeClient.Close()

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

func TestPublishHandler_DedupeKeySelection(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	const topic = "notifications.github.example-org.example-repo.dedupe-key"
	probe, err := client.Conn.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe topic probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	minted := func(key string) bool { return strings.HasPrefix(key, "publish.") && len(key) > len("publish.") }
	cases := []struct {
		name string
		body string
		want func(key string) bool
	}{
		{"explicit dedupe_key is used verbatim", `{"topic":"` + topic + `","message":"hello","dedupe_key":"publish.abc"}`, func(key string) bool { return key == "publish.abc" }},
		{"empty dedupe_key is absent", `{"topic":"` + topic + `","message":"hello","dedupe_key":""}`, minted},
		{"idempotency_key keeps the publish prefix", `{"topic":"` + topic + `","message":"hello","idempotency_key":"k1"}`, func(key string) bool { return key == "publish.k1" }},
		{"neither mints a key", `{"topic":"` + topic + `","message":"hello"}`, minted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			publishHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(tc.body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
			}
			var response contracts.Envelope
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !tc.want(response.DedupeKey) {
				t.Fatalf("response dedupe_key = %q", response.DedupeKey)
			}
			message, err := probe.NextMsg(5 * time.Second)
			if err != nil {
				t.Fatalf("read published envelope: %v", err)
			}
			var wire contracts.Envelope
			if err := json.Unmarshal(message.Data, &wire); err != nil {
				t.Fatalf("decode wire envelope: %v", err)
			}
			if wire.DedupeKey != response.DedupeKey || wire.EventID != response.EventID {
				t.Fatalf("wire envelope dedupe_key %q event %q; response %q %q", wire.DedupeKey, wire.EventID, response.DedupeKey, response.EventID)
			}
		})
	}
}

func TestPublishHandler_RejectsDedupeKeyWithIdempotencyKey(t *testing.T) {
	client := setupPublishTestClient(t)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client})
	const topic = "notifications.github.example-org.example-repo.dedupe-key-conflict"
	probe, err := client.Conn.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe topic probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	recorder := httptest.NewRecorder()
	publishHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"`+topic+`","message":"hello","dedupe_key":"publish.abc","idempotency_key":"k1"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	var response apiError
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode 400 body: %v", err)
	}
	if response.Error == "" || len(response.Expected) != 2 || response.Expected[0] != "dedupe_key" || response.Expected[1] != "idempotency_key" {
		t.Fatalf("400 body = %+v, want expected [dedupe_key idempotency_key]", response)
	}
	if _, err := probe.NextMsg(250 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
		t.Fatalf("a rejected publish must publish nothing: %v", err)
	}
}

// The role arbiter drops an envelope whose dedupe_key already carries the
// forward prefix (it is how a forwarded copy is kept out of the arbiter), so a
// caller who supplies one would get a 200 for a message that vanishes: no
// forward, no exception, no log line. The prefix is reserved at the API.
func TestPublishHandler_RejectsReservedDedupeKeyPrefix(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	for _, topic := range []string{
		"notifications.github.example-org.example-repo.reserved-prefix",
		contracts.RoleTopicPrefix + "reserved-prefix-unheld",
	} {
		t.Run(topic, func(t *testing.T) {
			probe, err := client.Conn.SubscribeSync(topic)
			if err != nil {
				t.Fatalf("subscribe topic probe: %v", err)
			}
			t.Cleanup(func() { _ = probe.Unsubscribe() })
			if err := client.Conn.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
			recorder := httptest.NewRecorder()
			publishHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/publish", strings.NewReader(`{"topic":"`+topic+`","message":"hello","dedupe_key":"`+roleForwardDedupePrefix+`publish.abc"}`)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var response apiError
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				t.Fatalf("decode 400 body: %v", err)
			}
			if response.Error != "dedupe_key must not begin with the reserved prefix "+roleForwardDedupePrefix || len(response.Expected) != 1 || response.Expected[0] != "dedupe_key" {
				t.Fatalf("400 body = %+v", response)
			}
			if _, err := probe.NextMsg(250 * time.Millisecond); !errors.Is(err, nats.ErrTimeout) {
				t.Fatalf("a rejected publish must publish nothing: %v", err)
			}
		})
	}
}

// The mutual-exclusion rule is publish's alone: send ignores dedupe_key as it
// ignores any field it does not read, so a body carrying both keys is still a
// send keyed by its idempotency_key (LEGION-108, architect ruling).
func TestSendHandler_IgnoresDedupeKeyBesideIdempotencyKey(t *testing.T) {
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, map[string]int{"ses_target": 1})
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions})
	recorder := httptest.NewRecorder()
	sendHandler(&state).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(`{"target_session":"ses_target","message":"hello","dedupe_key":"publish.abc","idempotency_key":"k1"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response contracts.Envelope
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.DedupeKey != "agent.ses_target.k1" {
		t.Fatalf("send dedupe_key = %q, want agent.ses_target.k1", response.DedupeKey)
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
func TestSubscribeHandlerStoresAndListsCapabilities(t *testing.T) {
	registry, sessions := setupSessionsTest(t, nil, nil)
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{registry: registry, sessions: sessions})
	handler := subscribeHandler(&state, "test-machine", logging.New("test"))

	for _, body := range []string{
		`{"session_id":"ses_capable","topics":[],"self_subscribed":true,"capabilities":["aside","btw"]}`,
		`{"session_id":"ses_legacy","topics":[],"self_subscribed":true}`,
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(
			recorder,
			httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("subscribe: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}

	list := httptest.NewRecorder()
	sessionsHandler(registry, sessions).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list sessions: status=%d body=%s", list.Code, list.Body.String())
	}
	var payload []struct {
		SessionID    string   `json:"session_id"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(list.Body).Decode(&payload); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("sessions = %#v, want two", payload)
	}
	if payload[0].SessionID != "ses_capable" || strings.Join(payload[0].Capabilities, ",") != "aside,btw" {
		t.Fatalf("capable session = %#v, want advertised capabilities", payload[0])
	}
	if payload[1].SessionID != "ses_legacy" || len(payload[1].Capabilities) != 0 {
		t.Fatalf("legacy session = %#v, want empty capabilities", payload[1])
	}
}
