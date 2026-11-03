package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// controllerBrokerToken is the access token the stub broker mints: a value no file and no
// environment value of the controller's may hold, since the session trades for its own.
const controllerBrokerToken = "broker-access-token-9f3a"

// stubBroker is the secrets broker's token endpoint and nothing else: it answers every trade
// with controllerBrokerToken, or, when refusal is set, with that OAuth refusal and a 400. It
// keeps the form each trade posted, which is where the session name and the launcher-credential
// parameter are read from.
type stubBroker struct {
	url    string
	trades atomic.Int64
	mu     sync.Mutex
	forms  []url.Values
}

// newStubBroker serves POST /v1/oauth/token, answering refusal's words when it is not empty.
func newStubBroker(t *testing.T, refusal string) *stubBroker {
	return newStubBrokerFixingASession(t, refusal, "")
}

// newStubBrokerFixingASession is newStubBroker for a broker whose entry for this machine fixes
// the session fixed: a trade naming any other name is refused exactly as the exchange's
// resolveSession refuses it (ErrSessionMismatch, invalid_grant, naming what the machine writes
// as), and a trade naming that one is taken.
func newStubBrokerFixingASession(t *testing.T, refusal, fixed string) *stubBroker {
	t.Helper()
	broker := &stubBroker{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broker.trades.Add(1)
		form := url.Values{}
		if err := r.ParseForm(); err == nil {
			form = r.PostForm
		}
		session := form.Get("session")
		broker.mu.Lock()
		broker.forms = append(broker.forms, form)
		broker.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		said := refusal
		if said == "" && fixed != "" && session != fixed {
			said = "this machine writes as " + fixed
		}
		if said != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": said})
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

// tradedSessions is the session name each trade asked under, in order.
func (b *stubBroker) tradedSessions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	asked := make([]string, 0, len(b.forms))
	for _, form := range b.forms {
		asked = append(asked, form.Get("session"))
	}
	return asked
}

// tradedLauncherCredential is what each trade said about the launcher credential its identity
// holds, in order: "false" declines it, and "" is a trade that asked by sending nothing.
func (b *stubBroker) tradedLauncherCredential() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	asked := make([]string, 0, len(b.forms))
	for _, form := range b.forms {
		asked = append(asked, form.Get("launcher_credential"))
	}
	return asked
}

// provesAnIdentity has the operator's machine prove a Kubernetes identity of its own at broker,
// as a pod running `legion controller start` does. What the controller does with that identity
// is its configuration's word: `dispatch_token_broker` (controllerOptions.dispatchBroker).
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

// TestAControllerOnTheBrokerRouteIsHandedNoBearerPointer: the broker's access token is
// DPoP-bound — Dispatch refuses it presented as a bearer (RFC 9449 Section 7.2) — and the Envoy
// client takes the broker route only where no bearer resolves at all. So a controller whose
// configuration names `dispatch_token_broker` is handed DISPATCH_URL and the broker's own
// variables, and neither DISPATCH_TOKEN_FILE nor DISPATCH_TOKEN: a pointer to such a token is
// one the session presents as a bearer and Dispatch refuses, which is the whole of the session's
// Dispatch writing.
func TestAControllerOnTheBrokerRouteIsHandedNoBearerPointer(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
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
		// What the client trades with, which it reads from this environment itself. The session
		// name among them is the operator's own, for the `agent-secrets` the controller's bash
		// runs: the controller's session acts as a session, so its Dispatch credential is traded
		// under its own session id and under no variable (brokerclient.TradeSession).
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
	// The check trades under a session name of its own, never the operator's variable: Oh My Pi
	// mints the controller's session id at session start, so a check that traded under a name no
	// session will use would pass for an entry that fixes that name while refusing the session.
	asked := broker.tradedSessions()
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "legion-controller-start-") || asked[0] == "legion-demo-controller" {
		t.Errorf("the check traded under %q, want one name of its own beginning legion-controller-start-", asked)
	}
	// And it declines the launcher credential its identity holds: this check enrols no session
	// and reads no secret, and its key dies with the call, so a credential minted for it would
	// put a machine login and a token_login audit row on the operator's list at every start,
	// keyed by a thumbprint nothing holds afterwards.
	if declined := broker.tradedLauncherCredential(); len(declined) != 1 || declined[0] != "false" {
		t.Errorf("the check's trade sent launcher_credential %q, want one trade sending \"false\"", declined)
	}
}

// TestAnEntryThatFixesASessionRefusesTheControllerStart: an entry that fixes a session answers
// every trade naming another `invalid_grant`, and the controller's session names its own id,
// which no deployment can have fixed. So a Kubernetes-identity controller whose entry fixes a
// session can write nothing, and the start says what the broker said instead of launching it.
// The operator's AGENT_SECRETS_SESSION naming that very session does not rescue it: the session
// does not trade under that variable.
func TestAnEntryThatFixesASessionRefusesTheControllerStart(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
	c.provesAnIdentity(newStubBrokerFixingASession(t, "", "legion-demo-controller"))

	c.refused("writes as legion-demo-controller")
	c.wantNoSecretRequest()
	if c.launched() {
		t.Error("Oh My Pi was launched for a controller whose every Dispatch write the broker refuses")
	}
}

// TestAnOperatorsWrittenTokenDoesNotShadowTheControllersBrokerRoute: the controller keeps the
// operator's HOME, and the envoy.json there is that person's own, `dispatch.token` and all, for
// the very Dispatch the controller writes. Dropping DISPATCH_TOKEN and DISPATCH_TOKEN_FILE does
// not reach a file, and the Envoy client resolves a written token before it trades anything: the
// controller would write Dispatch as the operator, which is the whole of what
// `dispatch_token_broker` exists to stop. So the environment says which route this session is on
// — DISPATCH_TOKEN_BROKER=1 — under which the client reads no bearer from any source at all and
// turns Dispatch off rather than falling back (packages/envoy-client/src/dispatch-config.ts).
func TestAnOperatorsWrittenTokenDoesNotShadowTheControllersBrokerRoute(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
	c.provesAnIdentity(newStubBroker(t, ""))
	// `~/.config/legion/envoy.json`, as a person who runs `dispatch` on this box has it: the
	// chosen file of every session started in this HOME, the controller's included.
	configDir := filepath.Join(c.home, ".config", "legion")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	written := `{"dispatch":{"serverUrl":"https://dispatch.test","token":"a-person-s-dispatch-token"}}` + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "envoy.json"), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errb := c.run()
	if code != 0 {
		t.Fatalf("legion controller start = %d, stderr %q", code, errb)
	}
	for _, env := range []map[string]string{c.env(), c.probeEnv()} {
		// The premise: that file is the controller's to read, since this is the operator's HOME.
		if got := env["HOME"]; got != c.home {
			t.Errorf("HOME = %q, want the operator's %q", got, c.home)
		}
		if got := env["DISPATCH_TOKEN_BROKER"]; got != "1" {
			t.Errorf("DISPATCH_TOKEN_BROKER = %q, want 1: without it the envoy.json in %s writes Dispatch as the operator", got, configDir)
		}
		for _, name := range []string{"DISPATCH_TOKEN_FILE", "DISPATCH_TOKEN"} {
			if value, set := env[name]; set {
				t.Errorf("%s = %q; this session trades for its own credential", name, value)
			}
		}
	}
}

// TestABrokerThatRefusesThisMachineRefusesTheStartBeforeTheMint: the credential the controller
// writes Dispatch with is traded at the broker, so a broker that refuses this machine's identity
// is a controller that can write nothing. The start says what the broker said and stops before
// the daemon mints a capability, which would have revoked the running controller's.
func TestABrokerThatRefusesThisMachineRefusesTheStartBeforeTheMint(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
	c.provesAnIdentity(newStubBroker(t, "the fleet is listed with no session of its own, so the request names one"))

	c.refused("is listed with no session of its own")
	c.wantNoSecretRequest()
	if c.launched() {
		t.Error("Oh My Pi was launched for a controller that can write nothing to Dispatch")
	}
}

// TestABrokerRouteWithNoBrokerRefusesTheStart: a deployment that put the controller on the broker
// route and named no broker has a mistake in it, and nothing of the controller is started on it.
func TestABrokerRouteWithNoBrokerRefusesTheStart(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
	c.provesAnIdentity(newStubBroker(t, ""))
	t.Setenv("AGENT_SECRETS_URL", "")

	c.refused("must name the secrets broker it proves it to")
	c.wantNoSecretRequest()
}

// TestABrokerRouteOnAMachineThatProvesNothingRefusesTheStart: `dispatch_token_broker` says the
// controller's Dispatch credential is this machine's own identity, so a machine that proves none
// is a controller that could write nothing. The refusal names the variable that would carry it.
func TestABrokerRouteOnAMachineThatProvesNothingRefusesTheStart(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{dispatchBroker: true})
	t.Setenv("AGENT_SECRETS_MACHINE_IDENTITY", "")

	c.refused("dispatch_token_broker is set, so the controller's Dispatch credential is this machine's own identity, and AGENT_SECRETS_MACHINE_IDENTITY names none")
	c.wantNoSecretRequest()
}

// TestADispatchURLWithNoCredentialRefusesTheStart: the loader takes a `dispatch_url` with neither
// credential, since it reads no environment and only a trade proves the broker route; the start
// is where that file is refused, naming both of the keys that would settle it.
func TestADispatchURLWithNoCredentialRefusesTheStart(t *testing.T) {
	d := newControllerDaemon(t)
	c := newControllerStart(t, d, controllerOptions{lines: []string{
		"project: " + controllerProject, "daemon_url: " + d.url, "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]",
		"dispatch_url: https://dispatch.test",
	}})

	c.refused("dispatch_url is configured with no Dispatch credential: set dispatch_token_file to the bearer's file, or dispatch_token_broker: true where this machine proves an identity of its own and the controller's session trades it at the secrets broker")
	c.wantNoSecretRequest()
}
