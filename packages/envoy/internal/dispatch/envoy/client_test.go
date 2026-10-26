package envoy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// LEGION-271. A send whose answer misses the client's window is the one failure a retry is safe
// after: the listener publishes before it answers, so the message may already be on the agent's
// subject. Dispatch can only word that honestly if the timeout arrives as its own error rather
// than as a string, and today client.go flattens the cause with %v so errors.Is sees nothing.
// A refusal is the discriminating control: nothing landed, and it must not read as a timeout.
func TestSendDistinguishesAReceiptTimeoutFromARefusal(t *testing.T) {
	held := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode send: %v", err)
		}
		if request.IdempotencyKey == "refused:steer" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"listener refused the send"}`))
			return
		}
		<-held
	}))
	defer func() {
		close(held)
		listener.Close()
	}()

	_, err := New(listener.URL, WithTimeout(150*time.Millisecond)).Send(context.Background(), SendInput{
		TargetSession: "s1", Message: "held past the window", IdempotencyKey: "held:steer",
	})
	if !errors.Is(err, ErrReceiptTimeout) {
		t.Fatalf("send that missed the window = %v, want ErrReceiptTimeout", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a receipt timeout is still a listener failure: %v", err)
	}

	_, err = New(listener.URL, WithTimeout(5*time.Second)).Send(context.Background(), SendInput{
		TargetSession: "s1", Message: "refused outright", IdempotencyKey: "refused:steer",
	})
	if err == nil || errors.Is(err, ErrReceiptTimeout) {
		t.Fatalf("refused send = %v, want a failure that is not a receipt timeout", err)
	}
}

// LEGION-271. The listener answers a send JetStream already held with duplicate: true: the stream
// stored nothing new, and the publish still reached the agent's subject. That flag is the only
// thing telling Dispatch its retry was a repeat. An older listener omits the field, which reads as
// false.
func TestSendCarriesTheListenersDuplicateVerdict(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode send: %v", err)
		}
		if request.IdempotencyKey == "already-held:steer" {
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1","duplicate":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"event_id":"envelope-2","recipient":"s1"}`))
	}))
	defer listener.Close()

	duplicate, err := New(listener.URL).Send(context.Background(), SendInput{
		TargetSession: "s1", Message: "the stream already had this", IdempotencyKey: "already-held:steer",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !duplicate.Duplicate {
		t.Fatalf("send result = %#v, want the listener's duplicate verdict carried", duplicate)
	}

	fresh, err := New(listener.URL).Send(context.Background(), SendInput{
		TargetSession: "s1", Message: "the stream did not have this", IdempotencyKey: "fresh:steer",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if fresh.Duplicate {
		t.Fatalf("send result = %#v, want an omitted duplicate read as false", fresh)
	}
}

// LEGION-271. A send that never reached the listener published nothing, so it must not be
// classed a receipt timeout: that class is what tells a human their message may already have
// been delivered. Only a request that was written and then went unanswered qualifies. The
// distinguishing case is a connect that times out - a timeout, but one where nothing was sent.
func TestSendDoesNotCallAConnectFailureAReceiptTimeout(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3: reserved, routed nowhere. The dial either times out or is
	// refused outright; both mean nothing was written, and neither may read as a receipt
	// timeout.
	_, err := New("http://203.0.113.1:9", WithTimeout(200*time.Millisecond)).Send(
		context.Background(), SendInput{
			TargetSession: "s1", Message: "never left the building", IdempotencyKey: "m1:steer",
		},
	)
	if err == nil {
		t.Fatal("a send to an unroutable address must fail")
	}
	if errors.Is(err, ErrReceiptTimeout) {
		t.Fatalf("connect failure = %v, want a failure that is not a receipt timeout", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("connect failure = %v, want it still reported as a listener failure", err)
	}
}

func TestSessionsMapsListenerRowsAndDefaultsMissingSlices(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"session_id":"s1","machine_id":"m1","dir":"/w/legion","port":9100,"title":"planner","self_subscribed":true,"topics":["notifications.agent.s1"],"roles":["legion-planner"],"updated_at":1700000000000,"last_seen":1700000000000}]`))
	}))
	defer listener.Close()

	sessions, err := New(listener.URL + "/").Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{{
		SessionID:    "s1",
		Title:        "planner",
		Dir:          "/w/legion",
		MachineID:    "m1",
		Roles:        []string{"legion-planner"},
		Capabilities: []string{},
		LastSeen:     1700000000000,
	}}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("sessions = %#v, want %#v", sessions, want)
	}
	encoded, err := json.Marshal(sessions[0])
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	for _, hidden := range []string{"port", "topics", "self_subscribed"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("%s leaked into %s", hidden, encoded)
		}
	}
}

func TestSessionsSendsEnvoyToken(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer listener-token" {
			t.Errorf("Authorization = %q, want Bearer listener-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer listener.Close()

	sessions, err := New(listener.URL, WithToken("listener-token")).Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions = %#v, want no sessions", sessions)
	}
}

func TestSessionsReportsUnavailableListener(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	if _, err := New(failing.URL).Sessions(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500: err = %v, want ErrUnavailable", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := New(closed.URL).Sessions(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refused: err = %v, want ErrUnavailable", err)
	}
}

func TestListInterestsMapsListenerRowsAndDefaultsMissingTopics(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/interests/" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"session_id":"s1","topics":["notifications.dispatch.issue.T-1","notifications.dispatch.issue.T-1.>"],"updated_at":1700000000000},{"session_id":"s2","updated_at":1700000000001}]`))
	}))
	defer listener.Close()

	interests, err := New(listener.URL).ListInterests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Interest{
		{
			SessionID: "s1",
			Topics:    []string{"notifications.dispatch.issue.T-1", "notifications.dispatch.issue.T-1.>"},
			UpdatedAt: 1700000000000,
		},
		{SessionID: "s2", Topics: []string{}, UpdatedAt: 1700000000001},
	}
	if !reflect.DeepEqual(interests, want) {
		t.Fatalf("interests = %#v, want %#v", interests, want)
	}
}

func TestListInterestsReportsUnavailableListener(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	if _, err := New(failing.URL).ListInterests(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500: err = %v, want ErrUnavailable", err)
	}
}

func TestInterestReturnsOneSessionsTopics(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/interests/s1" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"s1","topics":["notifications.dispatch.issue.T-1.>"],"updated_at":42}`))
	}))
	defer listener.Close()

	interest, err := New(listener.URL).Interest(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	want := Interest{SessionID: "s1", Topics: []string{"notifications.dispatch.issue.T-1.>"}, UpdatedAt: 42}
	if !reflect.DeepEqual(interest, want) {
		t.Fatalf("interest = %#v, want %#v", interest, want)
	}
}

// A session id the listener refuses as a KV key (400: outside nats.go's key alphabet or holding an
// empty token; 413: too long) names no interest a read can find, as an unknown one does, so a
// subscriber removal pending on such an id completes rather than wait on the listener forever.
func TestInterestReportsNotFoundForAnUnknownSession(t *testing.T) {
	for _, tc := range []struct {
		session string
		status  int
	}{
		{"ghost", http.StatusNotFound},
		{"ses:bad", http.StatusBadRequest},
		{"sess..x", http.StatusBadRequest},
		{"long", http.StatusRequestEntityTooLarge},
	} {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		_, err := New(listener.URL).Interest(context.Background(), tc.session)
		listener.Close()
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%d for %s: err = %v, want ErrNotFound", tc.status, tc.session, err)
		}
	}
}

func TestUnsubscribePostsSessionAndTopicsAndReturnsRemoved(t *testing.T) {
	var body struct {
		SessionID string   `json:"session_id"`
		Topics    []string `json:"topics"`
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/interests/unsubscribe" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"removed":["notifications.dispatch.issue.T-1.>"]}`))
	}))
	defer listener.Close()

	removed, err := New(listener.URL).Unsubscribe(
		context.Background(), "s1", []string{"notifications.dispatch.issue.T-1.>"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if body.SessionID != "s1" || !reflect.DeepEqual(body.Topics, []string{"notifications.dispatch.issue.T-1.>"}) {
		t.Fatalf("request body = %#v", body)
	}
	if !reflect.DeepEqual(removed, []string{"notifications.dispatch.issue.T-1.>"}) {
		t.Fatalf("removed = %#v", removed)
	}
}

func TestUnsubscribeReportsUnavailableListener(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	if _, err := New(failing.URL).Unsubscribe(context.Background(), "s1", []string{"t"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500: err = %v, want ErrUnavailable", err)
	}
}
func TestRoleAndSendPreserveTheListenerDeliveryContract(t *testing.T) {
	// A non-loopback listener requires the bearer on every /v1 call; the send is the one that
	// reaches a live agent, so an unauthenticated send fails delivery in production while
	// every read succeeds.
	var sent map[string]any
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer listener-token" {
			t.Errorf("%s %s Authorization = %q, want Bearer listener-token", r.Method, r.URL.Path, got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/legion-planner":
			_, _ = w.Write([]byte(`{"role":"legion-planner","holder":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","capabilities":["aside","btw"],"last_seen":42}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatalf("decode delivery: %v", err)
			}
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	client := New(listener.URL, WithToken("listener-token"))

	holder, err := client.Role(context.Background(), "legion-planner")
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if holder.SessionID != "s1" || holder.Title != "planner" || !reflect.DeepEqual(holder.Capabilities, []string{"aside", "btw"}) {
		t.Fatalf("holder = %#v", holder)
	}
	result, err := client.Send(context.Background(), SendInput{
		TargetSession:  "s1",
		Message:        "Can this ship?",
		Payload:        json.RawMessage(`{"event":{"type":"message.created"},"delivery":{"attempt":1,"mode":"btw"}}`),
		IdempotencyKey: "message-1:1",
		Urgency:        "high",
		ExpectsReply:   "required",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if result.EnvelopeID != "envelope-1" {
		t.Fatalf("envelope id = %q", result.EnvelopeID)
	}
	if sent["target_session"] != "s1" || sent["source"] != "dispatch" || sent["message"] != "Can this ship?" ||
		sent["idempotency_key"] != "message-1:1" || sent["expects_reply"] != "required" || sent["urgency"] != "high" {
		t.Fatalf("send payload = %#v", sent)
	}
	payload, ok := sent["payload"].(string)
	if !ok {
		t.Fatalf("send payload = %#v, want JSON string delivery frame", sent)
	}
	if payload != `{"event":{"type":"message.created"},"delivery":{"attempt":1,"mode":"btw"}}` {
		t.Fatalf("payload = %q", payload)
	}
}

func TestRolePreservesListenerNotFoundText(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no holder for role legion-planner"}`))
	}))
	defer listener.Close()

	_, err := New(listener.URL).Role(context.Background(), "legion-planner")
	if err == nil || err.Error() != "no holder for role legion-planner" {
		t.Fatalf("role error = %v, want listener error text", err)
	}
}

// A per-session read is the question "is this session live, and how is it named", so the body it
// decodes is one row of GET /v1/sessions: the same fields, filled the same way. The listener may
// leave the id out of its own answer, and leaves a session with no roles or no capabilities
// without those keys; a caller comparing capabilities must not have to nil-check first.
func TestSessionDecodesTheListenerRowAndFillsWhatItOmits(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Session
	}{
		{
			name: "a full row",
			body: `{"session_id":"s1","machine_id":"m1","host":"h1","dir":"/w/legion","port":9100,` +
				`"title":"planner","self_subscribed":true,"topics":["notifications.agent.s1"],` +
				`"roles":["legion-planner"],"capabilities":["aside","btw"],` +
				`"updated_at":1700000000000,"last_seen":1700000000001}`,
			want: Session{
				SessionID: "s1", Title: "planner", Dir: "/w/legion", MachineID: "m1", Host: "h1",
				Roles: []string{"legion-planner"}, Capabilities: []string{"aside", "btw"},
				LastSeen: 1700000000001,
			},
		},
		{
			name: "a row naming neither the session nor any role or capability",
			body: `{"machine_id":"m1","host":"h1","dir":"/w/legion","title":"planner","last_seen":42}`,
			want: Session{
				SessionID: "s1", Title: "planner", Dir: "/w/legion", MachineID: "m1", Host: "h1",
				Roles: []string{}, Capabilities: []string{}, LastSeen: 42,
			},
		},
	} {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/registry/s1" || r.Method != http.MethodGet {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer listener-token" {
				t.Errorf("Authorization = %q, want Bearer listener-token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.body))
		}))
		session, err := New(listener.URL, WithToken("listener-token")).Session(context.Background(), "s1")
		listener.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(session, tc.want) {
			t.Fatalf("%s: session = %#v, want %#v", tc.name, session, tc.want)
		}
	}
}

// The two answers Session must never confuse are "the listener holds no record under that id" -
// which ends a caller's wait - and "the listener could not say", which only means ask again. A
// 400 (an id outside nats.go's key alphabet, or holding an empty token) and a 413 (one too long)
// are the first: NATS refuses such an id as a key, so no session ever registered under it. Every
// other status, and a 200 whose body does not decode, are the second, and a 503 carries the
// listener's own reason because a person reads it.
func TestSessionSeparatesNoRecordFromNoAnswer(t *testing.T) {
	const busSilent = "envoy: the message bus did not answer: context deadline exceeded"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		reason string
	}{
		{name: "unknown session", status: http.StatusNotFound, body: `{"error":"no live session s1"}`, want: ErrNotFound},
		{name: "id NATS refuses as a key", status: http.StatusBadRequest, body: `{"error":"session_id is invalid"}`, want: ErrNotFound},
		{name: "id too long for a key", status: http.StatusRequestEntityTooLarge, body: `{"error":"session_id is too long"}`, want: ErrNotFound},
		{name: "the bus did not answer", status: http.StatusServiceUnavailable, body: `{"error":"` + busSilent + `"}`,
			want: ErrUnavailable, reason: busSilent},
		{name: "the listener broke", status: http.StatusInternalServerError, body: ``, want: ErrUnavailable},
		{name: "a body that does not decode", status: http.StatusOK, body: `{"session_id":`, want: ErrUnavailable},
	} {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(listener.URL).Session(context.Background(), "s1")
		listener.Close()
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s (%d): err = %v, want %v", tc.name, tc.status, err, tc.want)
		}
		if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("%s (%d): err = %q, want it to carry the listener's reason %q", tc.name, tc.status, err, tc.reason)
		}
	}
}

// A listener that cannot be reached at all says nothing about the session either, so it is the
// same "ask again" a 503 is - never grounds to call the session gone.
func TestSessionReportsAnUnreachableListenerAsUnavailable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	if _, err := New(closed.URL).Session(context.Background(), "s1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refused: err = %v, want ErrUnavailable", err)
	} else if errors.Is(err, ErrNotFound) {
		t.Fatalf("refused: err = %v, want it not to read as a missing session", err)
	}
}

const legionAgentRow = `{"session_id":"ses-old","legion_project":"infra","issue":"INFRA-448","role":"reviewer",` +
	`"role_token":"legion-infra-infra-448-reviewer","title":"Legion reviewer · INFRA-448","harness":"omp",` +
	`"started_at":"2026-10-26T01:00:00Z","recorded_at":"2026-10-26T01:00:05Z","live":false,` +
	`"replaced_by":"ses-new","replaced_at":"2026-10-26T02:00:00Z","added_later":1}`

func wantLegionAgentRow(t *testing.T, got LegionAgent) {
	t.Helper()
	newer, at := "ses-new", "2026-10-26T02:00:00Z"
	want := LegionAgent{
		SessionID: "ses-old", LegionProject: "infra", Issue: "INFRA-448", Role: "reviewer",
		RoleToken: "legion-infra-infra-448-reviewer", Title: "Legion reviewer · INFRA-448", Harness: "omp",
		StartedAt: "2026-10-26T01:00:00Z", RecordedAt: "2026-10-26T01:00:05Z", ReplacedBy: &newer, ReplacedAt: &at,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent = %#v, want %#v", got, want)
	}
}

// Agent asks the session's route with the listener's bearer and decodes its entry, ignoring a
// field it does not know.
func TestAgentDecodesTheRegistryEntry(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/legion/agents/sessions/ses-old" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.EscapedPath())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer listener-token" {
			t.Errorf("Authorization = %q, want Bearer listener-token", got)
		}
		_, _ = w.Write([]byte(legionAgentRow))
	}))
	defer listener.Close()
	agent, err := New(listener.URL, WithToken("listener-token")).Agent(context.Background(), "ses-old")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	wantLegionAgentRow(t, agent)
}

// An unknown session, or one the listener refuses as a name, is ErrNotFound: no entry is recorded
// under it. Every other answer is ErrUnavailable, a 503 carrying the listener's reason.
func TestAgentSeparatesNoEntryFromNoAnswer(t *testing.T) {
	const silent = "envoy: the legion agent registry did not answer: context deadline exceeded"
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
		reason string
	}{
		{name: "unknown session", status: http.StatusNotFound, body: `{"error":"unknown legion agent ses-old"}`, want: ErrNotFound},
		{name: "an id the registry refuses", status: http.StatusBadRequest, body: `{"error":"invalid legion agent: session_id"}`, want: ErrNotFound},
		{name: "an id too long for a key", status: http.StatusRequestEntityTooLarge, body: `{"error":"too large"}`, want: ErrNotFound},
		{name: "the registry did not answer", status: http.StatusServiceUnavailable, body: `{"error":"` + silent + `"}`, want: ErrUnavailable, reason: silent},
		{name: "the listener broke", status: http.StatusInternalServerError, want: ErrUnavailable},
		{name: "a body that does not decode", status: http.StatusOK, body: `{"session_id":`, want: ErrUnavailable},
	} {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(listener.URL).Agent(context.Background(), "ses-old")
		listener.Close()
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("%s: err = %q, want it to carry %q", tc.name, err, tc.reason)
		}
	}
}

// RoleAgents and IssueAgents ask their listing routes, escaping what they name, and decode the
// entries; an empty listing is no agents, not nil.
func TestRoleAndIssueAgentsDecodeTheListing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		query string
		call  func(*Client) ([]LegionAgent, error)
	}{
		{"by role token", "/v1/legion/agents/roles/legion-infra-infra-448-reviewer", "", func(c *Client) ([]LegionAgent, error) {
			return c.RoleAgents(context.Background(), "legion-infra-infra-448-reviewer")
		}},
		{"by issue", "/v1/legion/agents", "issue=INFRA-448", func(c *Client) ([]LegionAgent, error) {
			return c.IssueAgents(context.Background(), "INFRA-448")
		}},
	} {
		for _, listing := range []struct {
			body string
			want int
		}{{`{"entries":[` + legionAgentRow + `]}`, 1}, {`{"entries":[]}`, 0}} {
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tc.path || r.URL.RawQuery != tc.query {
					t.Errorf("%s: unexpected request %s %s?%s", tc.name, r.Method, r.URL.Path, r.URL.RawQuery)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer listener-token" {
					t.Errorf("%s: Authorization = %q", tc.name, got)
				}
				_, _ = w.Write([]byte(listing.body))
			}))
			agents, err := tc.call(New(listener.URL, WithToken("listener-token")))
			listener.Close()
			if err != nil || agents == nil || len(agents) != listing.want {
				t.Fatalf("%s: agents = %#v, %v; want %d", tc.name, agents, err, listing.want)
			}
			if listing.want == 1 {
				wantLegionAgentRow(t, agents[0])
			}
		}
	}
}

// A listing the listener does not answer with a 200, or answers with a body that does not decode,
// or that cannot be reached, is ErrUnavailable: never an empty list a caller would take for no
// agents.
func TestRoleAndIssueAgentsReportAnyOtherAnswerAsUnavailable(t *testing.T) {
	calls := map[string]func(*Client) ([]LegionAgent, error){
		"RoleAgents": func(c *Client) ([]LegionAgent, error) {
			return c.RoleAgents(context.Background(), "legion-infra-infra-448-reviewer")
		},
		"IssueAgents": func(c *Client) ([]LegionAgent, error) { return c.IssueAgents(context.Background(), "INFRA-448") },
	}
	for _, tc := range []struct {
		status int
		body   string
		reason string
	}{
		{http.StatusServiceUnavailable, `{"error":"envoy: the legion agent registry did not answer"}`, "did not answer"},
		{http.StatusBadRequest, `{"error":"invalid legion agent: issue"}`, "invalid legion agent"},
		{http.StatusNotFound, `{"error":"not found"}`, "not found"},
		{http.StatusOK, `{"entries":`, ""},
	} {
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		for name, call := range calls {
			agents, err := call(New(listener.URL))
			if !errors.Is(err, ErrUnavailable) || agents != nil {
				t.Fatalf("%s (%d): %v, %v; want ErrUnavailable", name, tc.status, agents, err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("%s (%d): err = %q, want it to carry %q", name, tc.status, err, tc.reason)
			}
		}
		listener.Close()
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for name, call := range calls {
		if _, err := call(New(closed.URL)); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s on an unreachable listener: %v, want ErrUnavailable", name, err)
		}
	}
}
