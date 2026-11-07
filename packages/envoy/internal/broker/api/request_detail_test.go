// request_detail_test.go pins the detail POST /v1/requests and GET /v1/requests/{id} answer: the
// broker's own sentence about why it denied a request — the secret that refused it and the rule
// that did — so the session that asked reads which secret it may not have and that asking again
// will not help. A request the policy did not deny answers null on both routes, present, so a
// client reads the absence of a reason rather than the absence of the field.
package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
)

// listedRequester is the one person GCLOUD_SAMI's requesters tag names below, and it is not the
// person whose session asks: the refusal under test is the list's, not the owner's or the tier's.
const listedRequester = "someone-else@example.com"

// listedLifetime is the lifetime every request below signs, an hour in seconds. GCLOUD_SAMI caps
// its grants at an hour of its own accord (policytest.FixtureMaxGrant), and a request signing
// longer than a secret's own max-grant is refused 400 before the policy decides it at all, which
// is not this file's question. The cap is no rule of the list's — policy.TagRules demands one of
// an admin secret and of a google-service-account one alone, and the other secrets here set none
// — so an hour is simply a length every secret on this server allows.
const listedLifetime = 3600

// detailAnswer is the fields of a request answer this file reads. Detail is the field's raw JSON,
// so an answer carrying no detail at all is told apart from one carrying null.
type detailAnswer struct {
	RequestID string          `json:"request_id"`
	State     string          `json:"state"`
	Detail    json.RawMessage `json:"detail"`
}

// detailText reads an answer's detail field: "" for null, and the string it carries otherwise. A
// missing field, which the contract does not allow, fails the test rather than reading as null.
func detailText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("the answer carries no detail field at all; the route's contract defines one")
	}
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("detail %s is neither a string nor null: %v", raw, err)
	}
	if text == nil {
		return ""
	}
	return *text
}

// TestARefusedRequestNamesTheSecretAndTheRuleOnBothRoutes: a session the requesters tag does not
// name is denied with no approval request, and both the answer to the request and a later read of
// it carry the broker's reason, naming the secret and requester-not-listed. A request the policy
// granted at once and one it left for a person to approve carry null: neither was refused, so
// there is nothing to explain.
func TestARefusedRequestNamesTheSecretAndTheRuleOnBothRoutes(t *testing.T) {
	ts := newTestServer(t)
	listed := policytest.Secret("GCLOUD_SAMI", testApprover, policy.TierAgent, "listed-v1")
	listed.Tags[policy.TagRequesters] = listedRequester
	listed.Tags[policy.TagMaxGrant] = policytest.FixtureMaxGrant
	ts.Secrets.Put(listed)
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/GCLOUD_SAMI/reread", nil, nil); status != http.StatusOK {
		t.Fatalf("reread GCLOUD_SAMI = %d: %s", status, body)
	}
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)

	for _, c := range []struct {
		name   string
		secret string
		state  string
		detail string // "" for null
	}{
		{"a secret whose requesters do not name the session", "GCLOUD_SAMI", "denied", "GCLOUD_SAMI: " + policy.ReasonRequesterNotListed},
		{"a secret the policy grants at once", "WORKER_TOKEN", "granted", ""},
		{"a secret a person must approve", "DEEL_API_KEY", "pending", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			compact := signAgentSecretRequestFor(t, sessionKey, ts.URL, "need it", listedLifetime, c.secret)
			status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
				map[string]any{"request": compact, "session_id": nil})
			if status != http.StatusOK {
				t.Fatalf("POST /v1/requests %s = %d: %s", c.secret, status, body)
			}
			created := decode[detailAnswer](t, body)
			if created.State != c.state || detailText(t, created.Detail) != c.detail {
				t.Fatalf("POST /v1/requests %s = %s; want state %q detail %q", c.secret, body, c.state, c.detail)
			}

			status, body = ts.session(t, sessionKey, enrollmentID, http.MethodGet, "/v1/requests/"+created.RequestID, nil)
			if status != http.StatusOK {
				t.Fatalf("GET /v1/requests/%s = %d: %s", created.RequestID, status, body)
			}
			read := decode[detailAnswer](t, body)
			if read.State != c.state || detailText(t, read.Detail) != c.detail {
				t.Fatalf("GET /v1/requests/{id} for %s = %s; want state %q detail %q", c.secret, body, c.state, c.detail)
			}
		})
	}
}
