package session

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go"
)

// ClaimStaleAfter is how long a claim may go unrefreshed before another holder
// may take the route. The plugin heartbeats every 2 minutes, so this allows two
// missed heartbeats before assuming the claiming process is gone.
const ClaimStaleAfter = 5 * time.Minute

// mergeForClaim decides which of two competing claims on a session route wins.
//
// Precedence, in order:
//   - a transient KV error is returned so the caller refuses to write
//   - no incumbent, or the same process re-claiming: accept
//   - a portless claim cannot displace a live portful route
//   - a driving claim always wins (beats a non-driving incumbent; a newer
//     driving claim beats an older one, which is a genuine re-home)
//   - a non-driving claim cannot displace a live driving incumbent; it may take
//     over only once that claim goes stale (process presumed gone)
func mergeForClaim(cur SessionEntry, curErr error, next SessionEntry, now int64) (SessionEntry, error) {
	if curErr != nil && !errors.Is(curErr, nats.ErrKeyNotFound) {
		return SessionEntry{}, curErr
	}
	if curErr == nil && next.Port == 0 && cur.Port > 0 && !claimStale(cur, now) {
		return cur, nil
	}
	if curErr == nil && next.Port != cur.Port && !next.Driving && cur.Driving && !claimStale(cur, now) {
		return cur, nil
	}
	next.UpdatedAt = now
	return next, nil
}

func claimStale(entry SessionEntry, now int64) bool {
	return now-entry.UpdatedAt >= int64(ClaimStaleAfter/time.Millisecond)
}
