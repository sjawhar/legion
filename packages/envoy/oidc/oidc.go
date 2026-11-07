// Package oidc is a public re-export of internal/oidc's token verification, existing solely so a
// module outside github.com/sjawhar/envoy — packages/daemon's settings route, which verifies the
// projected ServiceAccount token Dispatch presents — can use the one verifier this fleet has:
// Go's internal-package rule refuses to let any package whose own import path does not start with
// github.com/sjawhar/envoy import a package under github.com/sjawhar/envoy/internal/..., however
// workspace-linked the two modules are. It is shaped exactly as brokertest/brokertest.go, which
// exists for this same reason.
//
// This facade changes no behavior — every type is an alias for the internal package's own and
// every function forwards to it — so internal/oidc remains the one place the verification is
// described and changed. It re-exports exactly what its consumer names, as brokertest does:
// anything else internal/oidc holds is reached by adding it here when a caller outside this
// module needs it.
package oidc

import internal "github.com/sjawhar/envoy/internal/oidc"

// Claims is internal/oidc.Claims: the verified claims of a token. For a projected
// service-account token Subject is the Kubernetes subject,
// "system:serviceaccount:<namespace>:<name>".
type Claims = internal.Claims

// LazyVerifier is internal/oidc.LazyVerifier: a Verifier whose issuer is discovered when first
// needed, so a daemon boots and serves while its cluster's issuer is unreachable, and so building
// one costs no network read at all — which is what a rebuild under the settings holder's lock
// needs (internal/daemon's apiHolder).
type LazyVerifier = internal.LazyVerifier

// LooksLikeJWT forwards to internal/oidc.LooksLikeJWT: a caller's cheap discriminator between a
// JWT and the other bearer shapes it accepts, none of which contain a dot.
func LooksLikeJWT(s string) bool { return internal.LooksLikeJWT(s) }

// Reason forwards to internal/oidc.Reason: the class of a Verify failure in one word, which never
// repeats any part of the token.
func Reason(err error) string { return internal.Reason(err) }

// NewLazyVerifier forwards to internal/oidc.NewLazyVerifier: it reads nothing, and the first
// Verify (or Discover, or Run) reads the issuer's discovery document.
func NewLazyVerifier(issuer, audience string) *LazyVerifier {
	return internal.NewLazyVerifier(issuer, audience)
}
