package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The clean stop is one DELETE of the session's own key, with the listener's bearer, and it names
// the moment the stop belongs to: the listener deletes the registration only while nothing has
// been written under that session id since (if_not_registered_after).
func TestDeleteSessionAsksTheSessionRouteWithTheBearerAndTheMoment(t *testing.T) {
	var method, path, query, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, bearer = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	goneAt := time.Now()

	stopped, err := New(server.URL+"/", "envoy-bearer").DeleteSession(context.Background(), "ses_implementer", goneAt)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if !stopped.Deleted {
		t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
	}
	if method != http.MethodDelete || path != "/v1/sessions/ses_implementer" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a DELETE of the session's key with the listener's bearer", method, path, bearer)
	}
	if want := fmt.Sprintf("if_not_registered_after=%d", goneAt.UnixMilli()); query != want {
		t.Fatalf("query = %q, want the moment the stop belongs to, %q", query, want)
	}
}

// A listener that kept the registration says so, and the caller is told which it was: that answer
// is the one that stops a slow stop deleting the registration a relaunched process wrote under
// the same session id (dispatch://INFRA-392).
func TestDeleteSessionReportsARegistrationTheListenerKept(t *testing.T) {
	const reason = "the registration was written after the moment the delete belongs to"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"deleted":false,"reason":"`+reason+`"}`)
	}))
	defer server.Close()

	stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_relaunched", time.Now())
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if stopped.Deleted || stopped.Reason != reason {
		t.Fatalf("DeleteSession = %+v, want the registration kept with the listener's reason", stopped)
	}
}

// A registry that holds no record for the session has already reached the outcome the stop asks
// for, so its 404 is no failure; a refusal that leaves the registration standing is one, and says
// what the listener answered so an operator can see why the session still holds its role.
func TestDeleteSessionTellsAnAlreadyGoneSessionFromARefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "the listener removed it", status: http.StatusOK, body: `{"deleted":true}`},
		{name: "an older listener removed it without saying so", status: http.StatusOK, body: ``},
		{name: "the registry holds no record of it", status: http.StatusNotFound, body: `{"error":"session not found"}`},
		{name: "the bus could not be reached", status: http.StatusServiceUnavailable, body: `{"error":"session registry unavailable"}`,
			wantErr: "session registry unavailable"},
		{name: "the bearer is wrong", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantErr: "401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			stopped, err := New(server.URL, "").DeleteSession(context.Background(), "ses_implementer", time.Now())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("DeleteSession = %v, want the session taken as stopped", err)
			case tc.wantErr == "" && !stopped.Deleted:
				t.Fatalf("DeleteSession = %+v, want the registration taken as deleted", stopped)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("DeleteSession = %v, want a failure naming %q", err, tc.wantErr)
			}
		})
	}
}
