package oidc

import (
	"errors"
	"fmt"
	"testing"
)

// TestReasonNamesEveryClass pins the five words a caller may log or return to a
// rejected client. They are the whole vocabulary of a verification failure: a
// class the switch forgets would be reported as a signature failure and send an
// operator looking for the wrong fault.
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
