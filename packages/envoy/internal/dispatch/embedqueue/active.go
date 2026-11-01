package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// activeIdentity returns the embedder identity the embedding queue of every server process works
// for, the one row of embedder_active (migration 20261101030538), deciding it first when it must.
// Two processes, of one build or of two builds a release runs side by side, may be configured
// with different embedders; each one's coverage pass drops every vector another identity made, so
// without one identity they would drop each other's vectors in turn for as long as both serve.
//
// The rule, read over the live server_processes rows (processes.Live, embedder_identity):
//   - the active identity stays active while any live process carries it, so a process that
//     restarts, or a release that rolls one process after another, keeps it;
//   - when none does, the identity of the earliest-started live process that carries one becomes
//     active, and when no live process carries one (no process registered yet, or every live
//     process of a build before the column), the identity of the process that looks, own;
//   - with no row yet, the first process to look records own.
//
// The insert and the change are each a compare-and-set, so two processes that look at once agree:
// one writes and the other reads what it wrote on its next turn of the loop.
func activeIdentity(ctx context.Context, pool *store.Pool, own string) (string, error) {
	for range 3 {
		var active string
		err := pool.QueryRow(ctx, `select identity from embedder_active`).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := pool.Exec(ctx, `insert into embedder_active (identity) values ($1) on conflict (one) do nothing`, own); err != nil {
				return "", fmt.Errorf("record the active embedder identity: %w", err)
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read the active embedder identity: %w", err)
		}
		live, err := processes.Live(ctx, pool)
		if err != nil {
			return "", err
		}
		successor := ""
		for _, process := range live {
			if process.EmbedderIdentity == active {
				return active, nil
			}
			if successor == "" && process.EmbedderIdentity != "" {
				successor = process.EmbedderIdentity
			}
		}
		if successor == "" {
			successor = own
		}
		if successor == active {
			return active, nil
		}
		tag, err := pool.Exec(ctx, `update embedder_active set identity = $2, since = now() where identity = $1`, active, successor)
		if err != nil {
			return "", fmt.Errorf("change the active embedder identity: %w", err)
		}
		if tag.RowsAffected() == 1 {
			slog.Info("dispatch embedqueue: the active embedder identity changed: no live server process carried the one before",
				"before", active, "active", successor)
			return successor, nil
		}
	}
	return "", errors.New("decide the active embedder identity: it changed under every read")
}

// embedderActive reports whether this process's embedder has the active identity
// (activeIdentity), and which identity is active. Only then does it drop and re-admit other
// identities' rows (Cover), embed queue rows (ProcessBatch) and reserve tokens: a process whose
// identity is not active leaves every row to the process whose identity is.
func (d Deps) embedderActive(ctx context.Context) (bool, string, error) {
	own := d.Embedder.Identity()
	active, err := activeIdentity(ctx, d.Store.Pool, own)
	if err != nil {
		return false, "", err
	}
	return active == own, active, nil
}
