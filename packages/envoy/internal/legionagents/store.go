package legionagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// ReadTimeout bounds each call the store makes on the bucket, the listener's 2 s bucket read
// (roleHolderReadTimeout in cmd/listener), so a route answers inside the listener's 10 s HTTP write
// timeout while NATS does not.
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
}

// Option configures a Store.
type Option func(*options)

type options struct {
	replicas int
	now      func() time.Time
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

// Open opens the registry's bucket on conn, creating it when it does not exist: file storage, no
// TTL, one value per key, the replicas WithReplicas names.
func Open(conn *nats.Conn, opts ...Option) (*Store, error) {
	o := options{replicas: 1, now: time.Now}
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
	return &Store{kv: kv, now: o.now}, nil
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
func (s *Store) Get(ctx context.Context, sessionID string) (Agent, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return Agent{}, err
	}
	entry, err := s.read(ctx, sessionID)
	if err != nil {
		return Agent{}, err
	}
	siblings, err := s.list(ctx, rolePrefix(entry.RoleToken))
	if err != nil {
		return Agent{}, err
	}
	for _, agent := range succession(siblings) {
		if agent.SessionID == sessionID {
			return agent, nil
		}
	}
	// The role key is written before the session key, so the listing holds the session; a
	// listing that does not is answered with the session alone rather than refused.
	return Agent{Entry: entry}, nil
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

// read reads the session key, ErrNotFound when the bucket holds none.
func (s *Store) read(ctx context.Context, sessionID string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	found, err := s.bucket().Get(sessionKey(sessionID))
	if errors.Is(err, nats.ErrKeyNotFound) {
		return Entry{}, fmt.Errorf("%w %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("read legion agent %s: %w", sessionID, err)
	}
	var entry Entry
	if err := json.Unmarshal(found.Value(), &entry); err != nil {
		return Entry{}, fmt.Errorf("decode legion agent %s: %w", sessionID, err)
	}
	return entry, nil
}

// list reads every key under prefix, an issue's or a role token's, from one watch filtered to it
// and ended at its initial-values marker, the nil entry nats.go sends once it has delivered every
// key the filter held. A watch that ends any other way did not read the bucket, and fails: its
// idle timer gave up (nats.ErrKeyWatcherTimeout on Error()), its subscription ended, or ctx did.
func (s *Store) list(ctx context.Context, prefix string) ([]Entry, error) {
	watcher, err := s.bucket().Watch(prefix+"*", nats.IgnoreDeletes(), nats.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("list legion agents under %s: %w", prefix, err)
	}
	defer func() { _ = watcher.Stop() }()
	var entries []Entry
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("list legion agents under %s: %w", prefix, ctx.Err())
		case found, open := <-watcher.Updates():
			if !open {
				return nil, fmt.Errorf("list legion agents under %s: the watch ended after %d keys, before it had delivered them all", prefix, len(entries))
			}
			if found == nil {
				select {
				case err := <-watcher.Error():
					if err != nil {
						return nil, fmt.Errorf("list legion agents under %s: the watch stopped after %d keys: %w", prefix, len(entries), err)
					}
				default:
				}
				return entries, nil
			}
			var entry Entry
			if err := json.Unmarshal(found.Value(), &entry); err != nil {
				return nil, fmt.Errorf("decode legion agent key %s: %w", found.Key(), err)
			}
			entries = append(entries, entry)
		}
	}
}

// Put records one entry and answers it as stored (prepare, then write).
func (s *Store) Put(ctx context.Context, entry Entry) (Entry, error) {
	if err := Validate(entry); err != nil {
		return Entry{}, err
	}
	stored, err := s.prepare(ctx, entry, nil)
	if err != nil {
		return Entry{}, err
	}
	if err := s.write(ctx, stored); err != nil {
		return Entry{}, err
	}
	return stored, nil
}

// PutAll records entries in order and answers them as stored. Every entry is validated, then every
// session's stored entry read and checked, before the first write, so a bad entry (a *BatchError
// naming its index, wrapping ErrInvalid or a *ConflictError) or a read that fails refuses the whole
// batch with nothing written. A write that fails after that answers how many entries were written
// whole; every write is idempotent, so the caller sends the batch again.
func (s *Store) PutAll(ctx context.Context, entries []Entry) (stored []Entry, written int, err error) {
	if len(entries) > MaxBatch {
		return nil, 0, fmt.Errorf("%w: this batch holds %d", ErrTooMany, len(entries))
	}
	for i, entry := range entries {
		if err := Validate(entry); err != nil {
			return nil, 0, &BatchError{Index: i, Err: err}
		}
	}
	stored = make([]Entry, len(entries))
	earlier := map[string]Entry{}
	for i, entry := range entries {
		next, err := s.prepare(ctx, entry, earlier)
		if err != nil {
			return nil, 0, &BatchError{Index: i, Err: err}
		}
		stored[i], earlier[entry.SessionID] = next, next
	}
	for _, entry := range stored {
		if err := s.write(ctx, entry); err != nil {
			return nil, written, err
		}
		written++
	}
	return stored, written, nil
}

// prepare is entry as the registry will store it: refused with a *ConflictError when the session
// is recorded for another issue or role, and carrying the recorded_at of its first write, or now
// when this is its first. earlier holds the entries a batch prepared before this one, which its
// writes will have stored by the time this one's are made.
func (s *Store) prepare(ctx context.Context, entry Entry, earlier map[string]Entry) (Entry, error) {
	current, found := earlier[entry.SessionID]
	if !found {
		var err error
		current, err = s.read(ctx, entry.SessionID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return Entry{}, err
		default:
			found = true
		}
	}
	if !found {
		entry.RecordedAt = s.now().UTC().Format(time.RFC3339)
		return entry, nil
	}
	if !sameSession(current, entry) {
		return Entry{}, &ConflictError{SessionID: entry.SessionID, Issue: current.Issue, Role: current.Role}
	}
	entry.RecordedAt = current.RecordedAt
	return entry, nil
}

// write puts entry under its role key, its issue key, then its session key. The session key comes
// last, so a session a read finds is in both its listings. The role key comes first because it is
// the longest of the three, so a key NATS refused would refuse it before anything was written,
// though Validate's bounds keep every key well inside what NATS takes.
func (s *Store) write(ctx context.Context, entry Entry) error {
	value, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode legion agent %s: %w", entry.SessionID, err)
	}
	kv := s.bucket()
	for _, key := range []string{rolePrefix(entry.RoleToken) + entry.SessionID, issuePrefix(entry.Issue) + entry.SessionID, sessionKey(entry.SessionID)} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("write legion agent %s: %w", entry.SessionID, err)
		}
		if _, err := kv.Put(key, value); err != nil {
			return fmt.Errorf("write legion agent key %s: %w", key, err)
		}
	}
	return nil
}
