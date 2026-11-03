// Package brokerclient is a public re-export of internal/brokerclient's token client, existing
// solely so a module outside github.com/sjawhar/envoy — packages/daemon's `legion controller
// start`, which mints its own Dispatch token — can use it: Go's internal-package rule refuses to
// let any package whose own import path does not start with github.com/sjawhar/envoy import a
// package under github.com/sjawhar/envoy/internal/..., however workspace-linked the two modules
// are. packages/envoy/brokertest is the same facade for the broker's test rig, and says the same.
//
// This changes no behaviour — every type here is an alias for the internal package's own, and
// every function forwards to it — so internal/brokerclient remains the one place the client's
// behaviour is described and changed, and the repository still holds one implementation of the
// RFC 8693 trade, one renewal loop and one DPoP signer.
package brokerclient

import (
	"net/http"

	internal "github.com/sjawhar/envoy/internal/brokerclient"
)

// The client, its configuration, what a trade answers, and an identity a machine can prove.
type (
	Client       = internal.Client
	Config       = internal.Config
	Token        = internal.Token
	Source       = internal.Source
	RefusedError = internal.RefusedError
)

// The variables a program reads to build this machine's identity; internal/brokerclient's
// environment.go documents each.
const (
	BrokerURLVar        = internal.BrokerURLVar
	IdentityVar         = internal.IdentityVar
	MachineTokenFileVar = internal.MachineTokenFileVar
	MachineAudienceVar  = internal.MachineAudienceVar
	PoolAudienceVar     = internal.PoolAudienceVar
	PoolAccountVar      = internal.PoolAccountVar
	SessionVar          = internal.SessionVar
	IdentityKubernetes  = internal.IdentityKubernetes
	IdentityMetadata    = internal.IdentityMetadata
	IdentityPool        = internal.IdentityPool
)

// New forwards to internal/brokerclient.New.
func New(cfg Config) (*Client, error) { return internal.New(cfg) }

// MachineIdentity forwards to internal/brokerclient.MachineIdentity: the identity IdentityVar
// names, or nil where this machine proves none.
func MachineIdentity(getenv func(string) string) (Source, error) {
	return internal.MachineIdentity(getenv)
}

// TradeSession forwards to internal/brokerclient.TradeSession: the session a trade writes
// under, the caller's own where it acts as one and SessionVar's otherwise.
func TradeSession(getenv func(string) string, own string) string {
	return internal.TradeSession(getenv, own)
}

// HTTPClient forwards to internal/brokerclient.HTTPClient.
func HTTPClient() *http.Client { return internal.HTTPClient() }
