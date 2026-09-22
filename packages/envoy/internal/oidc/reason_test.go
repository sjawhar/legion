package oidc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestReasonNamesEveryClass pins the words a caller may log or return to a
// rejected client. They are the whole vocabulary of a verification failure: a
// class the switch forgets is reported as a signature failure, which sends an
// operator looking for a forgery that never happened.
func TestReasonNamesEveryClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"malformed", fmt.Errorf("%w: payload is not base64url", ErrMalformed), "malformed"},
		{"issuer", fmt.Errorf("%w: %q, want %q", ErrIssuer, "https://other", "https://mine"), "issuer"},
		{"audience", fmt.Errorf("%w: %q, want %q", ErrAudience, []string{"dispatch"}, "envoy"), "audience"},
		{"expired", fmt.Errorf("%w at 2026-01-01T00:00:00Z", ErrExpired), "expired"},
		{"cancelled", fmt.Errorf("%w: %v", ErrCancelled, context.Canceled), "cancelled"},
		{"signature", ErrSignature, "signature"},
		{"an error carrying no sentinel", errors.New("something else"), "signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reason(tc.err); got != tc.want {
				t.Fatalf("Reason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestVerifyDoesNotCallAnEndedRequestAForgery: signature is the residue class,
// and an operator reads it as "someone is presenting forged tokens". A client
// that disconnects mid-verification, or a deadline that expires while the key
// set is being fetched, must not land there.
func TestVerifyDoesNotCallAnEndedRequestAForgery(t *testing.T) {
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "signing-key")
	verifier, err := New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))

	// The token here is sound, so this case exists only while the key-set fetch
	// is what fails. go-oidc selects between the caller's ctx.Done() and its own
	// inflight fetch (jwks.go:230-234), and a local issuer answers so fast that
	// both are ready and Go picks at random: without this delay the fetch
	// sometimes wins, the sound token verifies, and the assertion below fails
	// for a reason that has nothing to do with classification. Over a network
	// the fetch is the slow side, which is the situation being described.
	issuer.DelayKeys(2 * time.Second)

	// Cancelled before the first Verify, so the key-set fetch is what fails:
	// the token itself is sound and would verify on a live context.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = verifier.Verify(cancelled, token)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Verify on an ended context = %v, want ErrCancelled", err)
	}
	if errors.Is(err, ErrSignature) {
		t.Fatalf("an ended request was classified as a signature failure: %v", err)
	}
	if got := Reason(err); got != "cancelled" {
		t.Fatalf("Reason = %q, want %q", got, "cancelled")
	}
}

// TestEndedRequestOutranksTheTokensOwnFault pins the other side of that trade,
// so nobody reverses the ordering believing it costs nothing. classify reads
// the context before the error, deliberately, because go-oidc formats every
// error in verify.go with %v and no context sentinel survives to match on. The
// price is this: a token that really is bad, arriving on a request already
// over, is reported cancelled. That is the documented meaning of the class —
// "no verdict is trustworthy" — and reversing it would spend the forgery word
// on callers that merely hung up.
func TestEndedRequestOutranksTheTokensOwnFault(t *testing.T) {
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "signing-key")
	verifier, err := New(context.Background(), issuer.URL(), testAudience)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Same key id as the published key, different key material: a forgery.
	forged := issuer.Mint(t, oidctest.NewKey(t, signing.KeyID()),
		issuer.Claims(testSubject, testAudience))

	if _, err := verifier.Verify(context.Background(), forged); !errors.Is(err, ErrSignature) {
		t.Fatalf("on a live context the forgery = %v, want ErrSignature", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifier.Verify(cancelled, forged); Reason(err) != "cancelled" {
		t.Fatalf("on an ended context the forgery = %q, want cancelled", Reason(err))
	}

	// A token bad by its claims, not its signature: the only fixture that tells
	// "the context is read first" apart from "the context is read last, in the
	// residue arm". Both report the forgery above as cancelled; only the first
	// reports this one that way, and only the first makes AGENTS.md's "a bad
	// token on a connection that is already gone lands here too" true.
	wrongAudience := issuer.Claims(testSubject, testAudience)
	wrongAudience["aud"] = []string{"somebody-else"}
	claimBad := issuer.Mint(t, signing, wrongAudience)

	if _, err := verifier.Verify(context.Background(), claimBad); !errors.Is(err, ErrAudience) {
		t.Fatalf("on a live context the wrong audience = %v, want ErrAudience", err)
	}
	if _, err := verifier.Verify(cancelled, claimBad); Reason(err) != "cancelled" {
		t.Fatalf("on an ended context the wrong audience = %q, want cancelled", Reason(err))
	}
}
