// Package docs defines the document-service boundary used by Dispatch's HTTP API.
package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ErrServiceUnavailable indicates that the configured document service cannot
// apply a live Yjs operation.
var ErrServiceUnavailable = errors.New("document service unavailable")

// API is the document operation surface. A4 replaces NoopAPI with a ygo-backed
// implementation without changing HTTP handlers.
type API interface {
	SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error
	ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) error
	Text(ctx context.Context, artifactID string) (string, error)
	SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error)
	ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error)
	ApplyReplace(ctx context.Context, artifactID string, anchor model.Anchor, with string, actor model.Actor) error
	NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error)
	CompactAll(ctx context.Context, keep int) error
}

type txContextKey struct{}

// WithTx allows a document operation to join the transaction that created its
// containing artifact/event. Callers must commit or roll back tx themselves.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	return tx, ok
}

// NoopAPI stores document text as artifact version markdown until the ygo
// implementation arrives. It deliberately rejects live edit operations.
type NoopAPI struct {
	store *store.Store
}

// NewNoopAPI creates the temporary Postgres-backed document service.
func NewNoopAPI(database *store.Store) *NoopAPI {
	return &NoopAPI{store: database}
}

// SeedText records initial document content in version 1 within tx.
func (n *NoopAPI) SeedText(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	result, err := tx.Exec(ctx, `
		update artifact_versions
		set markdown = $2
		where artifact_id = $1 and number = 1
	`, artifactID, markdown)
	if err != nil {
		return fmt.Errorf("seed document text: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("seed document text: initial version for artifact %s not found", artifactID)
	}
	return nil
}

// ReplaceText records an uploaded replacement as the latest version.
func (n *NoopAPI) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) error {
	return n.withTx(ctx, func(tx pgx.Tx) error {
		authors, err := json.Marshal([]model.Actor{actor})
		if err != nil {
			return fmt.Errorf("encode document authors: %w", err)
		}
		result, err := tx.Exec(ctx, `
			update artifact_versions
			set markdown = $2, authors = $3
			where artifact_id = $1
			  and number = (select max(number) from artifact_versions where artifact_id = $1)
		`, artifactID, markdown, authors)
		if err != nil {
			return fmt.Errorf("replace document text: %w", err)
		}
		if result.RowsAffected() != 1 {
			return fmt.Errorf("replace document text: artifact %s has no versions", artifactID)
		}
		return nil
	})
}

// Text returns live document text, represented by the latest version in the
// temporary implementation.
func (n *NoopAPI) Text(ctx context.Context, artifactID string) (string, error) {
	var markdown *string
	if err := n.store.Pool.QueryRow(ctx, `
		select markdown
		from artifact_versions
		where artifact_id = $1
		order by number desc
		limit 1
	`, artifactID).Scan(&markdown); err != nil {
		return "", fmt.Errorf("read document text: %w", err)
	}
	if markdown == nil {
		return "", fmt.Errorf("artifact %s is not a document", artifactID)
	}
	return *markdown, nil
}

// SnapshotVersion captures live text only when it diverges from the latest
// immutable version. It runs in the transaction which stores the anchor.
func (n *NoopAPI) SnapshotVersion(ctx context.Context, tx pgx.Tx, artifactID string, actor model.Actor) (model.Version, bool, error) {
	live, err := n.Text(ctx, artifactID)
	if err != nil {
		return model.Version{}, false, err
	}
	var version model.Version
	var markdown *string
	var authors []byte
	if err := tx.QueryRow(ctx, `
		select number, named, summary, authors, created_at, markdown
		from artifact_versions
		where artifact_id = $1
		order by number desc
		limit 1
	`, artifactID).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt, &markdown); err != nil {
		return model.Version{}, false, fmt.Errorf("read latest document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, false, fmt.Errorf("decode document version authors: %w", err)
	}
	if markdown != nil && *markdown == live {
		return version, false, nil
	}
	authors, err = json.Marshal([]model.Actor{actor})
	if err != nil {
		return model.Version{}, false, fmt.Errorf("encode document authors: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, $2, $3
		from artifact_versions
		where artifact_id = $1
		returning number, named, summary, authors, created_at
	`, artifactID, live, authors).Scan(
		&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt,
	); err != nil {
		return model.Version{}, false, fmt.Errorf("write unnamed document version: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, false, fmt.Errorf("decode document version authors: %w", err)
	}
	return version, true, nil
}

// ApplyOps is unavailable until the ygo-backed implementation lands.
func (n *NoopAPI) ApplyOps(context.Context, string, []model.EditOp, model.Actor) (int, error) {
	return 0, ErrServiceUnavailable
}

// ApplyReplace is unavailable until the ygo-backed implementation lands.
func (n *NoopAPI) ApplyReplace(context.Context, string, model.Anchor, string, model.Actor) error {
	return ErrServiceUnavailable
}

// NamedVersion copies the current live document into a named version.
func (n *NoopAPI) NamedVersion(ctx context.Context, artifactID, summary string, actor model.Actor) (model.Version, error) {
	var version model.Version
	err := n.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select id from artifacts where id = $1 for update`, artifactID); err != nil {
			return fmt.Errorf("lock artifact: %w", err)
		}
		var markdown string
		if err := tx.QueryRow(ctx, `
			select markdown from artifact_versions
			where artifact_id = $1
			order by number desc
			limit 1
		`, artifactID).Scan(&markdown); err != nil {
			return fmt.Errorf("read document version: %w", err)
		}
		authors, err := json.Marshal([]model.Actor{actor})
		if err != nil {
			return fmt.Errorf("encode document authors: %w", err)
		}
		var authorsRaw []byte
		if err := tx.QueryRow(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors, named, summary)
			select $1, max(number) + 1, $2, $3, true, $4
			from artifact_versions
			where artifact_id = $1
			returning number, named, summary, authors, created_at
		`, artifactID, markdown, authors, summary).Scan(
			&version.Number, &version.Named, &version.Summary, &authorsRaw, &version.CreatedAt,
		); err != nil {
			return fmt.Errorf("write named document version: %w", err)
		}
		if err := json.Unmarshal(authorsRaw, &version.Authors); err != nil {
			return fmt.Errorf("decode document version authors: %w", err)
		}
		return nil
	})
	return version, err
}

// CompactAll is a no-op until documents are represented by Yjs updates.
func (n *NoopAPI) CompactAll(context.Context, int) error { return nil }

func (n *NoopAPI) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if tx, ok := txFromContext(ctx); ok {
		return fn(tx)
	}
	if n.store == nil || n.store.Pool == nil {
		return errors.New("document store required")
	}
	tx, err := n.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin document transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit document transaction: %w", err)
	}
	return nil
}
