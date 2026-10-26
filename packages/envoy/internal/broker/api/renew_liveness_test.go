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

// TestAPodWhoseLeaseRanOutWhileNoBrokerServedRenewsAndReadsItsGrant is the outage the broker's
// serving clock (enroll.ServingClock) covers, seen at the API before that clock beats again: the
// database comes back to a broker that kept running, its last beat ten minutes back, longer than
// the threshold the last beat recorded. A pod whose lease ran out in that gap still proves who it
// is, has its grant's values released and renews; one whose lease ran out before the last beat is
// still refused as ended.
func TestAPodWhoseLeaseRanOutWhileNoBrokerServedRenewsAndReadsItsGrant(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	id, key := ts.newServicePod(t)
	lapsed, lapsedKey := ts.newServicePod(t)
	granted := ts.requestAs(t, key, id, "WORKER_TOKEN")
	if granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("request WORKER_TOKEN = %+v, want granted at once", granted)
	}
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`update broker_serving set last_serving_at = now() - interval '10 minutes', not_serving_after = interval '15 seconds'`, nil},
		{`update enrollments set lease_expires_at = now() - interval '1 minute' where id=$1`, []any{id}},
		{`update enrollments set lease_expires_at = now() - interval '20 minutes' where id=$1`, []any{lapsed}},
	} {
		if _, err := ts.Store.Pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}

	status, body := ts.session(t, key, id, http.MethodPost, "/v1/grants/"+*granted.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("values of the pod's grant after the gap = %d %s, want 200", status, body)
	}
	if values := decode[struct {
		Values map[string]string `json:"values"`
	}](t, body); values.Values["WORKER_TOKEN"] != "worker-v1" {
		t.Fatalf("values = %+v, want WORKER_TOKEN=worker-v1", values.Values)
	}
	status, body = ts.session(t, key, id, http.MethodPost, "/v1/enrollments/"+id+"/renew", nil)
	if status != http.StatusOK {
		t.Fatalf("renew the pod whose lease ran out while no broker served = %d %s, want 200", status, body)
	}
	status, body = ts.session(t, lapsedKey, lapsed, http.MethodPost, "/v1/enrollments/"+lapsed+"/renew", nil)
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "ENROLLMENT_NOT_LIVE" {
		t.Fatalf("renew the pod whose lease ran out before the last beat = %d %s, want 401 ENROLLMENT_NOT_LIVE", status, body)
	}
}
