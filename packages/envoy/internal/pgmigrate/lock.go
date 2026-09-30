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
)

// LockTimeout bounds every lock wait of every migration a runner applies. A migration's
// statements take locks production's reads and writes queue behind - ALTER TABLE takes ACCESS
// EXCLUSIVE, which conflicts with a plain read - and Postgres waits for a lock without limit
// unless lock_timeout says otherwise, so a migration queued behind one long transaction (a psql
// session left idle in its transaction, a stuck job) holds every caller of that table queued
// behind its own request for as long as the holder lives. Five seconds is far longer than any of
// the services' own transactions holds a table, so an ordinary deploy never trips it, and short
// enough that a deploy stalled behind a stray holder fails, and is noticed, instead of being
// absorbed by every request queued behind it. Exec sets it on every migration it runs; a migration
// that needs another bound sets its own with SET LOCAL lock_timeout, which lasts until the
// runner's transaction ends.
const LockTimeout = 5 * time.Second

// lockNotAvailable is the SQLSTATE Postgres cancels a statement with when its lock_timeout runs
// out.
const lockNotAvailable = "55P03"

// lockTimeoutSetting is LockTimeout as lock_timeout takes it.
var lockTimeoutSetting = strconv.FormatInt(LockTimeout.Milliseconds(), 10) + "ms"

// Exec is how a runner applies one migration's statements in tx. It first sets tx's lock_timeout
// to LockTimeout, so every migration a runner applies is bounded whatever the migration's file
// says, and then runs the statements while a watch reads what tx's backend waits for. A runner
// calls it once it holds its own advisory lock, never before: a second process's runner waiting
// for that lock is waiting for the first to finish, which queues no read or write behind it,
// since nothing but a runner takes that lock.
//
// Postgres cancels a statement whose lock wait outlasts lock_timeout saying only "canceling
// statement due to lock timeout", so Exec reports that cancellation as a *LockTimeoutError naming
// the migration, the lock it wanted and the sessions holding it, as the watch last saw them. Any
// other error comes back prefixed with the migration's file name.
func Exec(ctx context.Context, tx pgx.Tx, migration Migration) error {
	if _, err := tx.Exec(ctx, "select set_config('lock_timeout', $1, true)", lockTimeoutSetting); err != nil {
		return fmt.Errorf("migration %s: set lock_timeout: %w", migration.Name, err)
	}
	conn := tx.Conn()
	watch := startWatch(ctx, conn.Config(), conn.PgConn().PID())
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

// watch reads, every watchInterval until stopped, what one backend is waiting for. It reads on a
// connection of its own, since the backend's own connection is busy with the statement that
// waits: dialed from that connection's configuration at the first reading, so a migration that
// finishes inside one interval dials nothing, and closed when the watch stops. A connection
// outside every pool keeps it clear of a shared pool whose callers the migration may be holding up.
type watch struct {
	cancel context.CancelFunc
	done   chan struct{}
	// wait and err are written by the watch's goroutine alone and read by stop once it has ended.
	wait *LockWait
	err  error
	// heldBack records that an empty reading of the current wait has already been held back, so
	// the next empty one replaces it (observe). Only the watch's goroutine touches it.
	heldBack bool
}

func startWatch(ctx context.Context, config *pgx.ConnConfig, pid uint32) *watch {
	ctx, cancel := context.WithCancel(ctx)
	w := &watch{cancel: cancel, done: make(chan struct{})}
	go w.run(ctx, config, pid)
	return w
}

func (w *watch) run(ctx context.Context, config *pgx.ConnConfig, pid uint32) {
	defer close(w.done)
	var conn *pgx.Conn
	closeConn := func() {
		if conn == nil {
			return
		}
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn.Close(closing)
		conn = nil
	}
	defer closeConn()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if conn == nil {
			dialed, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				if ctx.Err() == nil {
					w.err = fmt.Errorf("connect the lock watch: %w", err)
				}
				continue
			}
			conn = dialed
		}
		var wait LockWait
		err := conn.QueryRow(ctx, lockWaitQuery, pid).Scan(&wait.LockType, &wait.Mode, &wait.Object, &wait.Holders)
		switch {
		case err == nil:
			w.observe(&wait)
		case errors.Is(err, pgx.ErrNoRows):
			w.observe(nil)
		case ctx.Err() == nil:
			// A failed read says nothing about the wait, so it neither holds a reading back nor
			// ends a run of empty ones: it is not observed at all.
			w.err = err
			closeConn()
		}
	}
}

// observe records one reading of what the migration waits for; wait is nil when it was not
// waiting at that reading. pg_locks and pg_blocking_pids read the lock table in separate passes, so
// one reading taken as the cancellation dequeues the migration can list its ungranted lock with no
// blocker: that reading is held back rather than replacing one of the same lock that named its
// holders. A second empty reading in a row does replace it, since a holder that has left - or one
// pg_blocking_pids cannot name, such as a prepared transaction, which it reports as pid 0 - must
// not stay named.
func (w *watch) observe(wait *LockWait) {
	if wait == nil {
		// Not waiting at this reading: an earlier wait the migration got past stays recorded, since
		// only a later one it did not get past would replace it, and a later wait starts afresh.
		w.heldBack = false
		return
	}
	sameLock := w.wait != nil && w.wait.Mode == wait.Mode && w.wait.Object == wait.Object
	if len(wait.Holders) == 0 && sameLock && len(w.wait.Holders) > 0 && !w.heldBack {
		w.heldBack = true
		return
	}
	w.heldBack = false
	w.wait = wait
}

// stop ends the watch and returns the last wait it saw and the last error its reads met.
func (w *watch) stop() (*LockWait, error) {
	w.cancel()
	<-w.done
	return w.wait, w.err
}
