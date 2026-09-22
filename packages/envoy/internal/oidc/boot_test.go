package oidc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDiscoverRefusesAnIssuerThatNeverAnswers is the case a 404 issuer does not
// cover: a partition or a black-holing proxy accepts the connection and then
// says nothing. Without a deadline the boot hangs there forever instead of
// refusing, and a hung boot never binds a port, so nothing reports it.
func TestDiscoverRefusesAnIssuerThatNeverAnswers(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Cleanups run LIFO, so these two are registered in the order that makes the
	// accept goroutine finish first: closing the listener makes Accept return, the
	// goroutine exits, and only then is the channel closed and drained. Registered
	// the other way round, a connection landing in the window between the close and
	// the goroutine's exit would be a send on a closed channel.
	accepted := make(chan net.Conn, 4)
	t.Cleanup(func() {
		close(accepted)
		for conn := range accepted {
			_ = conn.Close()
		}
	})
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			accepted <- conn // held open, never written to
		}
	}()

	issuer := "http://" + silent.Addr().String()
	start := time.Now()
	verifier, err := Discover(context.Background(), issuer, "dispatch", 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Discover against a silent issuer returned verifier %#v and no error", verifier)
	}
	if !strings.Contains(err.Error(), issuer) {
		t.Errorf("Discover error %q does not name the issuer %s", err, issuer)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Discover took %s to give up, want the deadline to bound it", elapsed)
	}
}

// TestDiscoverIsNilWhenNoIssuerIsConfigured pins the "unset means no verifier"
// half of the contract, which is what keeps both binaries byte-identical to
// their pre-OIDC behaviour.
func TestDiscoverIsNilWhenNoIssuerIsConfigured(t *testing.T) {
	verifier, err := Discover(context.Background(), "", "", time.Second)
	if err != nil || verifier != nil {
		t.Fatalf("Discover(\"\") = %#v, %v; want nil, nil", verifier, err)
	}
}

// TestConfigFromEnvHoldsBothOrNeither: the rule each binary used to write for
// itself. The refusal names the variable the operator has to set, because the
// one they did set is not the one that is missing.
func TestConfigFromEnvHoldsBothOrNeither(t *testing.T) {
	const issuerVar, audienceVar = "X_OIDC_ISSUER", "X_OIDC_AUDIENCE"
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	issuer, audience, err := ConfigFromEnv(env(nil), issuerVar, audienceVar)
	if err != nil || issuer != "" || audience != "" {
		t.Fatalf("neither set: %q, %q, %v; want empty, empty, nil", issuer, audience, err)
	}

	_, _, err = ConfigFromEnv(env(map[string]string{issuerVar: "https://oidc.example"}), issuerVar, audienceVar)
	if err == nil || !strings.Contains(err.Error(), audienceVar) {
		t.Fatalf("issuer alone: err = %v, want one naming %s", err, audienceVar)
	}

	_, _, err = ConfigFromEnv(env(map[string]string{audienceVar: "dispatch"}), issuerVar, audienceVar)
	if err == nil || !strings.Contains(err.Error(), issuerVar) {
		t.Fatalf("audience alone: err = %v, want one naming %s", err, issuerVar)
	}

	issuer, audience, err = ConfigFromEnv(env(map[string]string{
		issuerVar: "  https://oidc.example  ", audienceVar: " dispatch ",
	}), issuerVar, audienceVar)
	if err != nil || issuer != "https://oidc.example" || audience != "dispatch" {
		t.Fatalf("both set: %q, %q, %v; want the trimmed pair", issuer, audience, err)
	}
}
