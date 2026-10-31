package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The clean stop is one DELETE of the session's own key, with the listener's bearer.
func TestDeleteSessionAsksTheSessionRouteWithTheBearer(t *testing.T) {
	var method, path, bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, bearer = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := New(server.URL+"/", "envoy-bearer").DeleteSession(context.Background(), "ses_implementer"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if method != http.MethodDelete || path != "/v1/sessions/ses_implementer" || bearer != "Bearer envoy-bearer" {
		t.Fatalf("%s %s with %q, want a DELETE of the session's key with the listener's bearer", method, path, bearer)
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
		{name: "the listener removed it", status: http.StatusOK, body: `{"removed":true}`},
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

			err := New(server.URL, "").DeleteSession(context.Background(), "ses_implementer")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("DeleteSession = %v, want the session taken as stopped", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("DeleteSession = %v, want a failure naming %q", err, tc.wantErr)
			}
		})
	}
}
