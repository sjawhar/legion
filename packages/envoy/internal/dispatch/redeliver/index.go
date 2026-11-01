package redeliver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// The sweep's index is one entry per open delivery, keyed by the minute that delivery next needs a
// sweep, so a sweep reads what is due rather than everything the bucket remembers: the bucket
// keeps 72 hours of records, every delivery acted on in three days, and a sweep that read them all
// would pay for all of them every two minutes.
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
// key prefix and a consumer over several filter subjects need nothing added to it. An entry lives
// in the same bucket as the records, under its 72-hour TTL, its file storage and its
// ENVOY_NATS_REPLICAS copies (OpenState), so an entry a sweeper died before deleting expires with
// them.
const pendingPrefix = "pending."

// pendingLayout is the time in an entry's key, in the tokens a filter selects by: the day, then the
// hour, then the minute. pendingDayLayout and pendingHourLayout are its leading tokens, which are
// what a filter over a whole day or a whole hour selects by (pendingFilters).
const (
	pendingLayout     = "20060102.15.04"
	pendingDayLayout  = "20060102"
	pendingHourLayout = "20060102.15"
)

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
// due. A record with no next request -- one a build without this index wrote, or one whose request
// was never made -- carries the zero time, which is before every moment its callers floor it to, so
// such a record is due at the write that reads this (pendingFor).
func attention(r record) time.Time {
	if r.InFlight {
		return r.claimEndsAt()
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
	if r.closed() {
		return ""
	}
	at := attention(r)
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
	// end is the window's exclusive end: a whole day or hour belongs to it only if it reaches no
	// further than the minute after the ceiling.
	end := to.Add(time.Minute)
	for !from.After(to) {
		switch {
		case from.Hour() == 0 && from.Minute() == 0 && !from.Add(24*time.Hour).After(end):
			filters = append(filters, pendingPrefix+from.Format(pendingDayLayout)+".>")
			from = from.Add(24 * time.Hour)
		case from.Minute() == 0 && !from.Add(time.Hour).After(end):
			filters = append(filters, pendingPrefix+from.Format(pendingHourLayout)+".>")
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

// openRecord is one open delivery a sweep decides from its record rather than from a listing: the
// GUID the record is keyed by, the record, and the revision it was read at.
type openRecord struct {
	guid     string
	record   record
	revision uint64
}

// sortOpenRecords puts the newest delivery first, the order a listing returns them in.
func sortOpenRecords(open []openRecord) {
	slices.SortFunc(open, func(a, b openRecord) int {
		return b.record.Delivery.DeliveredAt.Compare(a.record.Delivery.DeliveredAt)
	})
}

// openDue returns the open deliveries the index says need a sweep in the window from floor to now,
// newest first, and how many entries its watch delivered. One the listing returned is left to the
// listing, which has its attempts, and an entry a record disowns is deleted as it is read: a
// sweeper that died between its index write and its record write, or between a record write and
// the delete of the entry before it, leaves one stray entry, and the sweep that reads it repairs
// it. A record disowns an entry by being closed, by being gone, or by naming an entry of its own;
// a record that names no entry at all disowns none. That last is a record whose writer did not
// keep the field -- a build without this index, which an ordinary rolling deploy and a rollback
// both leave records of -- so its entry is adopted rather than deleted, its delivery is decided
// like any other due one, and the write that decides it gives the record an entry of its own again
// (decide).
//
// A stray is deleted only once its key is older than cursorOverlap before now. An entry is written
// before the record that names it, so an entry keyed in that margin may be one another sweeper is
// about to name -- a sweep given a window of its own writes records a running sweep reads between
// the two writes -- and deleting it would leave that record naming a key that is gone. An entry is
// only ever keyed at the minute of a moment at or after its own write (pendingFor), so a key older
// than the margin was written before it; cursorOverlap is what the rest of the file allows for one
// sweeper's clock against another's. A stray written in this minute is left to a later sweep. A
// dry run repairs nothing, since it writes nothing at all: it leaves the delivery out of what it
// returns, the record naming no entry of its own, and leaves the entry itself to the next sweep
// that acts.
//
// A floor is safe because of what a complete sweep leaves behind. After one that started at T,
// every entry still in the bucket names a record whose attention time is at or after T: every entry
// that sweep selected was either acted on, which moved the entry forward, or left in place with an
// entry already at or after T. The floor is taken down to its own minute because an entry is keyed
// by the minute: one due at 10:00:30 is keyed 10:00, and a sweep that started at 10:00:10 and found
// it not yet due must still be able to select it afterwards.
//
// The completeness check matters more here than anywhere else in the sweep. nats.go ends a watch
// three ways and only one is a complete read (kvwatch.DrainExistingKeys); a sweep that read some of
// its due entries and called itself complete would advance its cursor past the rest, and nothing
// would ever bring those deliveries back. That is the one way this design could lose a delivery.
func (s *Sweeper) openDue(listed map[string][]githubapp.Delivery, floor, now time.Time, dryRun bool) ([]openRecord, int, error) {
	watcher, err := s.state.WatchFiltered(pendingFilters(floor, now), nats.MetaOnly(), nats.IgnoreDeletes())
	if err != nil {
		return nil, 0, fmt.Errorf("list the due redelivery index entries: %w", err)
	}
	// An entry's value is empty and its record is read by its own key, so the watch asks for no
	// value, and the keys are taken first: a Get for each inside the watch's loop would hold the
	// watch open for as long as the records take to read.
	var keys []string
	scan, err := kvwatch.DrainExistingKeys(watcher, "webhook redelivery index", s.Logger, func(entry nats.KeyValueEntry) {
		keys = append(keys, entry.Key())
	})
	if err != nil {
		return nil, 0, err
	}
	var open []openRecord
	seen := map[string]bool{}
	// A stray older than this was written before the margin and is no sweeper's to name now.
	repairable := now.Add(-cursorOverlap)
	for _, key := range keys {
		at, guid, ok := parsePendingKey(key)
		if !ok {
			continue
		}
		current, revision, err := s.readRecord(guid)
		if err != nil {
			return nil, scan.Puts, err
		}
		if revision == 0 || !current.open() || (current.Pending != "" && current.Pending != key) {
			if !dryRun && at.Before(repairable) {
				s.dropPending(key)
			}
			continue
		}
		// The filters are disjoint, so every key reaches this loop once, and of two keys naming
		// one GUID at most one is the Pending its record names -- but a record that names none is
		// adopted by each key that names it, so two entries can reach here for one delivery. The
		// map is what holds that delivery to the one decision it is owed; the entries the decision
		// does not take over are strays a later sweep deletes.
		if seen[guid] {
			continue
		}
		seen[guid] = true
		if _, listedNow := listed[guid]; listedNow {
			continue
		}
		open = append(open, openRecord{guid: guid, record: current, revision: revision})
	}
	sortOpenRecords(open)
	return open, scan.Puts, nil
}

// indexKey marks the bucket as one whose open records all have an index entry, so the back-fill a
// build that writes entries runs over a bucket an older build filled happens once (backfillIndex)
// rather than every two minutes.
const indexKey = "index"

// backfillIndex gives an index entry, and the Pending that names it, to every open record that has
// none, marks the bucket indexed, and answers whether it finished: a record it could not index
// leaves the bucket unmarked and the sweep incomplete (Sweep), so the next sweep reads every
// record again and repairs what this one left. A build that writes entries meets a bucket an older
// one filled with records that have none at all, and an index read would never return them.
//
// A record counts as indexed only when its write landed. A write the bucket refused wrote no entry
// a record names, and one that lost its compare-and-set may have lost it to a build that writes no
// entry at all, so neither marks the bucket.
//
// The marker lives in the same bucket under its 72-hour TTL, so it expires and this runs again at
// most once every three days: a standing repair rather than a one-off, which is also what covers a
// sweeper that died in the middle of one. Until it expires, a build without the Pending field is
// what leaves a record unindexed, and two rules carry those records: one it rewrote keeps the entry
// that names it, which openDue adopts rather than deletes, and one it wrote afresh is listed within
// Lookback and indexed by the sweep that decides it (decide). A rolling deploy leaves both, and so
// does a rollback and roll-forward inside the three days. A dry run writes neither the entries nor
// the marker.
func (s *Sweeper) backfillIndex(listed map[string][]githubapp.Delivery, now time.Time, dryRun bool) (bool, error) {
	if dryRun {
		return true, nil
	}
	marked, err := s.indexMarked()
	if err != nil {
		return false, err
	}
	if marked {
		return true, nil
	}
	open, err := s.scanOpenRecords(listed)
	if err != nil {
		return false, err
	}
	indexed := true
	for _, stored := range open {
		if stored.record.Pending != "" {
			continue
		}
		next := stored.record
		_, written, err := s.writeRecord(stored.guid, &next, stored.revision, now)
		switch {
		case err != nil:
			s.Logger.Warn("webhook redelivery record not back-filled into the index; the next sweep reads every record again",
				"guid", stored.guid, "error", err)
			indexed = false
		case !written:
			s.Logger.Warn("webhook redelivery record not back-filled into the index; another writer reached it first and the next sweep reads every record again",
				"guid", stored.guid)
			indexed = false
		}
	}
	if !indexed {
		return false, nil
	}
	if _, err := s.state.Put(indexKey, nil); err != nil {
		s.Logger.Warn("webhook redelivery index not marked back-filled; the next sweep reads every record again", "error", err)
	}
	return true, nil
}

// indexMarked reports whether a sweep has back-filled the index since the marker last expired
// (backfillIndex).
func (s *Sweeper) indexMarked() (bool, error) {
	_, err := s.state.Get(indexKey)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the redelivery index marker: %w", err)
	}
	return true, nil
}

// errNotWritten is a write of a delivery's state the bucket refused: its index entry
// (errPendingNotWritten) or its record (errRecordNotWritten). Neither carries the delivery on, so
// the sweep that met one is incomplete and the delivery is decided again by the sweep after it
// (Sweep). That is what tells them from a record write that lost its compare-and-set, which
// another sweeper's own write did carry on.
var (
	errNotWritten        = errors.New("webhook redelivery state not written")
	errPendingNotWritten = fmt.Errorf("%w: index entry", errNotWritten)
	errRecordNotWritten  = fmt.Errorf("%w: record", errNotWritten)
)

// writeRecord writes next over revision and keeps the delivery's index entry with it, which is why
// every record write but undoClaim's comes through here: the order lives in one place. The entry
// the record named before this write is the Pending next arrives with, and next.Pending is left as
// the entry this one wrote.
//
// The order is the new entry, then the record, then the entry the record named before it. Entries
// are written with Put and Delete and never Create or Update, so two sweepers writing one entry is
// nothing to settle, and the first Create against a delivery is still its claim.
//
//   - An entry that cannot be written stops the write there and returns errPendingNotWritten. The
//     record is left as it was, the sweep is marked incomplete, its cursor stays where it was and
//     the delivery is decided again by the sweep after it. That is what keeps a lost index write
//     from turning into a lost delivery, and why nothing here reads the whole bucket from time to
//     time looking for records the index forgot.
//   - A record the bucket refuses is errRecordNotWritten, and leaves the sweep incomplete for the
//     same reason: the record still holds the Pending it held, which is behind the floor of the
//     sweep after this one, so only an incomplete sweep brings the delivery back. The entry this
//     write put there first is a stray the sweep that reads it deletes.
//   - A record write that loses its compare-and-set is written false and no error: another sweeper
//     decided the delivery first, and the entry this one wrote is that same stray (openDue).
//   - A delete that fails is logged and no more: what it leaves is that same stray.
func (s *Sweeper) writeRecord(guid string, next *record, revision uint64, now time.Time) (uint64, bool, error) {
	previous := next.Pending
	want := pendingFor(*next, guid, now)
	next.Pending = want
	if want != "" && want != previous {
		if _, err := s.state.Put(want, nil); err != nil {
			s.Logger.Error("webhook redelivery index entry not written; the delivery is decided again by the next sweep",
				"guid", guid, "key", want, "error", err)
			return 0, false, fmt.Errorf("%w: %s: %w", errPendingNotWritten, guid, err)
		}
	}
	written, ok, err := s.putRecord(guid, *next, revision)
	if err != nil {
		s.Logger.Error("webhook redelivery record not written; the delivery is decided again by the next sweep",
			"guid", guid, "error", err)
		return 0, false, fmt.Errorf("%w: %s: %w", errRecordNotWritten, guid, err)
	}
	if !ok {
		return 0, false, nil
	}
	if previous != "" && previous != want {
		s.dropPending(previous)
	}
	return written, true, nil
}

// dropPending deletes an index entry no record names any more: the record it named is closed, gone,
// or indexed elsewhere, so nothing is owed a sweep by it. A delete that fails leaves an entry a
// later sweep reads once more and repairs then, so it is logged and not raised.
func (s *Sweeper) dropPending(key string) {
	if err := s.state.Delete(key); err != nil {
		s.Logger.Warn("webhook redelivery index entry not deleted", "key", key, "error", err)
	}
}
