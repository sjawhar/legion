package duty

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// HoldReleasedChannel is the channel Release notifies, with the hold's name as the payload, in the
// statement that deletes the hold: a standby that listens here takes a released name at once
// rather than waiting for a lapse that will never come.
const HoldReleasedChannel = "dispatch_duty_holds"

// Holds is the slice of a connection pool a hold needs, named here so this package depends on no
// store: *pgxpool.Pool and *store.Pool both satisfy it. Every statement a hold sends is one
// statement in a transaction of its own, so a hold needs neither a connection of its own nor an
// explicit transaction.
type Holds interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrHoldLost is SetToken finding that the hold's row no longer names this holder: the hold was
// taken over after it lapsed, or released.
var ErrHoldLost = errors.New("the hold no longer names this holder")

// HoldOptions are one hold's parameters.
//
// TTL is how long the hold lasts on the database's clock after each statement that takes or renews
// it. Renew is the interval between renewals, and also the bound on each renewal's wait for an
// answer. Margin is how long before the TTL runs out, on this process's monotonic clock, the hold
// reports itself lost. Token is the free text the holder records with the hold (SetToken changes
// it).
//
// TTL must exceed Margin, Margin must not be negative, and Renew must be positive and shorter than
// TTL - Margin, or no renewal could ever be confirmed before the hold reports itself lost.
type HoldOptions struct {
	TTL, Renew, Margin time.Duration
	Token              string
}

// The statements a hold sends. Every duration crosses as float seconds into make_interval, which
// keeps a Go duration to the microsecond the database stores, and every expiry is now() plus it,
// so each is on the database's clock.
const (
	// takeHold inserts the row, or takes it over when it has lapsed or is already this holder's,
	// in one statement. A live row of another holder leaves the statement affecting no row. Two
	// processes taking one name at once queue on the row: the second re-reads the first's row
	// once it commits and finds it live.
	takeHold = `insert into duty_holds (name, holder, token, expires_at)
		values ($1, $2, $3, now() + make_interval(secs => $4))
		on conflict (name) do update
			set holder = excluded.holder, token = excluded.token, expires_at = excluded.expires_at
			where duty_holds.expires_at <= now() or duty_holds.holder = excluded.holder`
	renewHold = `update duty_holds set expires_at = now() + make_interval(secs => $3)
		where name = $1 and holder = $2`
	setHoldToken = `update duty_holds set token = $3 where name = $1 and holder = $2`
	// releaseHold deletes the row only while it is this holder's, and notifies only when it
	// deleted one, in one statement and so in one transaction: a standby is woken exactly when
	// the name is free, and never by a holder that had already lost it.
	releaseHold = `with released as (
			delete from duty_holds where name = $1 and holder = $2 returning name)
		select pg_notify('` + HoldReleasedChannel + `', name) from released`
	// holdExpiry is how long name's hold has left, in microseconds on the database's clock.
	holdExpiry = `select (extract(epoch from expires_at - now()) * 1000000)::bigint
		from duty_holds where name = $1`
)

// Hold is one process's hold on a name: a row of duty_holds naming this holder, which the hold
// renews every Renew until it is released or lost.
//
// The guarantees, and the clock each rests on:
//
//   - One holder per name on the database's clock. A holder takes a name only when its row is
//     absent, lapsed or already its own, in one statement (takeHold), and every expiry is the
//     database's now() plus the TTL. No two processes ever both hold a live row.
//   - Loss is reported Margin before the TTL runs out, on this process's monotonic clock. The
//     loss deadline is the moment the last confirmed renewal (or the take) was sent, read before
//     the statement left, plus TTL - Margin. The database stamped that statement's expiry no
//     earlier than it arrived, so the row stays live at least Margin past the deadline: a holder
//     that stops when Lost closes, or that checks Deadline before each act, stops before any
//     other process can take the name. A renewal that fails or gets no answer within Renew
//     leaves the deadline where it was, and the next tick tries again; a renewal that affects no
//     row means the name is another's, and the hold is lost at once.
//   - Release frees the name at once and wakes the standbys: it deletes the row and notifies
//     HoldReleasedChannel in one transaction.
//
// A renewal the context abandoned can still reach the database later, once whatever held its row
// lets go; it extends the row only to its own send time plus the TTL, which delays a take-over and
// never lets two processes hold the name.
type Hold struct {
	db     Holds
	name   string
	holder uuid.UUID
	opts   HoldOptions

	// stop ends the renewal goroutine, which closes done when it returns.
	stop context.CancelFunc
	done chan struct{}

	// mu guards deadline, expired and the timer that closes lost at the deadline.
	mu       sync.Mutex
	deadline time.Time
	timer    *time.Timer
	expired  bool
	lost     chan struct{}
}

// TryHold takes name for holder, and returns nil, nil when another holder holds it live. It takes
// the name when its row is absent, lapsed or already holder's, in one statement that sets the
// expiry to the database's now() plus opts.TTL and records opts.Token. On success it starts the
// renewal, which runs until Release or until the hold is lost. ctx bounds the take alone.
func TryHold(ctx context.Context, db Holds, name string, holder uuid.UUID, opts HoldOptions) (*Hold, error) {
	if opts.Margin < 0 || opts.TTL <= opts.Margin || opts.Renew <= 0 || opts.Renew >= opts.TTL-opts.Margin {
		return nil, fmt.Errorf("hold %q: options TTL %s, Renew %s, Margin %s: need 0 <= Margin < TTL and 0 < Renew < TTL - Margin",
			name, opts.TTL, opts.Renew, opts.Margin)
	}
	sent := time.Now()
	tag, err := db.Exec(ctx, takeHold, name, holder, opts.Token, opts.TTL.Seconds())
	if err != nil {
		return nil, fmt.Errorf("take hold %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	renewing, stop := context.WithCancel(context.Background())
	h := &Hold{
		db:       db,
		name:     name,
		holder:   holder,
		opts:     opts,
		stop:     stop,
		done:     make(chan struct{}),
		deadline: sent.Add(opts.TTL - opts.Margin),
		lost:     make(chan struct{}),
	}
	h.mu.Lock()
	h.timer = time.AfterFunc(time.Until(h.deadline), h.expire)
	h.mu.Unlock()
	go h.renew(renewing)
	return h, nil
}

// Lost is closed when the hold is no longer this process's to act on: its loss deadline passed, a
// renewal or SetToken found the row another's, or Release was called. It is never reopened.
func (h *Hold) Lost() <-chan struct{} { return h.lost }

// Deadline is the current loss deadline: the send time of the last confirmed renewal (or the
// take), plus TTL - Margin, or the moment Lost closed if that came first. It carries the monotonic
// clock reading, so time.Now().Before(Deadline()) is the check a holder makes before each act it
// may make only while it holds the name. Once Lost has closed it never moves again.
func (h *Hold) Deadline() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deadline
}

// SetToken records token as the hold's token, guarded on the row naming this holder. A row that no
// longer does answers ErrHoldLost and closes Lost.
func (h *Hold) SetToken(ctx context.Context, token string) error {
	tag, err := h.db.Exec(ctx, setHoldToken, h.name, h.holder, token)
	if err != nil {
		return fmt.Errorf("set hold %q token: %w", h.name, err)
	}
	if tag.RowsAffected() == 0 {
		h.lose()
		return fmt.Errorf("set hold %q token: %w", h.name, ErrHoldLost)
	}
	return nil
}

// Release stops the renewal, closes Lost, and deletes the row while it still names this holder,
// notifying HoldReleasedChannel with the name in the same transaction. A row another holder has
// taken is left alone and wakes nobody. Calling it again repeats only the delete.
func (h *Hold) Release(ctx context.Context) error {
	h.stop()
	<-h.done
	h.lose()
	if _, err := h.db.Exec(ctx, releaseHold, h.name, h.holder); err != nil {
		return fmt.Errorf("release hold %q: %w", h.name, err)
	}
	return nil
}

// Expiry reads how long name's hold has left, as expires_at - now() on the database's clock, so a
// standby can arm one timer to the lapse instead of polling. held is false when no row exists. A
// lapsed row that nobody has taken over or released is still reported held, with remaining zero
// or negative: the name is free to take now.
func Expiry(ctx context.Context, db Holds, name string) (remaining time.Duration, held bool, err error) {
	var micros int64
	err = db.QueryRow(ctx, holdExpiry, name).Scan(&micros)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read hold %q expiry: %w", name, err)
	}
	return time.Duration(micros) * time.Microsecond, true, nil
}

// renew renews the hold every Renew until ctx ends or the hold is lost. Each renewal waits at most
// Renew for its answer. The send time is read before the statement leaves, because the database
// stamps the new expiry no earlier than the statement arrives, and only that order keeps the loss
// deadline Margin ahead of the row's lapse.
func (h *Hold) renew(ctx context.Context) {
	defer close(h.done)
	ticker := time.NewTicker(h.opts.Renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.lost:
			return
		case <-ticker.C:
		}
		sent := time.Now()
		bounded, cancel := context.WithTimeout(ctx, h.opts.Renew)
		tag, err := h.db.Exec(bounded, renewHold, h.name, h.holder, h.opts.TTL.Seconds())
		cancel()
		switch {
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("renew hold failed; the loss deadline stays where it was", "hold", h.name, "deadline", h.Deadline(), "error", err)
			}
		case tag.RowsAffected() == 0:
			slog.Warn("hold lost: its row names another holder or none", "hold", h.name)
			h.lose()
			return
		default:
			h.confirm(sent)
		}
	}
}

// confirm moves the loss deadline to sent + TTL - Margin for a renewal that was sent at sent and
// affected the row. A deadline that has already passed is final, so a renewal confirmed after it
// loses the hold rather than reviving it.
func (h *Hold) confirm(sent time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.expired {
		return
	}
	if !time.Now().Before(h.deadline) {
		h.loseLocked()
		return
	}
	h.deadline = sent.Add(h.opts.TTL - h.opts.Margin)
	h.timer.Reset(time.Until(h.deadline))
}

// expire is the deadline timer's callback. A renewal confirmed while the timer was firing has
// already moved the deadline and reset the timer, so only a deadline that has truly passed loses
// the hold.
func (h *Hold) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.expired || time.Now().Before(h.deadline) {
		return
	}
	h.loseLocked()
}

// lose closes Lost, once, and pulls a deadline still ahead back to now, so Deadline agrees with
// Lost.
func (h *Hold) lose() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loseLocked()
}

func (h *Hold) loseLocked() {
	if h.expired {
		return
	}
	h.expired = true
	h.timer.Stop()
	if now := time.Now(); now.Before(h.deadline) {
		h.deadline = now
	}
	close(h.lost)
}
