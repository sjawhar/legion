package session

import "github.com/sjawhar/envoy/internal/contracts"

// ClaimStaleAfter is how long a session's claim may go unrefreshed before a reader treats the
// process that wrote it as gone: contracts.SessionClaimStale, which allows three missed
// heartbeats at contracts.SessionHeartbeat (generated from SESSION_HEARTBEAT_MS) before assuming
// it is. The Legion daemon reads the same declaration as listenerClaimStale
// (packages/daemon/internal/daemon/notice_exceptions.go). No role resolution weighs it: a holder
// is judged by how long ago its registration was written (StateOf, on LiveWindow and
// GoneHorizon), and a session route's own write weighs no claim's freshness either, since the
// route follows whichever process heartbeated last (SessionRegistry.PutWithin).
const ClaimStaleAfter = contracts.SessionClaimStale

// stampClaim is the registration a claim writes: next, stamped with the moment of the write, so a
// reader weighing the route's freshness reads the latest heartbeat (ClaimStaleAfter). It keeps the
// fields of the stored registration cur, if there is one (found), that this build does not know
// (SessionEntry.unknown): a newer build's field is the session's, not the claiming process's, and
// survives its heartbeat.
func stampClaim(cur SessionEntry, found bool, next SessionEntry, now int64) SessionEntry {
	next.UpdatedAt = now
	if found {
		next.unknown = cur.unknown
	}
	return next
}
