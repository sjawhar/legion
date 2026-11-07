// Package sessiontiming holds the timings of Envoy session registration, for a caller outside
// this module: the interval at which a pane's pi-envoy plugin refreshes its listener
// registration, how long a session's last registration keeps it live to the listener, the window
// after which a role claim is stale, and one listener call's attempts, per-attempt timeout and
// retry delay.
//
// It exists because the Legion daemon (packages/daemon, module
// github.com/sjawhar/legion/daemon) is a Go module of its own, and Go's internal rule bars it
// from github.com/sjawhar/envoy/internal/contracts, where these numbers are generated from
// packages/contracts. This package is the one public door onto them, so the daemon reads the
// generated values rather than keeping copies of its own.
//
// No package of the envoy module imports this one. packages/envoy/docker/Dockerfile copies only
// go.mod, go.sum, cmd, internal and listenfd into the listener image's build context, so an
// internal/... import of this top-level directory would compile here and break that image build.
// An envoy package that needs one of these numbers reads internal/contracts directly.
package sessiontiming

import "github.com/sjawhar/envoy/internal/contracts"

// Heartbeat is how often a pane refreshes its registration, LiveWindow how long a session's last
// registration keeps it live to the listener, and ClaimStale how long a role claim may go
// unrefreshed before a reader treats its holder as gone. CallTimeout is how long one listener
// call attempt waits, CallRetryDelay the pause before the next attempt, and CallAttempts how many
// attempts one call makes. Each is the generated contracts constant itself, so a caller outside
// this module weighs the same numbers the listener does. The daemon's own code reads the first
// three. The three call numbers are exported only for the daemon test that holds the rule -
// three refreshes and one whole call strictly inside the live window - which sits in another
// module (packages/daemon/internal/daemon/session_timing_test.go); no production code reads them.
const (
	Heartbeat      = contracts.SessionHeartbeat
	LiveWindow     = contracts.SessionLiveWindow
	ClaimStale     = contracts.SessionClaimStale
	CallTimeout    = contracts.ListenerCallTimeout
	CallRetryDelay = contracts.ListenerCallRetryDelay
	CallAttempts   = contracts.ListenerCallAttempts
)
