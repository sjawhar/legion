// Package processes is the registry of live envoy-dispatch server processes (INFRA-555, migration
// 0121): one server_processes row per process that serves requests, written once the process has
// bound its port and before it serves anything, kept alive by a heartbeat, and deleted once its
// HTTP drain has ended. Which rows are live is measured on the database's clock, never a process's
// own, since two hosts' clocks differ by whatever theirs do.
//
// The registry is also where a process refuses to start beside a live process whose signing key
// set does not share its own both ways (Register, ErrKeySetMismatch): a session cookie minted by
// one process is presented to another on the next request, and a key one of them does not hold
// signs that person out. A process with no DISPATCH_SIGNING_KEY signs with a key it minted in its
// own data dir, which no other process holds, so it is refused beside any live peer, and so is a
// pod rolled with a changed secret beside pods that hold only the old one.
package processes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

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

// heartbeatTimeout bounds one heartbeat update, well inside HeartbeatInterval so that a database
// that has stopped answering costs a heartbeat and not the loop: the next tick tries again.
const heartbeatTimeout = 2 * time.Second

// registryLock is the transaction advisory lock Register takes before it reads the live rows, so
// two processes booting at once are serialised and the second one's read sees the first one's row.
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
// key set does not share this process's both ways. The error Register returns wraps it and names
// the other process.
var ErrKeySetMismatch = errors.New("this process's signing key set does not match a live server process's")

// Instance is what a process registers: the build it serves and the keys it signs and verifies
// with.
type Instance struct {
	// Commit is the legion commit the binary was stamped with, "" for an unstamped build.
	Commit string
	// DashboardBuild names the dashboard the process serves (DashboardBuild), "" for none.
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
	// pool is the registry's own one-connection pool, which Register opens and Deregister closes
	// (Register says why it is not the shared pool).
	pool    *pgxpool.Pool
	stop    context.CancelFunc
	stopped chan struct{}
}

// Register records this process in the registry and starts its heartbeat, unless a live process's
// signing key set does not share this one's: then it records nothing and returns an error wrapping
// ErrKeySetMismatch. Sharing means both ways: the other process's current key (the one its new
// cookies are signed with) is among this process's keys, and this process's current key is among
// the other's, so a cookie either one mints verifies on the other. "K1 K0" beside "K0 K1" shares;
// a process that minted a key of its own beside any other does not.
//
// The read and the insert are one transaction under registryLock, so two processes booting at once
// are serialised: the second reads the first's row and is refused if the two do not share. A row
// whose heartbeat is older than LiveWindow is a process gone, and refuses nothing.
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
	id, err := insertUnlessRefused(ctx, own, instance)
	if err != nil {
		own.Close()
		return nil, err
	}
	beating, stop := context.WithCancel(context.Background())
	registration := &Registration{ID: id, pool: own, stop: stop, stopped: make(chan struct{})}
	go registration.heartbeat(beating)
	return registration, nil
}

// insertUnlessRefused is Register's transaction: the lock, the read of the live rows, the check
// against each, and the insert.
func insertUnlessRefused(ctx context.Context, pool *pgxpool.Pool, instance Instance) (string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("register this server process: %w", err)
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLock); err != nil {
		return "", fmt.Errorf("register this server process: %w", err)
	}
	live, err := Live(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("register this server process: %w", err)
	}
	for _, peer := range live {
		if err := keySetsShared(peer, instance.SigningKeys); err != nil {
			return "", err
		}
	}
	var id string
	if err := tx.QueryRow(ctx, `
		insert into server_processes (instance, commit, dashboard_build, signing_keys)
		values (gen_random_uuid(), $1, $2, $3)
		returning instance::text`,
		instance.Commit, instance.DashboardBuild, instance.SigningKeys).Scan(&id); err != nil {
		return "", fmt.Errorf("register this server process: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("register this server process: %w", err)
	}
	return id, nil
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
// its last request is answered and Deregister ends it.
func (r *Registration) heartbeat(ctx context.Context) {
	defer close(r.stopped)
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.beat(ctx)
		}
	}
}

// beat is one heartbeat, bounded by heartbeatTimeout. A failure is logged and the next tick tries
// again; a process whose heartbeats keep failing for LiveWindow drops out of Live while it still
// serves, which is the cost of a registry measured on the database's clock. A row that is gone -
// pruned after PruneAge without a heartbeat - is not written again, since that would skip the key
// check Register makes; the line says so.
func (r *Registration) beat(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `update server_processes set heartbeat_at = now() where instance = $1`, r.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Error("dispatch: server process heartbeat", "instance", r.ID, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		slog.Error("dispatch: server process heartbeat found this process's registry row gone; it no longer counts as live", "instance", r.ID)
	}
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
// Each pass runs under pruneLease (duty.Exclusive), so one process prunes per period however many
// serve the database.
func RunPrune(ctx context.Context, pool *store.Pool, leases duty.Leases) {
	interval := PruneInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			duty.Exclusive(ctx, leases, pruneLease, interval, func(ctx context.Context) {
				pruned, err := prune(ctx, pool)
				if err != nil {
					slog.Warn("dispatch: prune the server process registry", "error", err)
					return
				}
				if pruned > 0 {
					slog.Info("dispatch: pruned server processes gone without deregistering", "rows", pruned)
				}
			})
		}
	}
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

// DashboardBuild names the dashboard build served from webDistDir: lowercase hex of the first 16
// bytes of the SHA-256 of its index.html, the page every dashboard route answers. They are the
// bytes routes.servePage hashes for that page's ETag, and the same hash, so two processes report
// one build exactly when they answer one page. It is "" when there is no dashboard to serve: no
// directory, no index.html, or one this process cannot read.
func DashboardBuild(webDistDir string) string {
	if webDistDir == "" {
		return ""
	}
	page, err := os.ReadFile(filepath.Join(webDistDir, "index.html"))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:16])
}
