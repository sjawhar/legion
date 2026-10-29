// Package processes is the registry of live envoy-dispatch server processes (INFRA-555, migration
// 0135): one server_processes row per process that serves requests, written once the process has
// bound its port and before it serves anything, kept alive by a heartbeat, and deleted once its
// HTTP drain has ended. Which rows are live is measured on the database's clock, never a process's
// own, since two hosts' clocks differ by whatever theirs do.
//
// The registry is also where a process refuses to start beside a live process whose signing key
// set does not share its own both ways (Register, ErrKeySetMismatch): a session cookie minted by
// one process is presented to another on the next request, and a key one of them does not hold
// signs that person out. A process with no DISPATCH_SIGNING_KEY signs with a key it minted in its
// own data dir, which no other process holds, so it is refused beside any live peer, and so is a
// pod rolled with a changed secret beside pods that hold only the old one. A refusal stands only
// beside a process that stays live: Register waits up to LiveWindow and one HeartbeatInterval for
// the refusing process to stop heartbeating, so a process restarted after a kill, whose dead row
// still counts as live for up to LiveWindow, starts once that row lapses.
package processes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The registry's intervals. Variables rather than constants only so this package's tests can
// shorten them; nothing else sets them.
var (
	// HeartbeatInterval is how often a registered process stamps its row's heartbeat_at.
	HeartbeatInterval = 5 * time.Second
	// LiveWindow is how recent a heartbeat must be, by the database's now(), for its process to
	// count as live: three heartbeats, so one slow or lost update does not drop a process that
	// still serves.
	LiveWindow = 15 * time.Second
	// PruneAge is how old a heartbeat must be before the prune loop deletes its row: a process
	// that died without deregistering (killed, out of memory, its node lost) leaves its row
	// behind, and long past LiveWindow nothing reads it.
	PruneAge = 10 * time.Minute
	// PruneInterval is the prune loop's period, and the period of its lease (duty.Exclusive).
	PruneInterval = time.Minute
)

// heartbeatTimeout bounds one heartbeat's transaction, well inside HeartbeatInterval so that a
// database that has stopped answering costs a heartbeat and not the loop: the next tick tries
// again.
const heartbeatTimeout = 2 * time.Second

// registryLock is the transaction advisory lock Register takes before it reads the live rows, so
// two processes booting at once are serialised and the second one's read sees the first one's row.
// Every heartbeat takes it too, so a process whose heartbeat lapsed and a process registering
// meanwhile are serialised the same way (Registration.renew).
// It is a single 64-bit key ("DPROCREG"): every other single-key advisory lock of the server is
// either a hashtext, which Postgres widens from 32 bits and so never reaches this value, or one of
// three fixed keys of their own (the migration runner's 8150001 in store/store.go, the events'
// commit order "DISPATCH" in events/broker.go and the file backfill's "LEGION52" in
// files/backfill.go), and the two-key form other locks take is a key space of its own.
const registryLock int64 = 0x4450524F43524547

// pruneLease is the duty.Exclusive lease the prune loop runs under, so one process prunes per
// PruneInterval however many serve the database.
const pruneLease = "server-processes"

// ErrKeySetMismatch is Register refusing to start a process beside a live process whose signing
// key set does not share this process's both ways, or a heartbeat refusing to count a process that
// had dropped out of Live as live again beside one (Registration.Lost). The error wraps it and
// names the other process.
var ErrKeySetMismatch = errors.New("this process's signing key set does not match a live server process's")

// Instance is what a process registers: the build it serves and the keys it signs and verifies
// with.
type Instance struct {
	// Commit is the legion commit the binary was stamped with, "" for an unstamped build.
	Commit string
	// DashboardBuild names the dashboard the process serves (routes.DashboardBuild), "" for none.
	DashboardBuild string
	// SigningKeys is the fingerprint of each key of the process's DISPATCH_SIGNING_KEY set
	// (auth.SigningKeys.Fingerprints), the current key first. It holds at least that one.
	SigningKeys []string
}

// Process is one row of the registry.
type Process struct {
	// ID is the process's instance id, the row's primary key.
	ID             string
	Commit         string
	DashboardBuild string
	// SigningKeys is the process's key fingerprints, its current key first.
	SigningKeys []string
	StartedAt   time.Time
	HeartbeatAt time.Time
}

// Querier is what Live reads through: the shared pool, or a transaction its caller already holds.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// selectLive reads every row whose heartbeat is within the live window of the database's now(),
// the oldest process first.
const selectLive = `
	select instance::text, commit, dashboard_build, signing_keys, started_at, heartbeat_at
	from server_processes
	where heartbeat_at > now() - make_interval(secs => $1)
	order by started_at, instance`

// Live returns the live processes: those whose last heartbeat is within LiveWindow of the
// database's now(), the oldest first.
func Live(ctx context.Context, db Querier) ([]Process, error) {
	rows, err := db.Query(ctx, selectLive, LiveWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("read the live server processes: %w", err)
	}
	defer rows.Close()
	var live []Process
	for rows.Next() {
		var p Process
		if err := rows.Scan(&p.ID, &p.Commit, &p.DashboardBuild, &p.SigningKeys, &p.StartedAt, &p.HeartbeatAt); err != nil {
			return nil, fmt.Errorf("read the live server processes: %w", err)
		}
		live = append(live, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the live server processes: %w", err)
	}
	return live, nil
}

// Registration is this process's row in the registry, kept live by a heartbeat until Deregister.
type Registration struct {
	// ID is the row's instance id.
	ID string
	// instance is what the row was registered with, kept so a heartbeat that finds the row gone
	// can write it again.
	instance Instance
	// pool is the registry's own one-connection pool, which Register opens and Deregister closes
	// (Register says why it is not the shared pool).
	pool    *pgxpool.Pool
	stop    context.CancelFunc
	stopped chan struct{}
	// lost carries the heartbeat's refusal (Lost); it holds one value and is sent at most once.
	lost chan error
}

// Register records this process in the registry and starts its heartbeat, unless a live process's
// signing key set does not share this one's: then it records nothing and returns an error wrapping
// ErrKeySetMismatch. Sharing means both ways: the other process's current key (the one its new
// cookies are signed with) is among this process's keys, and this process's current key is among
// the other's, so a cookie either one mints verifies on the other. "K1 K0" beside "K0 K1" shares;
// a process that minted a key of its own beside any other does not.
//
// A refusal stands only beside a process that stays live. A process killed without deregistering
// (SIGKILL, out of memory, its node lost) leaves a row that counts as live for up to LiveWindow
// after its last heartbeat, and a process started in that window with another key set - a dev
// sign-in server restarted after a kill, since its key is minted per process, or a pod replaced
// with a changed secret - would be refused by a process that no longer runs. So Register waits
// (waitForAdmission): a refusal is logged at WARN, naming the refusing process, and the admission
// is tried again every HeartbeatInterval until it is admitted, once the refusing rows have
// lapsed, or until LiveWindow and one HeartbeatInterval have passed since the first refusal, which
// a live process outlasts by heartbeating every HeartbeatInterval: then Register returns that last
// refusal. The caller serves nothing meanwhile, since it serves only once Register returns, and
// ctx ending ends the wait with ctx's error.
//
// Each admission is one transaction under registryLock (admit), so two processes booting at once
// are serialised: the second reads the first's row and is refused if the two do not share. A row
// whose heartbeat is older than LiveWindow is a process gone, and refuses nothing; should that
// process resume, its heartbeat checks its keys against this one before it counts as live again
// (Registration.renew).
//
// The registry's statements, the heartbeat above all, run on a pool of one connection of its own,
// opened here from the shared pool's configuration and closed by Deregister. Not the shared pool,
// for two reasons. A heartbeat queued behind sixteen requests that wait on one writer's lock is a
// heartbeat that lands late, and three late ones make a process that still serves look gone. And
// cmd/dispatch's shutdown decides that the database has stopped answering from the shared pool's
// connections (store.Pool.Use): a heartbeat timing out there every few seconds hands a connection
// back each time, which reads as a database still answering, and the process would never exit.
func Register(ctx context.Context, pool *store.Pool, instance Instance) (*Registration, error) {
	if len(instance.SigningKeys) == 0 {
		return nil, errors.New("register this server process: it names no signing key")
	}
	config := pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	own, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open the server process registry's pool: %w", err)
	}
	id := uuid.NewString()
	if err := waitForAdmission(ctx, own, id, instance); err != nil {
		own.Close()
		return nil, err
	}
	beating, stop := context.WithCancel(context.Background())
	registration := &Registration{ID: id, instance: instance, pool: own, stop: stop, stopped: make(chan struct{}), lost: make(chan error, 1)}
	go registration.heartbeat(beating)
	return registration, nil
}

// waitForAdmission is Register's wait: tryRegister every HeartbeatInterval until it admits the
// process, or until LiveWindow and one HeartbeatInterval have passed since its first refusal, when
// that last refusal is returned. A refusal naming another process than the one before is logged
// again, so the log names every process the wait was on.
func waitForAdmission(ctx context.Context, pool *pgxpool.Pool, id string, instance Instance) error {
	var giveUp time.Time
	logged := ""
	for {
		refusal, err := tryRegister(ctx, pool, id, instance)
		if err != nil {
			return fmt.Errorf("register this server process: %w", err)
		}
		if refusal == nil {
			return nil
		}
		if giveUp.IsZero() {
			giveUp = time.Now().Add(LiveWindow + HeartbeatInterval)
		}
		if !time.Now().Before(giveUp) {
			return refusal
		}
		if refusal.Error() != logged {
			logged = refusal.Error()
			slog.Warn("dispatch: a live server process refuses this one's signing keys; waiting for its heartbeat to lapse, as a process killed without deregistering lets it, before this one refuses to start",
				"instance", id, "refusal", refusal, "gives_up_in", time.Until(giveUp).Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("register this server process: %w", ctx.Err())
		case <-time.After(HeartbeatInterval):
		}
	}
}

// tryRegister is one admission of Register, in its own transaction under registryLock: admit's
// refusal, nil once the row is written, and an error when no decision was reached.
func tryRegister(ctx context.Context, pool *pgxpool.Pool, id string, instance Instance) (refusal error, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLock); err != nil {
		return nil, err
	}
	if refusal, err := admit(ctx, tx, id, instance); refusal != nil || err != nil {
		return refusal, err
	}
	return nil, tx.Commit(ctx)
}

// admit is the registry's admission check, made in tx under registryLock, which its caller holds:
// against every live process but id itself, it returns the refusal of the first whose key set does
// not share instance's (keySetsShared) and writes nothing; when each shares, it writes id's row as
// instance, or, where the row is there already (a lapsed heartbeat, Registration.renew), stamps its
// heartbeat. The second result is a failure to reach a decision. The caller commits.
func admit(ctx context.Context, tx pgx.Tx, id string, instance Instance) (refusal error, err error) {
	live, err := Live(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, peer := range live {
		if peer.ID == id {
			// A lapsed row Live still counts by the transaction's now() (Registration.renew).
			continue
		}
		if err := keySetsShared(peer, instance.SigningKeys); err != nil {
			return err, nil
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into server_processes (instance, commit, dashboard_build, signing_keys)
		values ($1, $2, $3, $4)
		on conflict (instance) do update set heartbeat_at = now()`,
		id, instance.Commit, instance.DashboardBuild, instance.SigningKeys); err != nil {
		return nil, err
	}
	return nil, nil
}

// rotationAdvice is what every refusal tells the operator to do.
const rotationAdvice = "give every server process the same DISPATCH_SIGNING_KEY (a process started without one " +
	"signs with a key of its own from its data dir, which no other process holds); to rotate the key, first add " +
	"the new key second to every process's setting and roll them all, then move it first and roll again"

// keySetsShared reports why peer and a process signing with mine (its current key first) cannot
// serve side by side, or nil when they can.
func keySetsShared(peer Process, mine []string) error {
	if len(peer.SigningKeys) == 0 || !slices.Contains(mine, peer.SigningKeys[0]) {
		return fmt.Errorf("%w: live server process %s (%s) signs its cookies with a key this process does not hold: %s",
			ErrKeySetMismatch, peer.ID, describe(peer), rotationAdvice)
	}
	if !slices.Contains(peer.SigningKeys, mine[0]) {
		return fmt.Errorf("%w: live server process %s (%s) does not hold the key this process would sign its cookies with: %s",
			ErrKeySetMismatch, peer.ID, describe(peer), rotationAdvice)
	}
	return nil
}

// describe is how a refusal names the other process beyond its id: its build and its start.
func describe(peer Process) string {
	commit := peer.Commit
	if commit == "" {
		commit = "unstamped"
	}
	return fmt.Sprintf("commit %s, started %s", commit, peer.StartedAt.UTC().Format(time.RFC3339))
}

// heartbeat stamps the row's heartbeat_at every HeartbeatInterval until ctx ends. It runs for as
// long as the process serves, through its HTTP drain, so a draining process counts as live until
// its last request is answered and Deregister ends it. It ends early, once, when a beat refuses
// to count this process as live again (beat); Lost delivers that refusal.
func (r *Registration) heartbeat(ctx context.Context) {
	defer close(r.stopped)
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if refusal := r.beat(ctx); refusal != nil {
				r.lost <- refusal
				return
			}
		}
	}
}

// Lost delivers, at most once, the error wrapping ErrKeySetMismatch with which a heartbeat refused
// to count this process as live again beside a live process whose key set it does not share
// (beat). The heartbeat has stopped by then and recorded nothing: the process no longer counts as
// live, and must stop serving, since each of the two would refuse the cookies the other mints.
func (r *Registration) Lost() <-chan error { return r.lost }

// beat is one heartbeat, bounded by heartbeatTimeout. It returns the refusal renew reports, and
// nil otherwise. A failure is logged and the next tick tries again; a process whose heartbeats
// keep failing for LiveWindow drops out of Live while it still serves, which is the cost of a
// registry measured on the database's clock, and renew checks its keys again before it counts as
// live once more.
func (r *Registration) beat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	refusal, err := r.renew(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("dispatch: server process heartbeat", "instance", r.ID, "error", err)
		}
		return nil
	}
	return refusal
}

// renew is beat's transaction, under registryLock. A row still live is stamped and nothing else is
// read: the key check was made when it was registered or last rejoined, and every process that
// registered since has checked its keys against this one. A row that had dropped out of Live -
// the process frozen, or cut from the database, for longer than LiveWindow - or that is gone,
// pruned after PruneAge, is a process that any registrant meanwhile counted as gone, so renew
// makes Register's check again (admit) before it writes the row: against every live process, it
// rejoins (logged at WARN) when each shares this process's key set, and otherwise records nothing
// and returns the refusal, the first result. The second is a failure to reach a decision.
//
// Whether the row is live is measured with clock_timestamp() read after the lock is held, not the
// transaction's now(). A registrant measures liveness with its own transaction's now(), which is
// never later than the moment this transaction acquires the lock the registrant held, so a row the
// registrant counted as gone is one this transaction counts as lapsed too: a process never stamps
// itself live again past a registrant that did not check its keys against it.
func (r *Registration) renew(ctx context.Context) (refusal error, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLock); err != nil {
		return nil, err
	}
	var live bool
	err = tx.QueryRow(ctx, `
		select heartbeat_at > clock_timestamp() - make_interval(secs => $2)
		from server_processes where instance = $1`, r.ID, LiveWindow.Seconds()).Scan(&live)
	gone := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !gone {
		return nil, err
	}
	if live {
		if _, err := tx.Exec(ctx, `update server_processes set heartbeat_at = now() where instance = $1`, r.ID); err != nil {
			return nil, err
		}
		return nil, tx.Commit(ctx)
	}
	refusal, err = admit(ctx, tx, r.ID, r.instance)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return fmt.Errorf("this server process's heartbeat lapsed, and it cannot count as live again: %w", refusal), nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	row := "lapsed"
	if gone {
		row = "gone"
	}
	slog.Warn("dispatch: this server process's heartbeat had lapsed; it shares its signing keys with every live server process and rejoined the registry",
		"instance", r.ID, "row", row)
	return nil, nil
}

// Deregister stops the heartbeat, deletes the row and closes the registry's pool. It is called
// once the process's HTTP drain has ended, and at most once.
func (r *Registration) Deregister(ctx context.Context) error {
	r.stop()
	<-r.stopped
	defer r.pool.Close()
	if _, err := r.pool.Exec(ctx, `delete from server_processes where instance = $1`, r.ID); err != nil {
		return fmt.Errorf("deregister server process %s: %w", r.ID, err)
	}
	return nil
}

// RunPrune deletes, every PruneInterval until ctx ends, the rows whose heartbeat is older than
// PruneAge by the database's clock: what processes that died without deregistering left behind.
// Each pass runs under pruneLease, so one process prunes per period however many serve the
// database. The ticks are duty.RunLeased's, a ticker's: the first one interval after the call,
// then one each interval; the first tick a pass overran comes at once with the moment it was due,
// and only the ones after it are dropped.
func RunPrune(ctx context.Context, pool *store.Pool, leases duty.Leases) {
	duty.RunLeased(ctx, leases, pruneLease, PruneInterval, duty.TickAfterInterval, func(ctx context.Context) {
		pruned, err := prune(ctx, pool)
		if err != nil {
			slog.Warn("dispatch: prune the server process registry", "error", err)
		} else if pruned > 0 {
			slog.Info("dispatch: pruned server processes gone without deregistering", "rows", pruned)
		}
	})
}

// prune is one pass of RunPrune.
func prune(ctx context.Context, pool *store.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `delete from server_processes where heartbeat_at < now() - make_interval(secs => $1)`,
		PruneAge.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
