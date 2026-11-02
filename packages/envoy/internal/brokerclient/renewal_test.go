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
	"strconv"
	"strings"
	"sync/atomic"
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

// TestABusyBrokerIsAskedAgainRatherThanEndingTheLoop: the broker's token endpoint spends a
// bucket per source address, so a 429 is the one refusal a retry cures. A loop that stopped on
// it would enrol nothing from the moment the credential in hand lapsed, with only a restart to
// cure it — the failure a dev stack reproduces in seconds and a deployment meets behind one
// address.
func TestABusyBrokerIsAskedAgainRatherThanEndingTheLoop(t *testing.T) {
	// The first trade lands; the three after it find the bucket empty; then it has refilled.
	const refusals = 3
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		if n > 0 && n <= refusals {
			return 429, map[string]any{"error": "invalid_request", "error_description": "too many token requests; try again later"}
		}
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": 3600}
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

	// A 429 while the token in hand is live is no reason to stop writing with it.
	client.held.Expiry = time.Now().Add(30 * time.Second)
	held, err := client.Token(ctx)
	if err != nil || held.AccessToken != first.AccessToken {
		t.Fatalf("Token while the broker is busy = %v, %v; want the token in hand, which is still live", held, err)
	}

	// And the renewal loop waits the bucket out rather than ending: every wait fires at once,
	// so the three refusals and the trade that follows them run at the test's speed.
	client.after = func(time.Duration) <-chan time.Time {
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	loop, cancel := context.WithCancel(ctx)
	defer cancel()
	var landed atomic.Int64
	err = client.Run(loop, func(context.Context, *Token) error {
		landed.Add(1)
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("Run = %v; want the 429s waited out, not the end of the loop", err)
	}
	if landed.Load() != 1 {
		t.Fatalf("the loop handed its caller %d tokens; want the one after the 429s", landed.Load())
	}
	if trades := len(broker.Requests()); trades != refusals+2 {
		t.Fatalf("the broker saw %d trades, want %d: Token's first, the loop's %d refused, and the one that landed",
			trades, refusals+2, refusals)
	}
}

// TestATokenShorterThanRenewBeforeIsNotRenewedInALoop: a token that lives less than
// RenewBefore is renewed at half of what is left, so a deployment minting short tokens — or a
// dev stack proving a renewal in seconds — renews steadily rather than trading again the
// instant each token lands, which empties the broker's bucket.
func TestATokenShorterThanRenewBeforeIsNotRenewedInALoop(t *testing.T) {
	now := time.Now()
	client, err := New(Config{BrokerURL: "https://secrets.example", Key: testKey(t), Subject: projectedStub(),
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(time.Hour)}); wait != time.Hour-DefaultRenewBefore {
		t.Errorf("a token of an hour is renewed in %s, want %s ahead of its expiry", wait, DefaultRenewBefore)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(2 * time.Second)}); wait != time.Second {
		t.Errorf("a token of two seconds is renewed in %s, want half of what is left", wait)
	}
	if wait := client.untilRenewal(&Token{Expiry: now.Add(-time.Second)}); wait != 0 {
		t.Errorf("a token already past its expiry is renewed in %s, want at once", wait)
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
