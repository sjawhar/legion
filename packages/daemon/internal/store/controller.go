package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/controller"
)

const controllerSelect = `select generation, capability_hash, session, secret_hash, registered_at
	from controllers where project = $1`

// MintController records a new controller capability for project, by its hash: the generation
// moves on by one (the first mint is 1), and whatever session registered with the capability it
// replaces is no longer registered.
func (s *Store) MintController(ctx context.Context, project string, capabilityHash []byte) (uint64, error) {
	var generation uint64
	err := s.pool.QueryRow(ctx, `insert into controllers (project, capability_hash, generation)
		values ($1, $2, 1)
		on conflict (project) do update set capability_hash = excluded.capability_hash,
			generation = controllers.generation + 1, session = '', secret_hash = null, registered_at = null
		returning generation`, project, capabilityHash).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("record the controller capability for %s: %w", project, err)
	}
	return generation, nil
}

// Controller is project's controller record, and false when no capability has been minted for it.
func (s *Store) Controller(ctx context.Context, project string) (controller.Record, bool, error) {
	return scanController(s.pool.QueryRow(ctx, controllerSelect, project), project)
}

// ControllerTx is Controller inside tx, for the state route's one snapshot.
func (s *Store) ControllerTx(ctx context.Context, tx pgx.Tx, project string) (controller.Record, bool, error) {
	return scanController(tx.QueryRow(ctx, controllerSelect, project), project)
}

// RegisterController records session as the controller registered with generation's capability,
// with the hash of the secret its grants authenticate with. It is fenced on the generation: when a
// mint has moved past it, nothing is written and the answer is false.
func (s *Store) RegisterController(ctx context.Context, project string, generation uint64, session string, secretHash []byte, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `update controllers set session = $3, secret_hash = $4, registered_at = $5
		where project = $1 and generation = $2`, project, generation, session, secretHash, at)
	if err != nil {
		return false, fmt.Errorf("record the controller registration for %s: %w", project, err)
	}
	return tag.RowsAffected() == 1, nil
}

func scanController(row pgx.Row, project string) (controller.Record, bool, error) {
	var record controller.Record
	var registeredAt *time.Time
	err := row.Scan(&record.Generation, &record.CapabilityHash, &record.Session, &record.SecretHash, &registeredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return controller.Record{}, false, nil
	}
	if err != nil {
		return controller.Record{}, false, fmt.Errorf("read the controller record for %s: %w", project, err)
	}
	record.RegisteredAt = orZero(registeredAt)
	return record, true, nil
}
