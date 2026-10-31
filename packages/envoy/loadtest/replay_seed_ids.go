package main

// loadgen replay-seed: loadtest.replay_ids, the logged-to-local id map a seed writes as it goes
// and a resumed seed reads (replay_seed.go).

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// idStore is loadtest.replay_ids, cached.
type idStore struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	m    map[[2]string]string
	// byLocal counts, by kind, the logged ids that map to each local id.
	byLocal map[string]map[string]int
}

func newIDStore(pool *pgxpool.Pool, m map[[2]string]string) *idStore {
	s := &idStore{pool: pool, m: map[[2]string]string{}, byLocal: map[string]map[string]int{}}
	for k, v := range m {
		s.set(k, v)
	}
	return s
}

func loadIDStore(ctx context.Context, pool *pgxpool.Pool) (*idStore, error) {
	m, err := readReplayIDs(ctx, pool)
	if err != nil {
		return nil, err
	}
	return newIDStore(pool, m), nil
}

// set maps k to local, keeping byLocal; the caller holds s.mu or has not shared s yet.
func (s *idStore) set(k [2]string, local string) {
	kind := k[0]
	if old, ok := s.m[k]; ok {
		if s.byLocal[kind][old]--; s.byLocal[kind][old] == 0 {
			delete(s.byLocal[kind], old)
		}
	}
	s.m[k] = local
	if s.byLocal[kind] == nil {
		s.byLocal[kind] = map[string]int{}
	}
	s.byLocal[kind][local]++
}

func (s *idStore) get(kind, logged string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[[2]string{kind, logged}]
	return v, ok
}

func (s *idStore) put(ctx context.Context, kind, logged, local string) error {
	if _, err := s.pool.Exec(ctx, `insert into loadtest.replay_ids (kind, logged_id, local_id) values ($1, $2, $3) on conflict do nothing`, kind, logged, local); err != nil {
		return fmt.Errorf("record %s %s: %w", kind, logged, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.set([2]string{kind, logged}, local)
	return nil
}

// mapped says whether a logged id of kind maps to local.
func (s *idStore) mapped(kind, local string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byLocal[kind][local] > 0
}

func (s *idStore) count(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.m {
		if k[0] == kind {
			n++
		}
	}
	return n
}

func (s *idStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
