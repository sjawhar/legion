package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/proof"
)

// TestRenewDistinguishesAnEndedEnrollmentFromARecoverableProofFailure makes the renewal contract
// explicit: an enrollment the broker knows ended is ENROLLMENT_NOT_LIVE, which a pod renewer may
// stop on; a live enrollment whose proof iat is outside skew is still PROOF_INVALID, which it must
// retry after the clock is corrected.
func TestRenewDistinguishesAnEndedEnrollmentFromARecoverableProofFailure(t *testing.T) {
	ts := newTestServer(t)
	id, key := ts.newSessionEnrollment(t, "renew-liveness", "ada@example.com")
	path := "/v1/enrollments/" + id + "/renew"
	if _, err := ts.Store.Pool.Exec(context.Background(), `update enrollments set lease_expires_at=now() - interval '1 second' where id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status, body := ts.session(t, key, id, http.MethodPost, path, nil)
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ENROLLMENT_NOT_LIVE" {
		t.Fatalf("renew a lapsed enrollment = %d %s, want 401 ENROLLMENT_NOT_LIVE", status, body)
	}

	id, key = ts.newSessionEnrollment(t, "renew-skew", "ada@example.com")
	path = "/v1/enrollments/" + id + "/renew"
	skewed, err := proof.Sign(key, id, http.MethodPost, ts.URL+path, time.Now().Add(-2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	status, body = ts.req(t, http.MethodPost, path, map[string]string{"Proof": skewed}, nil)
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "PROOF_INVALID" {
		t.Fatalf("renew a live enrollment with skewed iat = %d %s, want 401 PROOF_INVALID", status, body)
	}
}
