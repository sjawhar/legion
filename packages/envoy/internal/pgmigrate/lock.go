package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockTimeout bounds every lock wait of every migration a runner applies. A migration's
// statements take locks production's reads and writes queue behind - ALTER TABLE takes ACCESS
// EXCLUSIVE, which conflicts with a plain read - and Postgres waits for a lock without limit
// unless lock_timeout says otherwise, so a migration queued behind one long transaction (a psql
// session left idle in its transaction, a stuck job) holds every caller of that table queued
// behind its own request for as long as the holder lives. Five seconds is far longer than any of
// the services' own transactions holds a table, so an ordinary deploy never trips it, and short
// enough that a deploy stalled behind a stray holder fails, and is noticed, instead of being
// absorbed by every request queued behind it. A migration that needs another bound sets its own
// with SET LOCAL lock_timeout, which lasts until the runner's transaction ends.
const LockTimeout = 5 * time.Second

// lockNotAvailable is the SQLSTATE Postgres cancels a statement with when its lock_timeout runs
// out.
const lockNotAvailable = "55P03"

// BoundLockWaits sets tx's lock_timeout to LockTimeout until tx ends. A runner calls it once it
// holds its own advisory lock, and not before: a second process's runner waiting for that lock
// is waiting for the first to finish, which queues no read or write behind it, since nothing but
// a runner takes that lock.
func BoundLockWaits(ctx context.Context, tx pgx.Tx) error {
	setting := strconv.FormatInt(LockTimeout.Milliseconds(), 10) + "ms"
	if _, err := tx.Exec(ctx, "select set_config('lock_timeout', $1, true)", setting); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	return nil
}

// OpenWatchPool opens the one-connection pool Exec's watch reads on, from a copy of the runner's
// pool configuration; the runner closes it when its run ends. The migration's own connection is
// busy running the statement that waits, so the watch needs another, and taking it from a pool of
// its own keeps it off a shared pool whose callers the migration may be holding up. It dials only
// when a migration runs long enough for the watch's first read.
func OpenWatchPool(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
	config = config.Copy()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open migration lock watch pool: %w", err)
	}
	return pool, nil
}

// Exec runs migration's statements in tx while a watch on watchPool reads what tx's backend waits
// for. Postgres cancels a statement whose lock wait outlasts lock_timeout saying only "canceling
// statement due to lock timeout", so Exec reports that cancellation as a *LockTimeoutError naming
// the migration, the lock it wanted and the sessions holding it, as the watch last saw them. Any
// other error comes back prefixed with the migration's file name.
func Exec(ctx context.Context, tx pgx.Tx, watchPool *pgxpool.Pool, migration Migration) error {
	watch := startWatch(ctx, watchPool, tx.Conn().PgConn().PID())
	_, err := tx.Exec(ctx, migration.SQL)
	wait, watchErr := watch.stop()
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
		return &LockTimeoutError{Migration: migration.Name, Wait: wait, WatchErr: watchErr, Err: err}
	}
	return fmt.Errorf("migration %s: %w", migration.Name, err)
}

// LockWait is a lock a migration's backend was waiting for and the sessions ahead of it.
type LockWait struct {
	// LockType is pg_locks.locktype: relation for a table or index, transactionid for a row
	// another transaction has changed, and so on.
	LockType string
	// Mode is the lock mode it asked for, AccessExclusiveLock for most ALTER TABLE.
	Mode string
	// Object is the relation's name for a relation lock, and the lock type for any other.
	Object string
	// Holders are the sessions pg_blocking_pids named, oldest transaction first.
	Holders []LockHolder
}

// LockHolder is a session ahead of a migration in a lock's queue. Its query text is left out: an
// operator reads it from pg_stat_activity, and the runner's error goes to the service's logs.
type LockHolder struct {
	PID         uint32     `json:"pid"`
	User        string     `json:"user"`
	Application string     `json:"application"`
	State       string     `json:"state"`
	XactStart   *time.Time `json:"xact_start"`
}

// LockTimeoutError is a migration that gave up waiting for a lock and applied nothing, since its
// transaction rolled back.
type LockTimeoutError struct {
	Migration string
	// Wait is the lock the watch last saw the migration waiting for, nil when it saw none.
	Wait *LockWait
	// WatchErr is the last error the watch's reads met, which is why Wait can be nil.
	WatchErr error
	// Err is Postgres's own cancellation.
	Err error
}

func (e *LockTimeoutError) Error() string {
	var b strings.Builder
	if e.Wait == nil {
		fmt.Fprintf(&b, "migration %s gave up waiting for a lock when its lock_timeout ran out (%s unless the migration sets its own) and applied nothing; the lock watch did not see which lock", e.Migration, LockTimeout)
		if e.WatchErr != nil {
			fmt.Fprintf(&b, " (%v)", e.WatchErr)
		}
		fmt.Fprintf(&b, ". Find the transactions open longer than that: select pid, usename, application_name, state, xact_start, query from pg_stat_activity where xact_start < now() - interval '%d milliseconds' order by xact_start", LockTimeout.Milliseconds())
	} else {
		fmt.Fprintf(&b, "migration %s gave up waiting for %s on %s when its lock_timeout ran out (%s unless the migration sets its own) and applied nothing", e.Migration, e.Wait.Mode, e.Wait.Object, LockTimeout)
		if len(e.Wait.Holders) > 0 {
			b.WriteString("; it was queued behind ")
			for i, holder := range e.Wait.Holders {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "pid %d (user %s, application %q, %s", holder.PID, holder.User, holder.Application, holder.State)
				if holder.XactStart != nil {
					fmt.Fprintf(&b, ", transaction open since %s", holder.XactStart.UTC().Format(time.RFC3339))
				}
				b.WriteString(")")
			}
		}
		switch {
		case e.Wait.LockType == "relation":
			fmt.Fprintf(&b, ". Find the sessions holding locks on %s: select a.pid, a.usename, a.application_name, a.state, a.xact_start, a.query from pg_locks l join pg_stat_activity a on a.pid = l.pid where l.granted and l.relation = '%s'::regclass order by a.xact_start", e.Wait.Object, strings.ReplaceAll(e.Wait.Object, "'", "''"))
		case len(e.Wait.Holders) > 0:
			pids := make([]string, len(e.Wait.Holders))
			for i, holder := range e.Wait.Holders {
				pids[i] = strconv.FormatUint(uint64(holder.PID), 10)
			}
			fmt.Fprintf(&b, ". Read them: select pid, usename, application_name, state, xact_start, query from pg_stat_activity where pid in (%s)", strings.Join(pids, ", "))
		default:
			fmt.Fprintf(&b, ". Find the transactions open longer than the timeout: select pid, usename, application_name, state, xact_start, query from pg_stat_activity where xact_start < now() - interval '%d milliseconds' order by xact_start", LockTimeout.Milliseconds())
		}
	}
	fmt.Fprintf(&b, "; end the holder or let it finish, then start the service again: %v", e.Err)
	return b.String()
}

func (e *LockTimeoutError) Unwrap() error { return e.Err }

// watchInterval is how often a watch reads what the migration waits for: many times inside
// LockTimeout, so the last reading before Postgres gives up names the wait.
const watchInterval = 200 * time.Millisecond

// lockWaitQuery reads the lock backend $1 is waiting for, if any, and the sessions ahead of it.
const lockWaitQuery = `
	select w.locktype, w.mode, coalesce(w.relation::regclass::text, w.locktype),
		coalesce((
			select json_agg(json_build_object(
				'pid', a.pid, 'user', a.usename, 'application', a.application_name,
				'state', a.state, 'xact_start', a.xact_start) order by a.xact_start)
			from pg_stat_activity a
			where a.pid = any(pg_blocking_pids(w.pid))
		), '[]')
	from pg_locks w
	where w.pid = $1 and not w.granted
	limit 1`

// watch reads, every watchInterval until stopped, what one backend is waiting for.
type watch struct {
	cancel context.CancelFunc
	done   chan struct{}
	// wait and err are written by the watch's goroutine alone and read by stop once it has ended.
	wait *LockWait
	err  error
}

func startWatch(ctx context.Context, pool *pgxpool.Pool, pid uint32) *watch {
	ctx, cancel := context.WithCancel(ctx)
	w := &watch{cancel: cancel, done: make(chan struct{})}
	go w.run(ctx, pool, pid)
	return w
}

func (w *watch) run(ctx context.Context, pool *pgxpool.Pool, pid uint32) {
	defer close(w.done)
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		var wait LockWait
		err := pool.QueryRow(ctx, lockWaitQuery, pid).Scan(&wait.LockType, &wait.Mode, &wait.Object, &wait.Holders)
		switch {
		case err == nil:
			w.wait = &wait
		case errors.Is(err, pgx.ErrNoRows):
			// Not waiting at this reading; an earlier wait the migration got past stays recorded,
			// since only a later one it did not get past would replace it.
		case ctx.Err() == nil:
			w.err = err
		}
	}
}

// stop ends the watch and returns the last wait it saw and the last error its reads met.
func (w *watch) stop() (*LockWait, error) {
	w.cancel()
	<-w.done
	return w.wait, w.err
}
