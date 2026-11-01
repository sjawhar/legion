package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// LockTimeoutAttempts is how many times RetryLockTimeouts applies a migration whose lock wait
// outlasts its lock_timeout before the runner gives up and fails. LockTimeoutRetryPause is how long
// it waits between attempts, holding nothing: the attempt's transaction has rolled back, so the
// reads and writes that queued behind the migration's lock request run. A holder that ends within
// the pause, or soon after, lets the next attempt through; one that outlives every attempt fails
// the runner, as one outliving LockTimeout did before, after at most LockTimeoutAttempts waits of
// LockTimeout, each of which is all the time the migration can hold up those reads and writes.
const (
	LockTimeoutAttempts   = 3
	LockTimeoutRetryPause = 2 * time.Second
)

// RetryLockTimeouts runs attempt, which applies the migration named migration in a transaction of
// its own, until it succeeds, fails with anything but a *LockTimeoutError, or has timed out
// LockTimeoutAttempts times. It logs each timed-out attempt at WARN with its error, which names the
// lock, the sessions holding it and their diagnoses (LockTimeoutError), and pauses
// LockTimeoutRetryPause before the next. The last attempt's error comes back, wrapped with the
// number of attempts, so errors.As still finds the *LockTimeoutError. A runner that applies every
// migration in one transaction cannot use it: rolling back an attempt there would roll back the
// earlier migrations too, and retrying inside it would hold their locks across the pause.
func RetryLockTimeouts(ctx context.Context, migration string, attempt func(context.Context) error) error {
	for n := 1; ; n++ {
		err := attempt(ctx)
		var lockTimeout *LockTimeoutError
		if err == nil || !errors.As(err, &lockTimeout) {
			return err
		}
		if n == LockTimeoutAttempts {
			slog.Warn("pgmigrate: a migration gave up waiting for a lock on its last attempt; the runner fails", "migration", migration, "attempt", n, "attempts", LockTimeoutAttempts, "reason", err.Error())
			return fmt.Errorf("after %d attempts: %w", n, err)
		}
		slog.Warn("pgmigrate: a migration gave up waiting for a lock; the runner tries it again", "migration", migration, "attempt", n, "attempts", LockTimeoutAttempts, "pause", LockTimeoutRetryPause, "reason", err.Error())
		select {
		case <-ctx.Done():
			return fmt.Errorf("after %d attempts: %w", n, err)
		case <-time.After(LockTimeoutRetryPause):
		}
	}
}

// Exec is how a runner applies one migration's statements in tx. It first sets tx's lock_timeout
// to LockTimeout, so every migration a runner applies is bounded whatever the migration's file
// says, and then runs the statements while a watch reads what tx's backend waits for. A runner
// calls it once it holds its own advisory lock, never before: a second process's runner waiting
// for that lock is waiting for the first to finish, which queues no read or write behind it,
// since nothing but a runner takes that lock.
//
// signIn is the BeforeConnect of the pool tx came from, nil when the pool has none: the watch's
// own connection signs in as the pool's new connections do. A pool that signs in with a token
// minted per connection needs it, because tx's connection may have signed in long before (up to
// the pool's connection lifetime, an hour by default, plus however long the runner's advisory
// lock and earlier migrations took), past a token's own life, so its configuration's password
// cannot sign the watch in.
//
// Postgres cancels a statement whose lock wait outlasts lock_timeout saying only "canceling
// statement due to lock timeout", so Exec reports that cancellation as a *LockTimeoutError naming
// the migration, the lock it wanted and the sessions holding it, as the watch last saw them. Any
// other error comes back prefixed with the migration's file name.
func Exec(ctx context.Context, tx pgx.Tx, migration Migration, signIn func(context.Context, *pgx.ConnConfig) error) error {
	if _, err := tx.Exec(ctx, "select set_config('lock_timeout', $1, true)", lockTimeoutSetting); err != nil {
		return fmt.Errorf("migration %s: set lock_timeout: %w", migration.Name, err)
	}
	conn := tx.Conn()
	// The watch's own copy of the configuration (pgx.Conn.Config), which signIn may give a fresh
	// password before each dial.
	config := conn.Config()
	dial := func(ctx context.Context) (*pgx.Conn, error) {
		if signIn != nil {
			if err := signIn(ctx, config); err != nil {
				return nil, fmt.Errorf("sign the lock watch in: %w", err)
			}
		}
		dialed, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("connect the lock watch: %w", err)
		}
		return dialed, nil
	}
	watch := startWatch(ctx, dial, conn.PgConn().PID())
	_, err := tx.Exec(ctx, migration.SQL)
	wait, watchErr := watch.stop()
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
		timedOut := &LockTimeoutError{Migration: migration.Name, Wait: wait, WatchErr: watchErr, Err: err}
		if wait != nil {
			timedOut.Diagnoses = diagnoseHolders(ctx, dial, wait.Holders)
		}
		return timedOut
	}
	return fmt.Errorf("migration %s: %w", migration.Name, err)
}

// diagnoseHolders reads holderDiagnosis for each of holders on a connection of its own, in a
// read-only transaction, within LockTimeout: the migration's own transaction is aborted by its
// cancellation, and a diagnosis that cannot be had must not hold the runner up. A connection that
// cannot be had is one diagnosis saying why.
func diagnoseHolders(ctx context.Context, dial func(context.Context) (*pgx.Conn, error), holders []Session) []string {
	if len(holders) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, LockTimeout)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		return []string{"diagnosis not collected: " + err.Error()}
	}
	defer conn.Close(context.WithoutCancel(ctx))
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return []string{"diagnosis not collected: " + diagnosisFailure(err)}
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	diagnoses := make([]string, len(holders))
	for i, holder := range holders {
		diagnoses[i] = holderDiagnosis(ctx, tx, holder.PID)
	}
	return diagnoses
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

// Session is a database session a migration was queued behind or the census reports. It holds no
// query text: what names a session here goes to the service's or the deployment's logs, and the
// one place the census prints a session's query, a refusing lock holder's diagnosis, prints it
// redacted (holderDiagnosis). XactAge is how long its transaction had been open when it was read,
// to the microsecond Postgres keeps, nil when it was in no transaction or the census cannot see
// its transaction: pg_stat_activity shows another role's xact_start, and its state, only to a
// superuser, a member of that role or a member of pg_read_all_stats, records neither for a
// session with track_activities off (untrackedState), and never records a WAL sender's
// xact_start, to any role, though a replication=database connection runs SQL in transactions.
// The census takes only an idle session (idleState) that shows none for one in no transaction,
// and refuses any other lock holder whose age it cannot see, so a deployment that grants the
// census's role pg_read_all_stats lets it read the age of a holder of another role instead; a
// holder of the census's own role it sees, unless it is a WAL sender. Autovacuum is an autovacuum
// worker, which the census tells apart however much Postgres hides (lockHolders).
type Session struct {
	PID         uint32         `json:"pid"`
	User        string         `json:"user"`
	Application string         `json:"application"`
	State       string         `json:"state"`
	XactAge     *time.Duration `json:"xact_age"`
	Autovacuum  bool           `json:"autovacuum"`
}

// untrackedState is the state pg_stat_activity shows for a session with track_activities off,
// when the census's role may see that session: Postgres then records neither its activity text nor
// its transaction's start.
const untrackedState = "disabled"

// idleState is the state pg_stat_activity shows for a session waiting for its client outside a
// transaction, the one state in which the census takes a session that shows no transaction start
// for one in no transaction (holderRefusal says why).
const idleState = "idle"

// String is the session as every message names it, saying which fields Postgres hid.
func (s Session) String() string {
	user := "user " + s.User
	switch {
	case s.Autovacuum:
		user = "autovacuum worker"
	case s.User == "":
		user = "user not visible"
	}
	state := s.State
	age := "transaction age not visible"
	switch {
	case s.XactAge != nil:
		age = "transaction open " + s.XactAge.String()
	case state == idleState:
		age = "in no transaction when read"
	}
	if state == "" {
		state = "state not visible"
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
	// Diagnoses are the holders' diagnoses (holderDiagnosis), read just after the cancellation,
	// one for each of Wait.Holders.
	Diagnoses []string
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
	for _, diagnosis := range e.Diagnoses {
		b.WriteString("; " + diagnosis)
	}
	fmt.Fprintf(&b, "; end the holder or let it finish, then start the service again: %v", e.Err)
	return b.String()
}

func (e *LockTimeoutError) Unwrap() error { return e.Err }

// watchInterval is how often a watch reads what the migration waits for: many times inside
// LockTimeout, so the last reading before Postgres gives up names the wait.
const watchInterval = 200 * time.Millisecond

// lockWaitQuery reads the lock backend $1 is waiting for, if any, and the sessions ahead of it, each
// transaction's age in nanoseconds, as Session's time.Duration decodes it.
const lockWaitQuery = `
	select w.locktype, w.mode, coalesce(w.relation::regclass::text, w.locktype),
		coalesce((
			select json_agg(json_build_object(
				'pid', a.pid, 'user', a.usename, 'application', a.application_name,
				'state', a.state, 'xact_age', (extract(epoch from now() - a.xact_start) * 1000000000)::bigint)
				order by a.xact_start)
			from pg_stat_activity a
			where a.pid = any(pg_blocking_pids(w.pid))
		), '[]')
	from pg_locks w
	where w.pid = $1 and not w.granted
	limit 1`

// watch reads, every watchInterval until stopped, what one backend is waiting for. It reads on a
// connection of its own, since the backend's own connection is busy with the statement that
// waits: one dial opens, at the first reading, so a migration that finishes inside one interval
// dials nothing, and closed when the watch stops. A connection outside every pool keeps it clear
// of a shared pool whose callers the migration may be holding up.
type watch struct {
	cancel context.CancelFunc
	done   chan struct{}
	// wait and err are written by the watch's goroutine alone and read by stop once it has ended.
	wait *LockWait
	err  error
	// heldBack is true exactly when the latest reading observed was held back: an empty reading of
	// the recorded wait, taken just after a reading that was not held back.
	// Only the watch's goroutine touches it.
	heldBack bool
}

func startWatch(ctx context.Context, dial func(context.Context) (*pgx.Conn, error), pid uint32) *watch {
	ctx, cancel := context.WithCancel(ctx)
	w := &watch{cancel: cancel, done: make(chan struct{})}
	go w.run(ctx, dial, pid)
	return w
}

func (w *watch) run(ctx context.Context, dial func(context.Context) (*pgx.Conn, error), pid uint32) {
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
			dialed, err := dial(ctx)
			if err != nil {
				if ctx.Err() == nil {
					w.err = err
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
			// A failed read says nothing about the wait, so it is not observed at all: it neither
			// holds a reading back nor ends a run of empty ones. Do not call observe here, even with
			// nil: [named, empty, failed read, empty] would then name a holder that has left.
			w.err = err
			closeConn()
		}
	}
}

// observe records one reading of what the migration waits for; wait is nil when it was not
// waiting at that reading. pg_locks and pg_blocking_pids read the lock table in separate passes, so
// one reading taken as the cancellation dequeues the migration can list its ungranted lock with no
// blocker: an empty reading of the recorded wait's lock is held back rather than replacing it. A
// second empty reading in a row does replace it, since a holder that has left - or one
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
	w.heldBack = len(wait.Holders) == 0 && sameLock && !w.heldBack
	if !w.heldBack {
		w.wait = wait
	}
}

// stop ends the watch and returns the last wait it saw and the last error its reads met.
func (w *watch) stop() (*LockWait, error) {
	w.cancel()
	<-w.done
	return w.wait, w.err
}
