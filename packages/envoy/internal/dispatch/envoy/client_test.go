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
)

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

func TestInterestReportsNotFoundForAnUnknownSession(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()

	if _, err := New(listener.URL).Interest(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
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
