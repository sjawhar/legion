package sampler

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procread"
)

// The tests below run the sampler's own process over a pod the test wrote, as the collectors'
// tests do: a root of its own holding proc/… and sys/fs/cgroup/…, a loopback port the kernel
// chooses, and the two readings a test cannot write down — the sampler's own anonymous memory
// and a statfs — taken through the package vars pod.go already keeps for them. No test here
// allocates 64 MiB, takes a port by name or sleeps for a reading.

// holdMemoryLimit puts the Go heap's limit back after a test that ran the sampler. Run sets it
// for the process it is the whole of, and the test binary is not that process: a test that left
// it at 60 MiB would have every test after it collecting against a limit it never asked for.
func holdMemoryLimit(t *testing.T) {
	t.Helper()
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })
}

// guardEvery shortens the memory guard's rate, so a test drives the guard in milliseconds rather
// than waiting a second for each reading.
func guardEvery(t *testing.T, every time.Duration) {
	t.Helper()
	before := guardInterval
	guardInterval = every
	t.Cleanup(func() { guardInterval = before })
}

// useAnonymous is the test hook of the sampler's own memory rule: the reader the guard and the
// smaps pass both take their readings through. A test raises what it answers and the sampler
// exits on a number no test had to allocate.
func useAnonymous(t *testing.T, read func(root string, pid int) (int64, error)) {
	t.Helper()
	before := readAnonymous
	readAnonymous = read
	t.Cleanup(func() { readAnonymous = before })
}

// servingPrefix is the log line Run writes once it is bound, which is how a test that asked for
// port 0 learns the port the kernel gave it — the same line an operator reads.
const servingPrefix = "sampler: serving "

// serveSampler runs the sampler until the test ends and answers the address it serves on, which
// the log line it writes once it is bound names — a test asks for port 0 and learns the port
// the kernel gave it the way an operator reading the log would. The collector and the server
// both log through the test's own writer, and the run is joined in the test's cleanup, so
// nothing writes to the test's log after it has finished.
func serveSampler(t *testing.T, opts Options) string {
	t.Helper()
	holdMemoryLimit(t)
	var mu sync.Mutex
	serving := make(chan string, 1)
	write := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		t.Log(line)
		if rest, bound := strings.CutPrefix(line, servingPrefix); bound {
			if _, at, found := strings.Cut(rest, " mode on "); found {
				select {
				case serving <- at:
				default:
				}
			}
		}
	}
	opts.Log = write
	if opts.Pod != nil {
		opts.Pod.Log = write
	}
	if opts.Node != nil {
		opts.Node.Log = write
	}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-ended:
			if err != nil {
				t.Errorf("the sampler ended with %v, want the end of its context alone", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the sampler did not end within 10 s of its context ending")
		}
	})
	select {
	case address := <-serving:
		return address
	case err := <-ended:
		// Put it back, so the cleanup above reads the same end this did.
		ended <- err
		t.Fatalf("the sampler ended before it served anything: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler did not say what it was serving on within 10 s")
	}
	return ""
}

// scraper is a client of its own, so a request to a sampler that stopped answering fails this
// test rather than holding the package's whole run until its timeout.
var scraper = &http.Client{Timeout: 10 * time.Second}

// get is one request to the sampler's port, with its body read to the end.
func get(t *testing.T, address, path string) (*http.Response, string) {
	t.Helper()
	response, err := scraper.Get("http://" + address + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer of GET %s: %v", path, err)
	}
	return response, string(body)
}

// The sampler's two routes over a pod of the test's own: /metrics is the registry's exposition
// under the content type a Prometheus collector reads, holding what the collector's first pass
// read, and /healthz is 200 with the mode and whether a pass has been made.
//
// The first scrape is taken the moment the port answers, which is the scrape a collector makes
// of a sampler that has only just started: it is answered with the exposition and not with an
// error, since the first pass runs as the collector's loop begins rather than a tick later. The
// series of that pass are then waited for, because a scrape that arrives while the pass is
// still walking /proc is answered with what the pass has read so far.
func TestTheSamplerServesItsRegistryAndItsHealth(t *testing.T) {
	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(268435456, 12_500_000_000)
	guardEvery(t, 5*time.Millisecond)

	pod := f.options()
	address := serveSampler(t, Options{Listen: "127.0.0.1:0", Pod: &pod})

	response, body := get(t, address, "/metrics")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200: %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); got != ContentType {
		t.Errorf("GET /metrics content type = %q, want %q", got, ContentType)
	}
	if !strings.Contains(body, "legion_sampler_") {
		t.Fatalf("GET /metrics holds no series of this sampler at all: %q", body)
	}
	// A scrape resets every peak, so each of these is asked of a scrape of its own, which is
	// what a second collector reading the port would do.
	for _, want := range []string{
		`legion_sampler_pod_cpu_seconds_total{project="INFRA",issue="INFRA-1208"} 12.5`,
		"legion_sampler_self_cpu_seconds_total",
		`legion_sampler_cpu_seconds_total{project="INFRA",issue="INFRA-1208",role="implementer",kind="agent"}`,
		`legion_sampler_memory_resident_bytes{project="INFRA",issue="INFRA-1208",role="shared",kind="shared"}`,
	} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, body := get(t, address, "/metrics")
			if strings.Contains(body, want) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET /metrics does not hold %q after 10 s: %q", want, body)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The pass flag is the guard's, which runs every 5 ms here, so this waits for a reading
	// and never for the second a pod's guard takes.
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, body := get(t, address, "/healthz")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want 200: %q", response.StatusCode, body)
		}
		var answer health
		if err := json.Unmarshal([]byte(body), &answer); err != nil {
			t.Fatalf("GET /healthz answered %q, which is no JSON body: %v", body, err)
		}
		if answer.Mode != "pod" {
			t.Fatalf("GET /healthz mode = %q, want pod", answer.Mode)
		}
		if answer.Pass {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /healthz still reports no pass after 10 s: %q", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A port the sampler cannot bind ends it with one line naming the address and nothing else: the
// launcher starts it again five seconds later, and a pod's log is no place for a stack trace
// (dispatch://INFRA-1208/spec#b-a-safe). The command turns that error into its non-zero exit.
func TestAPortTheSamplerCannotBindIsOneLineAndNoPanic(t *testing.T) {
	holdMemoryLimit(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	defer held.Close()

	f := newPodFixture(t)
	pod := f.options()
	err = Run(context.Background(), Options{
		Listen: held.Addr().String(),
		Pod:    &pod,
		Log:    func(line string) { t.Log(line) },
	})
	if err == nil {
		t.Fatal("the sampler took a port another listener holds")
	}
	if !strings.Contains(err.Error(), held.Addr().String()) {
		t.Errorf("the refusal is %q, want the address %s named", err, held.Addr())
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal is %q, want one line", err)
	}
}

// The sampler ends when its own anonymous memory passes 64 MiB, which the test hook of the
// reader it takes that reading through drives without allocating a byte
// (dispatch://INFRA-1208/artifact/verification-md#b-ca-unit, "push its own anonymous memory past
// 64 MiB through a test hook"). The rule is written against anonymous memory and not resident
// memory because under gVisor a `legion` process's resident memory counts the mapped 102 MB
// binary, so a resident rule would end the sampler before it served a byte.
//
// It ends by returning, never by calling os.Exit: one line naming the reading, which the command
// prints and exits non-zero with, and which this test could not read at all from a process that
// had ended under it.
func TestTheSamplerEndsWhenItsOwnAnonymousMemoryPassesTheLimit(t *testing.T) {
	holdMemoryLimit(t)
	guardEvery(t, 5*time.Millisecond)
	const held = 65 << 20
	useAnonymous(t, func(string, int) (int64, error) { return held, nil })

	f := newPodFixture(t)
	f.writeProcesses(743092, 56, podTree()...)
	f.writeV1Totals(268435456, 12_500_000_000)
	pod := f.options()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := Run(ctx, Options{Listen: "127.0.0.1:0", Pod: &pod, Log: func(string) {}})
	if err == nil {
		t.Fatal("the sampler went on running past its memory limit")
	}
	if ctx.Err() != nil {
		t.Fatalf("the sampler ran to the test's own deadline: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the exit is %q, want one line", err)
	}
	for _, want := range []string{"68157440", "67108864"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the exit is %q, want the %s it read named", err, want)
		}
	}
}

// Options nothing could run with are refused before a port is bound, in the shape the
// collectors' own options are.
func TestTheSamplerRefusesOptionsItCouldNotRunWith(t *testing.T) {
	f := newPodFixture(t)
	pod := f.options()
	node := NodeOptions{Root: f.root, NodeName: "n", Registry: NewNodeRegistry(nil), Log: func(string) {}}
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no address", Options{Pod: &pod, Log: func(string) {}}, "listen address is required"},
		{"no collector", Options{Listen: "127.0.0.1:0", Log: func(string) {}}, "collector"},
		{"both collectors", Options{Listen: "127.0.0.1:0", Pod: &pod, Node: &node, Log: func(string) {}}, "never both"},
		{"no log", Options{Listen: "127.0.0.1:0", Pod: &pod}, "log is required"},
		{"the collector's own refusal", Options{Listen: "127.0.0.1:0", Node: &NodeOptions{Root: f.root}, Log: func(string) {}}, "node name is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(context.Background(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the sampler answered %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// What the sampler costs the pod it reads: dispatch://INFRA-1141/spec#b-r-cost caps it at 2% of
// one CPU and 64 MiB of anonymous memory, and this measures both over the pod that check names —
// 200 processes, read for the passes thirty seconds of a running sampler stands for.
//
// Neither side of the comparison is a wall clock, which is what makes it decidable on a shared
// CI runner. The passes are driven by hand, so costPasses ticks of podInterval stand for exactly
// costPasses seconds of a sampler running at the rates docs/sampler-metrics.md gives — the
// process walk and the volumes every 2 s, the smaps window every 10 s — whatever the machine was
// doing meanwhile. What those passes spent is the sampler's own CPU, read from /proc/self/stat,
// which no neighbour on the runner can inflate: a process descheduled for a second is charged
// none of it. So what this compares is the CPU the work took against the time that work stands
// for, which is the ratio the live check reads off the sampler's own series in a real pod.
//
// The memory is read the same way, from this process's own smaps: the whole test binary's
// anonymous memory stands above the sampler's share of it, so a reading under 64 MiB settles the
// cap for the sampler. What this cannot measure is the kernel's own cost of a real smaps read,
// 2.45 ms a process, since the fixture's files are the test's; that belongs to the live check.
func TestTheSamplerCostsUnderTwoPercentOfOneCPUOver200Processes(t *testing.T) {
	useMemoryFixture(t)
	const (
		costProcesses = 200
		costPasses    = 30
		costShare     = 0.02
	)
	f := newPodFixture(t)
	tree := costTree(costProcesses)
	if len(tree) != costProcesses {
		t.Fatalf("the fixture holds %d processes, want %d", len(tree), costProcesses)
	}
	f.writeProcesses(743092, 56, tree...)
	for _, process := range tree {
		f.writeSmaps(process.pid, int64(process.pages)*4)
	}
	f.writeV1Totals(268435456, 12_500_000_000)
	f.writeRoles(rolesOf())
	// Four volumes, each a directory that is there, so every pass takes the statfs a pod's
	// own volumes cost it rather than one the test answered for.
	f.writeVolumes([]Volume{
		{Name: "private-implementer", Paths: []string{f.root}, Medium: "memory", Role: "implementer"},
		{Name: "private-reviewer", Paths: []string{f.root}, Medium: "memory", Role: "reviewer"},
		{Name: "workspace", Paths: []string{f.root}, Medium: "disk"},
		{Name: "home", Paths: []string{f.root}, Medium: "disk"},
	})

	collector := f.collector()
	began := selfTicks(t)
	for tick := int64(0); tick < costPasses; tick++ {
		collector.pass(tick)
	}
	spent := float64(selfTicks(t)-began) / procread.ClockTicks
	stood := (costPasses * podInterval).Seconds()
	share := spent / stood
	t.Logf("%d passes over %d processes spent %.3f s of CPU for the %.0f s they stand for: %.2f%% of one CPU",
		costPasses, costProcesses, spent, stood, 100*share)
	if share > costShare {
		t.Errorf("the sampler spent %.2f%% of one CPU over %d processes; the cap is %.0f%%",
			100*share, costProcesses, 100*costShare)
	}

	anonymous, err := procread.ReadAnonymous("/", os.Getpid())
	if err != nil {
		t.Fatalf("this process's own anonymous memory could not be read: %v", err)
	}
	if anonymous > memoryExit {
		t.Errorf("the passes left %d bytes of anonymous memory held, past the %d-byte cap", anonymous, memoryExit)
	}

	// The passes did the work the cost is claimed for: every process read, nothing failed.
	e := f.scrape()
	wantValue(t, e, 0, smapsDeferred)
	for _, source := range readSources {
		wantValue(t, e, 0, readErrorsTotal, string(source))
	}
}

// selfTicks is the CPU this process has spent, in ticks of /proc's own clock: what a pod is
// charged for the sampler, and what the cost above is measured in. It reads the real /proc of
// the real process, which is why it takes neither the fixture's root nor the fixture's pid.
func selfTicks(t *testing.T) uint64 {
	t.Helper()
	stat, err := procread.ReadStat("/", os.Getpid())
	if err != nil {
		t.Fatalf("this process's own /proc/<pid>/stat could not be read: %v", err)
	}
	return stat.UserTicks + stat.SystemTicks
}

// useMemoryFixture writes the cost fixture to a filesystem in memory where the machine has one.
// The fixture stands in for /proc, and /proc is in memory: a fixture on a disk, or in this
// issue's own gVisor pod behind the gofer that serves its root, charges the sampler the
// filesystem under the test rather than its own work — measured in that pod on 2026-11-05, the
// same 30 passes over 200 processes spent 0.66 s of CPU against a gofer-backed root and 0.33 s
// against a tmpfs. Where the machine has no such directory the fixture goes where every other
// test's does, which is what t.TempDir() answers with TMPDIR untouched.
func useMemoryFixture(t *testing.T) {
	t.Helper()
	const inMemory = "/dev/shm"
	info, err := os.Stat(inMemory)
	if err != nil || !info.IsDir() {
		return
	}
	dir, err := os.MkdirTemp(inMemory, "sampler-cost-")
	if err != nil {
		return
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
}

// costTree is a busy issue pod of this many processes: the container's own processes and two
// roles, and under each role's harness the commands a busy role runs — a build with its
// compiler, a test run with its linker, and a language server — each with CPU and resident
// memory of its own, so the smaps pass has an order to read them in and every pass has a rise to
// charge. The counters are the walk's own units: ticks and pages.
func costTree(processes int) []fakeProcess {
	tree := rolesTree()
	for i := range tree {
		tree[i].start = 900
		tree[i].own = uint64(10 + i)
		tree[i].pages = uint64(1000 - 3*i)
	}
	shells := []int{13, 17, 21}
	for pid := 100; len(tree) < processes; pid++ {
		at := len(tree)
		tree = append(tree, fakeProcess{
			pid:    pid,
			parent: shells[at%len(shells)],
			args:   []string{"/usr/lib/go/pkg/tool/linux_amd64/compile", "-o", "a.out"},
			own:    uint64(at),
			start:  1000,
			pages:  uint64(500 + at),
		})
	}
	return tree
}
