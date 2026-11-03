package legion

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// fakeDaemon is a Legion daemon stand-in whose state document and status the test moves between
// passes, counting the reads it served.
type fakeDaemon struct {
	*httptest.Server
	mu     sync.Mutex
	body   string
	status int
	reads  int
}

func newFakeDaemon(t *testing.T, body string) *fakeDaemon {
	t.Helper()
	daemon := &fakeDaemon{body: body, status: http.StatusOK}
	daemon.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatePath {
			http.NotFound(w, r)
			return
		}
		daemon.mu.Lock()
		body, status := daemon.body, daemon.status
		daemon.reads++
		daemon.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(daemon.Close)
	return daemon
}

func (d *fakeDaemon) serves(body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.body, d.status = body, http.StatusOK
}

func (d *fakeDaemon) refuses(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.body, d.status = `{"error":"the daemon is restarting"}`, status
}

func (d *fakeDaemon) served() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads
}

// stateOf is a one-issue state document: the issue in phase, with planner or implementer holding
// the only claim.
func stateOf(phase, role string) string {
	return fmt.Sprintf(`{"issues":{"INFRA-1":{"key":"INFRA-1","phase":%q,"status":"doing",
		"slot":{"index":0,"admittedAt":"2026-11-03T10:00:00Z"},
		"workers":{%q:{"claim":{"session":"s-1","state":"working"},"rounds":1}}}}}`, phase, role)
}

// configured is a store with project INFRA naming daemon, and a sampler on it.
func configured(t *testing.T, daemonURL string) (*store.Store, *Sampler) {
	t.Helper()
	database := storetest.Open(t)
	seedLegionProject(t, database, "INFRA", daemonURL)
	return database, NewSampler(database.Pool)
}

func seedLegionProject(t *testing.T, database *store.Store, project, daemonURL string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`insert into projects (key, name) values ($1, $1) on conflict do nothing`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := PutSettings(context.Background(), database.Pool, project, daemonURL,
		model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("set the Legion setting of %s: %v", project, err)
	}
}

// observations is every observation of project, oldest first, as "<issue>/<field>=<value>".
func observations(t *testing.T, database *store.Store, project string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select issue_key, field, value::text from legion_observations where project = $1 order by observed_at, field
	`, project)
	if err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	defer rows.Close()
	var listed []string
	for rows.Next() {
		var issue, field, value string
		if err := rows.Scan(&issue, &field, &value); err != nil {
			t.Fatalf("scan an observation: %v", err)
		}
		listed = append(listed, issue+"/"+field+"="+value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	return listed
}

// readRow is one legion_reads row.
type readRow struct {
	ok    bool
	error *string
	bytes int
}

func reads(t *testing.T, database *store.Store, project string) []readRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select ok, error, bytes from legion_reads where project = $1 order by id
	`, project)
	if err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	defer rows.Close()
	var listed []readRow
	for rows.Next() {
		var row readRow
		if err := rows.Scan(&row.ok, &row.error, &row.bytes); err != nil {
			t.Fatalf("scan a read: %v", err)
		}
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the reads: %v", err)
	}
	return listed
}

// TestAChangedValueWritesOneRowAndAnUnchangedValueWritesNone is the sampler's whole rule: a pass
// writes a row for each value that moved and nothing for the rest, a field the issue carries none
// of writes no first null, and a role the read does not name keeps the observation it has.
func TestAChangedValueWritesOneRowAndAnUnchangedValueWritesNone(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	first := observations(t, database, "INFRA")
	want := []string{
		`INFRA-1/claim.planner="working"`,
		`INFRA-1/phase="planning"`,
		`INFRA-1/slot=0`,
		`INFRA-1/status="doing"`,
	}
	if len(first) != len(want) {
		t.Fatalf("the first pass wrote %v, want %v: a field the issue carries none of writes no first row", first, want)
	}
	for index, row := range want {
		if first[index] != row {
			t.Errorf("observation %d = %s, want %s", index, first[index], row)
		}
	}

	// Nothing moved, so nothing is written.
	sampler.Pass(t.Context())
	if again := observations(t, database, "INFRA"); len(again) != len(first) {
		t.Fatalf("a pass over an unchanged state wrote %d more rows: %v", len(again)-len(first), again)
	}

	// The phase moves and the planner hands over to the implementer: the phase and the new
	// claim are written, the status and the slot are not, and the planner's own last
	// observation stands, since a role this read does not name has said nothing.
	daemon.serves(stateOf("implementing", "implementer"))
	sampler.Pass(t.Context())
	moved := observations(t, database, "INFRA")[len(first):]
	if len(moved) != 2 {
		t.Fatalf("the third pass wrote %v, want the phase and the implementer's claim", moved)
	}
	if moved[0] != `INFRA-1/claim.implementer="working"` || moved[1] != `INFRA-1/phase="implementing"` {
		t.Errorf("the third pass wrote %v, want the implementer's claim and the new phase", moved)
	}

	if served := reads(t, database, "INFRA"); len(served) != 3 {
		t.Fatalf("legion_reads = %v, want one row per pass", served)
	} else {
		for index, row := range served {
			if !row.ok || row.error != nil || row.bytes == 0 {
				t.Errorf("read %d = %+v, want a successful read with its bytes", index, row)
			}
		}
	}
}

// TestAFailedReadRecordsTheFailureAndLeavesTheObservationsAlone: a daemon that refuses, and one
// that answers something other than the route's JSON, each leave the last good observations where
// they are and show up as the last failure.
func TestAFailedReadRecordsTheFailureAndLeavesTheObservationsAlone(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	good := observations(t, database, "INFRA")
	if len(good) == 0 {
		t.Fatal("the first pass wrote nothing")
	}

	daemon.refuses(http.StatusServiceUnavailable)
	sampler.Pass(t.Context())
	daemon.serves(`{"issues": {`)
	sampler.Pass(t.Context())

	if after := observations(t, database, "INFRA"); len(after) != len(good) {
		t.Fatalf("a failed read changed the observations: %v, want the %d the good read left", after, len(good))
	}
	served := reads(t, database, "INFRA")
	if len(served) != 3 {
		t.Fatalf("legion_reads = %v, want one row per pass, failures included", served)
	}
	for index, row := range served[1:] {
		if row.ok || row.error == nil || *row.error == "" {
			t.Errorf("failed read %d = %+v, want ok false and the failure named", index+1, row)
		}
	}
	if served[2].bytes == 0 {
		t.Errorf("the unparseable read's bytes = 0, want what the daemon sent")
	}
}

// TestTwoProcessesStoreEachChangeOnce is acceptance A4: production runs two envoy-dispatch
// processes against one Postgres, each with a pool of its own (storetest.SecondProcess), and the
// `legion-state` lease is what makes one of them serve a period. Both tick for the same moment;
// one pass runs, one daemon read is made, and each change is stored once.
func TestTwoProcessesStoreEachChangeOnce(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, first := configured(t, daemon.URL)
	second := storetest.SecondProcess(t, database)
	other := NewSampler(second.Pool)

	firstLeases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the first process's lease pool: %v", err)
	}
	secondLeases, err := second.Pool.Leases()
	if err != nil {
		t.Fatalf("open the second process's lease pool: %v", err)
	}

	// The two ticks a RunLeased loop makes for one period, with the moment each was due: the
	// value the ticker delivered, which is the same on both processes.
	due := time.Now()
	ranFirst := duty.Exclusive(t.Context(), firstLeases, SampleLease, due, SampleInterval, first.Pass)
	ranSecond := duty.Exclusive(t.Context(), secondLeases, SampleLease, due, SampleInterval, other.Pass)

	if ranFirst == ranSecond {
		t.Fatalf("first ran = %v, second ran = %v: exactly one process serves a period", ranFirst, ranSecond)
	}
	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads for one period, want one", served)
	}
	if rows := reads(t, database, "INFRA"); len(rows) != 1 {
		t.Errorf("legion_reads = %v, want one row for one period", rows)
	}
	stored := observations(t, database, "INFRA")
	if len(stored) != 4 {
		t.Fatalf("observations = %v, want each change stored once", stored)
	}

	// The next period, with the phase moved: whichever process serves it stores that one change
	// once, and the other stores nothing.
	daemon.serves(stateOf("implementing", "planner"))
	next := due.Add(SampleInterval)
	duty.Exclusive(t.Context(), secondLeases, SampleLease, next, SampleInterval, other.Pass)
	duty.Exclusive(t.Context(), firstLeases, SampleLease, next, SampleInterval, first.Pass)
	if moved := observations(t, database, "INFRA")[len(stored):]; len(moved) != 1 || moved[0] != `INFRA-1/phase="implementing"` {
		t.Fatalf("the second period wrote %v, want the one change once", moved)
	}
}

// TestAProjectWithNoSettingIsNeverRead: the sampler reads what legion_settings names and nothing
// else, and a setting removed stops its reads while what was observed stays.
func TestAProjectWithNoSettingIsNeverRead(t *testing.T) {
	daemon := newFakeDaemon(t, stateOf("planning", "planner"))
	database, sampler := configured(t, daemon.URL)

	sampler.Pass(t.Context())
	before := observations(t, database, "INFRA")

	deleted, err := DeleteSettings(t.Context(), database.Pool, "INFRA")
	if err != nil || !deleted {
		t.Fatalf("delete the setting: deleted=%v err=%v", deleted, err)
	}
	sampler.Pass(t.Context())

	if served := daemon.served(); served != 1 {
		t.Errorf("the daemon served %d reads, want only the one made while it was configured", served)
	}
	if after := observations(t, database, "INFRA"); len(after) != len(before) {
		t.Errorf("observations = %v, want the %d the configured read left", after, len(before))
	}
	if rows := reads(t, database, "INFRA"); len(rows) != 1 {
		t.Errorf("legion_reads = %v, want the one read", rows)
	}
}
