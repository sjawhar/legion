// The send route's 404: what it says about a target the session bucket does not hold.

package main

import (
	"fmt"
	"time"
)

// noLiveSessionRefusal is the send route's 404 for a target the session bucket answered is gone.
// It opens with "no live session <id>" whatever the cause, because that prefix is the contract:
// Dispatch classifies a delivery failure on it (internal/dispatch/api/comment_delivery.go) and
// the dashboard prints the whole text to a person.
//
// What follows the prefix is the one thing the sender cannot find out for itself, and the reason
// this route reads LastSeen at all: a session whose registration lapsed and an id that never
// registered were the same sentence, so a mistyped, stale or invented id read as a working
// session that had died. On 2026-10-29 an agent was given a session id as its first group alone,
// completed the rest itself, sent to the id it had made, and recorded "the session is not live"
// as a fact in a note and in messages to other agents, while the session it meant was
// registering every 90 s throughout. A UUIDv7's first group is the top 32 bits of its 48-bit
// millisecond timestamp, so every session started inside the same 65.536 s window shares it: 61
// of 409 live sessions that night shared a first group with another, and a prefix identifies
// nobody. The lapsed half is the other night's reading of the same sentence
// (dispatch://INFRA-392): a session whose process stalls longer than the bucket's TTL is gone
// from the registry while it runs, and its sender is told that it should come back rather than
// that its id is wrong.
//
// lastSeen is SessionRegistry.LastSeen, which the registry keeps for one TTL past an entry's
// expiry or removal, so the second sentence is only ever this listener's own record over that
// window, and never a claim that the id never existed: a registration that lapsed longer ago
// than the window, and one whose expiry this listener's cache never held, both read as no record.
func noLiveSessionRefusal(sessionID string, lastSeen int64, ttl time.Duration, now time.Time) string {
	if lastSeen > 0 {
		age := now.Sub(time.UnixMilli(lastSeen)).Truncate(time.Second)
		if age < 0 {
			age = 0
		}
		return fmt.Sprintf("no live session %s: its registration lapsed, last seen %s ago", sessionID, age)
	}
	if ttl > 0 {
		return fmt.Sprintf(
			"no live session %s: no registration, and none lapsed in the last %s; check the session id",
			sessionID, ttl,
		)
	}
	return fmt.Sprintf("no live session %s: no registration; check the session id", sessionID)
}
