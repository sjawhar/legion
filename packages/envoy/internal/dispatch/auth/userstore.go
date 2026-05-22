// UserStore abstracts where dispatch persists per-user dashboard state
// (refreshable tokens + watched-repos list).
//
// Two production-grade backends ship:
//
//   - FileUserStore: one JSON file per login under <dir>/<login>.json.
//     Used for local development and single-node deployments where the
//     filesystem is durable.
//
//   - KVUserStore: NATS JetStream KV, keyed by login. Used in production
//     deployments where the filesystem is ephemeral (Fargate, k8s, …) and
//     multiple dispatch replicas need to share state. Reuses the same NATS
//     cluster envoy already depends on.
//
// Both implementations satisfy the same interface; cmd/dispatch picks one
// at startup based on DISPATCH_USER_STORE.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"
)

// UserStore is the dispatch persistence layer for per-user state. Read
// returns (nil, nil) when no record exists (never logged in, or logged out).
type UserStore interface {
	Read(login string) (*User, error)
	Write(u *User) error
	Remove(login string) error
}

// ─── File-backed implementation ──────────────────────────────────────────

// FileUserStore wraps the existing ReadUser/WriteUser/RemoveUser helpers in
// the UserStore interface. dir is created with mode 0700 on first write.
type FileUserStore struct {
	Dir string
}

func (f *FileUserStore) Read(login string) (*User, error)  { return ReadUser(f.Dir, login) }
func (f *FileUserStore) Write(u *User) error               { return WriteUser(f.Dir, u) }
func (f *FileUserStore) Remove(login string) error         { return RemoveUser(f.Dir, login) }

// ─── NATS KV-backed implementation ──────────────────────────────────────

// DispatchUsersBucket is the JetStream KV bucket name. Aligned with the
// envoy_interests / envoy_roles naming pattern in internal/store/kv.go so
// operators can find dispatch state alongside envoy state.
const DispatchUsersBucket = "dispatch_users"

// KVUserStore stores user records in a NATS JetStream KV bucket.
//
// Replication is delegated to the bucket configuration (envoy ops sets it
// up alongside the other buckets). We don't watch the bucket — sessions are
// authenticated stateless via the dsession cookie, so per-request point
// reads against KV are the natural pattern.
type KVUserStore struct {
	kv nats.KeyValue
}

// OpenKVUserStore opens (or creates) the dispatch_users KV bucket.
func OpenKVUserStore(conn *nats.Conn, replicas int) (*KVUserStore, error) {
	if replicas < 1 {
		replicas = 1
	}
	js, err := conn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("get jetstream: %w", err)
	}
	kv, err := js.KeyValue(DispatchUsersBucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:   DispatchUsersBucket,
			Replicas: replicas,
			Storage:  nats.FileStorage,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", DispatchUsersBucket, err)
	}
	return &KVUserStore{kv: kv}, nil
}

func (s *KVUserStore) Read(login string) (*User, error) {
	if !loginShape.MatchString(login) {
		return nil, fmt.Errorf("invalid login %q", login)
	}
	entry, err := s.kv.Get(login)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u User
	if err := json.Unmarshal(entry.Value(), &u); err != nil {
		return nil, fmt.Errorf("parse user %q: %w", login, err)
	}
	if u.Login == "" {
		u.Login = login
	}
	u.WatchedRepos = normalizeRepoList(u.WatchedRepos)
	return &u, nil
}

func (s *KVUserStore) Write(u *User) error {
	if u == nil {
		return errors.New("KVUserStore.Write: nil user")
	}
	if !loginShape.MatchString(u.Login) {
		return fmt.Errorf("invalid login %q", u.Login)
	}
	u.WatchedRepos = normalizeRepoList(u.WatchedRepos)
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	_, err = s.kv.Put(u.Login, data)
	return err
}

func (s *KVUserStore) Remove(login string) error {
	if !loginShape.MatchString(login) {
		return fmt.Errorf("invalid login %q", login)
	}
	if err := s.kv.Delete(login); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}
