package processes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
// is live again within a heartbeat.
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
// live process shares when the only other row is a process gone.
func TestRegisterRefusesAKeySetALivePeerDoesNotShare(t *testing.T) {
	setInterval(t, &HeartbeatInterval, time.Hour)
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
			database := storetest.Open(t)
			peer := mustRegister(t, database, "c1", c.peer)
			if c.stale {
				ageHeartbeat(t, database, peer.ID, 16*time.Second)
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
// first's row and is refused, so exactly one of them registers. Both are held at their first
// read of the registry by a lock this test takes on the table, and released together, which is
// the moment two reads without the registry's own lock would both find the registry empty.
func TestTwoRegistrationsAtOnceWithKeysOfTheirOwnAreSerialised(t *testing.T) {
	setInterval(t, &HeartbeatInterval, time.Hour)
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

// The dashboard build is the page's ETag value: the first 16 bytes of the SHA-256 of index.html,
// in lowercase hex; "" when there is no dashboard to serve.
func TestDashboardBuildNamesTheIndexPage(t *testing.T) {
	dist := t.TempDir()
	page := []byte("<!doctype html><script src=/assets/index-abc.js></script>")
	if err := os.WriteFile(filepath.Join(dist, "index.html"), page, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(page)
	if got, want := DashboardBuild(dist), hex.EncodeToString(sum[:16]); got != want {
		t.Errorf("DashboardBuild = %q, want %q", got, want)
	}
	for name, dir := range map[string]string{
		"no setting":    "",
		"no directory":  filepath.Join(dist, "missing"),
		"no index.html": t.TempDir(),
		"index.html a dir": func() string {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, "index.html"), 0o700); err != nil {
				t.Fatal(err)
			}
			return d
		}(),
	} {
		if got := DashboardBuild(dir); got != "" {
			t.Errorf("DashboardBuild with %s = %q, want \"\"", name, got)
		}
	}
}
