//go:build clonecost

// TEMPORARY. The clone-cost measurement dispatch://INFRA-1075 asks a real envoy-go runner for:
// what one api test's floor is made of, what a clone and a drop cost under concurrency on each
// create-database strategy, how big the migrated template is, and what moving the drop off a
// test's critical path is worth. It is one file behind one build tag, run by one TEMPORARY step
// of the envoy-go job, and it comes out with that step before the pull request's final pair of
// measurement runs. Nothing outside this file knows it exists: no other file names the tag.
//
// It lives in package api because an api test's floor is what it measures. It builds the handler
// a test builds (newTestServer) and makes the requests the simplest tests make
// (createReferenceAPIProject, createReferenceAPIIssue, one GET), in the order a test makes them,
// with the test's own cleanups timed where they run. The clone and the drop are the statements
// storetest issues, reissued here against a template of this measurement's own, so each part can
// be timed alone without changing the harness every other test runs on.
//
// Three CPU numbers are reported, because the Postgres the work runs on is a container of its
// own: `self` is this process's getrusage, which holds only the Go client; `machine` is the
// /proc/stat busy delta over every core, which holds the server too wherever the server shares
// this kernel (CI's service container does); `server` is backends seen running rather than
// waiting in pg_stat_activity, sampled, which holds the server wherever it can be reached at all.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// measurePrefix names every database this measurement makes, so its cleanup can drop what a
	// killed run left behind.
	measurePrefix = "clonecost_"
	// floorRuns is how many times the floor table repeats a whole test's sequence.
	floorRuns = 10
	// harnessRuns is how many times the floor table calls the real storetest.Open, as a check on
	// the clone and pool-open rows it measures itself.
	harnessRuns = 6
	// burstRounds is how many times each concurrency cell repeats its burst.
	burstRounds = 3
	// sampleInterval is how often pg_stat_activity is read while a burst runs. Each sample is one
	// small query on a connection of its own.
	sampleInterval = 15 * time.Millisecond
	// measurePoolSize is storetest's testPoolSize, the pool every test store is opened with.
	measurePoolSize = 16
	// simTests is how many tests the drop simulation runs, at apiTestParallelism at a time.
	simTests = 96
	// tick is USER_HZ on linux/amd64: the resolution of every /proc/stat counter.
	tick = 10 * time.Millisecond
)

// burstSizes are the cells of the concurrency table. There is one strategy, file_copy: the
// repository requires it of every database a Go file creates (internal/testpg's check), and the
// wal_log arm this measurement once carried has answered its question - on a non-durable server
// file_copy amortised to 139-141 ms a clone against wal_log's 208-214, both CPU-bound
// (dispatch://INFRA-1075).
var (
	burstSizes = []int{8, 24}
)

// cpuReading is the CPU this process and this machine have burned so far.
type cpuReading struct {
	self    time.Duration
	machine time.Duration
	iowait  time.Duration
}

func readCPU() cpuReading {
	reading := cpuReading{}
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err == nil {
		reading.self = time.Duration(usage.Utime.Nano()) + time.Duration(usage.Stime.Nano())
	}
	reading.machine, reading.iowait = machineCPU()
	return reading
}

// machineCPU is /proc/stat's first line: every core's busy time and its iowait. Under a sandbox
// that hides the host's other containers it holds only what this sandbox ran, which is why the
// table also carries the server-side sampling.
func machineCPU() (busy, iowait time.Duration) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] != "cpu" {
			continue
		}
		var ticks [8]int64
		for i := range ticks {
			ticks[i], _ = strconv.ParseInt(fields[i+1], 10, 64)
		}
		// user, nice, system, irq, softirq, steal; idle and iowait are not busy.
		busy = time.Duration(ticks[0]+ticks[1]+ticks[2]+ticks[5]+ticks[6]+ticks[7]) * tick
		return busy, time.Duration(ticks[4]) * tick
	}
	return 0, 0
}

func (c cpuReading) since(before cpuReading) cpuReading {
	return cpuReading{self: c.self - before.self, machine: c.machine - before.machine, iowait: c.iowait - before.iowait}
}

// sampler counts, every sampleInterval, the backends running a statement this measurement issued,
// by what each is waiting on. A backend with no wait event is on the CPU.
type sampler struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	counts  map[string]int
	samples int
}

func startSampler(t *testing.T, dsn, like string) *sampler {
	t.Helper()
	s := &sampler{stop: make(chan struct{}), done: make(chan struct{}), counts: map[string]int{}}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the wait-event sampler's connection: %v", err)
	}
	go func() {
		defer close(s.done)
		defer conn.Close(context.Background())
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
			rows, err := conn.Query(context.Background(),
				`select coalesce(wait_event_type, 'CPU'), coalesce(wait_event, '')
				   from pg_stat_activity
				  where state = 'active' and pid <> pg_backend_pid() and query like $1`, like)
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.samples++
			for rows.Next() {
				var kind, event string
				if err := rows.Scan(&kind, &event); err != nil {
					continue
				}
				key := kind
				if event != "" {
					key += ":" + event
				}
				s.counts[key]++
			}
			s.mu.Unlock()
			rows.Close()
		}
	}()
	return s
}

// finish stops the sampler and answers the backend-seconds it saw, by wait event, and the total.
func (s *sampler) finish() (map[string]time.Duration, time.Duration, int) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]time.Duration, len(s.counts))
	total := time.Duration(0)
	for key, count := range s.counts {
		seen[key] = time.Duration(count) * sampleInterval
		total += time.Duration(count) * sampleInterval
	}
	return seen, total, s.samples
}

// top renders the busiest wait events of a burst as "key share%", busiest first.
func top(seen map[string]time.Duration, total time.Duration, n int) string {
	if total == 0 {
		return "nothing sampled"
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if seen[keys[i]] != seen[keys[j]] {
			return seen[keys[i]] > seen[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", key, 100*float64(seen[key])/float64(total)))
	}
	return strings.Join(parts, ", ")
}

// limitOf names what bounds a burst. Two things decide it: how much of the host's cores the work
// burned (busy, as a fraction of cores × wall), which is the only direct evidence of CPU, and
// what the server's own backends were doing. A backend active with no wait event is running or
// waiting for a core, so a high "no wait" share with an idle host means the client, not the
// server.
func limitOf(seen map[string]time.Duration, total time.Duration, busy float64) string {
	if total == 0 {
		return "unknown (nothing sampled)"
	}
	best, bestAt := "", time.Duration(0)
	for key, at := range seen {
		if at > bestAt || (at == bestAt && key < best) {
			best, bestAt = key, at
		}
	}
	share := 100 * float64(bestAt) / float64(total)
	what := ""
	switch {
	case best == "CPU":
		what = fmt.Sprintf("backends running, none waiting (%.0f%%)", share)
	case strings.HasPrefix(best, "IO:"):
		what = fmt.Sprintf("I/O (%s, %.0f%%)", best, share)
	case strings.HasPrefix(best, "Lock:"), strings.HasPrefix(best, "LWLock:"), strings.HasPrefix(best, "BufferPin:"):
		what = fmt.Sprintf("the server's own locks (%s, %.0f%%)", best, share)
	case strings.HasPrefix(best, "IPC:"):
		what = fmt.Sprintf("the server's own machinery (%s, %.0f%%)", best, share)
	default:
		what = fmt.Sprintf("%s (%.0f%%)", best, share)
	}
	switch {
	case busy >= 0.8:
		return "CPU: " + what
	case busy <= 0:
		return what + ", host CPU not readable here"
	default:
		return what + fmt.Sprintf(", host only %.0f%% busy", 100*busy)
	}
}

// measure holds what every table of this measurement shares.
type measure struct {
	dsn      string
	server   url.URL
	admin    *pgxpool.Pool
	template string
	suffix   string
	next     atomic.Int64
}

func (m *measure) url(database string) string {
	server := m.server
	server.Path = "/" + database
	return server.String()
}

func (m *measure) name(kind string) string {
	return fmt.Sprintf("%s%s_%s_%d", measurePrefix, kind, m.suffix, m.next.Add(1))
}

func (m *measure) exec(t *testing.T, statement string) {
	t.Helper()
	if _, err := m.admin.Exec(context.Background(), statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// try is exec for a goroutine, where t.Fatalf is not allowed.
func (m *measure) try(statement string) error {
	if _, err := m.admin.Exec(context.Background(), statement); err != nil {
		return fmt.Errorf("%s: %w", statement, err)
	}
	return nil
}

// cloneOf opens a clone of template as name. The strategy is `strategy file_copy` and nothing
// else: internal/testpg's check reads every Go file of the module for a creation without that
// clause, and the wal_log arm this measurement once ran against it is answered - on a
// non-durable server file_copy amortised to 139-141 ms a clone where wal_log took 208-214, both
// CPU-bound, so the cheaper strategy is the one the repository already requires
// (dispatch://INFRA-1075).
func cloneOf(name, template string) string {
	return "create database " + name + " template " + template + " strategy file_copy"
}

func dropOf(name string) string {
	return "drop database " + name + " with (force)"
}

// ---------------------------------------------------------------- tables

// row is one printed line; cells are already formatted.
type table struct {
	title   string
	caption string
	head    []string
	rows    [][]string
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

// print writes the table as a Markdown table, which pastes into a report unchanged.
func (t *table) print() {
	width := make([]int, len(t.head))
	for i, cell := range t.head {
		width[i] = len(cell)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(width) && len(cell) > width[i] {
				width[i] = len(cell)
			}
		}
	}
	line := func(cells []string) string {
		parts := make([]string, len(width))
		for i := range width {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			parts[i] = cell + strings.Repeat(" ", width[i]-len(cell))
		}
		return "| " + strings.Join(parts, " | ") + " |"
	}
	fmt.Printf("\n### %s\n\n", t.title)
	if t.caption != "" {
		fmt.Printf("%s\n\n", t.caption)
	}
	fmt.Println(line(t.head))
	dashes := make([]string, len(width))
	for i := range width {
		dashes[i] = strings.Repeat("-", width[i])
	}
	fmt.Println(line(dashes))
	for _, row := range t.rows {
		fmt.Println(line(row))
	}
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

func secs(d time.Duration) string { return fmt.Sprintf("%.2f", d.Seconds()) }

// ---------------------------------------------------------------- the measurement

func TestCloneCostMeasurement(t *testing.T) {
	raw := os.Getenv(storetest.DatabaseURLEnv)
	if raw == "" {
		t.Skip(storetest.DatabaseURLEnv + " must be set to run the clone-cost measurement")
	}
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", storetest.DatabaseURLEnv, err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("random name: %v", err)
	}
	m := &measure{dsn: raw, server: *server, suffix: hex.EncodeToString(random[:])}
	m.server.Path = "/postgres"
	m.dsn = m.server.String()

	ctx := context.Background()
	config, err := pgxpool.ParseConfig(m.dsn)
	if err != nil {
		t.Fatalf("parse the admin URL: %v", err)
	}
	// One connection per clone or drop of the widest burst, and headroom for the sampler and the
	// simulation's workers.
	config.MaxConns = 40
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the admin pool: %v", err)
	}
	m.admin = admin
	defer admin.Close()
	defer m.dropEverything(t)

	started := time.Now()
	fmt.Printf("\n## Clone cost on this job's Postgres (dispatch://INFRA-1075, TEMPORARY)\n\n")
	m.describeServer(t)

	m.template = m.name("template")
	m.exec(t, "create database "+m.template+" strategy file_copy")
	templateStore, err := store.Open(ctx, m.url(m.template), store.WithMaxConns(measurePoolSize))
	if err != nil {
		t.Fatalf("open the template: %v", err)
	}
	migrateStart := time.Now()
	if err := templateStore.Migrate(ctx); err != nil {
		templateStore.Pool.Close()
		t.Fatalf("migrate the template: %v", err)
	}
	migrateWall := time.Since(migrateStart)
	m.templateTable(t, templateStore, migrateWall)
	templateStore.Pool.Close()
	// Postgres refuses to clone a database a session is connected to, as storetest's own template
	// is closed for the same reason.
	m.exec(t, "alter database "+m.template+" with allow_connections false")

	section := func(name string, build func(*testing.T)) {
		start := time.Now()
		build(t)
		fmt.Printf("\n(%s took %s s of this step's wall.)\n", name, secs(time.Since(start)))
	}
	section("the floor table", m.floorTable)
	section("the concurrency tables", m.concurrencyTables)
	section("the drop-path table", m.dropPathTable)

	fmt.Printf("\nWhole measurement: %s s of wall.\n", secs(time.Since(started)))
}

// describeServer prints the server every number below was measured on.
func (m *measure) describeServer(t *testing.T) {
	t.Helper()
	var version, fsync, fullPage, commit, maxConns string
	row := m.admin.QueryRow(context.Background(),
		`select current_setting('server_version'), current_setting('fsync'), current_setting('full_page_writes'),
		        current_setting('synchronous_commit'), current_setting('max_connections')`)
	if err := row.Scan(&version, &fsync, &fullPage, &commit, &maxConns); err != nil {
		t.Fatalf("read the server's settings: %v", err)
	}
	fmt.Printf("Postgres %s, fsync=%s full_page_writes=%s synchronous_commit=%s max_connections=%s; %d cores on this host.\n",
		version, fsync, fullPage, commit, maxConns, runtimeCores())
}

func runtimeCores() int {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	cores := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "cpu") && !strings.HasPrefix(line, "cpu ") {
			cores++
		}
	}
	return cores
}

// ---------------------------------------------------------------- (c) the template

func (m *measure) templateTable(t *testing.T, templateStore *store.Store, migrateWall time.Duration) {
	t.Helper()
	ctx := context.Background()
	var size int64
	if err := m.admin.QueryRow(ctx, "select pg_database_size($1)", m.template).Scan(&size); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}
	kinds := map[string]int{}
	rows, err := templateStore.Pool.Query(ctx,
		`select c.relkind, count(*) from pg_class c join pg_namespace n on n.oid = c.relnamespace
		  where n.nspname not in ('pg_catalog', 'information_schema', 'pg_toast') group by 1`)
	if err != nil {
		t.Fatalf("count the template's relations: %v", err)
	}
	total := 0
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatalf("scan a relation count: %v", err)
		}
		kinds[kind] = count
		total += count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("count the template's relations: %v", err)
	}
	var catalog int
	if err := templateStore.Pool.QueryRow(ctx, "select count(*) from pg_class").Scan(&catalog); err != nil {
		t.Fatalf("count every relation: %v", err)
	}

	names := map[string]string{"r": "ordinary tables", "i": "indexes", "S": "sequences", "v": "views",
		"m": "materialized views", "t": "TOAST tables", "p": "partitioned tables", "I": "partitioned indexes", "c": "composite types", "f": "foreign tables"}
	table := &table{
		title:   "(c) The migrated template",
		caption: "The same migration set storetest's template carries, migrated here by the same store.Migrate.",
		head:    []string{"measure", "value"},
	}
	table.add("pg_database_size", fmt.Sprintf("%d bytes (%.2f MiB)", size, float64(size)/(1024*1024)))
	table.add("relations outside the system schemas", strconv.Itoa(total))
	keys := make([]string, 0, len(kinds))
	for kind := range kinds {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	for _, kind := range keys {
		label := names[kind]
		if label == "" {
			label = "relkind " + kind
		}
		table.add("  of which "+label, strconv.Itoa(kinds[kind]))
	}
	table.add("every relation in pg_class, system included", strconv.Itoa(catalog))
	table.add("store.Migrate over an empty database, once per package", ms(migrateWall)+" ms")
	table.print()
}

// ---------------------------------------------------------------- (a) one test's floor

// floorParts accumulates, over floorRuns whole tests, what each part of a test cost.
type floorParts struct {
	order []string
	wall  map[string]time.Duration
	cpu   map[string]cpuReading
	count map[string]int
	path  map[string]bool
}

func newFloorParts() *floorParts {
	return &floorParts{wall: map[string]time.Duration{}, cpu: map[string]cpuReading{}, count: map[string]int{}, path: map[string]bool{}}
}

func (f *floorParts) record(name string, onPath bool, wall time.Duration, cpu cpuReading) {
	if _, seen := f.wall[name]; !seen {
		f.order = append(f.order, name)
	}
	f.wall[name] += wall
	spent := f.cpu[name]
	spent.self += cpu.self
	spent.machine += cpu.machine
	f.cpu[name] = spent
	f.count[name]++
	f.path[name] = onPath
}

func (f *floorParts) time(name string, onPath bool, work func()) {
	before := readCPU()
	start := time.Now()
	work()
	wall := time.Since(start)
	f.record(name, onPath, wall, readCPU().since(before))
}

func (m *measure) floorTable(t *testing.T) {
	t.Helper()
	// One whole test and one storetest.Open are run and thrown away first: the first clone of a
	// run pays for a cold buffer cache, and the first storetest.Open pays for the template the
	// harness migrates once per package, neither of which a per-test mean should carry.
	warm := newFloorParts()
	m.floorRun(t, "warmup", warm)
	parts := newFloorParts()
	for i := range floorRuns {
		m.floorRun(t, fmt.Sprintf("floor%02d", i), parts)
	}
	for i := range harnessRuns + 1 {
		into, label := parts, "storetest.Open(t), the real harness (clone + pool, for comparison)"
		if i == 0 {
			into = warm
		}
		t.Run(fmt.Sprintf("harness%02d", i), func(t *testing.T) {
			before := readCPU()
			start := time.Now()
			storetest.Open(t)
			wall := time.Since(start)
			into.record(label, false, wall, readCPU().since(before))
		})
	}

	out := &table{
		title: "(a) One api test's floor, split, averaged over " + strconv.Itoa(floorRuns) + " whole tests",
		caption: "Each part timed where it happens in a test: clone, pool, handler, body, the test's own cleanups, drop. " +
			"`client CPU` is this process (getrusage); `host CPU` is /proc/stat over every core, which holds the Postgres " +
			"container only where it shares this kernel. Rows marked *no* are not what a test does today.",
		head: []string{"part of a test", "mean wall (ms)", "mean client CPU (ms)", "mean host CPU (ms)", "on a test's path"},
	}
	var wallTotal, selfTotal, machineTotal time.Duration
	for _, name := range parts.order {
		runs := time.Duration(parts.count[name])
		onPath := "yes"
		if !parts.path[name] {
			onPath = "no"
		}
		out.add(name, ms(parts.wall[name]/runs), ms(parts.cpu[name].self/runs), ms(parts.cpu[name].machine/runs), onPath)
		if parts.path[name] {
			wallTotal += parts.wall[name] / runs
			selfTotal += parts.cpu[name].self / runs
			machineTotal += parts.cpu[name].machine / runs
		}
	}
	out.add("**a whole test, serial, on-path parts only**", ms(wallTotal), ms(selfTotal), ms(machineTotal), "yes")
	out.print()
	fmt.Printf("\nstoretest.Open makes no migration and no schema check per test: the clone arrives migrated and " +
		"store.Open issues one Ping, which is the pool row above. The row marked *no* prices the check the harness " +
		"does not make, so a reader can see what adding one would cost.\n")
}

// floorRun runs one whole test the way an api test runs, timing each part into parts.
func (m *measure) floorRun(t *testing.T, label string, parts *floorParts) {
	t.Helper()
	ctx := context.Background()
	name := m.name("floor")
	var database *store.Store
	var cleanupStart, cleanupEnd time.Time
	var cleanupCPU cpuReading

	t.Run(label, func(t *testing.T) {
		// Registered first, so it runs last: the end of the test's own cleanups.
		t.Cleanup(func() { cleanupEnd = time.Now(); cleanupCPU = readCPU().since(cleanupCPU) })
		parts.time("clone (create database … template … strategy file_copy)", true, func() {
			m.exec(t, cloneOf(name, m.template))
		})
		parts.time("open the pool (store.Open: pgxpool + one Ping)", true, func() {
			var err error
			database, err = store.Open(ctx, m.url(name), store.WithMaxConns(measurePoolSize))
			if err != nil {
				t.Fatalf("open the clone: %v", err)
			}
		})
		parts.time("a per-test migration check, were one made (store.Migrate over the migrated clone)", false, func() {
			if err := database.Migrate(ctx); err != nil {
				t.Fatalf("re-migrate the clone: %v", err)
			}
		})
		var handler http.Handler
		parts.time("build the service and handler (newTestServer: docs.New, seedPeople, NewDeps, register)", true, func() {
			handler, _, _ = newTestServer(t, testServerOptions{store: database})
		})
		parts.time("the body (POST /projects, POST /issues, GET /issues/{key})", true, func() {
			createReferenceAPIProject(t, handler, "CORE")
			issue := createReferenceAPIIssue(t, handler, "CORE")
			if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("read the issue back: status=%d body=%s", response.Code, response.Body.String())
			}
		})
		// Registered last, so it runs first: the start of the test's own cleanups.
		t.Cleanup(func() { cleanupStart = time.Now(); cleanupCPU = readCPU() })
	})
	parts.record("the test's own cleanups (the document service's shutdown)", true, cleanupEnd.Sub(cleanupStart), cleanupCPU)
	parts.time("close the pool", true, func() { database.Pool.Close() })
	parts.time("drop (drop database … with (force))", true, func() { m.exec(t, dropOf(name)) })
}

// ---------------------------------------------------------------- (b) clone and drop concurrently

// cell is one measured burst of concurrent statements.
type cell struct {
	wall    time.Duration
	latency time.Duration
	cpu     cpuReading
	ops     int
	seen    map[string]time.Duration
	backend time.Duration
	onCPU   time.Duration
	samples int
}

// burst runs work n times at once, with pg_stat_activity sampled beside it. work answers the
// error of its statement rather than failing the test, because it runs off the test's goroutine.
func (m *measure) burst(t *testing.T, like string, n int, work func(int) error) cell {
	t.Helper()
	sampler := startSampler(t, m.dsn, like)
	var latency atomic.Int64
	before := readCPU()
	start := time.Now()
	var group sync.WaitGroup
	for i := range n {
		group.Go(func() {
			one := time.Now()
			err := work(i)
			latency.Add(int64(time.Since(one)))
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	wall := time.Since(start)
	cpu := readCPU().since(before)
	seen, backend, samples := sampler.finish()
	return cell{wall: wall, cpu: cpu, ops: n, latency: time.Duration(latency.Load()),
		seen: seen, backend: backend, onCPU: seen["CPU"], samples: samples}
}

func (c *cell) merge(other cell) {
	c.wall += other.wall
	c.latency += other.latency
	c.cpu.self += other.cpu.self
	c.cpu.machine += other.cpu.machine
	c.ops += other.ops
	c.backend += other.backend
	c.onCPU += other.onCPU
	c.samples += other.samples
	if c.seen == nil {
		c.seen = map[string]time.Duration{}
	}
	for key, at := range other.seen {
		c.seen[key] += at
	}
}

func (m *measure) concurrencyTables(t *testing.T) {
	t.Helper()
	cores := runtimeCores()
	columns := []string{"wall (s)", "amortized (ms)", "latency (ms)", "client CPU (core-s)",
		"host CPU (core-s)", "host busy (%)", "server backend-s active", "what bounds it", "top waits"}
	caption := "Each cell is " + strconv.Itoa(burstRounds) + " bursts of that many started at once; `wall` is the sum of " +
		"those bursts' walls, `amortized` that over the statements, `latency` the mean of one statement's own clock. " +
		"`client CPU` is this process's getrusage, which holds no part of the server. `host CPU` is /proc/stat over " +
		"every core, which holds the server wherever the server shares this kernel, and `host busy` is that over " +
		"cores × wall, the one direct reading of a CPU bound. `server backend-s active` is backend-seconds spent " +
		"active with no wait event, from pg_stat_activity sampled every " + sampleInterval.String() + ": a backend " +
		"counted there is running or waiting for a core, so it is not core-seconds."
	clones := &table{
		title:   "(b) the clone, concurrent",
		caption: caption,
		head:    append([]string{"strategy", "at once", "clones"}, columns...),
	}
	drops := &table{
		title:   "(b) `drop database … with (force)`, concurrent",
		caption: "The same bursts' databases, dropped at once. Columns as above.",
		head:    append([]string{"dropped after", "at once", "drops"}, columns...),
	}
	row := func(out *table, label string, size int, c cell) {
		busy := 0.0
		if cores > 0 && c.wall > 0 {
			busy = float64(c.cpu.machine) / (float64(cores) * float64(c.wall))
		}
		out.add(label, strconv.Itoa(size), strconv.Itoa(c.ops), secs(c.wall),
			ms(c.wall/time.Duration(c.ops)), ms(c.latency/time.Duration(c.ops)),
			secs(c.cpu.self), secs(c.cpu.machine), fmt.Sprintf("%.0f", 100*busy), secs(c.onCPU),
			limitOf(c.seen, c.backend, busy), top(c.seen, c.backend, 3))
	}
	for _, size := range burstSizes {
		var cloneCell, dropCell cell
		for range burstRounds {
			names := make([]string, size)
			for i := range names {
				names[i] = m.name("burst")
			}
			cloneCell.merge(m.burst(t, "create"+" database "+measurePrefix+"%", size, func(i int) error {
				return m.try(cloneOf(names[i], m.template))
			}))
			dropCell.merge(m.burst(t, "drop database "+measurePrefix+"%", size, func(i int) error {
				return m.try(dropOf(names[i]))
			}))
		}
		row(clones, "file_copy", size, cloneCell)
		row(drops, "file_copy", size, dropCell)
	}
	clones.print()
	drops.print()
}

// ---------------------------------------------------------------- (d) the drop off the critical path

type simulation struct {
	wall      time.Duration
	testsDone time.Duration
	perTest   time.Duration
	cpu       cpuReading
	peakStand int
	meanStand float64
}

func (m *measure) dropPathTable(t *testing.T) {
	t.Helper()
	out := &table{
		title: "(d) The drop on and off a test's critical path",
		caption: fmt.Sprintf("%d stand-in tests at %d at a time, each a clone, a pool, three statements, a pool close and a drop; "+
			"the body is a stand-in, so what the two rows compare is the drop. `databases standing` counts this "+
			"simulation's databases alive at once, sampled every 10 ms: with the drop waited for, that is about one per "+
			"worker; with it in the background, it is one per worker plus the drops still running. A database does not "+
			"exist until its creation commits, so a worker holds one only for the part of its cycle after the "+
			"clone: that is why the mean is far below the number of workers.", simTests, apiTestParallelism),
		head: []string{"where the drop runs", "tests", "at once", "last test ended (s)", "every drop done (s)",
			"mean test wall (ms)", "client CPU (core-s)", "host CPU (core-s)", "peak databases standing", "mean databases standing"},
	}
	add := func(label string, s simulation) {
		out.add(label, strconv.Itoa(simTests), strconv.Itoa(apiTestParallelism), secs(s.testsDone), secs(s.wall),
			ms(s.perTest), secs(s.cpu.self), secs(s.cpu.machine), strconv.Itoa(s.peakStand), fmt.Sprintf("%.1f", s.meanStand))
	}
	waited := m.simulate(t, false)
	background := m.simulate(t, true)
	add("in the test's cleanup, waited for", waited)
	add("in the background, waited for at the end", background)
	out.print()
	fmt.Printf("\nMoving the drop off a test takes %s ms off that test's own wall (%s ms against %s ms), and takes the peak "+
		"databases standing from %d to %d (mean %.1f to %.1f).\n",
		ms(waited.perTest-background.perTest), ms(waited.perTest), ms(background.perTest),
		waited.peakStand, background.peakStand, waited.meanStand, background.meanStand)
}

func (m *measure) simulate(t *testing.T, background bool) simulation {
	t.Helper()
	ctx := context.Background()
	kind := "simsync"
	if background {
		kind = "simbg"
	}
	stand := make(chan struct{})
	standing := make(chan [2]float64, 1)
	go func() {
		highest, total, samples := 0, 0, 0
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stand:
				mean := 0.0
				if samples > 0 {
					mean = float64(total) / float64(samples)
				}
				standing <- [2]float64{float64(highest), mean}
				return
			case <-ticker.C:
			}
			var alive int
			if err := m.admin.QueryRow(ctx, "select count(*) from pg_database where datname like $1", measurePrefix+kind+"_%").Scan(&alive); err != nil {
				continue
			}
			total, samples = total+alive, samples+1
			if alive > highest {
				highest = alive
			}
		}
	}()

	var drops sync.WaitGroup
	var tests sync.WaitGroup
	var totalTestWall atomic.Int64
	jobs := make(chan int, simTests)
	for i := range simTests {
		jobs <- i
	}
	close(jobs)
	before := readCPU()
	start := time.Now()
	for range apiTestParallelism {
		tests.Go(func() {
			for range jobs {
				name := fmt.Sprintf("%s%s_%s_%d", measurePrefix, kind, m.suffix, m.next.Add(1))
				testStart := time.Now()
				if _, err := m.admin.Exec(ctx, cloneOf(name, m.template)); err != nil {
					t.Errorf("clone %s: %v", name, err)
					return
				}
				database, err := store.Open(ctx, m.url(name), store.WithMaxConns(measurePoolSize))
				if err != nil {
					t.Errorf("open %s: %v", name, err)
					return
				}
				for range 3 {
					var count int
					if err := database.Pool.QueryRow(ctx, "select count(*) from issues").Scan(&count); err != nil {
						t.Errorf("read %s: %v", name, err)
						break
					}
				}
				database.Pool.Close()
				if background {
					drops.Go(func() {
						if _, err := m.admin.Exec(ctx, dropOf(name)); err != nil {
							t.Errorf("drop %s: %v", name, err)
						}
					})
				} else if _, err := m.admin.Exec(ctx, dropOf(name)); err != nil {
					t.Errorf("drop %s: %v", name, err)
				}
				totalTestWall.Add(int64(time.Since(testStart)))
			}
		})
	}
	tests.Wait()
	testsDone := time.Since(start)
	drops.Wait()
	wall := time.Since(start)
	cpu := readCPU().since(before)
	close(stand)
	seen := <-standing
	return simulation{wall: wall, testsDone: testsDone, perTest: time.Duration(totalTestWall.Load() / simTests),
		cpu: cpu, peakStand: int(seen[0]), meanStand: seen[1]}
}

// ---------------------------------------------------------------- cleanup

// dropEverything drops every database this measurement made, a killed run's included.
func (m *measure) dropEverything(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	rows, err := m.admin.Query(ctx, "select datname from pg_database where datname like $1", measurePrefix+"%")
	if err != nil {
		t.Errorf("list the measurement's databases: %v", err)
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Errorf("scan a database name: %v", err)
			continue
		}
		names = append(names, name)
	}
	rows.Close()
	for _, name := range names {
		if _, err := m.admin.Exec(ctx, dropOf(name)); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	}
}
