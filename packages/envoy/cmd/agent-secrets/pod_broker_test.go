//go:build unix

// packages/envoy/cmd/agent-secrets/pod_broker_test.go
//
// The fake broker `enroll --pod` is driven against: it takes the token login, the enrollment and
// a renewal, and records each, so a test can say exactly what the pod sent and in which order.
// It verifies nothing — the real broker's own tests do that — and holds no key.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// podCall is one request the fake broker took: its path, the Proof header it carried, and its
// JSON body as strings.
type podCall struct {
	path  string
	proof string
	body  map[string]string
}

// servePodBroker is a broker that takes the token login, the enrollment and a renewal, recording
// each. It answers a thumbprint it has already enrolled 200 with the same enrollment id and,
// like the real broker, without extending its lease, so a re-run has to renew.
func servePodBroker(t *testing.T, enrollmentID string) (*httptest.Server, func() []podCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []podCall
	enrolled := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]string{}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, podCall{path: r.URL.Path, proof: r.Header.Get("Proof"), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential_id": "credential-for-the-pod",
				"expires_at":    time.Now().Add(time.Hour),
			})
		case "/v1/enrollments":
			mu.Lock()
			live := enrolled[body["thumbprint"]]
			enrolled[body["thumbprint"]] = true
			mu.Unlock()
			if live {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enrollment_id":    enrollmentID,
				"lease_expires_at": time.Now().Add(time.Hour),
				"slot":             nil,
			})
		case "/v1/enrollments/" + enrollmentID + "/renew":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(time.Hour)})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []podCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]podCall(nil), calls...)
	}
}
