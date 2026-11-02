// packages/envoy/internal/brokerclient/projected_test.go
//
// A pod's whole path, which is an acceptance clause of dispatch://INFRA-958: the client is given
// a projected Kubernetes service-account token in a file, trades it for one of the broker's
// access tokens — the token its session writes Dispatch with — and renews that token before it
// expires, reading the file again as it does, because the kubelet rewrites a projected token in
// place and this grant issues no refresh token to spend instead.

package brokerclient

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProjectedServiceAccountTokenIsTradedAndRenewed drives one client from its first trade
// through one renewal, with nothing of the caller restarted in between.
func TestProjectedServiceAccountTokenIsTradedAndRenewed(t *testing.T) {
	const (
		firstProjected  = "projected.token.the.kubelet.wrote"
		secondProjected = "projected.token.the.kubelet.rewrote"
		lifetime        = 3600
	)
	path := filepath.Join(t.TempDir(), "token")
	writeProjected(t, path, firstProjected)

	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{
			"access_token":          []string{"dispatch-token-1", "dispatch-token-2"}[min(n, 1)],
			"issued_token_type":     "urn:ietf:params:oauth:token-type:access_token",
			"token_type":            "DPoP",
			"expires_in":            lifetime,
			"credential_id":         "cred-pod-1",
			"credential_expires_at": "2026-12-01T00:00:00Z",
		}
	})

	waits := make(chan time.Duration, 4)
	client, err := New(Config{
		BrokerURL:   broker.URL(),
		Key:         testKey(t),
		Subject:     NewProjectedToken(path),
		Session:     "example-worker-session",
		RenewBefore: 5 * time.Minute,
		After: func(d time.Duration) <-chan time.Time {
			waits <- d
			released := make(chan time.Time, 1)
			released <- time.Now()
			return released
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var held []*Token
	runErr := client.Run(ctx, func(context.Context, *Token) error {
		held = append(held, mustToken(t, client, ctx))
		switch len(held) {
		case 1:
			// The kubelet rewrites the projected token in place; the renewal must read it.
			writeProjected(t, path, secondProjected)
		case 2:
			stop()
		}
		return nil
	})
	if runErr != nil {
		t.Fatalf("Run: %v, want it to end with the context and nothing else", runErr)
	}

	if len(held) != 2 {
		t.Fatalf("the client held %d tokens, want the first and one renewal of it", len(held))
	}
	for i, want := range []string{"dispatch-token-1", "dispatch-token-2"} {
		if held[i].AccessToken != want {
			t.Errorf("the token in hand after trade %d is %q, want %q: a renewal replaces the token with nothing restarted", i+1, held[i].AccessToken, want)
		}
		if held[i].CredentialID != "cred-pod-1" {
			t.Errorf("trade %d's credential is %q, want the launcher credential the exchange minted", i+1, held[i].CredentialID)
		}
	}

	requests := broker.Requests()
	if len(requests) != 2 {
		t.Fatalf("the broker saw %d trades, want two: the first and its renewal", len(requests))
	}
	for i, want := range []string{firstProjected, secondProjected} {
		if got := requests[i].Form.Get("subject_token"); got != want {
			t.Errorf("trade %d carried the subject token %q, want %q: every trade reads the file again", i+1, got, want)
		}
	}
	if requests[0].Form.Get("session") != "example-worker-session" {
		t.Errorf("the trade named the session %q, want the one the client was configured with", requests[0].Form.Get("session"))
	}
	if requests[0].Proof == requests[1].Proof {
		t.Error("the renewal sent the first trade's proof again; each trade signs a proof of its own")
	}
	for _, request := range requests {
		if request.Form.Get("refresh_token") != "" {
			t.Error("a trade carried a refresh token: this grant issues none, so a renewal is a fresh subject token traded again")
		}
	}
	if wait := (<-waits).Round(time.Second); wait != (lifetime-300)*time.Second {
		t.Errorf("the client waited %s before renewing, want %s: RenewBefore ahead of the token's expiry", wait, (lifetime-300)*time.Second)
	}
}

// mustToken is what the caller of a live client asks for: the token in hand, which inside
// onToken is the one that trade just landed.
func mustToken(t *testing.T, client *Client, ctx context.Context) *Token {
	t.Helper()
	token, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	return token
}

func writeProjected(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the projected token: %v", err)
	}
}

// TestProjectedTokenThatCannotBeReadReachesTheBrokerNever: a pod whose token file is missing is
// a deployment's own fault, and the client says so rather than posting an empty subject token
// for the broker to refuse.
func TestProjectedTokenThatCannotBeReadReachesTheBrokerNever(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "never", "token_type": "DPoP", "expires_in": 60}
	})
	client, err := New(Config{
		BrokerURL: broker.URL(),
		Key:       testKey(t),
		Subject:   NewProjectedToken(filepath.Join(t.TempDir(), "absent")),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Token(context.Background()); err == nil {
		t.Fatal("Token answered a token for a pod whose projected token could not be read")
	}
	if len(broker.Requests()) != 0 {
		t.Error("the client posted a trade with no subject token at all")
	}
}
