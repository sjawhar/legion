package store

import (
	"context"
	"fmt"
)

// ResolveUnknownSessionStores assigns the configured store to claims that
// predate session-store tracking. It runs at daemon boot after migrations.
func (s *Store) ResolveUnknownSessionStores(ctx context.Context, kind string) error {
	if kind != "pvc" && kind != "postgres" && kind != "cloudsql-iam" {
		return fmt.Errorf("resolve unknown session stores: %q is not a session store", kind)
	}
	if _, err := s.pool.Exec(ctx, `update claims set session_store = $1 where session_store = ''`, kind); err != nil {
		return fmt.Errorf("resolve unknown session stores: %w", err)
	}
	return nil
}
