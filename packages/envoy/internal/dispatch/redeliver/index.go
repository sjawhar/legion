package redeliver

import (
	"strings"
	"time"
)

// The sweep's index is one entry per open delivery, keyed by the minute that delivery next needs a
// sweep, so a sweep reads what is due rather than everything the bucket remembers. The bucket
// keeps 72 hours of records, every delivery acted on in three days, and the running sweep's cost
// used to grow with all of them.
//
// A JetStream KV selects by subject filter and by nothing else: dots separate a key's tokens, `*`
// stands for one of them and `>` for one or more, and there is no filter on a value, no range scan
// and no secondary index (internal/bus/kv.go). So the due time has to be in the key, which is
// `pending.<YYYYMMDD>.<HH>.<MM>.<GUID>`, UTC and zero-padded, with an empty value: the record it
// names is read by its own key, and a window of minutes is a handful of filters (pendingFilters).
//
// Dispatch's NATS grant already covers these keys. It names the bucket's keys and its consumers
// whole -- `$KV.envoy_webhook_redelivery.>` and
// `$JS.API.CONSUMER.CREATE.KV_envoy_webhook_redelivery.>` (infra/core/permissions.go) -- so a new
// key prefix and a consumer over several filter subjects need nothing added to it. The bucket's own
// settings are another issue's and untouched here: an entry lives in the same bucket as the records
// under its 72-hour TTL, its file storage and its ENVOY_NATS_REPLICAS copies (OpenState), so an
// entry a sweeper died before deleting expires with them.
const pendingPrefix = "pending."

// pendingLayout is the time in an entry's key, in the tokens a filter selects by: the day, then the
// hour, then the minute.
const pendingLayout = "20060102.15.04"

// pendingKey is the index entry for guid, due at at.
func pendingKey(at time.Time, guid string) string {
	return pendingPrefix + at.UTC().Format(pendingLayout) + "." + guid
}

// parsePendingKey reads an entry key back. ok is false for anything that is not one: a key under
// another prefix, or one whose time is not the shape pendingKey writes. A GUID holding a dot of its
// own comes back whole, since the key's first three tokens are the time and the rest is the GUID.
func parsePendingKey(key string) (at time.Time, guid string, ok bool) {
	rest, found := strings.CutPrefix(key, pendingPrefix)
	if !found {
		return time.Time{}, "", false
	}
	tokens := strings.SplitN(rest, ".", 4)
	if len(tokens) != 4 || tokens[3] == "" {
		return time.Time{}, "", false
	}
	at, err := time.ParseInLocation(pendingLayout, strings.Join(tokens[:3], "."), time.UTC)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, tokens[3], true
}

// attention is the next moment a record needs a sweep: the end of the claim it is held by, so a
// claim is looked at once it can be void and not before, and otherwise the time its next request is
// due. A record with no next request -- one an older build wrote, or one whose request was never
// made -- is due now.
func attention(r record, now time.Time) time.Time {
	if r.InFlight {
		return r.ClaimedAt.Add(claimDeadline())
	}
	if r.NextAttemptAt.IsZero() {
		return now
	}
	return r.NextAttemptAt
}

// pendingFor is the index entry a record must have once it is written at now: none at all once the
// record is closed, since nothing asks for a delivered or expired delivery again, and otherwise the
// minute it next needs a sweep, never earlier than the write itself.
//
// That floor on the key is what makes a sweep's own floor safe (Sweep): an entry is never keyed
// before the moment it was written, so an entry waiting to be acted on can fall behind the cursor
// of the sweep that wrote it by less than the minute it was truncated to.
func pendingFor(r record, guid string, now time.Time) string {
	if r.Delivered || r.Expired {
		return ""
	}
	at := attention(r, now)
	if at.Before(now) {
		at = now
	}
	return pendingKey(at, guid)
}

// maxPendingFilters is the most filters a window of retention or less decomposes into, which is
// what holds the cost of one index read whatever a sweep's cursor says. From a floor at HH:MM,
// 60-MM minutes reach the next hour, 23-HH hours reach the next day, two whole days follow, then HH
// hours and MM+1 minutes reach a ceiling three days on: (60-MM) + (23-HH) + 2 + HH + (MM+1) = 86,
// whatever HH and MM are, since the two terms that hold them cancel. A wider window would cost
// more, which is why Sweep clamps its floor to now-retention.
const maxPendingFilters = 60 + 23 + 2 + 1

// pendingFilters decomposes the minutes from floor to ceiling, both included, into the
// pairwise-disjoint subject filters a watch of that window reads: whole days, whole hours and
// single minutes, taken greedily from the floor, largest first. They have to be disjoint because
// the server refuses a consumer whose filter subjects overlap; it caps how many there are nowhere.
// A floor past the ceiling -- a cursor from a clock ahead of this one -- is read as the ceiling's
// own minute, so a window is never empty.
func pendingFilters(floor, ceiling time.Time) []string {
	from, to := minute(floor), minute(ceiling)
	if from.After(to) {
		from = to
	}
	filters := make([]string, 0, maxPendingFilters)
	for !from.After(to) {
		switch {
		case from.Hour() == 0 && from.Minute() == 0 && !from.Add(24*time.Hour).After(to.Add(time.Minute)):
			filters = append(filters, pendingPrefix+from.Format("20060102")+".>")
			from = from.Add(24 * time.Hour)
		case from.Minute() == 0 && !from.Add(time.Hour).After(to.Add(time.Minute)):
			filters = append(filters, pendingPrefix+from.Format("20060102.15")+".>")
			from = from.Add(time.Hour)
		default:
			filters = append(filters, pendingPrefix+from.Format(pendingLayout)+".>")
			from = from.Add(time.Minute)
		}
	}
	return filters
}

// minute is at as an entry's key reads it: UTC, and the minute it falls in.
func minute(at time.Time) time.Time { return at.UTC().Truncate(time.Minute) }
