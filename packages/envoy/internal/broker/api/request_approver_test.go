// request_approver_test.go pins the approver POST /v1/requests and GET /v1/requests/{id} answer:
// whom the request's credential-request record names to decide it, a person's login or anyone
// signed in to Dispatch, so the asking session can tell whom it waits on. A request no person
// decides answers null.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// approverService is the one service newApproverTestServer registers, and SERVICE_KEY's owner.
const approverService = "example-service"

// newApproverTestServer is newTestServer plus SERVICE_KEY, an agent-tier secret approverService
// owns, with approverService registered to pods running as workerAccount, as BROKER_SERVICES
// registers a service.
func newApproverTestServer(t *testing.T) *testServer {
	t.Helper()
	return newTestServerWith(t, func(d *api.Deps) {
		local := d.Machine.Secrets.(secrets.AWS).Client.(*secrets.Local)
		local.Put(policytest.Secret("SERVICE_KEY", approverService, policy.TierAgent, "service-v1"))
		cur := policytest.Current(t, local, approverService)
		d.Policy, d.Machine.Policy, d.MachineLogin.Policy = cur, cur, cur
		d.Machine.ServiceAccounts = map[string]string{approverService: workerAccount}
	})
}

// newServicePod inserts a live pod enrollment under a launcher credential naming approverService,
// running as workerAccount: the pod a service's launcher enrolls, whose service owns its secrets.
func (ts *testServer) newServicePod(t *testing.T) (id string, key *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	key = newSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	credentialID := uuid.New()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'cluster.example',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, approverService, uuid.NewString()); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	id = uuid.NewString()
	if _, err := ts.Store.Pool.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, subject, launcher_credential_id, lease_expires_at)
		values ($1,'pod',$2,$3,$4,$5, now() + interval '1 hour')`, id, uuid.NewString(), thumbprint, workerAccount, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return id, key
}

// approverAnswer is the fields of a request answer this file reads. Approver is the field's raw
// JSON: nil when the body has no approver, `null` when it is null, and the quoted login otherwise.
type approverAnswer struct {
	RequestID string          `json:"request_id"`
	State     string          `json:"state"`
	RecordID  *string         `json:"record_id"`
	Coalesced bool            `json:"coalesced"`
	Approver  json.RawMessage `json:"approver"`
}

// approverJSON is login as the approver field carries it: a JSON string, or null for "".
func approverJSON(login string) string {
	if login == "" {
		return "null"
	}
	quoted, _ := json.Marshal(login)
	return string(quoted)
}

// ask posts a request for names as the session, failing t on anything but 200.
func (ts *testServer) ask(t *testing.T, key *ecdsa.PrivateKey, enrollmentID string, names ...string) approverAnswer {
	t.Helper()
	status, body := ts.session(t, key, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, key, ts.URL, "need it", names...), "session_id": nil})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/requests %v = %d: %s", names, status, body)
	}
	return decode[approverAnswer](t, body)
}

// statusOf reads GET /v1/requests/{id} as the session, failing t on anything but 200.
func (ts *testServer) statusOf(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, requestID string) (approverAnswer, []byte) {
	t.Helper()
	status, body := ts.session(t, key, enrollmentID, http.MethodGet, "/v1/requests/"+requestID, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/requests/%s = %d: %s", requestID, status, body)
	}
	return decode[approverAnswer](t, body), body
}

// TestARequestNamesWhomItWaitsOn pins approver on both answers: a request for a person's
// human-tier secret names that person, the same login its record names; one for a shared
// human-tier secret names record.AnyoneApprover; and a request the policy decided at once - an
// agent-tier secret its session gets without asking, a service's secret granted to that service's
// pod, or a service's secret denied to a person's session - answers approver null, present.
func TestARequestNamesWhomItWaitsOn(t *testing.T) {
	ts := newApproverTestServer(t)
	person, personKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "carol@example.com")
	pod, podKey := ts.newServicePod(t)
	for _, c := range []struct {
		name       string
		enrollment string
		key        *ecdsa.PrivateKey
		secret     string
		state      string
		approver   string // "" for null
	}{
		{"a person's human-tier secret", person, personKey, "DEEL_API_KEY", "pending", testApprover},
		{"a shared human-tier secret", person, personKey, "SHARED_KEY", "pending", record.AnyoneApprover},
		{"a shared agent-tier secret", person, personKey, "WORKER_TOKEN", "granted", ""},
		{"a service's secret, to its pod", pod, podKey, "SERVICE_KEY", "granted", ""},
		{"a service's secret, to a person", person, personKey, "SERVICE_KEY", "denied", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := approverJSON(c.approver)
			created := ts.ask(t, c.key, c.enrollment, c.secret)
			if created.State != c.state || string(created.Approver) != want {
				t.Fatalf("POST /v1/requests %s = state %q approver %q; want state %q approver %s",
					c.secret, created.State, created.Approver, c.state, want)
			}
			read, body := ts.statusOf(t, c.key, c.enrollment, created.RequestID)
			if read.State != c.state || string(read.Approver) != want {
				t.Fatalf("GET /v1/requests/{id} for %s = %s; want state %q approver %s", c.secret, body, c.state, want)
			}
			if c.approver == "" {
				return
			}
			_, body = ts.ui(t, http.MethodGet, "/v1/credential-requests/"+*created.RecordID, nil)
			if got := decode[wireRecord](t, body).Approver; got != c.approver {
				t.Fatalf("record %s names approver %q; the request answered %q", *created.RecordID, got, c.approver)
			}
		})
	}
}

// TestARequestKeepsTheApproverItsRecordNamesAfterTheOwnerChanges pins that approver is read from
// the request's record, which the Inbox lists, never from the current policy: a request made while
// its secret was one person's still answers that person once the secret's owner tag names another,
// on its own status and on the same session's identical request, which joins it; the record stays
// on the first owner's pending list and off the new owner's; that first owner may still deny it,
// and the denied request still names them; and a new session's request, which the current policy
// decides, names the new owner.
func TestARequestKeepsTheApproverItsRecordNamesAfterTheOwnerChanges(t *testing.T) {
	ts := newTestServer(t)
	const newOwner = "bob@example.com"
	session, sessionKey := ts.newSessionEnrollment(t, "box", "box-"+t.Name(), "carol@example.com")
	first := ts.ask(t, sessionKey, session, "DEEL_API_KEY")
	owner, wantOwner := testApprover, approverJSON(testApprover)
	if first.State != "pending" || string(first.Approver) != wantOwner {
		t.Fatalf("POST /v1/requests DEEL_API_KEY = state %q approver %q; want pending on %s", first.State, first.Approver, owner)
	}

	ts.Secrets.Put(policytest.Secret("DEEL_API_KEY", newOwner, policy.TierHuman, "deel-v1"))
	if status, body := ts.req(t, http.MethodPost, "/v1/secrets/DEEL_API_KEY/reread", nil, nil); status != http.StatusOK || !decode[wireReread](t, body).Served {
		t.Fatalf("reread DEEL_API_KEY after the owner change = %d %s", status, body)
	}

	if read, body := ts.statusOf(t, sessionKey, session, first.RequestID); read.State != "pending" || string(read.Approver) != wantOwner {
		t.Fatalf("GET /v1/requests/{id} after the owner change = %s; want pending on %s, whom its record names", body, owner)
	}
	again := ts.ask(t, sessionKey, session, "DEEL_API_KEY")
	if !again.Coalesced || again.RequestID != first.RequestID || string(again.Approver) != wantOwner {
		t.Fatalf("the same session asking again = %+v; want it joined to %s, on %s", again, first.RequestID, owner)
	}
	if !pendingFor(t, ts, owner, *first.RecordID) || pendingFor(t, ts, newOwner, *first.RecordID) {
		t.Fatalf("record %s: want it on %s's pending list and off %s's", *first.RecordID, owner, newOwner)
	}
	status, body := ts.ui(t, http.MethodPost, "/v1/credential-requests/"+*first.RecordID+"/deny", map[string]any{"approver": owner})
	if status != http.StatusOK {
		t.Fatalf("deny as %s after the owner change = %d %s", owner, status, body)
	}
	if read, body := ts.statusOf(t, sessionKey, session, first.RequestID); read.State != "denied" || string(read.Approver) != wantOwner {
		t.Fatalf("GET /v1/requests/{id} once denied = %s; want denied, still naming %s", body, owner)
	}

	other, otherKey := ts.newSessionEnrollment(t, "box", "box-other-"+t.Name(), "carol@example.com")
	fresh := ts.ask(t, otherKey, other, "DEEL_API_KEY")
	if fresh.State != "pending" || string(fresh.Approver) != approverJSON(newOwner) {
		t.Fatalf("a new session's request after the owner change = state %q approver %q; want pending on %s", fresh.State, fresh.Approver, newOwner)
	}
}

// pendingFor reports whether GET /v1/pending lists recordID for approver.
func pendingFor(t *testing.T, ts *testServer, approver, recordID string) bool {
	t.Helper()
	status, body := ts.ui(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(approver), nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/pending?approver=%s = %d: %s", approver, status, body)
	}
	for _, p := range decode[struct {
		Pending []wirePendingEntry `json:"pending"`
	}](t, body).Pending {
		if p.RecordID == recordID {
			return true
		}
	}
	return false
}
