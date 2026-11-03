package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// rereadBudgets are the two fleet budgets a broker process can divide: cmd/broker takes the GCP
// one under gcp-secret-manager and the default one otherwise. Every row below runs over both,
// because a pod of either deployment derives its rates the same way and only the budget differs.
var rereadBudgets = []struct {
	name   string
	budget ratelimit.Limit
	// shared is the shared bucket's rate at three pods, and address one address's, which is
	// RereadLimitFor of it.
	shared, address time.Duration
}{
	{"default", api.DefaultRereadFleetBudget, 2250 * time.Millisecond, 4500 * time.Millisecond},
	{"gcp", api.GCPRereadFleetBudget, 9 * time.Second, 18 * time.Second},
}

// newRereadRate is a rate on budget, with the buffer its log lines are written to.
func newRereadRate(budget ratelimit.Limit) (*api.RereadRate, *bytes.Buffer) {
	var logged bytes.Buffer
	return api.NewRereadRate(budget, slog.New(slog.NewTextHandler(&logged, nil))), &logged
}

// TestRereadOverallForDividesTheBudgetAmongThePods pins the derivation itself: one pod's share of
// the fleet's budget is the budget divided by the square of the replica count, because every
// reread a pod accepts costs a store read on every pod. A count below api.MinReplicas is counted
// as api.MinReplicas, so one pod alone never takes the whole fleet's budget.
func TestRereadOverallForDividesTheBudgetAmongThePods(t *testing.T) {
	for _, budget := range rereadBudgets {
		t.Run(budget.name, func(t *testing.T) {
			for _, row := range []struct {
				replicas int
				every    time.Duration
			}{
				{1, 4 * budget.budget.Every},
				{2, 4 * budget.budget.Every},
				{3, 9 * budget.budget.Every},
				{4, 16 * budget.budget.Every},
				{5, 25 * budget.budget.Every},
			} {
				got := api.RereadOverallFor(budget.budget, row.replicas)
				if got.Every != row.every {
					t.Errorf("RereadOverallFor(%s, %d).Every = %s, want %s",
						budget.name, row.replicas, got.Every, row.every)
				}
				if got.Burst != budget.budget.Burst {
					t.Errorf("RereadOverallFor(%s, %d).Burst = %d, want the budget's own %d: a burst admits one person's write burst, which the replica count does not change",
						budget.name, row.replicas, got.Burst, budget.budget.Burst)
				}
			}
		})
	}
}

// TestTheMountedRereadLimitsAreTheBudgetAtTwoPods pins that deriving the rates left the two
// constants every other test and every fixed-limit deployment reads exactly where they were: one
// reread a second on the default store and one every four seconds on Secret Manager, each a
// burst of ten. They are now the fleet budget at api.MinReplicas, and a budget edited without
// meaning to change them goes red here.
func TestTheMountedRereadLimitsAreTheBudgetAtTwoPods(t *testing.T) {
	for _, row := range []struct {
		name string
		got  ratelimit.Limit
		want ratelimit.Limit
	}{
		{"DefaultRereadOverallLimit", api.DefaultRereadOverallLimit, ratelimit.Limit{Every: time.Second, Burst: 10}},
		{"GCPRereadOverallLimit", api.GCPRereadOverallLimit, ratelimit.Limit{Every: 4 * time.Second, Burst: 10}},
	} {
		if row.got != row.want {
			t.Errorf("%s = %+v, want %+v unchanged", row.name, row.got, row.want)
		}
	}
}

// TestSetReplicasMovesTheLiveRereadBuckets is the half the log line cannot show: at three pods
// the buckets a caller really meets refill at the three-pod rate. Both are driven at synthetic
// instants, so the row waits for nothing and measures no wall clock; the millisecond on either
// side of each rate is there because a bucket's refill is floating-point arithmetic, and it is
// far inside the gap between any two sizings this derivation produces.
func TestSetReplicasMovesTheLiveRereadBuckets(t *testing.T) {
	const ms = time.Millisecond
	for _, budget := range rereadBudgets {
		t.Run(budget.name, func(t *testing.T) {
			rate, _ := newRereadRate(budget.budget)
			perAddress, shared := rate.Buckets()
			rate.SetReplicas(3)
			// At or after the instant SetReplicas moved the rates at, so both buckets are
			// full: every instant below is this one plus an exact offset.
			start := time.Now()

			for sent := range budget.budget.Burst {
				if _, ok := perAddress.AllowBothAt("203.0.113.7", shared, start); !ok {
					t.Fatalf("the shared bucket refused reread %d of its burst of %d", sent+1, budget.budget.Burst)
				}
			}
			if refused, ok := perAddress.AllowBothAt("203.0.113.7", shared, start); ok || refused.Every != budget.shared {
				t.Fatalf("the reread past the shared burst = %+v, %v; want refused at one every %s, the three-pod rate", refused, ok, budget.shared)
			}
			if _, ok := perAddress.AllowBothAt("203.0.113.7", shared, start.Add(budget.shared-ms)); ok {
				t.Fatalf("the shared bucket gave a reread back inside %s", budget.shared)
			}
			if _, ok := perAddress.AllowBothAt("203.0.113.7", shared, start.Add(budget.shared+ms)); !ok {
				t.Fatalf("the shared bucket gave no reread back after %s", budget.shared)
			}

			// The address bucket on its own: AllowAt spends it and leaves the shared bucket
			// alone, which is the only way to reach an address's own rate — the shared
			// bucket, a third of its depth, refuses a flood through AllowBothAt first.
			const address = "198.51.100.4"
			for sent := range api.DefaultRereadBurst {
				if !perAddress.AllowAt(address, start) {
					t.Fatalf("one address's bucket refused reread %d of its burst of %d", sent+1, api.DefaultRereadBurst)
				}
			}
			if perAddress.AllowAt(address, start.Add(budget.address-ms)) {
				t.Fatalf("one address's bucket gave a reread back inside %s", budget.address)
			}
			if !perAddress.AllowAt(address, start.Add(budget.address+ms)) {
				t.Fatalf("one address's bucket gave no reread back after %s", budget.address)
			}
		})
	}
}

// TestABucketSpentBeforeASetReplicasKeepsItsRereads pins what a change of the replica count does
// to a caller already using its bucket: the rereads it has left stay, and only the rate it gains
// them back at moves. A rewrite that refilled the bucket would hand every address a fresh burst
// at each rolling deploy, which is when the count changes.
func TestABucketSpentBeforeASetReplicasKeepsItsRereads(t *testing.T) {
	const address = "203.0.113.9"
	const spent = 5
	rate, _ := newRereadRate(api.DefaultRereadFleetBudget)
	perAddress, _ := rate.Buckets()
	before := time.Now()
	for sent := range spent {
		if !perAddress.AllowAt(address, before) {
			t.Fatalf("the two-pod bucket refused reread %d of its burst", sent+1)
		}
	}

	rate.SetReplicas(3)
	start := time.Now()
	for left := range api.DefaultRereadBurst - spent {
		if !perAddress.AllowAt(address, start) {
			t.Fatalf("the bucket refused reread %d of the %d it still held: the rate change emptied it",
				left+1, api.DefaultRereadBurst-spent)
		}
	}
	if perAddress.AllowAt(address, start) {
		t.Fatalf("the bucket served more than the %d rereads it held: the rate change refilled it", api.DefaultRereadBurst-spent)
	}
	// 4.5 s is the three-pod rate; at the two-pod rate of one every 2 s the reread two seconds
	// on would be served.
	if !perAddress.AllowAt(address, start.Add(4500*time.Millisecond)) {
		t.Fatal("the bucket gave no reread back after the three-pod rate of 4.5s")
	}
	if perAddress.AllowAt(address, start.Add(6500*time.Millisecond)) {
		t.Fatal("the bucket gave a reread back two seconds on: it kept the two-pod rate rather than taking the three-pod one")
	}
}

// TestARereadRefusalCarriesTheRateOfTheReplicaCountNow drives the whole broker: a flood of
// rereads against a real server whose Deps carry a rate, at two pods and at three. Retry-After
// is what a caller sees, and it is read off the bucket that refused, so this is the row that
// says Register mounted the derived buckets rather than a constant.
func TestARereadRefusalCarriesTheRateOfTheReplicaCountNow(t *testing.T) {
	for _, row := range []struct {
		replicas int
		want     string
	}{{2, "1"}, {3, "3"}} {
		t.Run(strconv.Itoa(row.replicas)+" pods", func(t *testing.T) {
			rate, _ := newRereadRate(api.DefaultRereadFleetBudget)
			ts := newTestServerWith(t, func(d *api.Deps) { d.RereadRate = rate })
			rate.SetReplicas(row.replicas)
			for sent := 1; sent <= 100; sent++ {
				status, headers, body := postReread(t, ts, "WORKER_TOKEN", nil)
				if status == http.StatusOK {
					continue
				}
				if status != http.StatusTooManyRequests || decode[wireError](t, body).Code != "RATE_LIMITED" {
					t.Fatalf("reread %d = %d %s; want 200 until a bucket refuses", sent, status, body)
				}
				if got := headers.Get("Retry-After"); got != row.want {
					t.Fatalf("a refused reread carries Retry-After %q, want %q: the wait a caller is told is the rate the refusing bucket refills at now",
						got, row.want)
				}
				return
			}
			t.Fatal("100 rereads and no bucket refused: the row cannot read a rate it never meets")
		})
	}
}

// TestSetReplicasLogsEachRateOnceAndWarnsAboveFour pins the two lines an operator reads: one per
// change of the count, none for a count that changes nothing, and the warning above the replica
// count this sizing supports. A later test parses these out of a real broker's stdout, so their
// words and keys are the thing asserted.
func TestSetReplicasLogsEachRateOnceAndWarnsAboveFour(t *testing.T) {
	rate, logged := newRereadRate(api.DefaultRereadFleetBudget)

	rate.SetReplicas(2)
	first := lines(t, logged)
	if len(first) != 1 || !strings.Contains(first[0], `level=INFO msg="broker reread rate" replicas=2 every=1s burst=10`) {
		t.Fatalf("the first count logged %v, want one line naming two pods and one reread a second", first)
	}

	rate.SetReplicas(2)
	if again := lines(t, logged); len(again) != 0 {
		t.Fatalf("a count that changes no rate logged %v, want nothing: the clock beats every sweep", again)
	}

	rate.SetReplicas(3)
	three := lines(t, logged)
	if len(three) != 1 || !strings.Contains(three[0], `level=INFO msg="broker reread rate" replicas=3 every=2.25s burst=10`) {
		t.Fatalf("three pods logged %v, want one line naming one reread every 2.25s", three)
	}

	rate.SetReplicas(5)
	five := lines(t, logged)
	if len(five) != 2 {
		t.Fatalf("five pods logged %v, want the rate and the warning", five)
	}
	if !strings.Contains(five[0], `level=INFO msg="broker reread rate" replicas=5 every=6.25s burst=10`) {
		t.Fatalf("five pods logged %q as its rate, want one reread every 6.25s", five[0])
	}
	if !strings.Contains(five[1], `level=WARN msg="broker reread rate above the replica count this sizing supports" replicas=5 every=6.25s store_reads_at_burst=250`) {
		t.Fatalf("five pods logged %q as its warning, want the momentary cost of the burst at five pods", five[1])
	}
}

// lines takes every log line written so far and empties the buffer, and records them, so a run
// with -v quotes the broker's own rendering of each.
func lines(t *testing.T, logged *bytes.Buffer) []string {
	t.Helper()
	written := strings.TrimSuffix(logged.String(), "\n")
	logged.Reset()
	if written == "" {
		return nil
	}
	got := strings.Split(written, "\n")
	for _, line := range got {
		t.Logf("logged: %s", line)
	}
	return got
}
