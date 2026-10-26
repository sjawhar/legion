package legionagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// ReadTimeout bounds each call the store makes on the bucket, and the listener bounds a route's
// whole read with it too (legionAgentReadContext in cmd/listener), as roleHolderReadTimeout bounds
// the session bucket's, so a route answers inside the listener's 10 s HTTP write timeout while NATS
// does not.
const ReadTimeout = 2 * time.Second

// openTimeout bounds the bucket's open, which creates it on a first start, as store.Open's.
const openTimeout = 10 * time.Second

// Store reads and writes the registry. It keeps no cache: every read is a key read or a filtered
// listing of the bucket itself, so an absence it answers is the bucket's.
type Store struct {
	// kvMu guards kv, which Reopen moves to a replacement connection.
	kvMu sync.RWMutex
	kv   bus.KeyValue
	now  func() time.Time
	log  *slog.Logger
}

// Option configures a Store.
type Option func(*options)

type options struct {
	replicas int
	now      func() time.Time
	log      *slog.Logger
}

// WithReplicas sets the bucket's replica count when Open creates it, the listener's NATS replicas.
// Use 1 for a single-node test NATS.
func WithReplicas(n int) Option {
	return func(o *options) { o.replicas = n }
}

// WithClock sets the clock recorded_at reads. Tests set it to tell a first write from a rewrite.
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// WithLogger sets the logger the one line a listing's failed watch stop writes goes to, the
// listener's own handler (store.WithLogger says why not slog.SetDefault). The default is
// slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(o *options) { o.log = log }
}

// Open opens the registry's bucket on conn, creating it when it does not exist: file storage, no
// TTL, one value per key, the replicas WithReplicas names.
func Open(conn *nats.Conn, opts ...Option) (*Store, error) {
	o := options{replicas: 1, now: time.Now, log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	js, err := conn.JetStream(nats.MaxWait(openTimeout))
	if err != nil {
		return nil, fmt.Errorf("open legion agent registry JetStream: %w", err)
	}
	if _, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.LegionAgentBucket,
		History:  1,
		Replicas: o.replicas,
		Storage:  nats.FileStorage,
	}); err != nil {
		return nil, fmt.Errorf("open legion agent KV bucket: %w", err)
	}
	kv, err := openHandle(conn)
	if err != nil {
		return nil, err
	}
	return &Store{kv: kv, now: o.now, log: o.log}, nil
}

// openHandle opens the bucket on conn under ReadTimeout, which every call through the handle waits
// at most: a get, a put, a listing's consumer create and its idle wait for the next key.
func openHandle(conn *nats.Conn) (bus.KeyValue, error) {
	js, err := conn.JetStream(nats.MaxWait(ReadTimeout))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open legion agent registry JetStream: %w", err)
	}
	kv, err := bus.OpenKeyValue(js, bus.LegionAgentBucket)
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open legion agent KV bucket: %w", err)
	}
	return kv, nil
}

// Reopen moves the store to conn. The listener's reconnect hook calls it, so after the bus replaces
// a closed connection the registry reads and writes on the new one. A failure leaves the store on
// the connection it had.
func (s *Store) Reopen(conn *nats.Conn) error {
	kv, err := openHandle(conn)
	if err != nil {
		return err
	}
	s.kvMu.Lock()
	s.kv = kv
	s.kvMu.Unlock()
	return nil
}

func (s *Store) bucket() bus.KeyValue {
	s.kvMu.RLock()
	defer s.kvMu.RUnlock()
	return s.kv
}

func sessionKey(id string) string { return "session." + id }

func issuePrefix(issue string) string { return "issue." + issue + "." }

func rolePrefix(token string) string { return "role." + token + "." }

// Get answers the session's entry with its replacement within its role token: the session key,
// then the role token's listing. A session the bucket holds no key for is ErrNotFound.
//
// The session key is claimed before the index keys are written, so a write that ended between them
// leaves the role token listing the session's older entry, or not listing it yet. The listing's
// entry for the session is therefore replaced with the session key's, or the session key's added,
// before the replacements are derived, and the write's retry brings the role key up to date.
func (s *Store) Get(ctx context.Context, sessionID string) (Agent, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return Agent{}, err
	}
	entry, _, err := s.read(ctx, sessionID)
	if err != nil {
		return Agent{}, err
	}
	siblings, err := s.list(ctx, rolePrefix(entry.RoleToken))
	if err != nil {
		return Agent{}, err
	}
	siblings = slices.DeleteFunc(siblings, func(sibling Entry) bool { return sibling.SessionID == sessionID })
	agents := succession(append(siblings, entry))
	return agents[slices.IndexFunc(agents, func(agent Agent) bool { return agent.SessionID == sessionID })], nil
}

// ByRoleToken answers every session of the role token, newest first, each with its replacement:
// the first is the role's current holder. A token no session holds answers none.
func (s *Store) ByRoleToken(ctx context.Context, token string) ([]Agent, error) {
	if err := ValidateRoleToken(token); err != nil {
		return nil, err
	}
	entries, err := s.list(ctx, rolePrefix(token))
	if err != nil {
		return nil, err
	}
	return succession(entries), nil
}

// ByIssue answers every session of the issue, by role in workflow order (Roles), then newest
// first, each with its replacement within its role token. An issue no session worked answers none.
func (s *Store) ByIssue(ctx context.Context, issue string) ([]Agent, error) {
	if err := ValidateIssue(issue); err != nil {
		return nil, err
	}
	entries, err := s.list(ctx, issuePrefix(issue))
	if err != nil {
		return nil, err
	}
	return issueOrder(entries), nil
}

// read reads the session key and the revision it is at, ErrNotFound when the bucket holds none.
func (s *Store) read(ctx context.Context, sessionID string) (Entry, uint64, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, 0, err
	}
	found, err := s.bucket().Get(sessionKey(sessionID))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return Entry{}, 0, fmt.Errorf("%w %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return Entry{}, 0, fmt.Errorf("read legion agent %s: %w", sessionID, err)
	}
	var entry Entry
	if err := json.Unmarshal(found.Value(), &entry); err != nil {
		return Entry{}, 0, fmt.Errorf("decode legion agent %s: %w", sessionID, err)
	}
	return entry, found.Revision(), nil
}

// list reads every key under prefix, an issue's or a role token's, from one watch filtered to it
// (kvwatch.ScanExistingKeys, which fails a watch that did not deliver every key the filter held).
func (s *Store) list(ctx context.Context, prefix string) ([]Entry, error) {
	var entries []Entry
	_, err := kvwatch.ScanExistingKeys(ctx, s.bucket(), prefix+"*", "list legion agents under "+prefix, s.log, func(found nats.KeyValueEntry) error {
		var entry Entry
		if err := json.Unmarshal(found.Value(), &entry); err != nil {
			return fmt.Errorf("decode legion agent key %s: %w", found.Key(), err)
		}
		entries = append(entries, entry)
		return nil
	}, nats.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// claimAttempts bounds how often one write claims its session key: a claim another write's moved
// the key under is read again and claimed at the revision read, and a key that keeps moving fails
// the write, which the caller retries.
const claimAttempts = 3

// Put records one entry and answers it as stored (prepare, then write).
func (s *Store) Put(ctx context.Context, entry Entry) (Entry, error) {
	if err := Validate(entry); err != nil {
		return Entry{}, err
	}
	next, err := s.prepare(ctx, entry, nil)
	if err != nil {
		return Entry{}, err
	}
	return s.write(ctx, next)
}

// PutAll records entries in order and answers them as stored. Every entry is validated, then every
// session's stored entry read and checked, before the first write, so a bad entry (a *BatchError
// naming its index, wrapping ErrInvalid or a *ConflictError) refuses the whole batch with nothing
// written. Any failure after that, a read or a write the bucket did not answer, or a session
// another write claimed in the meantime, is a *PartialWriteError naming how many entries were
// written whole, 0 when it came before the first write, and wrapping the failed entry's
// *BatchError. Every write is idempotent, so the caller sends the batch again.
func (s *Store) PutAll(ctx context.Context, entries []Entry) ([]Entry, error) {
	if len(entries) > MaxBatch {
		return nil, fmt.Errorf("%w: this batch holds %d", ErrTooMany, len(entries))
	}
	for i, entry := range entries {
		if err := Validate(entry); err != nil {
			return nil, &BatchError{Index: i, Err: err}
		}
	}
	batch := make([]prepared, len(entries))
	earlier := map[string]prepared{}
	for i, entry := range entries {
		next, err := s.prepare(ctx, entry, earlier)
		var conflict *ConflictError
		switch {
		case errors.As(err, &conflict):
			return nil, &BatchError{Index: i, Err: err}
		case err != nil:
			return nil, &PartialWriteError{Written: 0, Total: len(entries), Err: &BatchError{Index: i, Err: err}}
		}
		batch[i], earlier[entry.SessionID] = next, next
	}
	stored := make([]Entry, len(entries))
	for i, next := range batch {
		entry, err := s.write(ctx, next)
		if err != nil {
			return nil, &PartialWriteError{Written: i, Total: len(entries), Err: &BatchError{Index: i, Err: err}}
		}
		stored[i] = entry
	}
	return stored, nil
}

// prepared is an entry as the registry will store it, with the revision of the session key it was
// prepared against, 0 for a session the bucket holds no key for.
type prepared struct {
	entry    Entry
	revision uint64
}

// prepare is entry as the registry will store it: refused with a *ConflictError when the session
// is recorded for another Legion project, issue or role, and carrying the recorded_at of its first
// write, or now when this is its first. earlier holds the entries a batch prepared before this one,
// which its writes will have stored by the time this one's are made; the revision such an entry
// carries is stale by then, so this one's claim reads the key again (write).
func (s *Store) prepare(ctx context.Context, entry Entry, earlier map[string]prepared) (prepared, error) {
	current, found := earlier[entry.SessionID]
	if !found {
		recorded, revision, err := s.read(ctx, entry.SessionID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return prepared{}, err
		default:
			current, found = prepared{entry: recorded, revision: revision}, true
		}
	}
	if !found {
		entry.RecordedAt = s.now().UTC().Format(time.RFC3339)
		return prepared{entry: entry}, nil
	}
	if !sameSession(current.entry, entry) {
		return prepared{}, &ConflictError{SessionID: entry.SessionID, Issue: current.entry.Issue, Role: current.entry.Role}
	}
	entry.RecordedAt = current.entry.RecordedAt
	return prepared{entry: entry, revision: current.revision}, nil
}

// write claims the session key, then puts the entry under its role key and its issue key, and
// answers the entry as stored. The claim is a compare-and-swap at the revision prepare read: a
// create for a session the bucket holds no key for, an update at that revision for a recorded one.
// So of two writes moving one session to different issues only one claims it, and the index keys,
// which every listing reads, are written only by the write that holds the session key. A claim
// another write moved the key under is read again: a session now recorded for another issue or role
// is a *ConflictError, and one recorded for the same one is claimed again at the new revision. A
// write that ends after its claim leaves the session key alone, which refuses a move as any recorded
// session does, and which the same write sent again completes: every write of a recorded session
// writes its index keys again. Validate's bounds keep every key well inside what NATS takes, so no
// index key is refused after a claim.
func (s *Store) write(ctx context.Context, next prepared) (Entry, error) {
	kv := s.bucket()
	for attempt := 1; ; attempt++ {
		value, err := json.Marshal(next.entry)
		if err != nil {
			return Entry{}, fmt.Errorf("encode legion agent %s: %w", next.entry.SessionID, err)
		}
		err = claim(ctx, kv, next, value)
		if err == nil {
			if err := putIndexKeys(ctx, kv, next.entry, value); err != nil {
				return Entry{}, err
			}
			return next.entry, nil
		}
		if !isClaimConflict(err) {
			return Entry{}, err
		}
		if attempt == claimAttempts {
			return Entry{}, fmt.Errorf("claim legion agent %s: its key moved under %d claims in a row: %w", next.entry.SessionID, claimAttempts, err)
		}
		if next, err = s.prepare(ctx, next.entry, nil); err != nil {
			return Entry{}, err
		}
	}
}

// claim writes value under the session key at the revision next was prepared against.
func claim(ctx context.Context, kv bus.KeyValue, next prepared, value []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write legion agent %s: %w", next.entry.SessionID, err)
	}
	key := sessionKey(next.entry.SessionID)
	var err error
	if next.revision == 0 {
		_, err = kv.Create(key, value)
	} else {
		_, err = kv.Update(key, value, next.revision)
	}
	if err != nil {
		return fmt.Errorf("claim legion agent key %s: %w", key, err)
	}
	return nil
}

// putIndexKeys writes value under entry's role key, then its issue key.
func putIndexKeys(ctx context.Context, kv bus.KeyValue, entry Entry, value []byte) error {
	for _, key := range []string{rolePrefix(entry.RoleToken) + entry.SessionID, issuePrefix(entry.Issue) + entry.SessionID} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write legion agent %s: %w", entry.SessionID, err)
		}
		if _, err := kv.Put(key, value); err != nil {
			return fmt.Errorf("write legion agent key %s: %w", key, err)
		}
	}
	return nil
}

// isClaimConflict reports whether err is a claim another write's moved the session key under: a
// create of a key that exists (nats.ErrKeyExists) or an update at a revision the key is no longer
// at (JetStream's "wrong last sequence"), as cistore's isCASConflict reads them.
func isClaimConflict(err error) bool {
	return errors.Is(err, nats.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence")
}
