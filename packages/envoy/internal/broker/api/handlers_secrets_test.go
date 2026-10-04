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

func TestRereadIsRateLimitedPerAddress(t *testing.T) {
	ts := newTestServerWith(t, func(d *api.Deps) { d.RereadLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1} })
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/WORKER_TOKEN/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("first reread: %d %s", status, body)
	}
	request, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/secrets/WORKER_TOKEN/reread", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "3600" || decode[wireError](t, body).Code != "RATE_LIMITED" {
		t.Fatalf("second reread: %d Retry-After=%q %s, want 429 RATE_LIMITED with Retry-After 3600", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}
