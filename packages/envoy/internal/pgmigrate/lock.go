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
	Holders []Session
}

// Session is a database session a migration was queued behind or the census reports. Its query
// text is never read: an operator reads it from pg_stat_activity, and what names a session here
// goes to the service's or the deployment's logs. XactSeconds is how long its transaction had been
// open when it was read, nil when Postgres hides it: pg_stat_activity shows another role's
// xact_start, and its state, only to a superuser or a member of pg_read_all_stats. The census
// refuses a lock holder whose age it cannot see, so a deployment that grants the census's role
// pg_read_all_stats lets it read the age instead.
type Session struct {
	PID         uint32 `json:"pid"`
	User        string `json:"user"`
	Application string `json:"application"`
	State       string `json:"state"`
	XactSeconds *int64 `json:"xact_seconds"`
}

// String is the session as every message names it, saying which fields Postgres hid.
func (s Session) String() string {
	user := "user " + s.User
	if s.User == "" {
		user = "user not visible"
	}
	state := s.State
	if state == "" {
		state = "state not visible"
	}
	age := "transaction age not visible"
	if s.XactSeconds != nil {
		age = "transaction open " + seconds(*s.XactSeconds)
	}
	return fmt.Sprintf("pid %d (%s, application %q, %s, %s)", s.PID, user, s.Application, state, age)
}

// sessionList names the sessions for a sentence, or none.
func sessionList(sessions []Session) string {
	if len(sessions) == 0 {
		return "none"
	}
	parts := make([]string, len(sessions))
	for i, s := range sessions {
		parts[i] = s.String()
	}
	return strings.Join(parts, "; ")
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
			fmt.Fprintf(&b, "; it was queued behind %s", sessionList(e.Wait.Holders))
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
				'state', a.state, 'xact_seconds', extract(epoch from now() - a.xact_start)::bigint)
				order by a.xact_start)
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
			// pg_locks and pg_blocking_pids read the lock table in separate passes, so a reading
			// taken as the cancellation dequeues the migration can list its ungranted lock with no
			// blocker. It does not replace a reading of the same lock that named its holders.
			if len(wait.Holders) == 0 && w.wait != nil && len(w.wait.Holders) > 0 &&
				w.wait.Mode == wait.Mode && w.wait.Object == wait.Object {
				continue
			}
			w.wait = &wait
		case errors.Is(err, pgx.ErrNoRows):
			// Not waiting at this reading; an earlier wait the migration got past stays recorded,
			// since only a later one it did not get past would replace it.
		case ctx.Err() == nil:
			w.err = err
			closeConn()
		}
	}
}

// stop ends the watch and returns the last wait it saw and the last error its reads met.
func (w *watch) stop() (*LockWait, error) {
	w.cancel()
	<-w.done
	return w.wait, w.err
}
