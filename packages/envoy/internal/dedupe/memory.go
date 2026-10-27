package dedupe

import (
	"sync"
	"time"
)

// memorySweepEvery is how often a write sweeps out records past their age.
const memorySweepEvery = time.Minute

type memoryRecord struct {
	delivered bool
	at        time.Time
}

// Memory is the delivery ledger of a listener that cannot open the listener state bucket (its
// NATS user lacks the grant) and so delivers as the machine's one process: it keeps the records
// in process, and a restart forgets them. No other process writes to it, so it never defers.
type Memory struct {
	mu        sync.Mutex
	records   map[Key]memoryRecord
	now       func() time.Time
	lastSweep time.Time
}

// NewMemory makes an empty in-process ledger.
func NewMemory() *Memory {
	return &Memory{records: make(map[Key]memoryRecord), now: time.Now}
}

// Attempt is Delivered for a pair delivered within DeliveredAge and InFlight for one attempted
// within AttemptedAge; otherwise it records the attempt and is Proceed.
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

func (m *Memory) live(record memoryRecord, now time.Time) bool {
	age := now.Sub(record.at)
	if record.delivered {
		return age <= DeliveredAge
	}
	return age <= AttemptedAge
}

// sweepLocked drops every record past its age, at most once per memorySweepEvery.
func (m *Memory) sweepLocked(now time.Time) {
	if now.Sub(m.lastSweep) < memorySweepEvery {
		return
	}
	m.lastSweep = now
	for pair, record := range m.records {
		if !m.live(record, now) {
			delete(m.records, pair)
		}
	}
}
