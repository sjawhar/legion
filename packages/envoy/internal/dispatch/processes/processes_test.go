package processes

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// fingerprints is the fingerprint list a process holding setting (DISPATCH_SIGNING_KEY's form)
// registers with.
func fingerprints(t *testing.T, setting string) []string {
	t.Helper()
	keys, err := auth.ParseSigningKeys(setting)
	if err != nil {
		t.Fatalf("parse %q: %v", setting, err)
	}
	return keys.Fingerprints()
}

// setInterval sets one of the package's intervals for the test and puts it back after.
func setInterval(t *testing.T, interval *time.Duration, value time.Duration) {
	t.Helper()
	previous := *interval
	*interval = value
	t.Cleanup(func() { *interval = previous })
}

// quickRegistry shortens the heartbeat to 100ms and the live window to a second for the test, so
// Register's wait beside a refusing peer (LiveWindow and one HeartbeatInterval) takes about a
// second, and a peer registered in the test heartbeats ten times a window.
func quickRegistry(t *testing.T) {
	t.Helper()
	setInterval(t, &HeartbeatInterval, 100*time.Millisecond)
	setInterval(t, &LiveWindow, time.Second)
}

// register registers a process holding setting and deregisters it when the test ends, unless the
// test has.
func register(t *testing.T, database *store.Store, commit, setting string) (*Registration, error) {
	t.Helper()
	registration, err := Register(context.Background(), database.Pool, Instance{
		Commit: commit, DashboardBuild: "build-" + commit, SigningKeys: fingerprints(t, setting),
	})
	if err == nil {
		t.Cleanup(func() { _ = registration.Deregister(context.Background()) })
	}
	return registration, err
}

// mustRegister is register for a process that must be accepted.
func mustRegister(t *testing.T, database *store.Store, commit, setting string) *Registration {
	t.Helper()
	registration, err := register(t, database, commit, setting)
	if err != nil {
		t.Fatalf("register a process holding %q: %v", setting, err)
	}
	return registration
}

// ageHeartbeat sets the row's heartbeat to age before the database's now(), the clock the
// registry measures by.
func ageHeartbeat(t *testing.T, database *store.Store, id string, age time.Duration) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(),
		`update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1`, id, age.Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age process %s's heartbeat by %v: %v (%d rows)", id, age, err, tag.RowsAffected())
	}
}

// liveIDs is the instance id of every live process.
func liveIDs(t *testing.T, database *store.Store) []string {
	t.Helper()
	live, err := Live(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the live processes: %v", err)
	}
	ids := make([]string, 0, len(live))
	for _, p := range live {
		ids = append(ids, p.ID)
	}
	return ids
}

// rowExists reports whether the registry holds a row for id, live or not.
func rowExists(t *testing.T, database *store.Store, id string) bool {
	t.Helper()
	var exists bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from server_processes where instance = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("read process %s's row: %v", id, err)
	}
	return exists
}

// A registered process is live and reads back as it registered: its build, its dashboard and its
// key fingerprints, current first.
func TestRegisterRecordsTheProcessAsLive(t *testing.T) {
	database := storetest.Open(t)
	registration := mustRegister(t, database, "c1", "K1 K0")

	live, err := Live(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the live processes: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live processes = %+v, want the one registered", live)
	}
	got := live[0]
	if got.ID != registration.ID || got.Commit != "c1" || got.DashboardBuild != "build-c1" ||
		!slices.Equal(got.SigningKeys, fingerprints(t, "K1 K0")) {
		t.Errorf("live process = %+v, want id %s, commit c1, dashboard build-c1 and the fingerprints of K1 then K0", got, registration.ID)
	}
	if got.StartedAt.IsZero() || got.HeartbeatAt.Before(got.StartedAt) {
		t.Errorf("live process started %v with heartbeat %v, want both set by the database", got.StartedAt, got.HeartbeatAt)
	}
}

// liveProcess is id's row as Live reads it.
func liveProcess(t *testing.T, database *store.Store, id string) Process {
	t.Helper()
	live, err := Live(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the live processes: %v", err)
	}
	for _, p := range live {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("process %s is not live; live = %+v", id, live)
	return Process{}
}

// The four fleet columns - the schema version and creatable version the process's documents
// read and write, its embedder's identity and whether it stores files in a bucket - read back as
// the process registered them, and again after a heartbeat writes a pruned row back.
func TestTheFleetColumnsRoundTripThroughRegisterAndLive(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 50*time.Millisecond)
	database := storetest.Open(t)
	registration, err := Register(context.Background(), database.Pool, Instance{
		Commit: "c1", DashboardBuild: "build-c1", SigningKeys: fingerprints(t, "K0"),
		SchemaVersion: 3, SchemaCreatable: 2, EmbedderIdentity: "azure:example.openai.azure.com/embed", FileStore: true,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { _ = registration.Deregister(context.Background()) })
	want := func(what string, got Process) {
		t.Helper()
		if got.SchemaVersion != 3 || got.SchemaCreatable != 2 || got.EmbedderIdentity != "azure:example.openai.azure.com/embed" ||
			!got.FileStore || got.PredatesFleetColumns() {
			t.Errorf("%s = %+v, want schema version 3, creatable 2, the Azure identity and a file store", what, got)
		}
	}
	want("the registered row", liveProcess(t, database, registration.ID))

	underRegistryLock(t, database, func(tx pgx.Tx) { prunedRow(t, tx, registration.ID) })
	waitForLive(t, database, registration.ID, "a process whose row was pruned")
	want("the row written again after a prune", liveProcess(t, database, registration.ID))
}

// A row written by a build that predates the fleet columns carries none of them, so each holds its
// default: it reads as a process that predates them, with no schema numbers, no embedder identity
// and no file store.
func TestARowAtTheDefaultsPredatesTheFleetColumns(t *testing.T) {
	database := storetest.Open(t)
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, commit, dashboard_build, signing_keys)
		values (gen_random_uuid(), 'c0', 'build-c0', $1)
		returning instance::text`, fingerprints(t, "K0")).Scan(&id); err != nil {
		t.Fatalf("insert a row as a build that predates the fleet columns writes it: %v", err)
	}
	got := liveProcess(t, database, id)
	if !got.PredatesFleetColumns() || got.SchemaVersion != 0 || got.SchemaCreatable != 0 || got.EmbedderIdentity != "" || got.FileStore {
		t.Errorf("a row at the defaults = %+v, want one that predates the fleet columns: 0, 0, no identity, no file store", got)
	}
}

// Live is measured on the database's clock: a heartbeat 14 seconds before the database's now() is
// live, one 16 seconds before it is not, whatever this process's clock says.
func TestLiveCountsAHeartbeatWithinFifteenSecondsOfTheDatabaseClock(t *testing.T) {
	setInterval(t, &HeartbeatInterval, time.Hour)
	database := storetest.Open(t)
	fresh := mustRegister(t, database, "c1", "K0")
	stale := mustRegister(t, database, "c1", "K0")
	ageHeartbeat(t, database, fresh.ID, 14*time.Second)
	ageHeartbeat(t, database, stale.ID, 16*time.Second)

	if got := liveIDs(t, database); !slices.Equal(got, []string{fresh.ID}) {
		t.Errorf("live = %v, want only the process whose heartbeat is 14 s old (%s), not the 16 s one (%s)", got, fresh.ID, stale.ID)
	}
}

// The heartbeat keeps a registered process live: a row the database dates past the live window
// is live again within a heartbeat, since no live process disagrees with its key set.
func TestTheHeartbeatKeepsARegisteredProcessLive(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 100*time.Millisecond)
	database := storetest.Open(t)
	registration := mustRegister(t, database, "c1", "K0")
	ageHeartbeat(t, database, registration.ID, time.Hour)

	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(liveIDs(t, database), registration.ID) {
		if time.Now().After(deadline) {
			t.Fatal("a registered process aged past the live window was not live again within 10 s of heartbeats")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Deregister deletes the row and stops the heartbeat, so the process is neither live nor written
// again.
func TestDeregisterDeletesTheRow(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 50*time.Millisecond)
	database := storetest.Open(t)
	registration := mustRegister(t, database, "c1", "K0")
	if err := registration.Deregister(context.Background()); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if rowExists(t, database, registration.ID) {
		t.Error("the deregistered process's row is still in the registry")
	}
	if got := liveIDs(t, database); len(got) != 0 {
		t.Errorf("live = %v after the only process deregistered, want none", got)
	}
}

// The prune loop deletes the rows whose heartbeat the database dates more than ten minutes ago,
// and keeps every newer one, live or not.
func TestPruneRemovesRowsOlderThanTenMinutesAndKeepsNewerOnes(t *testing.T) {
	setInterval(t, &HeartbeatInterval, time.Hour)
	setInterval(t, &PruneInterval, 200*time.Millisecond)
	database := storetest.Open(t)
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	old := mustRegister(t, database, "c1", "K0")
	recent := mustRegister(t, database, "c1", "K0")
	live := mustRegister(t, database, "c1", "K0")
	ageHeartbeat(t, database, old.ID, 10*time.Minute+time.Second)
	ageHeartbeat(t, database, recent.ID, 9*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunPrune(ctx, database.Pool, leases)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(10 * time.Second)
	for rowExists(t, database, old.ID) {
		if time.Now().After(deadline) {
			t.Fatal("the prune loop did not delete a row whose heartbeat is ten minutes and a second old within 10 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rowExists(t, database, recent.ID) {
		t.Error("the prune loop deleted a row whose heartbeat is nine minutes old")
	}
	if !rowExists(t, database, live.ID) {
		t.Error("the prune loop deleted a live process's row")
	}
}

// A live process refuses a newcomer whose key set does not share its own both ways, each way on
// its own, and the refusal records nothing and says what to do. Key sets that overlap with the
// current keys swapped, which is the middle of a rotation, are accepted, and so is a key set no
// live process shares when the only other row is a process gone. A peer that refuses heartbeats
// throughout, so its refusal stands through Register's wait for it to lapse.
func TestRegisterRefusesAKeySetALivePeerDoesNotShare(t *testing.T) {
	for _, c := range []struct {
		name, peer, mine string
		stale            bool
		refused          bool
		says             string
	}{
		{name: "the peer's current key is not one of mine", peer: "K0 K1", mine: "K1", refused: true,
			says: "signs its cookies with a key this process does not hold"},
		{name: "my current key is not one of the peer's", peer: "K0", mine: "K1 K0", refused: true,
			says: "does not hold the key this process would sign its cookies with"},
		{name: "keys of their own", peer: "K0", mine: "K9", refused: true,
			says: "signs its cookies with a key this process does not hold"},
		{name: "the rotation's middle: K1 K0 beside K0 K1", peer: "K1 K0", mine: "K0 K1"},
		{name: "one key set", peer: "K0", mine: "K0"},
		{name: "another key, but the peer is gone", peer: "K0", mine: "K9", stale: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.stale {
				// A gone process does not heartbeat back into Live.
				setInterval(t, &HeartbeatInterval, time.Hour)
			} else {
				quickRegistry(t)
			}
			database := storetest.Open(t)
			peer := mustRegister(t, database, "c1", c.peer)
			if c.stale {
				ageHeartbeat(t, database, peer.ID, LiveWindow+time.Second)
			}
			registration, err := register(t, database, "c2", c.mine)
			if !c.refused {
				if err != nil {
					t.Fatalf("a process holding %q beside a peer holding %q was refused: %v", c.mine, c.peer, err)
				}
				if !rowExists(t, database, registration.ID) {
					t.Errorf("the accepted process has no row")
				}
				return
			}
			if !errors.Is(err, ErrKeySetMismatch) {
				t.Fatalf("a process holding %q beside a live peer holding %q registered with %v, want ErrKeySetMismatch", c.mine, c.peer, err)
			}
			for _, part := range []string{peer.ID, "commit c1", c.says, "DISPATCH_SIGNING_KEY", "add the new key second"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal %q does not say %q", err, part)
				}
			}
			var rows int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from server_processes`).Scan(&rows); err != nil {
				t.Fatalf("count the registry's rows: %v", err)
			}
			if rows != 1 {
				t.Errorf("the registry holds %d rows after the refusal, want the peer's alone", rows)
			}
		})
	}
}

// Two processes with keys of their own booting at once are serialised: the second reads the
// first's row and is refused, so exactly one of them registers; the first's heartbeat keeps it
// live through the second's wait. Both are held at their first read of the registry by a lock
// this test takes on the table, and released together, which is the moment two reads without the
// registry's own lock would both find the registry empty.
func TestTwoRegistrationsAtOnceWithKeysOfTheirOwnAreSerialised(t *testing.T) {
	quickRegistry(t)
	database := storetest.Open(t)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("open the transaction holding the registry: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(context.Background(), `lock table server_processes in access exclusive mode`); err != nil {
		t.Fatalf("hold the registry: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, setting := range []string{"K0", "K1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = register(t, database, "c1", setting)
		}()
	}
	if waiting := storetest.WaitForLockWaiters(t, gate, 2, 10*time.Second); waiting < 2 {
		t.Fatalf("%d registrations queued behind the held registry within 10 s, want both", waiting)
	}
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the registry: %v", err)
	}
	wg.Wait()

	refused := 0
	for _, err := range results {
		switch {
		case err == nil:
		case errors.Is(err, ErrKeySetMismatch):
			refused++
		default:
			t.Fatalf("a registration failed with %v, want acceptance or ErrKeySetMismatch", err)
		}
	}
	if refused != 1 {
		t.Errorf("%d of two registrations with keys of their own were refused, want exactly one: %v", refused, results)
	}
	if got := liveIDs(t, database); len(got) != 1 {
		t.Errorf("live = %v, want the one registration accepted", got)
	}
}

// A process killed without deregistering leaves a row that counts as live until LiveWindow after
// its last heartbeat, and a newcomer whose key set it does not share - a dev sign-in server
// restarted after a kill, whose key is minted per process - is refused beside it at first.
// Register waits for the row to lapse instead of refusing at once, and admits the newcomer then.
func TestARefusalBesideARowThatStopsHeartbeatingIsAdmittedOnceTheRowLapses(t *testing.T) {
	quickRegistry(t)
	database := storetest.Open(t)
	var dead string
	underRegistryLock(t, database, func(tx pgx.Tx) { dead = livePeer(t, tx, "K9") })

	registration, err := register(t, database, "c2", "K0")
	if err != nil {
		t.Fatalf("a process beside a row that stopped heartbeating was refused: %v", err)
	}
	if got := liveIDs(t, database); !slices.Equal(got, []string{registration.ID}) {
		t.Errorf("live = %v, want the admitted process %s alone, the dead row %s lapsed", got, registration.ID, dead)
	}
}

// Beside a peer that keeps heartbeating, Register's wait ends in the refusal: the peer's row stays
// live past LiveWindow and one HeartbeatInterval after the first refusal, and nothing is recorded.
func TestARefusalBesideAPeerThatKeepsHeartbeatingStandsAfterTheWait(t *testing.T) {
	quickRegistry(t)
	database := storetest.Open(t)
	peer := mustRegister(t, database, "c1", "K9")

	started := time.Now()
	_, err := register(t, database, "c2", "K0")
	waited := time.Since(started)
	if !errors.Is(err, ErrKeySetMismatch) || !strings.Contains(err.Error(), peer.ID) {
		t.Fatalf("a process beside a heartbeating peer with another key registered with %v, want ErrKeySetMismatch naming %s", err, peer.ID)
	}
	if bound := LiveWindow + HeartbeatInterval; waited < bound {
		t.Errorf("refused %v after Register began, want once it waited %v for the peer to lapse", waited, bound)
	}
	if got := liveIDs(t, database); !slices.Equal(got, []string{peer.ID}) {
		t.Errorf("live = %v after the refusal, want the peer %s alone", got, peer.ID)
	}
}

// Register's wait ends with its context: a process stopped (SIGTERM) while it waits for a refusing
// peer to lapse stops then, registered nowhere, not once the wait is over.
func TestRegistersWaitEndsWithItsContext(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 100*time.Millisecond)
	database := storetest.Open(t)
	peer := mustRegister(t, database, "c1", "K9")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Register(ctx, database.Pool, Instance{Commit: "c2", SigningKeys: fingerprints(t, "K0")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register beside a refusing peer, its context ended, returned %v; want the context's error", err)
	}
	if waited := time.Since(started); waited > LiveWindow/2 {
		t.Errorf("Register returned %v after it began, want soon after its context ended at 300ms", waited)
	}
	if got := liveIDs(t, database); !slices.Equal(got, []string{peer.ID}) {
		t.Errorf("live = %v, want the peer %s alone", got, peer.ID)
	}
}

// underRegistryLock runs change in one transaction holding the registry's lock, so no heartbeat
// lands between its statements: a row aged and a peer added there are what the next heartbeat
// finds together.
func underRegistryLock(t *testing.T, database *store.Store, change func(tx pgx.Tx)) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("open the transaction holding the registry's lock: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), `select pg_advisory_xact_lock($1)`, registryLock); err != nil {
		t.Fatalf("take the registry's lock: %v", err)
	}
	change(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit the registry change: %v", err)
	}
}

// lapse dates the row's heartbeat past the live window, as a process frozen or cut from the
// database for that long leaves it.
func lapse(t *testing.T, tx pgx.Tx, id string) {
	t.Helper()
	tag, err := tx.Exec(context.Background(),
		`update server_processes set heartbeat_at = now() - make_interval(secs => $2) where instance = $1`,
		id, (LiveWindow + time.Second).Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lapse process %s's heartbeat: %v (%d rows)", id, err, tag.RowsAffected())
	}
}

// prunedRow deletes the row, as the prune loop does once its heartbeat is PruneAge old.
func prunedRow(t *testing.T, tx pgx.Tx, id string) {
	t.Helper()
	tag, err := tx.Exec(context.Background(), `delete from server_processes where instance = $1`, id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete process %s's row: %v (%d rows)", id, err, tag.RowsAffected())
	}
}

// livePeer writes the row of a live process holding setting straight into the registry, past
// Register's check, as a process registered while ours had dropped out of Live leaves it.
func livePeer(t *testing.T, tx pgx.Tx, setting string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), `
		insert into server_processes (instance, commit, dashboard_build, signing_keys)
		values (gen_random_uuid(), 'c9', 'build-c9', $1)
		returning instance::text`, fingerprints(t, setting)).Scan(&id); err != nil {
		t.Fatalf("add a live peer holding %q: %v", setting, err)
	}
	return id
}

// waitForLive waits up to 10 s of heartbeats for id to be live again.
func waitForLive(t *testing.T, database *store.Store, id, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(liveIDs(t, database), id) {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not live again within 10 s of heartbeats", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireNotLost fails when the registration's heartbeat has reported a refusal.
func requireNotLost(t *testing.T, registration *Registration, what string) {
	t.Helper()
	select {
	case err := <-registration.Lost():
		t.Fatalf("%s reported a refusal: %v", what, err)
	default:
	}
}

// requireRefusal waits up to 10 s for the registration's heartbeat to report a refusal naming the
// peer and the rotation advice, then requires that, a few heartbeats later, nothing made the
// process live again.
func requireRefusal(t *testing.T, database *store.Store, registration *Registration, peer, what string) {
	t.Helper()
	var refusal error
	select {
	case refusal = <-registration.Lost():
	case <-time.After(10 * time.Second):
		t.Fatalf("%s reported no refusal within 10 s of heartbeats; live = %v", what, liveIDs(t, database))
	}
	if !errors.Is(refusal, ErrKeySetMismatch) {
		t.Fatalf("%s reported %v, want ErrKeySetMismatch", what, refusal)
	}
	for _, part := range []string{peer, "commit c9", "DISPATCH_SIGNING_KEY", "add the new key second"} {
		if !strings.Contains(refusal.Error(), part) {
			t.Errorf("the refusal %q does not say %q", refusal, part)
		}
	}
	time.Sleep(5 * HeartbeatInterval)
	if got := liveIDs(t, database); slices.Contains(got, registration.ID) || !slices.Equal(got, []string{peer}) {
		t.Errorf("live = %v after %s was refused, want the peer %s alone", got, what, peer)
	}
}

// A process whose heartbeat lapsed beside a process registered meanwhile with a key set it does
// not share is refused by its next heartbeat, which reports it (Lost) and records nothing; beside a
// process that shares its keys it rejoins, and its row is the one it registered.
func TestALapsedHeartbeatChecksTheKeysAgainBeforeTheProcessCountsAsLive(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 50*time.Millisecond)
	for _, c := range []struct {
		name, peer string
		refused    bool
	}{
		{name: "beside a peer with a key of its own", peer: "K9", refused: true},
		{name: "beside a peer that does not hold my current key", peer: "K0", refused: true},
		{name: "beside a peer with the same key set", peer: "K1 K0"},
		{name: "beside a peer in the rotation's middle", peer: "K0 K1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			database := storetest.Open(t)
			registration := mustRegister(t, database, "c1", "K1 K0")
			var peer string
			underRegistryLock(t, database, func(tx pgx.Tx) {
				lapse(t, tx, registration.ID)
				peer = livePeer(t, tx, c.peer)
			})
			if c.refused {
				requireRefusal(t, database, registration, peer, "a lapsed process beside a peer holding "+c.peer)
				if !rowExists(t, database, registration.ID) {
					t.Error("the refused process's lapsed row is gone; the refusal must record nothing")
				}
				return
			}
			waitForLive(t, database, registration.ID, "a lapsed process beside a peer holding "+c.peer)
			requireNotLost(t, registration, "a lapsed process beside a peer holding "+c.peer)
			live, err := Live(context.Background(), database.Pool)
			if err != nil {
				t.Fatalf("read the live processes: %v", err)
			}
			for _, p := range live {
				if p.ID == registration.ID && (p.Commit != "c1" || !slices.Equal(p.SigningKeys, fingerprints(t, "K1 K0"))) {
					t.Errorf("the rejoined row = %+v, want commit c1 and the fingerprints of K1 then K0", p)
				}
			}
		})
	}
}

// A process whose row was pruned is written again by its next heartbeat, as it registered, when no
// live process disagrees with its key set; beside one that does, the heartbeat reports the refusal
// and writes nothing.
func TestAPrunedRowIsWrittenAgainUnlessALivePeerDoesNotShareTheKeys(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 50*time.Millisecond)
	t.Run("no live peer disagrees", func(t *testing.T) {
		database := storetest.Open(t)
		registration := mustRegister(t, database, "c1", "K1 K0")
		underRegistryLock(t, database, func(tx pgx.Tx) {
			prunedRow(t, tx, registration.ID)
			livePeer(t, tx, "K0 K1")
		})
		waitForLive(t, database, registration.ID, "a process whose row was pruned")
		requireNotLost(t, registration, "a process whose row was pruned")
		live, err := Live(context.Background(), database.Pool)
		if err != nil {
			t.Fatalf("read the live processes: %v", err)
		}
		for _, p := range live {
			if p.ID == registration.ID && (p.Commit != "c1" || p.DashboardBuild != "build-c1" ||
				!slices.Equal(p.SigningKeys, fingerprints(t, "K1 K0"))) {
				t.Errorf("the row written again = %+v, want commit c1, dashboard build-c1 and the fingerprints of K1 then K0", p)
			}
		}
	})
	t.Run("a live peer does not share the keys", func(t *testing.T) {
		database := storetest.Open(t)
		registration := mustRegister(t, database, "c1", "K1 K0")
		var peer string
		underRegistryLock(t, database, func(tx pgx.Tx) {
			prunedRow(t, tx, registration.ID)
			peer = livePeer(t, tx, "K9")
		})
		requireRefusal(t, database, registration, peer, "a process whose row was pruned, beside a peer with a key of its own")
		if rowExists(t, database, registration.ID) {
			t.Error("the refused process's row was written again")
		}
	})
}

// A live row's heartbeat stamps it and checks nothing: the keys were checked when the process
// registered, and every process registered since checked its own against them. A peer that
// disagrees, written past Register's check, refuses nothing while this process's row stays live.
func TestALiveRowsHeartbeatChecksNoPeer(t *testing.T) {
	setInterval(t, &HeartbeatInterval, 50*time.Millisecond)
	database := storetest.Open(t)
	registration := mustRegister(t, database, "c1", "K1 K0")
	underRegistryLock(t, database, func(tx pgx.Tx) { livePeer(t, tx, "K9") })

	heartbeat := func() time.Time {
		var at time.Time
		if err := database.Pool.QueryRow(context.Background(),
			`select heartbeat_at from server_processes where instance = $1`, registration.ID).Scan(&at); err != nil {
			t.Fatalf("read process %s's heartbeat: %v", registration.ID, err)
		}
		return at
	}
	// Three heartbeats stamped after the peer was written.
	last := heartbeat()
	deadline := time.Now().Add(10 * time.Second)
	for stamped := 0; stamped < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("the live process's heartbeat stamped its row %d times in 10 s, want 3", stamped)
		}
		if at := heartbeat(); at.After(last) {
			last = at
			stamped++
		}
		time.Sleep(10 * time.Millisecond)
	}
	requireNotLost(t, registration, "a live process beside a disagreeing peer")
}
