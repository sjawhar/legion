// Package ratelimit is the GitHub rate limits every Dispatch process obeys together: one row of
// github_rate_limits (migration 0123) per scope, judged by the database's clock.
//
// GitHub budgets the App and each installation, not a process, and warns that requests sent while
// limited may get the integration banned. So a limit one process meets is recorded here, and every
// process reads it before it sends anything under that scope: githubapp.Client through the Limits
// it is given (Wait and Record), and the webhook redelivery sweep for its own doubling backoff
// (Strike, InForce, Get and Clear). The database's now() is the one clock both processes read, as
// embeddings_rate_limit's is (migration 0078), so they agree on when a limit ends whatever their
// hosts' clocks say.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Store reads and writes github_rate_limits on a pool.
type Store struct {
	pool *store.Pool
}

// New returns the store on pool.
func New(pool *store.Pool) *Store { return &Store{pool: pool} }

// Limit is one scope's row as it was read or written.
type Limit struct {
	Scope string
	// Until is when a request under Scope may be sent again, by the database's clock.
	Until time.Time
	// Wait is Until less the database's now() when the row was read or written: how long the limit
	// had left then, zero or less once it has passed.
	Wait time.Duration
	// Strikes counts the limits Strike recorded with no Clear in between.
	Strikes int
	// updatedAt is the database's now() at the row's last write, which Clear matches so it deletes
	// only the row as read.
	updatedAt time.Time
}

// columns is what every read and write returns, in Limit's order.
const columns = `scope, until, extract(epoch from until - now()), strikes, updated_at`

func scan(row pgx.Row) (Limit, error) {
	var limit Limit
	var seconds float64
	err := row.Scan(&limit.Scope, &limit.Until, &seconds, &limit.Strikes, &limit.updatedAt)
	limit.Wait = time.Duration(seconds * float64(time.Second))
	return limit, err
}

// Wait is how long the limit in force on scope has left, zero when none is. It is the read
// githubapp.Client makes before every request it sends.
func (s *Store) Wait(ctx context.Context, scope string) (time.Duration, error) {
	limit, found, err := s.InForce(ctx, scope)
	if err != nil || !found {
		return 0, err
	}
	return limit.Wait, nil
}

// InForce returns, of the limits in force on scopes, the one that ends last; found is false when
// none is.
func (s *Store) InForce(ctx context.Context, scopes ...string) (limit Limit, found bool, err error) {
	limit, err = scan(s.pool.QueryRow(ctx, `
		select `+columns+` from github_rate_limits
		where scope = any($1) and until > now()
		order by until desc
		limit 1
	`, scopes))
	if errors.Is(err, pgx.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("read the GitHub rate limit of %v: %w", scopes, err)
	}
	return limit, true, nil
}

// Get returns scope's row whether or not its limit is still in force; found is false when scope
// has none.
func (s *Store) Get(ctx context.Context, scope string) (limit Limit, found bool, err error) {
	limit, err = scan(s.pool.QueryRow(ctx, `select `+columns+` from github_rate_limits where scope = $1`, scope))
	if errors.Is(err, pgx.ErrNoRows) {
		return Limit{}, false, nil
	}
	if err != nil {
		return Limit{}, false, fmt.Errorf("read the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, true, nil
}

// Record records that GitHub asked for wait before the next request under scope. The limit ends
// at the later of now() plus wait and a limit already recorded: two processes meeting one limit
// moments apart never shorten it.
func (s *Store) Record(ctx context.Context, scope string, wait time.Duration) error {
	if _, err := s.pool.Exec(ctx, `
		insert into github_rate_limits as l (scope, until, updated_at)
		values ($1, now() + make_interval(secs => $2), now())
		on conflict (scope) do update set until = greatest(l.until, excluded.until), updated_at = now()
	`, scope, wait.Seconds()); err != nil {
		return fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	return nil
}

// strike is Strike's statement: one more strike, and the latest of now() plus GitHub's wait, one
// minute doubled for each strike before this one (up to 64 minutes), and a limit already recorded.
const strike = `
	insert into github_rate_limits as l (scope, until, strikes, updated_at)
	values ($1, now() + greatest(make_interval(secs => $2), interval '1 minute'), 1, now())
	on conflict (scope) do update set
		strikes = l.strikes + 1,
		until = greatest(l.until, now() + greatest(make_interval(secs => $2), interval '1 minute' * power(2, least(l.strikes, 6)))),
		updated_at = now()
	returning ` + columns

// Strike records a limit GitHub answered with under scope, whose wait doubles with every limit met
// before a Clear, as GitHub asks of a request that keeps failing on a secondary limit. It returns
// the row it wrote.
func (s *Store) Strike(ctx context.Context, scope string, wait time.Duration) (Limit, error) {
	limit, err := scan(s.pool.QueryRow(ctx, strike, scope, wait.Seconds()))
	if err != nil {
		return Limit{}, fmt.Errorf("record the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, nil
}

// PreviewStrike returns the row Strike would write, and writes nothing: Strike's own statement in
// a transaction it rolls back, so the two never differ.
func (s *Store) PreviewStrike(ctx context.Context, scope string, wait time.Duration) (Limit, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Limit{}, fmt.Errorf("preview the GitHub rate limit of %s: %w", scope, err)
	}
	defer tx.Rollback(ctx)
	limit, err := scan(tx.QueryRow(ctx, strike, scope, wait.Seconds()))
	if err != nil {
		return Limit{}, fmt.Errorf("preview the GitHub rate limit of %s: %w", scope, err)
	}
	return limit, nil
}

// Clear deletes limit's row only if nothing has written it since it was read: a limit another
// process recorded meanwhile stands.
func (s *Store) Clear(ctx context.Context, limit Limit) error {
	if _, err := s.pool.Exec(ctx, `delete from github_rate_limits where scope = $1 and updated_at = $2`, limit.Scope, limit.updatedAt); err != nil {
		return fmt.Errorf("clear the GitHub rate limit of %s: %w", limit.Scope, err)
	}
	return nil
}
