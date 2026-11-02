// Package brokerclient is a public re-export of internal/brokerclient, the Go token client,
// existing solely so a module outside github.com/sjawhar/envoy can use it: `legion controller
// start` (packages/daemon) gets its own Dispatch token through it. Go's internal-package rule
// refuses to let any package whose import path does not start with github.com/sjawhar/envoy
// import one under github.com/sjawhar/envoy/internal/..., however workspace-linked the two
// modules are, and packages/envoy/brokertest exists for the same reason.
//
// It changes no behaviour — every type here is an alias of the internal package's own, and
// every function forwards — so internal/brokerclient remains the one place the client's
// behaviour is described and changed.
package brokerclient

import (
	"crypto/ecdsa"

	internal "github.com/sjawhar/envoy/internal/brokerclient"
)

// Client, Token, Source and Error are internal/brokerclient's own types.
type (
	Client = internal.Client
	Token  = internal.Token
	Source = internal.Source
	Error  = internal.Error
)

// MachineIdentityVar and GoogleAudienceVar name the two variables a caller outside this module
// reports on when a machine has no identity to trade.
const (
	MachineIdentityVar = internal.MachineIdentityVar
	GoogleAudienceVar  = internal.GoogleAudienceVar
)

// NewKey is internal/brokerclient.NewKey: a fresh key for one process's own use.
func NewKey() (*ecdsa.PrivateKey, error) { return internal.NewKey() }

// MachineSource is internal/brokerclient.MachineSource: this machine's identity as the
// environment names it, and nil where it has none.
func MachineSource(getenv func(string) string) (Source, error) { return internal.MachineSource(getenv) }

// SessionName is internal/brokerclient.SessionName: the session name a trade asks under.
func SessionName(getenv func(string) string, prefix string) (string, error) {
	return internal.SessionName(getenv, prefix)
}
