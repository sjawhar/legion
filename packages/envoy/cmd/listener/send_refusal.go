// The send route's refusals for a target the session bucket does not hold: whether the id is a
// session id at all, and, when it is, which of the two absences this is.

package main

import (
	"fmt"
	"strings"
	"time"
)

// canonicalUUIDLength is the length of a UUID written the way every session id is written:
// 8-4-4-4-12 hexadecimal digits with four hyphens.
const canonicalUUIDLength = 36

// uuidHyphenPositions are the indexes of the four hyphens in that form.
var uuidHyphenPositions = map[int]bool{8: true, 13: true, 18: true, 23: true}

// uuidShapeTo reports whether s is written as a canonical UUID is, as far as s goes: every
// character stands where a hexadecimal digit or one of the four hyphens stands. A whole canonical
// UUID has uuidShapeTo true at length canonicalUUIDLength; anything shorter with it true is a
// piece of one, cut off somewhere.
func uuidShapeTo(s string) bool {
	if len(s) == 0 || len(s) > canonicalUUIDLength {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if uuidHyphenPositions[i] {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// notASessionIDRefusal is the send route's 400 for a target the session bucket holds no record of
// and that is not written as a session id is. It answers "" for an id this listener cannot call
// malformed, which is every canonical UUID.
//
// It is reached only after the store itself has answered that nothing is registered under the id
// and this listener has never seen a registration under it lapse, so a session registered under an
// id of another shape - an OpenCode `ses_…` id, a fixture - is found by that read and never
// refused here. Nothing resolves a partial id anywhere on this path: the id is a KV key, read
// whole or not at all. This refusal says so rather than being helpful about it, because a surface
// that resolved a prefix would deliver to the wrong session sooner or later.
//
// A UUIDv7's first group is the top 32 bits of its 48-bit millisecond timestamp, so every session
// started inside the same 65.536-second window shares it: measured against the production listener
// on 2026-10-29, 157 of 409 live sessions shared a first group with another session, across 68
// groups, five sessions in the largest. A first group is therefore no identifier, and the sender
// that completed one into a whole UUID itself - the id 01a17b06-bac0-7626-b8bb-0d2c4a5d7cf7, which
// no session ever carried - is why the refusal says what to read the real id from.
func notASessionIDRefusal(sessionID string) string {
	if len(sessionID) == canonicalUUIDLength && uuidShapeTo(sessionID) {
		return ""
	}
	const (
		shape  = "a session id is a UUID, 8-4-4-4-12 hexadecimal digits"
		remedy = "Nothing was sent; read the target's whole id from envoy_sessions or envoy_whoami."
	)
	if uuidShapeTo(strings.TrimSuffix(sessionID, "-")) {
		return fmt.Sprintf(
			"%s is not a session id and nothing is registered under it: %s, and this is the first part of one. "+
				"A UUID's first group is the top 32 bits of a timestamp in milliseconds, so every session started "+
				"inside the same 65.536-second window carries it, and part of an id names no session. %s",
			sessionID, shape, remedy,
		)
	}
	return fmt.Sprintf("%s is not a session id and nothing is registered under it: %s. %s",
		sessionID, shape, remedy)
}

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
// registering every 90 s throughout. The lapsed half is the other night's reading of the same
// sentence (dispatch://INFRA-392): a session whose process stalls longer than the bucket's TTL is
// gone from the registry while it runs, and its sender is told that it should come back rather
// than that its id is wrong. An id that is not a UUID at all is a third fact again, and is
// answered by notASessionIDRefusal above rather than by this sentence.
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
