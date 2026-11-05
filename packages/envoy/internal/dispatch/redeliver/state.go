package redeliver

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// The sweep's state is the NATS KV bucket OpenState opens, and this file is the layer that reads
// and writes it: the record one delivery's memory is kept as and what a sweeper asks of it, the
// cursor record, the read and the write of each, the scan of every record the bucket holds, and
// what tells a lost compare-and-set from a refusal. It decides nothing about a delivery -- that is
// sweep.go -- and keys no entry by the minute it is next due, which is index.go.

// record is the sweep's memory of one delivery, keyed by its GUID.
type record struct {
	// Attempts counts redeliveries GitHub accepted.
	Attempts int `json:"attempts"`
	// Refusals counts redelivery requests GitHub refused. Attempts and Refusals add up to the
	// requests made for the delivery.
	Refusals int `json:"refusals"`
	// LastAttemptAt is when the last request was made, accepted or refused.
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	// NextAttemptAt is when the next request is due: the last request plus the backoff. A record
	// without it is due.
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	// FirstDeliveredAt is the oldest failed attempt the sweep has seen for the delivery, kept as
	// the minimum on every write; the delivery expires 72 hours after it.
	FirstDeliveredAt time.Time `json:"first_delivered_at,omitzero"`
	// Delivered is GitHub having recorded an OK attempt for the delivery: nothing more is asked.
	Delivered bool `json:"delivered,omitempty"`
	// Expired is the sweep having stopped asking for an undelivered delivery, for ExpiredReason.
	Expired       bool   `json:"expired,omitempty"`
	ExpiredReason string `json:"expired_reason,omitempty"`
	// Delivery is the attempt the last request, or the claim in flight, named. A refused request
	// adds no attempt to GitHub's log, so this is how a sweep asks again for a delivery its listing
	// no longer returns.
	Delivery githubapp.Delivery `json:"delivery"`
	// InFlight is a claim: a sweeper is asking GitHub for the delivery and has not recorded the
	// answer. The claim is the only write before the request, so a sweeper that dies before its
	// outcome write leaves the record as it was but for this, ClaimedAt and Delivery.
	InFlight bool `json:"in_flight,omitempty"`
	// ClaimedAt is when the claim was written, on the claiming sweeper's clock. The claim stands
	// until claimDeadline after it.
	ClaimedAt time.Time `json:"claimed_at,omitzero"`
	// Pending is the index entry this record's last writer wrote (pendingFor), empty once the
	// record is closed and in a record an older build wrote, which had no index to write to. An
	// entry some other record names, or one no record names at all, is a stray a sweep that reads
	// it deletes, so this is what makes that repair exact.
	Pending string `json:"pending,omitempty"`
}

// claimDeadline is how long a claim stands: three times githubapp.RequestTimeout, the longest one
// redelivery request takes before its client gives up on it, so a live claimant has GitHub's answer
// and writes it well before another sweeper takes the delivery from it. A claim older than that is
// void: its sweeper died between the claim and the outcome write, and the delivery is decided
// again. One sweeper's clock writes ClaimedAt and another's reads it; a minute is far above the
// clock skew between nodes.
func claimDeadline() time.Duration { return 3 * githubapp.RequestTimeout }

// claimEndsAt is when a claim written at claimedAt is void and the delivery it holds is decided
// again (claimDeadline).
func claimEndsAt(claimedAt time.Time) time.Time { return claimedAt.Add(claimDeadline()) }

// claimEndsAt is the same moment for a record that is InFlight, read from the ClaimedAt the claim
// wrote. It means nothing for a record that is not, whose ClaimedAt is zero.
func (r record) claimEndsAt() time.Time { return claimEndsAt(r.ClaimedAt) }

// closed reports that nothing more is asked of the delivery: GitHub recorded an OK attempt for it,
// or the sweep stopped asking.
func (r record) closed() bool { return r.Delivered || r.Expired }

// open reports that the delivery is still the sweep's to act on: not closed, and naming the
// delivery a request would be made for. A record that names none is a claim undone before its
// request was sent, which the listing brings back itself.
func (r record) open() bool { return !r.closed() && r.Delivery.GUID != "" }

// cursorRecord is where the last complete sweep started, and what wrote it.
type cursorRecord struct {
	At time.Time `json:"at"`
	// Indexed is this build's mark on the cursor: every sweep that writes one sets it, and a
	// build without the index writes a cursor carrying no such field. So a cursor read without
	// it says a build that writes no index entry has swept since this build last did, and the
	// records that build created have none -- which is what makes the back-fill run again
	// whatever its marker says (backfillIndex). A bucket with no cursor at all has never been
	// swept, and its records are this build's own.
	Indexed bool `json:"indexed"`
}

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

// scanOpenRecords reads every record the bucket holds and returns the deliveries, newest first,
// whose record is open (neither Delivered nor Expired) and whose failed attempts the listing did
// not return. A refused request adds no attempt to GitHub's log, and a failed attempt leaves the
// window long before 72 hours have passed, so once a delivery's failed attempts are older than the
// window, its record is all that brings it back: to be asked again when it is due, expired, or
// closed when the listing shows GitHub delivered it. A record in flight is a sweeper's claim, which
// made no request yet: it comes back too, as ClaimedElsewhere while the claim stands
// (claimDeadline) and asked for again after it. A record that names no delivery is a claim undone
// before its request was sent; no request was made, and the listing still brings that delivery
// back.
//
// A sweep given a window of its own reads this rather than the index, so `envoy-dispatch
// redeliver-webhooks`, dry run or not, reports every open delivery, due or not, whatever the index
// holds, and pays three days of records for it. The index's back-fill reads it too
// (backfillIndex).
//
// nats.go ends a watch three ways and only one of them is a complete read -- the watch that
// delivered its nil end marker -- so this drains it through the one place that tells them apart
// (kvwatch.DrainExistingKeys). A scan an idle timer ended is an error here and not a bucket with
// nothing more in it, which would under-report the open deliveries and say nothing.
func (s *Sweeper) scanOpenRecords(listed map[string][]githubapp.Delivery) ([]openRecord, error) {
	watcher, err := s.state.Watch(recordPrefix+"*", nats.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("list the redelivery records: %w", err)
	}
	var open []openRecord
	var failed error
	if _, err := kvwatch.DrainExistingKeys(watcher, "webhook redelivery records", s.Logger, func(entry nats.KeyValueEntry) {
		if failed != nil {
			return
		}
		guid := strings.TrimPrefix(entry.Key(), recordPrefix)
		if _, seen := listed[guid]; seen {
			return
		}
		var current record
		if err := json.Unmarshal(entry.Value(), &current); err != nil {
			failed = fmt.Errorf("decode the redelivery record of %s: %w", guid, err)
			return
		}
		if !current.open() {
			return
		}
		if current.Delivery.GUID != guid {
			failed = fmt.Errorf("the redelivery record of %s names delivery %q", guid, current.Delivery.GUID)
			return
		}
		open = append(open, openRecord{guid: guid, record: current, revision: entry.Revision()})
	}); err != nil {
		return nil, err
	}
	if failed != nil {
		return nil, failed
	}
	sortOpenRecords(open)
	return open, nil
}

// readState reads key's JSON value and its revision; an absent key is the zero value at revision
// zero. what names the value in an error.
func readState[T any](kv bus.KeyValue, key, what string) (T, uint64, error) {
	var value T
	entry, err := kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return value, 0, nil
	}
	if err != nil {
		return value, 0, fmt.Errorf("read the redelivery %s: %w", what, err)
	}
	if err := json.Unmarshal(entry.Value(), &value); err != nil {
		return value, 0, fmt.Errorf("decode the redelivery %s: %w", what, err)
	}
	return value, entry.Revision(), nil
}

// writeState writes value's JSON at key only over revision (absent when zero), so a lost race is
// a conflict (isConflict), and returns the new revision.
func writeState(kv bus.KeyValue, key string, value any, revision uint64) (uint64, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	if revision == 0 {
		return kv.Create(key, data)
	}
	return kv.Update(key, data, revision)
}

// openState opens the state with Open unless a sweep already has, and keeps it for every sweep
// after: the one place the bucket is opened. A sweep it fails for opens nothing, so the next one
// calls Open again.
func (s *Sweeper) openState() error {
	if s.state.KeyValue != nil {
		return nil
	}
	state, err := s.Open()
	if err != nil {
		return err
	}
	s.state = state
	return nil
}

// readCursor reads the cursor whole, since what wrote it matters as much as where it is
// (cursorRecord). A bucket with no cursor answers the zero value at revision zero.
func (s *Sweeper) readCursor() (cursorRecord, uint64, error) {
	return readState[cursorRecord](s.state, cursorKey, "cursor")
}

// writeCursor records the sweep's start, marked as this build's (cursorRecord). A lost race means
// another sweeper wrote a cursor at the same moment; either value serves.
func (s *Sweeper) writeCursor(at time.Time, revision uint64) {
	if _, err := writeState(s.state, cursorKey, cursorRecord{At: at, Indexed: true}, revision); err != nil && !isConflict(err) {
		s.Logger.Warn("webhook redelivery cursor not written", "error", err)
	}
}

func (s *Sweeper) readRecord(guid string) (record, uint64, error) {
	return readState[record](s.state, recordPrefix+guid, "record of "+guid)
}

// putRecord writes next only if the record is still at revision (absent when zero), so two
// sweepers never both request one attempt, and returns the new revision. It is the record write on
// its own; every caller but undoClaim goes through writeRecord, which keeps the index with it.
//
// A write that loses its compare-and-set is written false and no error: another sweeper decided
// the delivery first, and what that sweeper wrote carries the delivery on. A write the bucket
// refused is that error, and carries nothing on at all: the record still holds what it held, so
// its caller is what keeps the delivery from being lost (writeRecord, undoClaim).
func (s *Sweeper) putRecord(guid string, next record, revision uint64) (uint64, bool, error) {
	written, err := writeState(s.state, recordPrefix+guid, next, revision)
	switch {
	case err == nil:
		return written, true, nil
	case isConflict(err):
		return 0, false, nil
	default:
		return 0, false, err
	}
}

func isConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}
