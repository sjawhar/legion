package dedupe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	// TakeoverAfter is the age past which another process's unresolved attempt is taken over:
	// the longest a prompt can take (session.Deliverer.RequestLimit) and a margin.
	TakeoverAfter time.Duration
	// DeliveredAge and AttemptedAge are DeliveredAge and AttemptedAge when zero; a test shortens
	// them.
	DeliveredAge time.Duration
	AttemptedAge time.Duration
	// Now is time.Now when nil: the clock each record's KV timestamp is aged against.
	Now func() time.Time
}

// KV is the delivery ledger every pod of one machine shares, the `delivery.` keys of the listener
// state bucket: one record per (dedupe key, session) pair, under
// `delivery.<durable>.<first 32 hex of sha256(dedupe key)>.<session token>` (sessionToken).
type KV struct {
	kv     bus.KeyValue
	prefix string
	cfg    KVConfig
}

// NewKV makes the ledger cfg describes over kv, the listener state bucket.
func NewKV(kv bus.KeyValue, cfg KVConfig) *KV {
	if cfg.DeliveredAge == 0 {
		cfg.DeliveredAge = DeliveredAge
	}
	if cfg.AttemptedAge == 0 {
		cfg.AttemptedAge = AttemptedAge
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &KV{kv: kv, prefix: "delivery." + cfg.Durable + ".", cfg: cfg}
}

// plainSessionID is a session id that is a KV key token as it stands, short enough to keep every
// ledger key well inside the bound bus.KeyValue holds a key to.
var plainSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// sessionToken is the session's token in a ledger key: the id itself when plainSessionID holds,
// so an operator reading the bucket sees whose record it is, else "=" and the first 32 hex of the
// id's sha256, since an id can hold a character a KV key refuses (a dot splits a token, `:`
// leaves nats.go's key alphabet) or run past the key bound. A plain id holds no "=", so the two
// forms never meet.
func sessionToken(sessionID string) string {
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
	return l.prefix + digest(dedupeKey) + "." + sessionToken(sessionID)
}

func (l *KV) value(state string) []byte {
	value, _ := json.Marshal(kvRecord{State: state, Boot: l.cfg.Boot, At: l.cfg.Now().UnixMilli()})
	return value
}

// Attempt writes an attempted record for the pair with Create. When the key holds a record it
// reads it: delivered within DeliveredAge is Delivered; attempted by this boot within AttemptedAge
// is InFlight; attempted by another boot (or a record this build cannot read) younger than
// TakeoverAfter is Deferred until it reaches that age; anything older is taken over with Update at
// its revision, which is Proceed.
func (l *KV) Attempt(dedupeKey, sessionID string) (Attempt, error) {
	pair := Key{DedupeKey: dedupeKey, SessionID: sessionID}
	key := l.Key(dedupeKey, sessionID)
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
		if verdict, retryIn, holds := l.judge(entry); holds {
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
// holds=false when the record is too old to hold anyone and this process takes it over.
func (l *KV) judge(entry nats.KeyValueEntry) (verdict Verdict, retryIn time.Duration, holds bool) {
	age := l.cfg.Now().Sub(entry.Created())
	var record kvRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		record = kvRecord{}
	}
	switch {
	case record.State == stateDelivered:
		return Delivered, 0, age < l.cfg.DeliveredAge
	case record.State == stateAttempted && record.Boot == l.cfg.Boot:
		return InFlight, 0, age < l.cfg.AttemptedAge
	default:
		return Deferred, l.cfg.TakeoverAfter - age, age < l.cfg.TakeoverAfter
	}
}

// Record writes the pair's delivered record with Put, over whatever the key holds.
func (l *KV) Record(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	if _, err := l.kv.Put(attempt.key, l.value(stateDelivered)); err != nil {
		return fmt.Errorf("record delivery %s: %w", attempt.key, err)
	}
	return nil
}

// Clear deletes the pair's record at the revision attempt wrote, so a record another process has
// written since (a take-over, a delivery) stays.
func (l *KV) Clear(attempt Attempt) error {
	if attempt.Verdict != Proceed {
		return nil
	}
	err := l.kv.Delete(attempt.key, nats.LastRevision(attempt.revision))
	if err == nil || errors.Is(err, nats.ErrKeyExists) {
		return nil
	}
	return fmt.Errorf("clear delivery attempt %s: %w", attempt.key, err)
}
