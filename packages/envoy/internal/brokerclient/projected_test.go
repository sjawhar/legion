// packages/envoy/internal/brokerclient/projected_test.go

package brokerclient

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// TestProjectedServiceAccountTokenIsTradedAndRenewed drives the client with a pod's projected
// Kubernetes service-account token read from a file: it gets the Dispatch access token the
// broker answered, and one renewal of it, with nothing restarted. Between the two trades the
// file is rewritten, as a kubelet rewrites a projected token in place, and the renewal must
// carry the new token: the grant issues no refresh token, so a renewal is a fresh subject token
// traded again.
func TestProjectedServiceAccountTokenIsTradedAndRenewed(t *testing.T) {
	stub := newStubBroker(t,
		stubAnswer{status: 200, body: `{"access_token":"first-dispatch-token","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600,"credential_id":"example-credential","credential_expires_at":"2126-01-02T15:04:05Z"}`},
		stubAnswer{status: 200, body: `{"access_token":"second-dispatch-token","token_type":"DPoP","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","expires_in":3600}`},
	)
	path := writeTokenFile(t, "projected-token-one")
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	// The loop's clock: the test hands it each wait, so a renewal happens when the test says so
	// rather than an hour from now.
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	client := &Client{
		BrokerURL: stub.URL(), Key: key, Session: "example-pod-session",
		Subject: &FileSource{Path: path},
		After: func(d time.Duration) <-chan time.Time {
			waits <- d
			return fire
		},
	}

	held := make(chan *Token, 4)
	ctx, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx, func(token *Token) error { held <- token; return nil }) }()

	first := <-held
	if first.AccessToken != "first-dispatch-token" || first.CredentialID != "example-credential" {
		t.Fatalf("the first trade held %+v; want the Dispatch token the broker answered and its launcher credential", first)
	}
	if wait := <-waits; wait < 25*time.Minute || wait > 30*time.Minute {
		t.Fatalf("the loop waits %s before renewing an hour-long token; want about half its life", wait)
	}

	// The kubelet rotates the projected token in place, and the renewal must send what the file
	// says then, not what it said at the first trade.
	if err := os.WriteFile(path, []byte("projected-token-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fire <- time.Now()

	second := <-held
	if second.AccessToken != "second-dispatch-token" {
		t.Fatalf("the renewal held %+v; want the second Dispatch token, with nothing restarted", second)
	}
	if stub.calls() != 2 {
		t.Fatalf("the broker was asked for %d exchanges; want the trade and its one renewal", stub.calls())
	}
	for call, want := range map[int]string{0: "projected-token-one", 1: "projected-token-two"} {
		form, _ := stub.call(t, call)
		if form.Get("subject_token") != want {
			t.Errorf("exchange %d carried the subject token %q; want %q, the file's content at that moment", call, form.Get("subject_token"), want)
		}
		if form.Get("subject_token_type") != oauthwire.SubjectTokenTypeKubernetes {
			t.Errorf("exchange %d presented the subject token as %q; a projected service-account token is %q", call, form.Get("subject_token_type"), oauthwire.SubjectTokenTypeKubernetes)
		}
	}

	stop(nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the loop ended with %v; a cancelled context ends it quietly", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not end when its context did")
	}
}

// TestProjectedTokenThatCannotBeRenewedStopsWithAPlainError: once the token in hand has lapsed
// and the broker still refuses, the loop stops and says so, rather than leaving a caller writing
// with no identity.
func TestProjectedTokenThatCannotBeRenewedStopsWithAPlainError(t *testing.T) {
	stub := newStubBroker(t, stubAnswer{status: 400, body: `{"error":"invalid_grant","error_description":"this pod is registered for no session"}`})
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{BrokerURL: stub.URL(), Key: key, Session: "example-pod-session",
		Subject: &FileSource{Path: writeTokenFile(t, "projected-token")}}
	err = client.Run(t.Context(), func(*Token) error {
		t.Fatal("the loop handed over a token the broker never answered")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("the loop ended with %v; want a plain error carrying the broker's refusal", err)
	}
}
