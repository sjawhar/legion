// packages/envoy/internal/brokerclient/renewal_test.go
//
// What the renewal loop does when a trade does not land, which is the half of the contract a
// corpus cannot hold: a broker that is down is waited out, because this grant has no rotation
// window to miss and an outage past one token's life is the same event as a short one; a broker
// that refuses the identity stops the loop with a plain error, because no retry cures that; and
// a caller is handed no identity it cannot use, so nothing of its writes lands unauthenticated.

package brokerclient

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestARefusedIdentityStopsTheRenewalLoopWithAPlainError: the broker is told to refuse, and the
// loop ends saying so rather than asking again every quarter minute for ever.
func TestARefusedIdentityStopsTheRenewalLoopWithAPlainError(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this account"}
	})
	client := refusalClient(t, broker)
	err := client.Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Run = %v, want the broker's refusal", err)
	}
	if !strings.Contains(err.Error(), "no entry lists this account") {
		t.Errorf("Run's error is %q, want the broker's own words in it", err)
	}
	if trades := len(broker.Requests()); trades != 1 {
		t.Errorf("the loop made %d trades, want one: a refusal is not retried", trades)
	}
}

// TestABrokerThatIsDownIsWaitedOut: the first trade lands, the broker then answers nothing an
// answer can be read out of, and the loop keeps asking until it is up again — with the caller
// handed the token it already holds while that token is live, and no token at all once it is not.
func TestABrokerThatIsDownIsWaitedOut(t *testing.T) {
	down := true
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n > 0 && down {
			return 503, map[string]any{"error": "unavailable"}
		}
		return 200, map[string]any{
			"access_token": "dispatch-token", "token_type": "DPoP", "expires_in": 3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		}
	})
	client, err := New(Config{BrokerURL: broker.URL(), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	first, err := client.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	// The held token is still live, so the caller keeps writing while the broker is down.
	client.held.Expiry = time.Now().Add(30 * time.Second)
	held, err := client.Token(ctx)
	if err != nil || held.AccessToken != first.AccessToken {
		t.Fatalf("Token while the broker is down = %v, %v; want the token in hand, which is still live", held, err)
	}

	// Once it has run out there is no identity to write with, and the client says so rather
	// than handing its caller a token the broker will refuse.
	client.held.Expiry = time.Now().Add(-time.Second)
	if token, err := client.Token(ctx); err == nil {
		t.Fatalf("Token with nothing live left = %+v, want the failure", token)
	}

	down = false
	after, err := client.Token(ctx)
	if err != nil || after.AccessToken != "dispatch-token" {
		t.Fatalf("Token once the broker is up again = %v, %v; want a fresh trade, with no step by a person", after, err)
	}
}

func refusalClient(t *testing.T, broker *stubBroker) *Client {
	t.Helper()
	client, err := New(Config{BrokerURL: broker.URL(), Key: testKey(t), Subject: projectedStub()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func projectedStub() Source {
	return &scriptedSource{
		token:     "projected.service.account.token",
		tokenType: "urn:envoy:params:oauth:token-type:kubernetes-service-account",
		describe:  "a pod's projected service-account token",
	}
}
