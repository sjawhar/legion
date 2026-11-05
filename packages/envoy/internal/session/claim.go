package session

import "github.com/sjawhar/envoy/internal/contracts"

// ClaimStaleAfter is how long a session's claim may go unrefreshed before a reader treats the
// process that wrote it as gone: contracts.SessionClaimStale, which allows three missed
// heartbeats at contracts.SessionHeartbeat (generated from SESSION_HEARTBEAT_MS) before
// assuming it is. Role resolution reads it (roles.Fresh, and with it the role lane's
// stale-holder check and Dispatch's outbox); a session route's own write
// weighs no claim's freshness, since the route follows whichever process heartbeated last
// (SessionRegistry.PutWithin).
const ClaimStaleAfter = contracts.SessionClaimStale
