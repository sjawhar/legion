package session

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go"
)

// ClaimStaleAfter is how long a session's claim may go unrefreshed before a reader treats the
// process that wrote it as gone. The plugin heartbeats every 90 seconds (DEFAULT_HEARTBEAT_MS,
// packages/envoy-client/src/envoy-settings.ts), so this allows three missed heartbeats before
// assuming it is. Role resolution reads it (roles.Fresh, and with it the role lane's stale-holder
// check and Dispatch's outbox); mergeForClaim itself weighs no claim's freshness any more, for
// the reason its own comment gives.
const ClaimStaleAfter = 5 * time.Minute

// mergeForClaim decides what a claim on a session route writes. A transient KV error is returned
// so the caller refuses to write: the stored entry did not answer, and a write built on a read
// that failed would discard whatever that entry holds. Every other claim is accepted and stamped
// with now, so the route names the process that heartbeated last.
//
// It arbitrated between competing processes until dispatch://INFRA-889. A harness can keep a
// session's state on shared disk, so several live processes can hold one session id - the one
// driving it, plus any that merely have it loaded - and each heartbeats a claim. Two rules chose
// between them: a claim naming no serving port could not displace a live route that named one,
// and a non-driving claim could not displace a live driving incumbent until that incumbent's
// claim went stale. Both turned on the two claims naming different serving ports, and a
// registration may no longer carry a port at all (cmd/listener/api.go's subscribeHandler refuses
// one), so every claim now names port 0, neither rule could fire again, and the entry follows the
// latest heartbeat. The incumbent is still read by the caller, for curErr alone: what it holds no
// longer enters the decision, so it is no longer passed here.
func mergeForClaim(curErr error, next SessionEntry, now int64) (SessionEntry, error) {
	if curErr != nil && !errors.Is(curErr, nats.ErrKeyNotFound) {
		return SessionEntry{}, curErr
	}
	next.UpdatedAt = now
	return next, nil
}
