package dedupe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// The states a ledger record holds.
const (
	stateAttempted = "attempted"
	stateDelivered = "delivered"
)

// kvRecord is a ledger record's value. at is informational: every age is read from the entry's
// KV timestamp, the server's clock, which every pod shares.
type kvRecord struct {
	State string `json:"state"`
	Boot  string `json:"boot"`
	At    int64  `json:"at"`
}

// kvAttemptRaces bounds the rounds of one Attempt that another process's write to the same key
// can cost it (a create that finds the key, then an update that finds it changed).
const kvAttemptRaces = 4

// KVConfig configures a KV ledger.
type KVConfig struct {
	// Durable is the listener's durable (bus.ListenerDurable), which names the machine whose
	// records these are.
	Durable string
	// Boot is this process's boot id (the delivery lease's), random per process start: an
	// attempt under another boot is another process's.
	Boot string
	// TakeoverAfter is the age past which an unresolved attempt is taken over: another process's,
	// and this process's own that nothing has settled (Record, Clear, Keep), whose prompt has
	// ended by then. It is the longest a prompt can take (session.Deliverer.RequestLimit) and a
	// margin.
	TakeoverAfter time.Duration
	// GoneBoot reports whether boot belongs to a process that no longer holds the machine's
	// delivery lease: one the current holder took the lease over from. Its unresolved attempt is
	// taken over at once, however young, since that process delivers nothing more. Nil treats
	// every other boot as one that may still be prompting.
	GoneBoot func(boot string) bool
	// DeliveredAge is DeliveredAge when zero; a test shortens it. There is no field for the
	// attempted age: both ledgers hold an attempt by the package's one rule (attemptHolds), and a
	// field only one of them read would let a change to it move the sweep without moving the hold.
	DeliveredAge time.Duration
	// Now is time.Now when nil: the clock each record's KV timestamp, and each attempt this
	// process holds, is aged against.
	Now func() time.Time
}

// KV is the delivery ledger every pod of one machine shares, the `delivery.` keys of the listener
// state bucket: one record per (dedupe key, session) pair, under
// `delivery.<durable>.<first 32 hex of sha256(dedupe key)>.<session token>` (bus.ListenerLedgerPrefix,
// SessionToken).
type KV struct {
	kv     bus.KeyValue
	prefix string
	cfg    KVConfig

	// mu guards writing, held and lastSweep.
	mu sync.Mutex
	// writing holds the pairs an Attempt of this process is writing the record of, each entry the
	// channel that Attempt closes when it returns: the per-pair lock another Attempt for the pair
	// waits on (claim, release).
	writing map[Key]chan struct{}
	// held is what this process knows of its own attempts, by pair: each attempt Attempt answered
	// Proceed for that nothing has settled yet, and each Keep kept. An attempted record of this
	// boot holds this process (InFlight) only while held holds the attempt that wrote it; any
	// other such record, one a clear could not delete or a write whose reply was lost left, is
	// taken over. A write sweeps out the entries past their age (sweepEvery), so held holds no
	// more than the attempts of the last AttemptedAge.
	held      map[Key]heldAttempt
	lastSweep time.Time
}

// heldAttempt is one of this process's attempts, as KV.held holds it.
type heldAttempt struct {
	// revision is the revision of the attempted record the attempt wrote; zero for none.
	revision uint64
	// at is when the attempt was written, by KVConfig.Now.
	at time.Time
	// kept marks an attempt Keep settled: it holds for AttemptedAge, an unsettled one for
	// TakeoverAfter.
	kept bool
}

// NewKV makes the ledger cfg describes over kv, the listener state bucket.
func NewKV(kv bus.KeyValue, cfg KVConfig) *KV {
	if cfg.DeliveredAge == 0 {
		cfg.DeliveredAge = DeliveredAge
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &KV{
		kv:      kv,
		prefix:  bus.ListenerLedgerPrefix(cfg.Durable),
		cfg:     cfg,
		writing: make(map[Key]chan struct{}),
		held:    make(map[Key]heldAttempt),
	}
}

// plainSessionID is a session id that is a KV key token as it stands, short enough to keep every
// key of the listener state bucket that ends in one well inside the bound bus.KeyValue holds a key
// to.
var plainSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// SessionToken is the session's token in a key of the listener state bucket (a ledger key here,
// the session's last-seen record in internal/session): the id itself when plainSessionID holds,
// so an operator reading the bucket sees whose record it is, else "=" and the first 32 hex of the
// id's sha256, since an id can hold a character a KV key refuses (a dot splits a token, `:`
// leaves nats.go's key alphabet) or run past the key bound. A plain id holds no "=", so the two
// forms never meet.
func SessionToken(sessionID string) string {
	if plainSessionID.MatchString(sessionID) {
		return sessionID
	}
	return "=" + digest(sessionID)
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// Key returns the ledger key of (dedupeKey, sessionID).
func (l *KV) Key(dedupeKey, sessionID string) string {
	return l.prefix + digest(dedupeKey) + "." + SessionToken(sessionID)
}

func (l *KV) value(state string) []byte {
	value, _ := json.Marshal(kvRecord{State: state, Boot: l.cfg.Boot, At: l.cfg.Now().UnixMilli()})
	return value
}

// Attempt writes an attempted record for the pair with Create. When the key holds a record it
// reads it: delivered within DeliveredAge is Delivered; attempted by this boot is InFlight while
// this process holds the attempt that wrote it (unsettled and younger than TakeoverAfter, or kept
// and younger than AttemptedAge), and taken over otherwise; attempted by a boot GoneBoot reports
// is taken over at once; attempted by another boot (or a record this build cannot read) younger
// than TakeoverAfter is Deferred until it reaches that age; anything older is taken over with
// Update at its revision, which is Proceed. Two Attempts of this process for one pair run one
// after the other.
func (l *KV) Attempt(dedupeKey, sessionID string) (Attempt, error) {
	pair := Key{DedupeKey: dedupeKey, SessionID: sessionID}
	writing := l.claim(pair)
	attempt, err := l.attempt(pair, l.Key(dedupeKey, sessionID))
	l.release(pair, attempt, writing)
	return attempt, err
}

func (l *KV) attempt(pair Key, key string) (Attempt, error) {
	for range kvAttemptRaces {
		revision, err := l.kv.Create(key, l.value(stateAttempted))
		if err == nil {
			return Attempt{Verdict: Proceed, pair: pair, key: key, revision: revision}, nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return Attempt{}, fmt.Errorf("create delivery record %s: %w", key, err)
		}
		entry, err := l.kv.Get(key)
		if errors.Is(err, nats.ErrKeyNotFound) {
			continue // cleared between the create and the read
		}
		if err != nil {
			return Attempt{}, fmt.Errorf("read delivery record %s: %w", key, err)
		}
		if verdict, retryIn, holds := l.judge(pair, entry); holds {
			return Attempt{Verdict: verdict, RetryIn: retryIn, pair: pair, key: key}, nil
		}
		revision, err = l.kv.Update(key, l.value(stateAttempted), entry.Revision())
		if err == nil {
			return Attempt{Verdict: Proceed, pair: pair, key: key, revision: revision}, nil
		}
		if !errors.Is(err, nats.ErrKeyExists) {
			return Attempt{}, fmt.Errorf("take over delivery record %s: %w", key, err)
		}
	}
	return Attempt{}, fmt.Errorf("delivery record %s: changed by another writer at each of %d tries", key, kvAttemptRaces)
}

// judge reads entry, the pair's record, and reports the verdict it holds this process to, or
// holds=false when the record holds no one and this process takes it over.
func (l *KV) judge(pair Key, entry nats.KeyValueEntry) (verdict Verdict, retryIn time.Duration, holds bool) {
	age := l.cfg.Now().Sub(entry.Created())
	var record kvRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		record = kvRecord{}
	}
	switch {
	case record.State == stateDelivered:
		return Delivered, 0, deliveredStops(age, l.cfg.DeliveredAge)
	case record.State == stateAttempted && record.Boot == l.cfg.Boot:
		return InFlight, 0, l.holds(pair, entry.Revision())
	case record.State == stateAttempted && record.Boot != "" && l.cfg.GoneBoot != nil && l.cfg.GoneBoot(record.Boot):
		return Deferred, 0, false
	default:
		return Deferred, l.cfg.TakeoverAfter - age, age < l.cfg.TakeoverAfter
	}
}

// claim takes the pair's write lock for an Attempt of this process, once any other Attempt of
// this process for the pair has returned, and returns the channel release closes.
func (l *KV) claim(pair Key) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		l.sweepLocked(l.cfg.Now())
		writing, locked := l.writing[pair]
		if !locked {
			writing = make(chan struct{})
			l.writing[pair] = writing
			return writing
		}
		l.mu.Unlock()
		<-writing
		l.mu.Lock()
	}
}

// release gives the pair's write lock back: a Proceed attempt is now this process's unsettled
// attempt for the pair; any other answer leaves what this process held of the pair as it was.
func (l *KV) release(pair Key, attempt Attempt, writing chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if attempt.Verdict == Proceed {
		l.held[pair] = heldAttempt{revision: attempt.revision, at: l.cfg.Now()}
	}
	delete(l.writing, pair)
	close(writing)
}

// holds reports whether this process holds the attempt that wrote revision of pair's record, by
// the ledgers' shared rule (attemptHolds).
func (l *KV) holds(pair Key, revision uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.held[pair]
	return held.revision == revision && attemptHolds(l.cfg.Now().Sub(held.at), held.kept, l.cfg.TakeoverAfter)
}

// forget drops attempt from what this process holds, unless a later attempt of this process has
// replaced it.
func (l *KV) forget(attempt Attempt) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if held, ok := l.held[attempt.pair]; ok && held.revision == attempt.revision {
		delete(l.held, attempt.pair)
	}
}

// sweepLocked drops every attempt this process holds that is past the longest age it can hold
// for, at most once per sweepEvery, leaving the pairs an Attempt is writing to its release. The
// caller holds l.mu.
func (l *KV) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	limit := max(AttemptedAge, l.cfg.TakeoverAfter)
	for pair, held := range l.held {
		if _, writing := l.writing[pair]; !writing && now.Sub(held.at) >= limit {
			delete(l.held, pair)
		}
	}
}

// Record writes the pair's delivered record with Put, over whatever the key holds. A record the
// bucket does not take keeps the attempt (Keep): the pair was delivered, and a repeat of the
// message within AttemptedAge is not prompted again.
func (l *KV) Record(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	if _, err := l.kv.Put(attempt.key, l.value(stateDelivered)); err != nil {
		l.Keep(attempt)
		return fmt.Errorf("record delivery %s: %w", attempt.key, err)
	}
	l.forget(attempt)
	return nil
}

// Clear gives the attempt up in this process and deletes the pair's record at the revision
// attempt wrote, so a record another process has written since (a take-over, a delivery) stays. A
// delete the bucket refuses leaves the record, which this process's next Attempt for the pair
// takes over.
func (l *KV) Clear(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	l.forget(attempt)
	err := l.kv.Delete(attempt.key, nats.LastRevision(attempt.revision))
	if err == nil || errors.Is(err, nats.ErrKeyExists) {
		return nil
	}
	return fmt.Errorf("clear delivery attempt %s: %w", attempt.key, err)
}

// Keep holds attempt in this process until it is AttemptedAge old: its record stands, and Attempt
// answers InFlight for the pair until then.
func (l *KV) Keep(attempt Attempt) {
	if attempt.Verdict != Proceed {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if held, ok := l.held[attempt.pair]; ok && held.revision == attempt.revision {
		held.kept = true
		l.held[attempt.pair] = held
	}
}
