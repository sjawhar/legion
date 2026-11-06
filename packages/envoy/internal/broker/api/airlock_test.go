package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/api"
)

// wireAirlockValues is POST /v1/grants/{id}/values's answer as a session reads it when the grant
// holds a name behind the airlock: every field by its wire name, so a change to one of them
// fails here.
type wireAirlockValues struct {
	Values  map[string]string `json:"values"`
	Airlock *struct {
		ProxyURL      string `json:"proxy_url"`
		CACertificate string `json:"ca_certificate"`
		ProxyPassword string `json:"proxy_password"`
		Names         map[string]struct {
			Value string            `json:"value"`
			Slots map[string]string `json:"slots"`
			Hosts []string          `json:"hosts"`
		} `json:"names"`
	} `json:"airlock"`
}

// TestTheValuesAnswerCarriesTheAirlock: a release of a grant whose one name the rules name hands
// the session no value at all and the airlock instead — the proxy, its certificate, the password
// its connections open with, and the name's placeholder, slots and hosts. A session that gets
// this and no "values" entry for the name is what makes an old CLI refuse to run rather than run
// with a placeholder it does not understand.
func TestTheValuesAnswerCarriesTheAirlock(t *testing.T) {
	const (
		proxyURL      = "http://127.0.0.1:13381"
		caCertificate = "-----BEGIN CERTIFICATE-----\ntest-airlock-ca\n-----END CERTIFICATE-----\n"
	)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.Machine.Airlock = airlock.Rules{Secrets: []airlock.SecretRule{{Name: "WORKER_TOKEN", Hosts: []string{"api.example.com"}}}}
		d.Machine.AirlockProxyURL, d.Machine.AirlockCACertificate = proxyURL, caCertificate
	})
	enrollmentID, sessionKey := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	_, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/requests",
		map[string]any{"request": signAgentSecretRequest(t, sessionKey, ts.URL, "read the worker token", "WORKER_TOKEN"), "session_id": nil})
	created := decode[wireCreateRequestResponse](t, body)
	if created.GrantID == nil {
		t.Fatalf("POST /v1/requests = %s, want an automatic grant", body)
	}

	status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/grants/"+*created.GrantID+"/values", nil)
	if status != http.StatusOK {
		t.Fatalf("POST values = %d: %s", status, body)
	}
	answer := decode[wireAirlockValues](t, body)
	if len(answer.Values) != 0 {
		t.Fatalf("values = %v; want none: the one granted name is behind the airlock", answer.Values)
	}
	if answer.Airlock == nil {
		t.Fatalf("the answer carries no airlock: %s", body)
	}
	if answer.Airlock.ProxyURL != proxyURL || answer.Airlock.CACertificate != caCertificate {
		t.Errorf("airlock = %+v; want the proxy %q and its certificate", answer.Airlock, proxyURL)
	}
	held, named := answer.Airlock.Names["WORKER_TOKEN"]
	if !named || len(answer.Airlock.Names) != 1 {
		t.Fatalf("airlock names = %v; want WORKER_TOKEN alone", answer.Airlock.Names)
	}
	if !strings.HasPrefix(held.Value, "agent-secrets-placeholder-") {
		t.Errorf("WORKER_TOKEN = %q; want a placeholder", held.Value)
	}
	if held.Slots["value"] != held.Value || len(held.Slots) != 1 {
		t.Errorf("WORKER_TOKEN's slots = %v; want the value slot holding %q", held.Slots, held.Value)
	}
	if len(held.Hosts) != 1 || held.Hosts[0] != "api.example.com" {
		t.Errorf("WORKER_TOKEN's hosts = %v; want the entry's", held.Hosts)
	}
	if answer.Airlock.ProxyPassword != held.Value {
		t.Errorf("proxy password = %q; want the grant's one placeholder %q", answer.Airlock.ProxyPassword, held.Value)
	}
	if strings.Contains(string(body), "worker-v1") {
		t.Errorf("the answer carries WORKER_TOKEN's real value: %s", body)
	}
	// The field order and the whole set of keys, which is the contract a client reads.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"expires_at", "values", "airlock"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("the answer has no %q: %s", want, body)
		}
	}
	if len(keys) != 3 {
		t.Errorf("the answer carries %d fields: %s; want expires_at, values and airlock", len(keys), body)
	}
}
