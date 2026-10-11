// Package brokertest is a public re-export of internal/broker/brokertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon's
// agentsecrets contract test — can drive it: Go's internal-package rule refuses to let any
// package whose own import path does not start with github.com/sjawhar/envoy import a package
// under github.com/sjawhar/envoy/internal/..., which blocks a foreign module from importing
// internal/broker/brokertest directly, however workspace-linked the two modules are. This facade
// changes no behavior — Rig and Option are type aliases for the internal package's own types, and
// NewRig and WithCredentialLifetime simply forward to it — so every exported field and method
// internal/broker/brokertest.Rig carries (Req, UI, MintPodToken, Store, Operator, ...) stays
// reachable exactly as documented there; that file remains the one place the rig's own behavior is
// described and changed.
package brokertest

import (
	"testing"
	"time"

	internal "github.com/sjawhar/envoy/internal/broker/brokertest"
)

// Rig is internal/broker/brokertest.Rig under a name a foreign module may import.
type Rig = internal.Rig

// Option is internal/broker/brokertest.Option under a name a foreign module may import.
type Option = internal.Option

// NewRig forwards to internal/broker/brokertest.NewRig, including its own skip when
// BROKER_TEST_DATABASE_URL is unset.
func NewRig(t *testing.T, options ...Option) *Rig {
	return internal.NewRig(t, options...)
}

// WithCredentialLifetime forwards to internal/broker/brokertest.WithCredentialLifetime.
func WithCredentialLifetime(lifetime time.Duration) Option {
	return internal.WithCredentialLifetime(lifetime)
}
