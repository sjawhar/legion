package dedupe

import (
	"sync"
	"time"
)

// sweepEvery is how often a write sweeps out records past their age.
const sweepEvery = time.Minute

type memoryRecord struct {
	delivered bool
	// kept marks an attempt Keep settled: it holds for AttemptedAge, an unsettled one for the
	// takeover age.
	kept bool
	at   time.Time
}

// Memory is the delivery ledger of a listener that cannot open the listener state bucket (its
// NATS user lacks the grant) and so delivers as the machine's one process: it keeps the records
// in process, and a restart forgets them. No other process writes to it, so it never defers.
type Memory struct {
	mu            sync.Mutex
	records       map[Key]memoryRecord
	takeoverAfter time.Duration
	now           func() time.Time
	lastSweep     time.Time
}

// NewMemory makes an empty in-process ledger. takeoverAfter is how long an attempt nothing has
// settled holds the pair, as KVConfig.TakeoverAfter: the longest a prompt can take and a margin.
func NewMemory(takeoverAfter time.Duration) *Memory {
	return &Memory{records: make(map[Key]memoryRecord), takeoverAfter: takeoverAfter, now: time.Now}
}

// Attempt is Delivered for a pair delivered within DeliveredAge, and InFlight for one whose attempt
// nothing has settled within the takeover age or that Keep kept within AttemptedAge; otherwise it
// records the attempt and is Proceed.
func (m *Memory) Attempt(dedupeKey, sessionID string) (Attempt, error) {
	pair := Key{DedupeKey: dedupeKey, SessionID: sessionID}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if record, ok := m.records[pair]; ok && m.live(record, now) {
		if record.delivered {
			return Attempt{Verdict: Delivered, pair: pair}, nil
		}
		return Attempt{Verdict: InFlight, pair: pair}, nil
	}
	m.sweepLocked(now)
	m.records[pair] = memoryRecord{at: now}
	return Attempt{Verdict: Proceed, pair: pair}, nil
}

// Record marks the attempt's pair delivered.
func (m *Memory) Record(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[attempt.pair] = memoryRecord{delivered: true, at: m.now()}
	return nil
}

// Clear drops the attempt's pair unless it has since been recorded delivered.
func (m *Memory) Clear(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.records[attempt.pair]; ok && !record.delivered {
		delete(m.records, attempt.pair)
	}
	return nil
}

// Keep holds the attempt's pair until the attempt is AttemptedAge old, unless it has since been
// recorded delivered or dropped.
func (m *Memory) Keep(attempt Attempt) {
	if attempt.Verdict != Proceed {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if record, ok := m.records[attempt.pair]; ok && !record.delivered {
		record.kept = true
		m.records[attempt.pair] = record
	}
}

// live reports whether record still stops a delivery of its pair, by the ledgers' shared rules: a
// delivered record by deliveredStops, an attempt by attemptHolds.
func (m *Memory) live(record memoryRecord, now time.Time) bool {
	age := now.Sub(record.at)
	if record.delivered {
		return deliveredStops(age, DeliveredAge)
	}
	return attemptHolds(age, record.kept, m.takeoverAfter)
}

// sweepLocked drops every record past the longest age it can hold for, at most once per
// sweepEvery: an attempt Keep may still keep is kept until AttemptedAge.
func (m *Memory) sweepLocked(now time.Time) {
	if now.Sub(m.lastSweep) < sweepEvery {
		return
	}
	m.lastSweep = now
	attemptLimit := max(AttemptedAge, m.takeoverAfter)
	for pair, record := range m.records {
		if record.delivered && !m.live(record, now) || !record.delivered && now.Sub(record.at) > attemptLimit {
			delete(m.records, pair)
		}
	}
}
