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
