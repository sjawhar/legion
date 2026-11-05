package cireconcile

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// One process serves each period. Two Reconcilers stand for the two processes a deployment runs,
// each with a GitHub client of its own over the one stand-in and both over one database; each
// runs its pass under the pass's own lease, and over two periods the stand-in sees each period's
// reads from one process only. The pass is safe to run twice -- every write it makes is a
// compare-and-swap of an observation GitHub already holds -- so what the lease saves is the
// App's budget.
func TestOneProcessServesEachPeriod(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	other := h.replica()
	leases, err := h.pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}

	// Two ticks one Interval apart, the second process first in the second period, so neither
	// test passes by being the one that always runs.
	due := time.Now()
	for _, period := range []struct {
		name   string
		due    time.Time
		first  *Reconciler
		second *Reconciler
	}{
		{name: "the first period", due: due, first: h.reconciler, second: other},
		{name: "the period after it", due: due.Add(Interval), first: other, second: h.reconciler},
	} {
		before := h.github.count()
		if ran := duty.Exclusive(h.ctx, leases, Lease, period.due, Interval, period.first.Pass); !ran {
			t.Fatalf("%s: the process that took the lease ran no pass", period.name)
		}
		served := h.github.count()
		if served == before {
			t.Fatalf("%s: the process that served it sent GitHub nothing", period.name)
		}
		if ran := duty.Exclusive(h.ctx, leases, Lease, period.due, Interval, period.second.Pass); ran {
			t.Fatalf("%s: both processes ran a pass", period.name)
		}
		if sent := h.github.count(); sent != served {
			t.Fatalf("%s: the process whose pass was refused sent GitHub %d requests", period.name, sent-served)
		}
	}
}

// A limit GitHub answers one process with stops every process: it is recorded in the shared
// ledger under the scope the request was budgeted under, the installation's, and while it stands
// each process's pass reaches GitHub with no request at all. GitHub budgets the App and its
// installations, not a process, and warns that requests sent inside a limit may get the
// integration banned.
func TestARecordedLimitStopsEveryProcess(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	other := h.replica()

	// Both processes mint their installation token first, so neither has to mint one while the
	// limit stands: a mint is budgeted under the App, not under the installation the limit is on,
	// and this test is about the reads that limit stops.
	h.pass()
	other.Pass(h.ctx)

	h.github.rateLimitNext(checkRunsPath)

	h.pass()

	scope := githubapp.InstallationScope(h.github.installationID())
	limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
	if err != nil {
		t.Fatalf("read the shared GitHub rate limits: %v", err)
	}
	if !found {
		t.Fatalf("github_rate_limits holds no limit under %s", scope)
	}
	if limit.Wait <= 0 {
		t.Fatalf("the limit under %s has %s left, want one still in force", scope, limit.Wait)
	}

	// The stand-in answers as it did before; nothing may reach it from either process while the
	// limit it recorded stands.
	h.github.forgetRequests()

	h.pass()
	other.Pass(h.ctx)

	if sent := h.github.count(); sent != 0 {
		t.Fatalf("the two processes sent GitHub %d requests under a limit in force: %+v", sent, h.github.received())
	}
}

// The stop is in the pass line: a pass GitHub's own rate-limited answer ended logs its one INFO
// line, saying it was rate-limited, under which scope, and when GitHub will take a request again.
// An alarm reads the pass by that line alone.
func TestTheStopIsInThePassLine(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.rateLimitNext(checkRunsPath)

	h.pass()

	lines := h.lines("ci reconcile pass")
	if len(lines) != 1 {
		t.Fatalf("the pass logged %d pass lines, want exactly one:\n%s", len(lines), h.logs.String())
	}
	line := lines[0]
	if line["rate_limited"] != true {
		t.Fatalf("the pass line reports rate_limited = %v, want true: %v", line["rate_limited"], line)
	}
	scope := githubapp.InstallationScope(h.github.installationID())
	if line["rate_limit_scope"] != scope {
		t.Fatalf("the pass line names scope %v, want %s, the installation the head's read was budgeted under: %v",
			line["rate_limit_scope"], scope, line)
	}
	text, ok := line["rate_limit_until"].(string)
	if !ok {
		t.Fatalf("the pass line says when the limit ends: %v", line)
	}
	until, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("the pass line's rate_limit_until is not RFC3339: %v", err)
	}
	// The stand-in's own answer resets ninety seconds out, so the line's time is ahead of now and
	// not far ahead of it.
	if !until.After(time.Now()) || until.After(time.Now().Add(5*time.Minute)) {
		t.Fatalf("the pass line says the limit ends at %s, want the stand-in's reset, which is moments away", until)
	}
}
