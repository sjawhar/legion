// Package oidctest is a public re-export of internal/oidc/oidctest's local OIDC issuer, existing
// solely so a module outside github.com/sjawhar/envoy — packages/daemon's settings route tests,
// which present a projected ServiceAccount token to the daemon — can drive it, as
// brokertest/brokertest.go does for the broker's rig: Go's internal-package rule refuses a
// foreign module the import of anything under github.com/sjawhar/envoy/internal/....
//
// This facade changes no behavior — Issuer and Key are type aliases for the internal package's
// own types, so every exported method they carry stays reachable exactly as documented there,
// and the two constructors forward.
package oidctest

import (
	internal "github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TB is internal/oidc/oidctest.TB: the part of *testing.T the issuer uses, taken as an interface
// so the testing package stays out of a non-test build.
type TB = internal.TB

// Issuer is internal/oidc/oidctest.Issuer: an httptest server answering OIDC discovery and JWKS
// for keys the test controls, and the minting side a Kubernetes pod's projected service-account
// token would come from.
type Issuer = internal.Issuer

// Key is internal/oidc/oidctest.Key: one RSA signing key of the issuer, verifiable once the
// issuer publishes it.
type Key = internal.Key

// New forwards to internal/oidc/oidctest.New: an issuer on a random loopback port, serving no
// keys yet, closed at the test's cleanup.
func New(t TB) *Issuer { return internal.New(t) }

// NewKey forwards to internal/oidc/oidctest.NewKey: a signing key the issuer does not serve until
// Publish.
func NewKey(t TB, kid string) *Key { return internal.NewKey(t, kid) }
