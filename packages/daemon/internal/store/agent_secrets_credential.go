package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
)

// AgentSecretsCredential is the secrets broker's launcher credential project's daemon kept for the
// broker at brokerURL, and false when it keeps none (migration 0037).
func (s *Store) AgentSecretsCredential(ctx context.Context, project, brokerURL string) (agentsecrets.Kept, bool, error) {
	var kept agentsecrets.Kept
	var expires *time.Time
	err := s.pool.QueryRow(ctx, `select credential_id, private_key, next_private_key, expires_at
		from agent_secrets_credentials where project = $1 and broker_url = $2`, project, brokerURL).
		Scan(&kept.CredentialID, &kept.PrivateKey, &kept.NextPrivateKey, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentsecrets.Kept{}, false, nil
	}
	if err != nil {
		return agentsecrets.Kept{}, false, fmt.Errorf("read the kept agent-secrets launcher credential for %s: %w", project, err)
	}
	kept.ExpiresAt = orZero(expires)
	return kept, true, nil
}

// SaveAgentSecretsCredential keeps kept as project's launcher credential for the broker at
// brokerURL, replacing whatever was kept there.
func (s *Store) SaveAgentSecretsCredential(ctx context.Context, project, brokerURL string, kept agentsecrets.Kept) error {
	var expires *time.Time
	if !kept.ExpiresAt.IsZero() {
		expires = &kept.ExpiresAt
	}
	_, err := s.pool.Exec(ctx, `insert into agent_secrets_credentials
		(project, broker_url, credential_id, private_key, next_private_key, expires_at, saved_at)
		values ($1, $2, $3, $4, $5, $6, now())
		on conflict (project, broker_url) do update set credential_id = excluded.credential_id,
			private_key = excluded.private_key, next_private_key = excluded.next_private_key,
			expires_at = excluded.expires_at, saved_at = excluded.saved_at`,
		project, brokerURL, kept.CredentialID, kept.PrivateKey, kept.NextPrivateKey, expires)
	if err != nil {
		return fmt.Errorf("keep the agent-secrets launcher credential for %s: %w", project, err)
	}
	return nil
}

// CompareAndSaveAgentSecretsCredential replaces project's kept launcher credential only when its
// complete prior state (credential id, private key and candidate next key) is expected. A broker
// renewal moves the broker's row from that old state to a new key under its own lock; matching the
// candidate too prevents another daemon's candidate preflight or loser cleanup from overwriting the
// durable key the winner saved. false, nil means another process already changed the row.
func (s *Store) CompareAndSaveAgentSecretsCredential(ctx context.Context, project, brokerURL string, expected, kept agentsecrets.Kept) (bool, error) {
	var expires *time.Time
	if !kept.ExpiresAt.IsZero() {
		expires = &kept.ExpiresAt
	}
	tag, err := s.pool.Exec(ctx, `update agent_secrets_credentials
		set credential_id = $5, private_key = $6, next_private_key = $7, expires_at = $8, saved_at = now()
		where project = $1 and broker_url = $2 and credential_id = $3 and private_key = $4
			and next_private_key is not distinct from $9`,
		project, brokerURL, expected.CredentialID, expected.PrivateKey,
		kept.CredentialID, kept.PrivateKey, kept.NextPrivateKey, expires, expected.NextPrivateKey)
	if err != nil {
		return false, fmt.Errorf("compare and keep the agent-secrets launcher credential for %s: %w", project, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetAgentSecretsCredential drops project's kept launcher credential for the broker at brokerURL
// when it is credentialID, and nothing otherwise.
func (s *Store) ForgetAgentSecretsCredential(ctx context.Context, project, brokerURL, credentialID string) error {
	if _, err := s.pool.Exec(ctx, `delete from agent_secrets_credentials
		where project = $1 and broker_url = $2 and credential_id = $3`, project, brokerURL, credentialID); err != nil {
		return fmt.Errorf("forget the agent-secrets launcher credential for %s: %w", project, err)
	}
	return nil
}
