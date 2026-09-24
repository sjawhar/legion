package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

var _ supervise.Store = (*Store)(nil)

// claimSelect reads a claim with the delivery pending on it, if any: the two tables are written
// separately (a delivery changes far more often than the claim it rides on) and read together.
const claimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.session,
	c.session_file, c.locator, c.state, c.launch_failures, c.prompt_failures, c.prompt_retires,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.workspace_lost,
	d.delivery_id, d.task, d.queued_at, d.delivered_at, d.confirmed_at
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`

// PutClaim writes a claim, replacing the row its token names. The pending delivery is not part of
// the write: it has its own, PutDelivery and RetireDelivery.
func (s *Store) PutClaim(ctx context.Context, c supervise.Claim) error {
	if c.Generation > math.MaxInt64 {
		return fmt.Errorf("put claim %s: generation %d does not fit a bigint", c.Token, c.Generation)
	}
	var locator []byte
	if c.Locator != nil {
		encoded, err := json.Marshal(c.Locator)
		if err != nil {
			return fmt.Errorf("put claim %s: encode its locator: %w", c.Token, err)
		}
		locator = encoded
	}
	_, err := s.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, locator, state, launch_failures, prompt_failures, prompt_retires,
		boot_token_hash, capability_hash, uncertain_streak, workspace_lost)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		on conflict (token) do update set project = excluded.project, tree = excluded.tree,
		issue = excluded.issue, role = excluded.role, generation = excluded.generation,
		session = excluded.session, session_file = excluded.session_file,
		locator = excluded.locator, state = excluded.state,
		launch_failures = excluded.launch_failures, prompt_failures = excluded.prompt_failures,
		prompt_retires = excluded.prompt_retires, boot_token_hash = excluded.boot_token_hash,
		capability_hash = excluded.capability_hash, uncertain_streak = excluded.uncertain_streak,
		workspace_lost = excluded.workspace_lost,
		updated_at = now()`,
		string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), int64(c.Generation),
		c.Session, c.SessionFile, locator, string(c.State),
		c.Budgets.LaunchFailures, c.Budgets.PromptFailures, c.Budgets.PromptRetires,
		c.BootTokenHash, c.CapabilityHash, c.UncertainStreak, c.WorkspaceLost,
	)
	if err != nil {
		return fmt.Errorf("put claim %s: %w", c.Token, err)
	}
	return nil
}

// Claims reads every claim with its pending delivery, in token order. A stored locator that does
// not validate refuses the whole read, naming the claim: a daemon that loaded it would believe in
// a process it has no way to act on.
func (s *Store) Claims(ctx context.Context) ([]supervise.Claim, error) {
	rows, err := s.pool.Query(ctx, claimSelect+" order by c.token")
	if err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	defer rows.Close()
	var claims []supervise.Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	return claims, nil
}

// ClaimByBootTokenHash finds the claim whose current launch minted the boot token with this hash
// — how a shim's hello and an agent's registration find the claim they belong to.
func (s *Store) ClaimByBootTokenHash(ctx context.Context, hash []byte) (supervise.Claim, bool, error) {
	c, err := scanClaim(s.pool.QueryRow(ctx, claimSelect+" where c.boot_token_hash = $1", hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, false, nil
	}
	if err != nil {
		return supervise.Claim{}, false, err
	}
	return c, true, nil
}

// PutDelivery writes the claim's pending delivery, replacing the one it had: a claim has at most
// one. A token no claim carries is refused.
func (s *Store) PutDelivery(ctx context.Context, token claim.Token, d supervise.Delivery) error {
	_, err := s.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task,
		queued_at, delivered_at, confirmed_at) values ($1, $2, $3, $4, $5, $6)
		on conflict (claim_token) do update set delivery_id = excluded.delivery_id,
		task = excluded.task, queued_at = excluded.queued_at,
		delivered_at = excluded.delivered_at, confirmed_at = excluded.confirmed_at`,
		string(token), d.ID, d.Task, d.QueuedAt, instant(d.DeliveredAt), instant(d.ConfirmedAt),
	)
	if err != nil {
		return fmt.Errorf("put delivery %s on %s: %w", d.ID, token, err)
	}
	return nil
}

// RetireDelivery removes the claim's pending delivery, fenced on its id: retiring a delivery the
// claim does not hold is refused, and the one it does hold stays.
func (s *Store) RetireDelivery(ctx context.Context, token claim.Token, deliveryID string) error {
	tag, err := s.pool.Exec(ctx,
		"delete from pending_task_deliveries where claim_token = $1 and delivery_id = $2",
		string(token), deliveryID,
	)
	if err != nil {
		return fmt.Errorf("retire delivery %s on %s: %w", deliveryID, token, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("retire delivery %s on %s: the claim holds no such delivery", deliveryID, token)
	}
	return nil
}

func scanClaim(row pgx.Row) (supervise.Claim, error) {
	var (
		c                   supervise.Claim
		token, role, state  string
		generation          int64
		locator             []byte
		deliveryID, task    *string
		queuedAt, delivered *time.Time
		confirmed           *time.Time
	)
	err := row.Scan(&token, &c.Project, &c.Tree, &c.Issue, &role, &generation, &c.Session,
		&c.SessionFile, &locator, &state, &c.Budgets.LaunchFailures, &c.Budgets.PromptFailures,
		&c.Budgets.PromptRetires, &c.BootTokenHash, &c.CapabilityHash, &c.UncertainStreak, &c.WorkspaceLost,
		&deliveryID, &task, &queuedAt, &delivered, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return supervise.Claim{}, err
	}
	if err != nil {
		return supervise.Claim{}, fmt.Errorf("read claim: %w", err)
	}
	c.Token, c.Role, c.State = claim.Token(token), claim.Role(role), supervise.ClaimState(state)
	c.Generation = uint64(generation)
	if locator != nil {
		var loc runtime.Locator
		if err := json.Unmarshal(locator, &loc); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: decode its locator: %w", token, err)
		}
		if err := loc.Validate(); err != nil {
			return supervise.Claim{}, fmt.Errorf("read claim %s: %w", token, err)
		}
		c.Locator = &loc
	}
	if deliveryID != nil {
		c.Pending = &supervise.Delivery{
			ID:          *deliveryID,
			Task:        *task,
			QueuedAt:    *queuedAt,
			DeliveredAt: orZero(delivered),
			ConfirmedAt: orZero(confirmed),
		}
	}
	return c, nil
}

// instant is a time as a nullable column: the zero time is "not yet", which SQL says as null.
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func orZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
