package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// controllerBrokerToken is the access token the stub broker mints: a value no file and no
// environment value of the controller's may hold, since the session trades for its own.
const controllerBrokerToken = "broker-access-token-9f3a"

// stubBroker is the secrets broker's token endpoint and nothing else: it answers every trade
// with controllerBrokerToken, or, when refusal is set, with that OAuth refusal and a 400.
type stubBroker struct {
	url    string
	trades atomic.Int64
}

// newStubBroker serves POST /v1/oauth/token, answering refusal's words when it is not empty.
func newStubBroker(t *testing.T, refusal string) *stubBroker {
	t.Helper()
	broker := &stubBroker{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broker.trades.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if refusal != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "error_description": refusal})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": controllerBrokerToken, "token_type": "DPoP", "expires_in": 3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	}))
	t.Cleanup(server.Close)
	broker.url = server.URL
	return broker
}

// provesAnIdentity has the operator's machine prove a Kubernetes identity of its own at broker,
// as a pod running `legion controller start` does.
func (c *operatorMachine) provesAnIdentity(broker *stubBroker) {
	c.t.Helper()
	projected := filepath.Join(c.dir, "projected-token")
	if err := os.WriteFile(projected, []byte("projected.service.account.token\n"), 0o600); err != nil {
		c.t.Fatal(err)
	}
	c.t.Setenv("AGENT_SECRETS_URL", broker.url)
	c.t.Setenv("AGENT_SECRETS_MACHINE_IDENTITY", "kubernetes")
	c.t.Setenv("AGENT_SECRETS_MACHINE_TOKEN_FILE", projected)
	c.t.Setenv("AGENT_SECRETS_SESSION", "legion-demo-controller")
}

// TestAControllerWithAMachineIdentityIsHandedNoBearerPointer: the broker's access token is
// DPoP-bound — Dispatch refuses it presented as a bearer (RFC 9449 Section 7.2) — and the Envoy
// client takes the broker route only where no bearer resolves at all. So a controller whose
// machine proves an identity is handed DISPATCH_URL and the broker's own variables, and neither
// DISPATCH_TOKEN_FILE nor DISPATCH_TOKEN: a pointer to such a token is one the session presents
// as a bearer and Dispatch refuses, which is the whole of the session's Dispatch writing.
func TestAControllerWithAMachineIdentityIsHandedNoBearerPointer(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	broker := newStubBroker(t, "")
	c.provesAnIdentity(broker)
	// The operator's own shell carries a Dispatch bearer and a pointer of its own; both would
	// be resolved before the broker route and presented in its place.
	t.Setenv("DISPATCH_TOKEN", "a-person-s-dispatch-token")
	t.Setenv("DISPATCH_TOKEN_FILE", filepath.Join(c.dir, "dispatch-token"))

	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for _, env := range []map[string]string{c.env(), c.probeEnv()} {
		for _, name := range []string{"DISPATCH_TOKEN_FILE", "DISPATCH_TOKEN"} {
			if value, set := env[name]; set {
				t.Errorf("%s = %q; a machine with an identity of its own is handed no bearer, and Dispatch refuses this token as one", name, value)
			}
		}
		if got := env["DISPATCH_URL"]; got != "https://dispatch.test" {
			t.Errorf("DISPATCH_URL = %q, want the configured Dispatch", got)
		}
		// What the client trades with, which it reads from this environment itself.
		for name, want := range map[string]string{
			"AGENT_SECRETS_URL":                broker.url,
			"AGENT_SECRETS_MACHINE_IDENTITY":   "kubernetes",
			"AGENT_SECRETS_MACHINE_TOKEN_FILE": filepath.Join(c.dir, "projected-token"),
			"AGENT_SECRETS_SESSION":            "legion-demo-controller",
		} {
			if got := env[name]; got != want {
				t.Errorf("%s = %q, want %q: the session trades for its own credential", name, got, want)
			}
		}
		for name, value := range env {
			if strings.Contains(value, controllerBrokerToken) {
				t.Errorf("%s carries the broker's access token", name)
			}
		}
	}

	// Nothing of the start's own trade is written down: no token file for the session to point
	// at, and no renewal log, since the session renews its own credential.
	if _, err := os.Stat(filepath.Join(c.defaultDir, "secrets", "dispatch-token")); !os.IsNotExist(err) {
		t.Errorf("a Dispatch token file exists (%v); the session's credential is never written down", err)
	}
	if _, err := os.Stat(filepath.Join(c.defaultDir, "dispatch-token.log")); !os.IsNotExist(err) {
		t.Errorf("a Dispatch token renewal log exists (%v); this process renews nothing", err)
	}
	if err := filepath.WalkDir(c.defaultDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), controllerBrokerToken) {
			t.Errorf("%s holds the broker's access token", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the state directory: %v", err)
	}
	if trades := broker.trades.Load(); trades != 1 {
		t.Errorf("the start made %d trades, want the one that proves this machine can hold a credential", trades)
	}
}

// TestABrokerThatRefusesThisMachineRefusesTheStartBeforeTheMint: the credential the controller
// writes Dispatch with is traded at the broker, so a broker that refuses this machine's identity
// is a controller that can write nothing. The start says what the broker said and stops before
// the daemon mints a capability, which would have revoked the running controller's.
func TestABrokerThatRefusesThisMachineRefusesTheStartBeforeTheMint(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	c.provesAnIdentity(newStubBroker(t, "the fleet is listed with no session of its own, so the request names one"))

	c.refused("is listed with no session of its own")
	c.wantNoSecretRequest()
	if c.launched() {
		t.Error("Oh My Pi was launched for a controller that can write nothing to Dispatch")
	}
}

// TestAMachineIdentityWithNoBrokerRefusesTheStart: a deployment that named an identity and no
// broker has a mistake in it, and nothing of the controller is started on it.
func TestAMachineIdentityWithNoBrokerRefusesTheStart(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{})
	c.provesAnIdentity(newStubBroker(t, ""))
	t.Setenv("AGENT_SECRETS_URL", "")

	c.refused("must name the secrets broker it proves it to")
	c.wantNoSecretRequest()
}
