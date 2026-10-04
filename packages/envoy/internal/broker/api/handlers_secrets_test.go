package api_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/ratelimit"
)

// wireSettings is GET /v1/settings's answer as the CLI reads it.
type wireSettings struct {
	SecretsPrefix string `json:"secrets_prefix"`
	KMSKeyARN     string `json:"kms_key_arn"`
	AWSAccountID  string `json:"aws_account_id"`
	AWSRegion     string `json:"aws_region"`
}

// wireReread is POST /v1/secrets/{name}/reread's answer.
type wireReread struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
}

func TestSettingsAnswersTheNamespaceAndKey(t *testing.T) {
	ts := newTestServer(t)
	status, body := ts.req(t, http.MethodGet, "/v1/settings", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	got := decode[wireSettings](t, body)
	want := wireSettings{SecretsPrefix: policytest.Prefix, KMSKeyARN: policytest.KeyARN, AWSAccountID: "111122223333", AWSRegion: "us-east-1"}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

func TestRereadServesANewSecretAndDropsADeletedOne(t *testing.T) {
	ts := newTestServer(t)
	ts.Secrets.Put(policytest.Secret("NEW_KEY", testApprover, policy.TierAgent, "v1"))
	status, body := ts.req(t, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reread: %d %s", status, body)
	}
	if r := decode[wireReread](t, body); !r.Served || r.Reason != "" {
		t.Fatalf("reread = %+v, want served", r)
	}
	ts.Secrets.Delete(policytest.ID("NEW_KEY"))
	_, body = ts.req(t, http.MethodPost, "/v1/secrets/NEW_KEY/reread", nil, nil)
	if r := decode[wireReread](t, body); r.Served || r.Reason != policy.ReasonAbsent {
		t.Fatalf("after delete = %+v, want absent", r)
	}
	if status, body = ts.req(t, http.MethodPost, "/v1/secrets/not_a_name/reread", nil, nil); status != http.StatusBadRequest || decode[wireError](t, body).Code != "SECRET_NAME_INVALID" {
		t.Fatalf("malformed name: %d %s, want 400 SECRET_NAME_INVALID", status, body)
	}
}

// postReread sends one reread of name with headers, answering the status, headers and body: the
// 429 each reread limit answers carries a Retry-After, which testServer.req does not give back.
func postReread(t *testing.T, ts *testServer, name string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/secrets/"+name+"/reread", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, body
}

func TestRereadIsRateLimitedPerAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1} })
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("first reread: %d %s", status, body)
	}
	status, headers, body := postReread(t, ts, "WORKER_TOKEN", nil)
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("second reread: %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600", status, headers.Get("Retry-After"), body)
	}
}

// TestRereadIsRateLimitedAcrossEveryAddress pins the broker-wide bound. Every reread takes the
// policy's writer lock, so a flood spread over addresses that are each on their first reread —
// well inside their own limit — is refused all the same, and names the broker-wide bucket's own
// Retry-After.
func TestRereadIsRateLimitedAcrossEveryAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.RereadOverallLimit = &ratelimit.Limit{Every: time.Hour, Burst: 2}
		d.TrustedProxyHeader = "X-Forwarded-For"
	})
	for _, address := range []string{"198.51.100.1", "198.51.100.2"} {
		if status, _, body := postReread(t, ts, "WORKER_TOKEN", map[string]string{"X-Forwarded-For": address}); status != http.StatusOK {
			t.Fatalf("reread from %s: %d %s, want 200 (its own first reread)", address, status, body)
		}
	}
	status, headers, body := postReread(t, ts, "WORKER_TOKEN", map[string]string{"X-Forwarded-For": "198.51.100.3"})
	if status != http.StatusTooManyRequests || headers.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("third address: %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600 past the broker-wide burst", status, headers.Get("Retry-After"), body)
	}
}
