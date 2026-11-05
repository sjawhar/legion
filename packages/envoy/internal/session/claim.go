package session

import "time"

// ClaimStaleAfter is how long a session's claim may go unrefreshed before a reader treats the
// process that wrote it as gone. The plugin heartbeats every 90 seconds (DEFAULT_HEARTBEAT_MS,
// packages/envoy-client/src/envoy-settings.ts), so this allows three missed heartbeats before
// assuming it is. Role resolution reads it (roles.Fresh, and with it the role lane's stale-holder
// check and Dispatch's outbox); a session route's own write weighs no claim's freshness, since
// the route follows whichever process heartbeated last (SessionRegistry.PutWithin).
const ClaimStaleAfter = 5 * time.Minute
