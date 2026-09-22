package oidc

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
