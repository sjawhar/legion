// Package brokertest is a public re-export of internal/broker/brokertest's real-broker test rig,
// existing solely so a module outside github.com/sjawhar/envoy — packages/daemon-go's
// agentsecrets contract test — can drive it: Go's internal-package rule refuses to let any
// package whose own import path does not start with github.com/sjawhar/envoy import a package
// under github.com/sjawhar/envoy/internal/..., which blocks a foreign module from importing
// internal/broker/brokertest directly, however workspace-linked the two modules are. This facade
// changes no behavior — Rig is a type alias for the internal package's own type, and NewRig
// simply forwards to it — so every exported field and method internal/broker/brokertest.Rig
// carries (Req, UI, MintPodToken, Store, Approver, ...) stays reachable exactly as documented
// there; that file remains the one place the rig's own behavior is described and changed.
package brokertest

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/broker/brokertest"
)

// Origin and RPID mirror internal/broker/brokertest's own fixed WebAuthn origin/rpId.
const (
	Origin = internal.Origin
	RPID   = internal.RPID
)

// Rig is internal/broker/brokertest.Rig under a name a foreign module may import.
type Rig = internal.Rig

// NewRig forwards to internal/broker/brokertest.NewRig, including its own skip when
// BROKER_TEST_DATABASE_URL is unset.
func NewRig(t *testing.T) *Rig {
	return internal.NewRig(t)
}
