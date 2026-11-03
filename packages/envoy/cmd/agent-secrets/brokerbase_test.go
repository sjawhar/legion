// packages/envoy/cmd/agent-secrets/brokerbase_test.go
//
// The broker's address as an operator writes it. AGENT_SECRETS_URL is typed by hand into a unit
// file, a pod's environment or a shell, so a URL ending in more than one slash is a typo rather
// than a shape to refuse — and the two halves of this one token client must read it the same way.
// The TypeScript half drops every slash a URL ends in
// (`packages/broker-client/src/token-client.ts`, `withoutTrailingSlashes`, and the same helper in
// `packages/envoy-client/src/dispatch-config.ts`); this half reads AGENT_SECRETS_URL through
// `brokerBaseURL`, which every form of the command uses.

package main

import "testing"

// TestBrokerBaseURLDropsEverySlashItEndsIn: one extra slash must not leave this half of the client
// building `//v1/...` where the other half builds `/v1/...`. A proof's target URI is the URL the
// caller built, so a path the broker's own mux then cleans no longer matches the proof, and the
// call is refused naming the credential rather than the URL an operator could fix.
func TestBrokerBaseURLDropsEverySlashItEndsIn(t *testing.T) {
	const want = "https://secrets.internal.example"
	for _, given := range []string{
		want,
		want + "/",
		want + "//",
		want + "///",
	} {
		t.Setenv(brokerURLVar, given)
		if got := brokerBaseURL(); got != want {
			t.Errorf("brokerBaseURL() with %s=%q = %q, want %q", brokerURLVar, given, got, want)
		}
	}
}
