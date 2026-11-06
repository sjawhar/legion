package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
		{"issuer unavailable", fmt.Errorf("%w: %w", ErrIssuerUnavailable, fmt.Errorf("%w: https://issuer.example/keys answered 503", ErrKeySet)), "unavailable"},
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

	// The token here is sound, so this case exists only while the key-set read
	// is what fails. It fails for certain: the verifier reads the key set
	// through *Keys, whose read is one HTTP request made on the caller's own
	// context, and a request made on a context that has already ended never
	// reaches the issuer. The delay stands in for the network all the same, so
	// that a read which somehow did start is still the slow side.
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

// TestAKeySetOutageIsNotAForgery separates the two failures that both used to read "signature".
// An issuer whose key set cannot be read reaches no verdict at all, so it is
// ErrIssuerUnavailable, the class Dispatch answers 503 and retries; a token signed by a key the
// issuer never published is a verdict — it is a token the issuer never signed — so it stays in
// the signature class, the word an operator reads as a forgery attempt.
func TestAKeySetOutageIsNotAForgery(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "signing-key")
	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	sound := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))

	// No set has been read yet, so the outage is what answers rather than a cached key: an
	// issuer that is down stops no verification it does not have to (Keys.Key).
	issuer.FailKeys(http.StatusInternalServerError)
	_, err := verifier.Verify(context.Background(), sound)
	if !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Verify while the key set answers 500 = %v, want ErrIssuerUnavailable", err)
	}
	if errors.Is(err, ErrSignature) {
		t.Fatalf("a key-set outage was classified as a signature failure: %v", err)
	}
	if got := Reason(err); got != "unavailable" {
		t.Fatalf("Reason = %q, want %q", got, "unavailable")
	}
	if strings.Contains(err.Error(), sound) {
		t.Errorf("Verify error %q repeats the token", err)
	}

	issuer.FailKeys(0)
	if _, err := verifier.Verify(context.Background(), sound); err != nil {
		t.Fatalf("Verify once the key set reads again: %v", err)
	}

	forged := issuer.Mint(t, oidctest.NewKey(t, "never-published"), issuer.Claims(testSubject, testAudience))
	_, err = verifier.Verify(context.Background(), forged)
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("a token signed by a key the issuer never published = %v, want ErrSignature", err)
	}
	if errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("a key id the issuer publishes nothing under was classified as an outage: %v", err)
	}
	if got := Reason(err); got != "signature" {
		t.Fatalf("Reason = %q, want %q", got, "signature")
	}
}
