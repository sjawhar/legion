package daemon

// The wave the wake pacing exists for, driven through the daemon itself and read from what a
// consumer of it sees: the moments its launches reach the runtime, the schedule its state serves
// (api.State.WakesWaiting), the `next_at` of the rows a lane holds, the one line each wake writes
// (supervise's wakeRan) and the claims the operator's own routes answer with. Every wait here is
// the harness clock moving (waveClock): nothing sleeps for a wake's turn.
//
// The pace is the shipped one, eight wakes a minute in bursts of one (testWakeRate,
// testWakeBurst in wake_catch_up_test.go), so one interval is 7.5 s and a minute holds at most
// nine paced wakes. The wave is forty claims, the size of the roll of 2026-11-04 cut to what a
// test can read: forty wakes at that pace span five minutes of the test clock.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// waveSize is how many claims each wave wakes at once.
const waveSize = 40

// waveClock is the machines' time, held still until a test moves it: a timer is due at a moment
// rather than after a wait, because a paced wake is armed for the turn the limiter gave it and a
// test that advances to that turn must fire that timer and no other. heldClock
// (supervision_test.go) fires by the exact wait a timer was armed for, which no wave can name: the
// daemon jitters every paced release, so the forty waits of one wave are forty different numbers.
type waveClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*waveTimer
}

// waveTimer is one armed timer: the moment it is due and what runs then.
type waveTimer struct {
	clock *waveClock
	due   time.Time
	f     func()
	done  bool
}

// newWaveClock starts at the wall clock, as heldClock does, so nothing the daemon compares
// against the database's own now reads as years out.
func newWaveClock() *waveClock {
	return &waveClock{now: time.Now().UTC()}
}

func (c *waveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep returns at once, as the daemon tests' clocks do: a launch backoff is not what these tests
// measure, and a sleep on a clock a test drives would never return.
func (c *waveClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (c *waveClock) AfterFunc(d time.Duration, f func()) supervise.Cancel {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &waveTimer{clock: c, due: c.now.Add(d), f: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *waveTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// advanceTo moves the clock to at and runs every timer due by then, earliest first, until none is
// left: a wake's turn arms the next thing the claim waits for, and a test that advanced past that
// moment has to see it fire too.
func (c *waveClock) advanceTo(at time.Time) {
	c.mu.Lock()
	if at.After(c.now) {
		c.now = at
	}
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *waveTimer
		for _, timer := range c.timers {
			if timer.done || timer.due.After(c.now) {
				continue
			}
			if next == nil || timer.due.Before(next.due) {
				next = timer
			}
		}
		if next != nil {
			next.done = true
		}
		c.mu.Unlock()
		if next == nil {
			return
		}
		next.f()
	}
}

// advance moves the clock on by d.
func (c *waveClock) advance(d time.Duration) { c.advanceTo(c.Now().Add(d)) }

// waveRuntime is the fake runtime with the moment of every launch on the test's clock: what the
// wave's consumer sees is when each process started, and the fake records the call alone.
type waveRuntime struct {
	runtime.Runtime
	clock *waveClock
	mu    sync.Mutex
	runs  []waveLaunch
}

// waveLaunch is one launch the runtime was asked for: whose claim, when on the test clock, and
// whether it resumed a recorded session rather than spawning a new one.
type waveLaunch struct {
	claim   claim.Token
	at      time.Time
	resumed bool
}

func newWaveRuntime(clock *waveClock) *waveRuntime {
	return &waveRuntime{Runtime: fake.NewRuntime(), clock: clock}
}

// fake is the runtime underneath, for the calls and the observations a test drives through it.
func (r *waveRuntime) fake() *fake.Runtime { return r.Runtime.(*fake.Runtime) }

func (r *waveRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.record(spec.Claim, false)
	return r.Runtime.Spawn(ctx, spec)
}

func (r *waveRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.record(spec.Claim, true)
	return r.Runtime.Resume(ctx, prev, spec)
}

func (r *waveRuntime) record(token claim.Token, resumed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, waveLaunch{claim: token, at: r.clock.Now(), resumed: resumed})
}

// launches is every launch so far, in the order the runtime was asked for them.
func (r *waveRuntime) launches() []waveLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// launchesOf is how many launches of token the runtime was asked for.
func (r *waveRuntime) launchesOf(token claim.Token) int {
	count := 0
	for _, launch := range r.launches() {
		if launch.claim == token {
			count++
		}
	}
	return count
}

// waveConfig is a daemon at the shipped pace whose every other timer is put out past the wave's
// own span: forty wakes at eight a minute span five minutes of the test clock, and a boot timeout
// or a probe firing inside that span would launch something the pacing never reserved.
func waveConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.WorkerWakesPerMinute, cfg.WorkerWakeBurst = testWakeRate, testWakeBurst
	cfg.WorkerBootTimeout = time.Hour
	cfg.ProbeInterval = time.Hour
	return cfg
}

// waveClaims stores claims of cfg's project on role, one per issue of its own, as the previous
// daemon left them: build fills in what the wave is made of, and the token, project, tree, issue
// and role are the store's to keep consistent. Each claim's role prompt is written, so every
// launch of the wave has one to read. One store session admits them all; putClaim's own migration
// per claim would be forty migrations.
func waveClaims(t *testing.T, cfg config.Config, role claim.Role, build func(i int, token claim.Token) supervise.Claim) []claim.Token {
	t.Helper()
	ctx := context.Background()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	tokens := make([]claim.Token, 0, waveSize)
	for i := 1; i <= waveSize; i++ {
		issue := fmt.Sprintf("%s-%d", cfg.Project, i)
		token, err := claim.NewToken(project, issue, role)
		if err != nil {
			t.Fatal(err)
		}
		c := build(i, token)
		c.Token, c.Project, c.Tree, c.Issue, c.Role = token, project, issue, issue, role
		if _, err := st.OpenTreeLifecycle(ctx, project, issue, treelifecycle.AuthorityOperator); err != nil {
			t.Fatalf("admit the tree of %s: %v", token, err)
		}
		if _, err := st.AdmitClaim(ctx, c); err != nil {
			t.Fatalf("put %s: %v", token, err)
		}
		writePrompt(t, cfg, token)
		tokens = append(tokens, token)
	}
	return tokens
}

// waveWorkers stores count further unfinished launches on the first trees of the wave, each an
// implementer rather than its tree's root: the claims an operator's stop ends, where a root's own
// end is its tree's close. They join the wave boot raises, since boot relaunches every unfinished
// launch it finds.
func waveWorkers(t *testing.T, cfg config.Config, count int) []claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make([]claim.Token, 0, count)
	for i := 1; i <= count; i++ {
		issue := waveIssue(cfg, i)
		token, err := claim.NewToken(project, issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		c := unfinishedLaunch(t, waveSize+i)
		c.Token, c.Project, c.Tree, c.Issue, c.Role = token, project, issue, issue, claim.RoleImplementer
		putClaim(t, cfg, c)
		writePrompt(t, cfg, token)
		tokens = append(tokens, token)
	}
	return tokens
}

// unfinishedLaunch is a claim as a daemon that was stopped mid-launch left it: launching, at its
// generation, with no process recorded. Boot relaunches every one of them (launchUnfinished),
// which is the wave a restart causes.
func unfinishedLaunch(t *testing.T, i int) supervise.Claim {
	t.Helper()
	return supervise.Claim{
		Generation: 1, State: supervise.StateLaunching,
		BootTokenHash: supervise.HashBootToken(fmt.Sprintf("unfinished-%d-%s", i, randomSuffix(t))),
	}
}

// liveClaim is a claim whose process the daemon re-adopts at boot: working, on a tmux pane of its
// own, with the session a relaunch resumes and the plugin contract given — this daemon's, unless a
// test is driving the stale-plugin lane.
func liveClaim(i int, token claim.Token, contract int) supervise.Claim {
	return supervise.Claim{
		Generation: 1, State: supervise.StateWorking, Session: "ses_" + string(token),
		SessionFile: "/sessions/" + string(token) + ".jsonl", PluginContract: contract,
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: fmt.Sprintf("42%02d:1", i),
			Tmux: &runtime.TmuxLocator{Window: fmt.Sprintf("@%d", i), Pane: fmt.Sprintf("%%%d", i)},
		},
	}
}

// startWaveDaemon runs a daemon over rt whose machines keep clock's time and whose log is
// written as JSON to logged, with its orphan sweep an hour out unless tune moves it: a sweep on
// the wall clock while a test holds the machines' own would reap and re-post on a cadence of its
// own.
func startWaveDaemon(t *testing.T, cfg config.Config, clock *waveClock, rt *waveRuntime, logged *syncBuffer, tune func(*overrides)) *daemon {
	t.Helper()
	o := fakeRuntime(rt, &built{})
	o.clock = clock
	o.orphanSweep = time.Hour
	if tune != nil {
		tune(&o)
	}
	return startDaemonWithin(t, cfg, o, slog.New(slog.NewJSONHandler(logged, nil)), 30*time.Second)
}

// waitingWakes is the wakes the daemon's state says the pacing holds, earliest first: the queue as
// the operator reads it.
func waitingWakes(t *testing.T, d *daemon) []api.WakeWaiting {
	t.Helper()
	waiting := d.state().WakesWaiting
	slices.SortStableFunc(waiting, func(a, b api.WakeWaiting) int { return a.DueAt.Compare(b.DueAt) })
	return waiting
}

// awaitWave waits until the whole wave has reserved its turn: one wake out at once, which the
// burst lets through, and the rest waiting in the state. It returns the queue, earliest first.
//
// The queue must be read twice alike before it is believed. A wave's sites fan out over one
// goroutine per claim (the boot's launchUnfinished, the sweep's posts, the outbox's lanes), so
// the count can reach the wave's size while a straggler is still between its reserve and the
// write the state reads it from, and a queue read in that moment names one claim the turns do
// not. Two equal reads in a row say the fan-out is over, and every caller then advances the
// clock to the moments this returned.
func awaitWave(t *testing.T, d *daemon, count int) []api.WakeWaiting {
	t.Helper()
	var waiting, settled []api.WakeWaiting
	testwait.Eventually(t, fmt.Sprintf("the pacing to hold %d of the wave's %d wakes", count-1, count), func() bool {
		waiting, settled = waitingWakes(t, d), waiting
		return len(waiting) == count-1 && slices.Equal(waiting, settled)
	})
	return waiting
}

// wakeMoments is the moment each wake of a wave runs at: the first at origin, which the burst let
// straight out, then every waiting wake's own turn.
func wakeMoments(origin time.Time, waiting []api.WakeWaiting) []time.Time {
	moments := make([]time.Time, 0, len(waiting)+1)
	moments = append(moments, origin)
	for _, wake := range waiting {
		moments = append(moments, wake.DueAt)
	}
	return moments
}

// assertSpread is the whole of what c-spread asks of a wave's schedule: the first wake goes at
// once, every gap is at least one interval and less than two — one interval plus the part of one
// the daemon jitters each release by — the gaps are not all equal, because that part is drawn
// afresh for every wake, and no minute of the test clock holds more than the rate plus the burst.
func assertSpread(t *testing.T, what string, moments []time.Time, origin time.Time) {
	t.Helper()
	if len(moments) < 3 {
		t.Fatalf("%s: %d wakes, want a wave of at least three to read a spread from", what, len(moments))
	}
	if !moments[0].Equal(origin) {
		t.Fatalf("%s: the first wake is at %s, want %s: the burst lets it straight out", what, moments[0], origin)
	}
	equal := true
	for i := 1; i < len(moments); i++ {
		gap := moments[i].Sub(moments[i-1])
		if gap < testWakeInterval || gap >= 2*testWakeInterval {
			t.Fatalf("%s: wakes %d and %d are %s apart, want at least %s and less than %s",
				what, i-1, i, gap, testWakeInterval, 2*testWakeInterval)
		}
		if i > 1 && gap != moments[i-1].Sub(moments[i-2]) {
			equal = false
		}
	}
	if equal {
		t.Fatalf("%s: every gap is %s, want gaps that differ: each release waits a random part of an interval after its turn",
			what, moments[1].Sub(moments[0]))
	}
	for i, moment := range moments {
		inMinute := 0
		for _, other := range moments[i:] {
			if other.Sub(moment) < time.Minute {
				inMinute++
			}
		}
		if inMinute > testWakeRate+testWakeBurst {
			t.Fatalf("%s: the minute from %s holds %d wakes, want at most %d, the rate plus the burst",
				what, moment, inMinute, testWakeRate+testWakeBurst)
		}
	}
}

// wakeLines is every line a wake wrote as it ran (supervise's wakeRan), in order: of a machine's
// own gate, and of the outbox where a paced catch-up publishes (the outbox's wakeRan).
func wakeLines(t *testing.T, logged string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logged) {
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] == "supervise: wake ran" {
			lines = append(lines, entry)
		}
	}
	return lines
}

// wakeLineOf is the line the named claim's wake wrote, and whether it wrote one at all.
func wakeLineOf(t *testing.T, logged *syncBuffer, token claim.Token) (map[string]any, bool) {
	t.Helper()
	for _, line := range wakeLines(t, logged.String()) {
		if line["claim"] == string(token) {
			return line, true
		}
	}
	return nil, false
}

// assertWakeLines is the one line each wake of a paced wave writes where it runs: the kind of wake
// it was, whether it waited and for how long, that the pacing could have held it, and the rate and
// burst the operator set. The wake the burst let out says it waited for nothing; every other says
// how long it was held.
func assertWakeLines(t *testing.T, logged *syncBuffer, kind string, moments []time.Time) {
	t.Helper()
	lines := wakeLines(t, logged.String())
	if len(lines) != len(moments) {
		t.Fatalf("wake lines = %d, want one for each of the wave's %d wakes", len(lines), len(moments))
	}
	waited := 0
	for _, line := range lines {
		if line["kind"] != kind {
			t.Fatalf("a wake line says kind %v, want %q: %v", line["kind"], kind, line)
		}
		if line["paced"] != true || line["rate"] != float64(testWakeRate) || line["burst"] != float64(testWakeBurst) {
			t.Fatalf("a wake line says paced %v at rate %v burst %v, want true at %d and %d: %v",
				line["paced"], line["rate"], line["burst"], testWakeRate, testWakeBurst, line)
		}
		if line["waited"] == true {
			waited++
			held, ok := line["waitedFor"].(float64)
			if !ok || time.Duration(held) < testWakeInterval {
				t.Fatalf("a wake that waited says it waited %v, want at least one interval, %s: %v",
					line["waitedFor"], testWakeInterval, line)
			}
			continue
		}
		if _, says := line["waitedFor"]; says {
			t.Fatalf("a wake that did not wait says waitedFor %v, want the field left out: %v", line["waitedFor"], line)
		}
	}
	if waited != len(moments)-1 {
		t.Fatalf("%d of %d wake lines say they waited, want every one but the burst's", waited, len(lines))
	}
}

// runWave advances the clock to each waiting wake's turn, one turn at a time, and requires that
// turn to run that wake's launch and no other: nothing goes before its moment, and what the state
// promised for a moment is what happens at it. before is how many launches the wave had already
// made when the queue was read.
func runWave(t *testing.T, clock *waveClock, rt *waveRuntime, waiting []api.WakeWaiting, before int) []waveLaunch {
	t.Helper()
	for i, wake := range waiting {
		clock.advanceTo(wake.DueAt)
		want := before + i + 1
		testwait.Eventually(t, fmt.Sprintf("the launch of %s at its turn, %s", wake.Claim, wake.DueAt), func() bool {
			return len(rt.launches()) >= want
		})
		launches := rt.launches()
		if len(launches) != want {
			t.Fatalf("%d launches at %s, want %d: one wake's turn runs one wake", len(launches), wake.DueAt, want)
		}
		if got := launches[want-1]; got.claim != wake.Claim || !got.at.Equal(wake.DueAt) {
			t.Fatalf("the launch at %s is %s at %s, want %s at its own turn", wake.DueAt, got.claim, got.at, wake.Claim)
		}
	}
	return rt.launches()
}

// Forty launches a restart raises in one instant — every claim of the roll, launching with no
// process recorded, relaunched by boot (launchUnfinished) — go out at the operator's pace: the
// burst lets the first out at once, the state names every other with the turn the limiter gave it,
// no minute of the test clock holds more than the rate plus the burst, the gaps differ because
// each release waits a random part of an interval after its turn, and each claim's process starts
// at its own turn and not before. Each wake writes the one line an operator counts a minute's
// wakes from.
//
// The negative control is the pacing reverted for this case, the rate set to 0
// (TestWithNoPacingTheBootWaveGoesOutInOneTick): all forty launches then go out inside one tick of
// the test clock, which is the restart of 2026-11-04.
func TestABootWaveOfLaunchesGoesOutAtTheOperatorsPace(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)

	waiting := awaitWave(t, d, waveSize)
	for _, wake := range waiting {
		if wake.Kind != "launch" || !slices.Contains(tokens, wake.Claim) {
			t.Fatalf("the state names a waiting wake %+v, want a launch of one of the wave's claims", wake)
		}
		if wake.Issue != waveIssue(cfg, slices.Index(tokens, wake.Claim)+1) || wake.Role != claim.RoleImplementer {
			t.Fatalf("the waiting wake %+v does not name its claim's issue and role", wake)
		}
	}
	first := rt.launches()
	if len(first) != 1 {
		t.Fatalf("%d launches while the pacing holds the wave, want the one the burst let out", len(first))
	}
	if !first[0].at.Equal(origin) {
		t.Fatalf("the burst's launch is at %s, want the instant the wave was raised, %s", first[0].at, origin)
	}

	moments := wakeMoments(origin, waiting)
	assertSpread(t, "the boot wave's launches", moments, origin)

	launches := runWave(t, clock, rt, waiting, 1)
	launched := map[claim.Token]int{}
	for _, launch := range launches {
		launched[launch.claim]++
	}
	for _, token := range tokens {
		if launched[token] != 1 {
			t.Fatalf("%s launched %d times, want once: every claim of the wave is woken exactly once", token, launched[token])
		}
	}
	if left := waitingWakes(t, d); len(left) != 0 {
		t.Fatalf("wakesWaiting once the wave has gone = %+v, want none", left)
	}
	assertWakeLines(t, logged, "launch", moments)
}

// waveIssue is the issue of the wave's ith claim, counted from one as waveClaims counts them.
func waveIssue(cfg config.Config, i int) string {
	return fmt.Sprintf("%s-%d", cfg.Project, i)
}

// Forty re-admissions whose architects are all still running write forty catch-ups in place of
// forty launches, entered one at a time as the outbox runs one start row after another, and the
// rows they write hold the whole wave's schedule: the first is due at once, the gaps are at
// least one interval and less than two and they differ, no minute of the test clock holds more
// than the rate plus the burst, and the wakes come in the order they became ready — row by row,
// since this lane enters them one at a time and the row ids are that entry order. The workflow
// wake of a single issue written in the middle of the wave takes no token at all: it is due at
// once, unpaced, and the wave behind it keeps its own unbroken spacing.
//
// The negative control is the pacing reverted for this lane, the engine given no Reserve at all
// (TestWithNoPacingTheCatchUpWaveIsWrittenDueAtOnce): all forty rows are then due in the same
// instant, which is forty architects reading their whole context in one minute.
func TestAWaveOfCatchUpsIsWrittenOnTheLimitersScheduleInTheOrderItBecameReady(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	now := func() time.Time { return wakeOrigin }
	engine := wakeEngine(records, pacedWakes(), now)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{engine, admit.New(records, engine, 2, "legion", quietLogger())}, now: now}

	// The wave: each re-admission's start row finds its architect already running and writes the
	// catch-up that wakes it in place of a launch.
	for i := 1; i <= waveSize; i++ {
		key := fmt.Sprintf("LEGION-%d", i)
		runningArchitect(t, pool, records, sup, key)
		if err := runner.execute(ctx, startRow(t, key, wakeOrigin)); err != nil {
			t.Fatalf("start the running architect of %s: %v", key, err)
		}
		// Half way through the wave, one issue's own ready: the workflow wake of a single issue,
		// which is not the wave the pacing exists for and takes nothing from the rate.
		if i == waveSize/2 {
			single := fmt.Sprintf("LEGION-%d", waveSize+i)
			runningArchitect(t, pool, records, sup, single)
			if _, err := intake.ApplyFact(ctx, pool, "test", "ready:"+single,
				intake.ClaimReady{Issue: single, Role: claim.RoleArchitect, Launch: 1}, engine); err != nil {
				t.Fatalf("apply the ready of %s: %v", single, err)
			}
		}
	}

	rows := noticeRows(t, pool)
	if len(rows) != waveSize+1 {
		t.Fatalf("notice rows = %d, want the wave's %d catch-ups and the single issue's own wake", len(rows), waveSize)
	}
	var paced []noticeRow
	var single []noticeRow
	for _, row := range rows {
		if row.paced() {
			paced = append(paced, row)
			continue
		}
		single = append(single, row)
	}
	if len(single) != 1 || !single[0].nextAt.Equal(wakeOrigin) {
		t.Fatalf("the unpaced rows are %+v, want the single issue's own wake alone, due at once at %s: a workflow notice of one issue takes no token",
			single, wakeOrigin)
	}

	// The rows are read in id order, which is the order the lane entered them at the limiter, so
	// a schedule that rises row by row is the wave going out in the order it became ready.
	moments := make([]time.Time, 0, len(paced))
	for _, row := range paced {
		moments = append(moments, row.nextAt)
	}
	assertSpread(t, "the catch-up wave's rows", moments, wakeOrigin)
}

// The negative control of the catch-up case: the engine with no Reserve, which is the daemon's
// own wiring when the rate is 0 (openWorkflow). Every catch-up of the wave is then written due at
// once, and the architects it wakes all read their context in the same minute.
func TestWithNoPacingTheCatchUpWaveIsWrittenDueAtOnce(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	now := func() time.Time { return wakeOrigin }
	engine := wakeEngine(records, nil, now)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{engine, admit.New(records, engine, 2, "legion", quietLogger())}, now: now}

	for i := 1; i <= waveSize; i++ {
		key := fmt.Sprintf("LEGION-%d", i)
		runningArchitect(t, pool, records, sup, key)
		if err := runner.execute(ctx, startRow(t, key, wakeOrigin)); err != nil {
			t.Fatalf("start the running architect of %s: %v", key, err)
		}
	}

	rows := noticeRows(t, pool)
	if len(rows) != waveSize {
		t.Fatalf("notice rows = %d, want one catch-up each", len(rows))
	}
	for _, row := range rows {
		if row.paced() || !row.nextAt.Equal(wakeOrigin) {
			t.Fatalf("row %d reserved %s and is due at %s, want no reservation and due at once at %s", row.id, row.pacedAt, row.nextAt, wakeOrigin)
		}
	}
}

// A boot that finds forty live claims on a plugin contract this daemon has moved past queues each
// one an update task — the wake the stale-plugin lane causes, which wakes a live session in place
// of a launch — at the operator's pace: the state names every one of them as a `plugin-task` wake
// with the turn the limiter gave it, the schedule is spread like any other wave's, and each
// claim's own round opens at its own turn rather than at boot. The boot's own start returns inside
// the thirty seconds startWaveDaemon allows it, with the posts still going out, and the API serves
// every read this test makes while the wave waits. The sweep's next turn runs meanwhile: it reaps
// orphans again and carries the one round already open and answered to its end, while the forty
// claims of the wave still wait their turns.
//
// The negative control is the pacing reverted for this case, the rate set to 0
// (TestWithNoPacingTheStalePluginWaveOpensEveryRoundInOneTick): all forty rounds then open inside
// one tick of the test clock, which is forty live sessions woken in the same minute.
func TestAStalePluginWaveQueuesItsUpdateTasksAtTheOperatorsPace(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		return liveClaim(i, token, supervise.PodUpdateContract)
	})
	answered := answeredPluginRound(t, cfg)
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()
	reaped := func() int { return len(rt.fake().CallsOf("ReconcileOrphans")) }

	d := startWaveDaemon(t, cfg, clock, rt, logged, func(o *overrides) { o.orphanSweep = 100 * time.Millisecond })

	waiting := awaitWave(t, d, waveSize)
	for _, wake := range waiting {
		if wake.Kind != "plugin-task" || !slices.Contains(tokens, wake.Claim) {
			t.Fatalf("the state names a waiting wake %+v, want a plugin task of one of the wave's claims", wake)
		}
	}
	if launched := rt.launches(); len(launched) != 0 {
		t.Fatalf("%d launches while the plugin wave waits, want none: the round's task wakes the session that runs", len(launched))
	}
	round := supervise.PluginRound(api.DaemonAPIVersion)
	for _, wake := range waiting {
		if open := d.claim(wake.Claim).UpdateRound; open != "" {
			t.Fatalf("%s is already in round %q while its update task waits its turn, want none until that turn", wake.Claim, open)
		}
	}
	if open := openRounds(t, d, tokens, round); open != 1 {
		t.Fatalf("%d of the wave's rounds are open while it waits, want the one the burst let out", open)
	}

	// The sweep runs while the wave waits: it reaps again, and the round already answered is
	// carried to its end — a claim with no process, so it ends launching nothing.
	atBoot := reaped()
	testwait.Eventually(t, "the sweep's next turn to reap the runtime's orphans again", func() bool { return reaped() > atBoot })
	testwait.Eventually(t, "the sweep's next turn to carry the answered round to its end", func() bool {
		return d.claim(answered).UpdateRound == ""
	})
	if got := waitingWakes(t, d); len(got) != len(waiting) {
		t.Fatalf("%d wakes wait after the sweep's turn, want the %d it found waiting", len(got), len(waiting))
	}

	moments := wakeMoments(origin, waiting)
	assertSpread(t, "the stale-plugin wave's update tasks", moments, origin)
	opened := 1
	for _, wake := range waiting {
		clock.advanceTo(wake.DueAt)
		opened++
		testwait.Eventually(t, fmt.Sprintf("the update round of %s to open at its turn", wake.Claim), func() bool {
			return d.claim(wake.Claim).UpdateRound == round
		})
		if got := openRounds(t, d, tokens, round); got != opened {
			t.Fatalf("%d of the wave's rounds are open at %s, want %d: one turn opens one round", got, wake.DueAt, opened)
		}
	}
	if launched := rt.launches(); len(launched) != 0 {
		t.Fatalf("the plugin wave launched %d processes, want none: every worker was asked to push first", len(launched))
	}
	assertWakeLines(t, logged, "plugin-task", moments)
}

// answeredPluginRound stores a claim of cfg's project whose own plugin round is open and already
// answered, its process let go by the suspension that followed: the round the sweep has left to
// carry while a wave waits. It returns its token.
func answeredPluginRound(t *testing.T, cfg config.Config) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	issue := cfg.Project + "-900"
	token, err := claim.NewToken(project, issue, claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	answeredAt := time.Now().UTC()
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleTester, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_answered",
		SessionFile: "/sessions/answered.jsonl", PluginContract: supervise.PodUpdateContract,
		UpdateRound: supervise.PluginRound(api.DaemonAPIVersion), UpdateReasons: "the process speaks an earlier plugin contract",
		UpdateOpenedAt: &answeredAt, UpdateSignalledAt: &answeredAt,
	})
	writePrompt(t, cfg, token)
	return token
}

// openRounds is how many of tokens are in the round named.
func openRounds(t *testing.T, d *daemon, tokens []claim.Token, round string) int {
	t.Helper()
	open := 0
	for _, token := range tokens {
		if d.claim(token).UpdateRound == round {
			open++
		}
	}
	return open
}

// The negative control of the stale-plugin case: the pacing reverted, the rate at 0, and every one
// of the forty rounds opens inside one tick of the test clock — forty live sessions woken in the
// same minute, which is the plugin wave the cap counts.
func TestWithNoPacingTheStalePluginWaveOpensEveryRoundInOneTick(t *testing.T) {
	cfg := waveConfig(t)
	cfg.WorkerWakesPerMinute = 0
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		return liveClaim(i, token, supervise.PodUpdateContract)
	})
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	round := supervise.PluginRound(api.DaemonAPIVersion)

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)

	testwait.Eventually(t, "every round of the unpaced plugin wave to open", func() bool {
		return openRounds(t, d, tokens, round) == len(tokens)
	})
	if waiting := waitingWakes(t, d); len(waiting) != 0 {
		t.Fatalf("wakesWaiting with the pacing off = %+v, want none", waiting)
	}
}

// The negative control of every c-spread launch case: the pacing reverted, the rate set to 0, so
// the limiter releases every wake at its own now. The whole wave then launches inside one tick of
// the test clock — the restart of 2026-11-04 — which is what the paced cases must not do, and
// this test holds the daemon to it so that a pacing silently turned off is not read as a pacing
// that works.
func TestWithNoPacingTheBootWaveGoesOutInOneTick(t *testing.T) {
	cfg := waveConfig(t)
	cfg.WorkerWakesPerMinute = 0
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)

	testwait.Eventually(t, "every launch of the unpaced wave", func() bool { return len(rt.launches()) == len(tokens) })
	for _, launch := range rt.launches() {
		if !launch.at.Equal(origin) {
			t.Fatalf("an unpaced launch is at %s, want the one instant %s: with the pacing off nothing waits", launch.at, origin)
		}
	}
	if waiting := waitingWakes(t, d); len(waiting) != 0 {
		t.Fatalf("wakesWaiting with the pacing off = %+v, want none", waiting)
	}
	for _, line := range wakeLines(t, logged.String()) {
		if line["paced"] != false || line["waited"] != false {
			t.Fatalf("a wake line of an unpaced daemon says paced %v waited %v, want both false: %v",
				line["paced"], line["waited"], line)
		}
	}
}

// A pod roll displaces forty processes at once — the Sandbox runtime replacing the pods their
// panes ran in, which reaches the launch through displaced and replaceProcess rather than through
// any request — and that wave is paced like the restart's: the first relaunch goes at once, the
// rest wait their turns, no minute holds more than the rate plus the burst, and each claim's
// recorded session is resumed at its own turn. A displacement is not a death, so none of these
// claims is charged, and the wave is raised with no operator asking for anything.
//
// The negative control is this same wave with the rate at 0
// (TestWithNoPacingTheDisplacedWaveGoesOutInOneTick): all forty resume inside one tick.
func TestADisplacedWaveOfLaunchesGoesOutAtTheOperatorsPace(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		return liveClaim(i, token, api.DaemonAPIVersion)
	})
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)
	testwait.Eventually(t, "every re-adopted claim to be supervised", func() bool {
		return len(d.state().Issues) == waveSize
	})
	if launched := rt.launches(); len(launched) != 0 {
		t.Fatalf("%d launches on a boot that re-adopts every claim, want none", len(launched))
	}

	origin := clock.Now()
	for _, token := range tokens {
		c := d.claim(token)
		rt.fake().Emit(runtime.Observation{Locator: *c.Locator, Kind: runtime.Gone, At: origin, Displaced: true})
	}

	waiting := awaitWave(t, d, waveSize)
	moments := wakeMoments(origin, waiting)
	assertSpread(t, "the displaced wave's launches", moments, origin)
	launches := runWave(t, clock, rt, waiting, 1)
	for _, launch := range launches {
		if !launch.resumed {
			t.Fatalf("%s was spawned rather than resumed; a displaced claim comes back on its recorded session", launch.claim)
		}
	}
	for _, token := range tokens {
		if c := d.claim(token); c.Budgets.LaunchFailures != 0 {
			t.Fatalf("%s is charged %d launch failures, want none: a pod the daemon replaced is no death",
				token, c.Budgets.LaunchFailures)
		}
	}
	assertWakeLines(t, logged, "launch", moments)
}

// The negative control of the displaced case: the pacing reverted, the rate at 0, and the whole
// roll's forty relaunches go out inside one tick of the test clock.
func TestWithNoPacingTheDisplacedWaveGoesOutInOneTick(t *testing.T) {
	cfg := waveConfig(t)
	cfg.WorkerWakesPerMinute = 0
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		return liveClaim(i, token, api.DaemonAPIVersion)
	})
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)
	testwait.Eventually(t, "every re-adopted claim to be supervised", func() bool {
		return len(d.state().Issues) == waveSize
	})

	origin := clock.Now()
	for _, token := range tokens {
		c := d.claim(token)
		rt.fake().Emit(runtime.Observation{Locator: *c.Locator, Kind: runtime.Gone, At: origin, Displaced: true})
	}

	testwait.Eventually(t, "every relaunch of the unpaced roll", func() bool { return len(rt.launches()) == len(tokens) })
	for _, launch := range rt.launches() {
		if !launch.at.Equal(origin) {
			t.Fatalf("an unpaced relaunch is at %s, want the one instant %s", launch.at, origin)
		}
	}
	if waiting := waitingWakes(t, d); len(waiting) != 0 {
		t.Fatalf("wakesWaiting with the pacing off = %+v, want none", waiting)
	}
}

// An operator's resume through the daemon's API and the project controller's own keeper ask for a
// wake while forty wait: both start at once, neither takes anything from the rate — every waiting
// wake keeps the turn it already held, and the wave's own releases stay unbroken from the instant
// it was raised — and each writes a line that says the pacing could not have held it.
//
// The negative control is the two exemptions reverted, which no configuration reaches: with the
// rate at 0 the whole wave goes out in one tick (TestWithNoPacingTheBootWaveGoesOutInOneTick), and
// a non-exempt operator resume would leave the resumed claim parked launching with the state
// naming its wake, which this test's own assertions would then read.
func TestAnOperatorsResumeAndTheControllersOwnWakeNeverWait(t *testing.T) {
	cfg := waveConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	// A claim of this project the operator suspended, and the controller's own claim, retired by
	// a daemon that could not keep it: the keeper retries it on a wait of its own, which falls
	// while the wave is still waiting.
	suspended, err := claim.NewToken(project, cfg.Project+"-901", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	putClaim(t, cfg, supervise.Claim{
		Token: suspended, Project: project, Tree: cfg.Project + "-901", Issue: cfg.Project + "-901",
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator,
		Session: "ses_suspended", SessionFile: "/sessions/suspended.jsonl", PluginContract: api.DaemonAPIVersion,
	})
	writePrompt(t, cfg, suspended)
	controller := claim.ControllerToken(project)
	putClaim(t, cfg, supervise.Claim{
		Token: controller, Project: project, Role: claim.RoleController, Generation: 1, State: supervise.StateRetired,
	})
	writePrompt(t, cfg, controller)

	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()
	d := startWaveDaemon(t, cfg, clock, rt, logged, func(o *overrides) {
		o.orphanSweep = 100 * time.Millisecond
		o.controllerRetry = 200 * time.Millisecond
	})

	waiting := awaitWave(t, d, waveSize)
	promised := map[claim.Token]time.Time{}
	for _, wake := range waiting {
		promised[wake.Claim] = wake.DueAt
	}

	// The operator's resume: answered with a running claim, its process started at once, with the
	// whole wave still waiting and the clock where it was.
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(suspended)+"/resume", nil, true)
	if status != http.StatusOK {
		t.Fatalf("the operator's resume = %d; body %s", status, body)
	}
	if got := rt.launchesOf(suspended); got != 1 {
		t.Fatalf("the operator's resume started %d processes, want one: an operator's wake never waits", got)
	}
	if c := d.claim(suspended); c.State == string(supervise.StateSuspended) {
		t.Fatalf("the resumed claim is still %s, want its launch made at once", c.State)
	}

	// The controller's keeper: its retry comes while the wave waits, and the controller's own
	// claim is the one wake the pacing never holds whoever asks for it.
	testwait.Eventually(t, "the controller's keeper to launch the project's controller", func() bool {
		return rt.launchesOf(controller) == 1
	})
	for _, launch := range rt.launches() {
		if (launch.claim == controller || launch.claim == suspended) && !launch.at.Equal(origin) {
			t.Fatalf("the exempt launch of %s is at %s, want the instant it was asked for, %s: it waited for a turn",
				launch.claim, launch.at, origin)
		}
	}
	for _, token := range []claim.Token{controller, suspended} {
		line, wrote := wakeLineOf(t, logged, token)
		if !wrote {
			t.Fatalf("%s wrote no wake line, want the one every wake writes where it runs", token)
		}
		if line["paced"] != false || line["waited"] != false {
			t.Fatalf("the wake line of %s says paced %v waited %v, want both false: no pacing could have held it: %v",
				token, line["paced"], line["waited"], line)
		}
	}

	// Neither took anything from the rate: every wake that was waiting is still due at the moment
	// it already held, and the wave's own releases are unbroken — an exempt wake that had taken a
	// token would leave a gap of two intervals or more where its turn fell.
	after := waitingWakes(t, d)
	if len(after) != len(waiting) {
		t.Fatalf("%d wakes wait after the two exempt ones, want the %d that waited before", len(after), len(waiting))
	}
	for _, wake := range after {
		if was, waited := promised[wake.Claim]; !waited || !wake.DueAt.Equal(was) {
			t.Fatalf("the wake of %s is due at %s, want the turn it already held, %s", wake.Claim, wake.DueAt, was)
		}
	}
	assertSpread(t, "the wave the exempt wakes went around", wakeMoments(origin, after), origin)
	runWave(t, clock, rt, after, rt.launchesOf(controller)+rt.launchesOf(suspended)+1)
	for _, token := range tokens {
		if got := rt.launchesOf(token); got != 1 {
			t.Fatalf("%s launched %d times, want once: the wave kept its own spacing after the exempt wakes", token, got)
		}
	}
}

// Every event that reaches a claim the pacing holds is decided while the wave still waits, with
// the clock where the wave left it: a suspend suspends its claim where it arrives, a stop and a
// tree close retire theirs, each of the three taking that claim's wake with it, and a task
// delivered to a claim whose turn has not come is queued as it is for any launch in flight. None
// of them waits for a turn, and none of them moves anyone else's: every wake still waiting is due
// at the moment it already held. Then the turns come: the three claims whose wakes went launch
// nothing at theirs, and the drops cost the queue nothing — every wake behind them runs at the
// turn it already had.
//
// The suspend is the one that waited until this issue's tester found it. A claim the gate parked
// launching with no process holds no process for a suspension to protect, and over a wave its turn
// is tens of minutes out, so a drain answered 202 and applied only at that turn leaves the
// operator waiting that long — and a restart inside it forgets the hold, which lives in memory
// alone, and relaunches the claim the operator told it to drain
// (supervise's holdSuspendUntilReady, and the restart's own proof below).
//
// The negative control is that wait put back, the suspension held for the claim's turn: the route
// then answers 202 with the claim still launching, which this test reads at the suspend itself,
// and the claim is suspended only when its turn comes.
func TestEveryEventOnAWaitingClaimIsDecidedWhileTheWakesStillWait(t *testing.T) {
	cfg := waveConfig(t)
	waveClaims(t, cfg, claim.RoleArchitect, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	// Two claims of the wave that are not their trees' roots, so one of them is always waiting when
	// the burst has let one wake out: an operator's stop is refused for a root, whose tree's close
	// is what ends it, and the close is refused for anything else.
	workers := waveWorkers(t, cfg, 2)
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)

	waiting := awaitWave(t, d, waveSize+len(workers))
	promised := map[claim.Token]time.Time{}
	roots := make([]claim.Token, 0, waveSize)
	var stopped claim.Token
	for _, wake := range waiting {
		promised[wake.Claim] = wake.DueAt
		if slices.Contains(workers, wake.Claim) {
			if stopped == "" {
				stopped = wake.Claim
			}
			continue
		}
		roots = append(roots, wake.Claim)
	}
	if stopped == "" || len(roots) < 3 {
		t.Fatalf("the queue holds %d roots and the stoppable claim %q, want three roots and one claim that is not a root", len(roots), stopped)
	}
	drained, closed, delivered := roots[0], roots[1], roots[2]

	// The suspend: decided where it arrives, because the claim it reaches records no process for
	// the suspension to protect. The claim is suspended by the operator at once, in the store as
	// well as in memory, and its wake goes with it, as a stop's and a close's do.
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(drained)+"/suspend", nil, true)
	if status != http.StatusOK {
		t.Fatalf("the suspend of a waiting claim = %d; body %s, want 200 with the claim suspended", status, body)
	}
	if c := d.claim(drained); c.State != string(supervise.StateSuspended) || c.SuspendedBy != string(supervise.SuspendedByOperator) || c.SuspensionHeld {
		t.Fatalf("the suspended waiting claim is %s suspended by %q with suspensionHeld %v, want suspended by the operator with nothing held",
			c.State, c.SuspendedBy, c.SuspensionHeld)
	}
	if stored := storedClaim(t, cfg, drained); stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the stored claim is %s suspended by %q, want suspended by the operator: a restart must relaunch no drained claim",
			stored.State, stored.SuspendedBy)
	}

	// The stop and the tree close: each retires its claim at once, and the retirement takes the
	// wake with it (letGo).
	for _, each := range []struct {
		token claim.Token
		route string
	}{{stopped, "stop"}, {closed, "close"}} {
		status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(each.token)+"/"+each.route, nil, true)
		if status != http.StatusOK {
			t.Fatalf("the %s of a waiting claim = %d; body %s", each.route, status, body)
		}
		if c := d.claim(each.token); c.State != string(supervise.StateRetired) {
			t.Fatalf("the %s left the waiting claim %s, want retired", each.route, c.State)
		}
	}

	// What a decision that asks whether the claim runs something sees: a claim parked with no
	// process is written launching with no locator, which is a launch in flight, so the task is
	// queued for the ready that follows rather than ignored or launched a second time.
	status, body = d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(delivered)+"/deliver",
		api.DeliverRequest{Task: "read the plan"}, true)
	if status != http.StatusOK {
		t.Fatalf("the deliver to a waiting claim = %d; body %s", status, body)
	}
	parked := d.claim(delivered)
	if parked.State != string(supervise.StateLaunching) || parked.Locator != nil || parked.Generation != 1 {
		t.Fatalf("the waiting claim is %+v, want launching at its generation with no process: what a launch in flight looks like", parked)
	}
	if parked.Pending == nil || parked.Pending.DeliveredAt != nil {
		t.Fatalf("the task delivered to a waiting claim is %+v, want it queued for the ready that follows", parked.Pending)
	}

	// Nothing waited for a turn: the clock is where the wave left it, and every wake still in the
	// queue is due at the moment it already held.
	if now := clock.Now(); !now.Equal(origin) {
		t.Fatalf("the test clock is at %s, want %s: every event was decided without a wake's turn coming", now, origin)
	}
	after := waitingWakes(t, d)
	if len(after) != len(waiting)-3 {
		t.Fatalf("%d wakes wait after the three events, want %d: the suspend, the stop and the close took their own claims' wakes and no others",
			len(after), len(waiting)-3)
	}
	for _, wake := range after {
		if wake.Claim == drained || wake.Claim == stopped || wake.Claim == closed {
			t.Fatalf("the wake of the decided claim %s still waits, want it gone with the decision", wake.Claim)
		}
		if was := promised[wake.Claim]; !wake.DueAt.Equal(was) {
			t.Fatalf("the wake of %s is due at %s, want the turn it already held, %s", wake.Claim, wake.DueAt, was)
		}
	}

	// Each turn in order: the three claims whose wakes went launch nothing at the turns those
	// wakes held, and every wake behind them runs at the turn it already held — a dropped wake
	// frees nothing and takes nothing.
	launched := 1
	for _, wake := range waiting {
		clock.advanceTo(wake.DueAt)
		switch wake.Claim {
		case drained, stopped, closed:
			if got := rt.launchesOf(wake.Claim); got != 0 {
				t.Fatalf("the claim %s, whose wake went with the event that decided it, launched %d times at the turn that wake held, want none",
					wake.Claim, got)
			}
		default:
			launched++
			testwait.Eventually(t, fmt.Sprintf("the launch of %s at its own turn %s", wake.Claim, wake.DueAt), func() bool {
				return rt.launchesOf(wake.Claim) == 1
			})
			launches := rt.launches()
			if got := launches[len(launches)-1]; !got.at.Equal(wake.DueAt) {
				t.Fatalf("the launch of %s is at %s, want the turn it held, %s: the three events moved nobody's turn",
					wake.Claim, got.at, wake.DueAt)
			}
		}
	}
	if got := len(rt.launches()); got != launched {
		t.Fatalf("%d launches once every turn has come, want %d: the wave less the suspended and the two retired claims", got, launched)
	}
	if c := d.claim(drained); c.State != string(supervise.StateSuspended) {
		t.Fatalf("the claim suspended while its wake waited is %s once every turn has come, want it left %s",
			c.State, supervise.StateSuspended)
	}
	if left := waitingWakes(t, d); len(left) != 0 {
		t.Fatalf("wakesWaiting once every turn has come = %+v, want none", left)
	}
}

// A relaunch of a claim whose process still runs lets that process go no earlier than it does
// today: the gate leaves such a claim exactly as it stands — idle, with its process recorded — so
// the process runs until the wake's turn, when the relaunch stops it and starts the next one. No
// sweep takes it for an orphan meanwhile: the claim still records it, so every reconciliation the
// sweep runs knows it. The wave here is the stale-plugin lane's other branch, the relaunch of a
// worker whose plugin cannot answer an update round at all, which is a relaunch no request raised.
//
// The negative control is the pacing reverted, the rate at 0: every one of the forty processes is
// then stopped and replaced inside one tick of the test clock
// (TestWithNoPacingTheStaleRelaunchWaveReplacesEveryProcessInOneTick).
func TestARelaunchKeepsTheRunningProcessUntilItsTurnAndNoSweepReapsIt(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		c := liveClaim(i, token, supervise.PodUpdateContract-1)
		// Idle rather than working: a worker in a turn has its relaunch held for that turn's end
		// (holdStaleRelaunch), which is a wait of its own and not the pacing's.
		c.State = supervise.StateIdle
		return c
	})
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	d := startWaveDaemon(t, cfg, clock, rt, logged, func(o *overrides) { o.orphanSweep = 100 * time.Millisecond })

	waiting := awaitWave(t, d, waveSize)
	running := map[claim.Token]runtime.Locator{}
	for _, wake := range waiting {
		c := d.claim(wake.Claim)
		if c.State != string(supervise.StateIdle) || c.Locator == nil {
			t.Fatalf("the claim %s whose relaunch waits is %s with locator %+v, want it left idle on the process it still runs",
				wake.Claim, c.State, c.Locator)
		}
		running[wake.Claim] = *c.Locator
	}
	// Nothing of a waiting claim's process has been touched: the relaunch hands the process it
	// replaces to the runtime as the previous incarnation of its own launch (Resume's previous),
	// and that call is the first and only thing that ends it.
	for _, call := range rt.fake().Calls() {
		if _, waiting := running[call.Spec.Claim]; waiting && call.Method != "ReconcileOrphans" {
			t.Fatalf("the runtime was asked to %s %s while its relaunch waits, want nothing: the process runs until its turn",
				call.Method, call.Spec.Claim)
		}
	}

	// The sweep knows every waiting claim's process, so nothing reaps it: a reconciliation that
	// did not know it would end it as an orphan of no claim.
	testwait.Eventually(t, "a sweep's reconciliation after the wave was raised", func() bool {
		return len(rt.fake().CallsOf("ReconcileOrphans")) > 1
	})
	calls := rt.fake().CallsOf("ReconcileOrphans")
	known := map[claim.Token]runtime.Locator{}
	for _, each := range calls[len(calls)-1].Known {
		if each.Locator != nil {
			known[each.Claim] = *each.Locator
		}
	}
	for token, locator := range running {
		if got, told := known[token]; !told || got.Incarnation != locator.Incarnation {
			t.Fatalf("the sweep was told %+v of %s, want the process it still runs, %s: an unknown process is reaped as an orphan",
				got, token, locator.Incarnation)
		}
	}

	moments := wakeMoments(origin, waiting)
	assertSpread(t, "the stale-plugin relaunch wave", moments, origin)
	for _, wake := range waiting {
		clock.advanceTo(wake.DueAt)
		testwait.Eventually(t, fmt.Sprintf("the relaunch of %s at its turn", wake.Claim), func() bool {
			return rt.launchesOf(wake.Claim) == 1
		})
		launches := rt.launches()
		if got := launches[len(launches)-1]; got.claim != wake.Claim || !got.at.Equal(wake.DueAt) {
			t.Fatalf("the relaunch at %s is of %s at %s, want %s at its own turn", wake.DueAt, got.claim, got.at, wake.Claim)
		}
		was := running[wake.Claim]
		if previous := resumedPrevious(t, rt, wake.Claim); previous.Incarnation != was.Incarnation {
			t.Fatalf("the relaunch of %s replaced the process at %s, want the one it still ran, %s: the process is let go at its turn and no earlier",
				wake.Claim, previous.Incarnation, was.Incarnation)
		}
	}
	for _, token := range tokens {
		if got := rt.launchesOf(token); got != 1 {
			t.Fatalf("%s was relaunched %d times, want once", token, got)
		}
	}
	assertWakeLines(t, logged, "launch", moments)
}

// resumedPrevious is the incarnation the claim's latest resume replaced: the process the relaunch
// let go as it started the next one.
func resumedPrevious(t *testing.T, rt *waveRuntime, token claim.Token) runtime.Locator {
	t.Helper()
	calls := rt.fake().CallsOf("Resume")
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Spec.Claim != token {
			continue
		}
		if calls[i].Previous == nil {
			t.Fatalf("the resume of %s recorded no previous incarnation, want the process it replaced", token)
		}
		return *calls[i].Previous
	}
	t.Fatalf("the runtime was asked for no resume of %s", token)
	return runtime.Locator{}
}

// The negative control of the stale-plugin relaunch case: the pacing reverted, the rate at 0, and
// every one of the forty running processes is stopped and replaced inside one tick of the test
// clock.
func TestWithNoPacingTheStaleRelaunchWaveReplacesEveryProcessInOneTick(t *testing.T) {
	cfg := waveConfig(t)
	cfg.WorkerWakesPerMinute = 0
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, token claim.Token) supervise.Claim {
		c := liveClaim(i, token, supervise.PodUpdateContract-1)
		c.State = supervise.StateIdle
		return c
	})
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)
	origin := clock.Now()

	startWaveDaemon(t, cfg, clock, rt, logged, nil)

	testwait.Eventually(t, "every relaunch of the unpaced stale-plugin wave", func() bool {
		return len(rt.launches()) == len(tokens)
	})
	for _, launch := range rt.launches() {
		if !launch.at.Equal(origin) {
			t.Fatalf("an unpaced relaunch is at %s, want the one instant %s", launch.at, origin)
		}
	}
}

// A restart while the pacing still holds a wave loses no wake and runs none twice: the claims the
// first daemon launched are re-adopted, the claims whose turn had not come are still launching
// with no process recorded, and the second daemon raises them again as its own wave, at its own
// pace. Counted over both daemons, every claim of the wave is launched exactly once.
//
// The negative control is the pacing reverted, the rate at 0: the first daemon then launches the
// whole wave before the restart (TestWithNoPacingTheBootWaveGoesOutInOneTick), and there is no
// wave left for a restart to lose.
func TestARestartRaisesTheWakesStillWaitingAndEachHappensOnce(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)
	waiting := awaitWave(t, d, waveSize)

	// A few turns of the wave go out, and the daemon is stopped with the rest still waiting.
	const before = 3
	runWave(t, clock, rt, waiting[:before], 1)
	stillWaiting := waitingWakes(t, d)
	if len(stillWaiting) != waveSize-before-1 {
		t.Fatalf("%d wakes wait at the restart, want the %d the wave has left", len(stillWaiting), waveSize-before-1)
	}
	launchedFirst := map[claim.Token]int{}
	for _, launch := range rt.launches() {
		launchedFirst[launch.claim]++
	}
	d.stop()

	rebindHeldPorts(t, &cfg)
	restartClock, restartLog := newWaveClock(), &syncBuffer{}
	restarted := newWaveRuntime(restartClock)
	restartOrigin := restartClock.Now()
	second := startWaveDaemon(t, cfg, restartClock, restarted, restartLog, nil)

	// The wave the restart finds is exactly the wakes the stop left waiting: the claims already
	// launched record a process and are re-adopted, never launched again.
	again := awaitWave(t, second, len(stillWaiting))
	assertSpread(t, "the restart's own wave", wakeMoments(restartOrigin, again), restartOrigin)
	runWave(t, restartClock, restarted, again, 1)

	total := map[claim.Token]int{}
	for token, count := range launchedFirst {
		total[token] += count
	}
	for _, launch := range restarted.launches() {
		total[launch.claim]++
	}
	for _, token := range tokens {
		if total[token] != 1 {
			t.Fatalf("%s launched %d times over the restart, want exactly once: no wake of the wave is lost and none runs twice",
				token, total[token])
		}
	}
	if left := waitingWakes(t, second); len(left) != 0 {
		t.Fatalf("wakesWaiting once the restart's wave has gone = %+v, want none", left)
	}
}

// R2 of dispatch://INFRA-976/spec asks of a waiting wake that it lose nothing, not only that its
// claim still decide: an operator's suspend of a claim whose wake waits over no process writes no
// outbox row and, were it held, would live in the machine's memory alone, so a restart inside the
// tens of minutes such a wake waits would forget the drain while the claim stood persisted
// launching — and boot relaunches every unfinished launch it finds (launchUnfinished), the claim
// the operator drained among them. Decided where it arrives, the suspension is written: the
// restart finds the claim suspended, raises no wake for it and launches nothing of it, while
// every other claim of the wave is still woken exactly once.
//
// The negative control is the suspension held for the wake's turn instead: the claim is then
// stored launching when the daemon stops, the restart counts it among the unfinished launches,
// and it is relaunched — the drain undone by a restart nobody connected to it.
func TestASuspendDecidedWhileTheWakeWaitedSurvivesARestart(t *testing.T) {
	cfg := waveConfig(t)
	tokens := waveClaims(t, cfg, claim.RoleImplementer, func(i int, _ claim.Token) supervise.Claim { return unfinishedLaunch(t, i) })
	clock, logged := newWaveClock(), &syncBuffer{}
	rt := newWaveRuntime(clock)

	d := startWaveDaemon(t, cfg, clock, rt, logged, nil)
	waiting := awaitWave(t, d, waveSize)

	// The claim the operator drains is the last of the queue, whose turn is the whole wave away.
	drained := waiting[len(waiting)-1].Claim
	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(drained)+"/suspend", nil, true)
	if status != http.StatusOK {
		t.Fatalf("the suspend of a waiting claim = %d; body %s, want 200 with the claim suspended", status, body)
	}
	if stored := storedClaim(t, cfg, drained); stored.State != supervise.StateSuspended || stored.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the stored claim is %s suspended by %q before the restart, want %s by the operator",
			stored.State, stored.SuspendedBy, supervise.StateSuspended)
	}
	launchedFirst := map[claim.Token]int{}
	for _, launch := range rt.launches() {
		launchedFirst[launch.claim]++
	}
	d.stop()

	rebindHeldPorts(t, &cfg)
	restartClock, restartLog := newWaveClock(), &syncBuffer{}
	restarted := newWaveRuntime(restartClock)
	restartOrigin := restartClock.Now()
	second := startWaveDaemon(t, cfg, restartClock, restarted, restartLog, nil)

	// The drained claim comes back suspended, and the wave the restart raises is the rest of the
	// queue: the claim already launched records a process and is re-adopted, and the drained one is
	// no unfinished launch at all.
	if c := second.claim(drained); c.State != string(supervise.StateSuspended) || c.SuspendedBy != string(supervise.SuspendedByOperator) {
		t.Fatalf("after the restart the drained claim is %s suspended by %q, want %s by the operator",
			c.State, c.SuspendedBy, supervise.StateSuspended)
	}
	again := awaitWave(t, second, waveSize-2)
	for _, wake := range again {
		if wake.Claim == drained {
			t.Fatalf("the restart holds a wake for the drained claim, %+v, want none: it is suspended, not an unfinished launch", wake)
		}
	}
	assertSpread(t, "the restart's own wave", wakeMoments(restartOrigin, again), restartOrigin)
	runWave(t, restartClock, restarted, again, 1)

	total := map[claim.Token]int{}
	for token, count := range launchedFirst {
		total[token] += count
	}
	for _, launch := range restarted.launches() {
		total[launch.claim]++
	}
	for _, token := range tokens {
		want := 1
		if token == drained {
			want = 0
		}
		if total[token] != want {
			t.Fatalf("%s launched %d times over the restart, want %d: the drained claim is relaunched never and every other wake happens once",
				token, total[token], want)
		}
	}
	if c := second.claim(drained); c.State != string(supervise.StateSuspended) {
		t.Fatalf("the drained claim is %s once the restart's wave has gone, want it left %s", c.State, supervise.StateSuspended)
	}
	if left := waitingWakes(t, second); len(left) != 0 {
		t.Fatalf("wakesWaiting once the restart's wave has gone = %+v, want none", left)
	}
}

// pacedLaneTree is a lane tree whose claims' launches draw on a limiter a wave of forty has
// already spent, so the next launch this issue raises is held exactly as the fortieth of a roll
// is: the limiter is the daemon's own, at the shipped pace, and the machines keep the lane's own
// held clock so a wake's turn is a moment the test fires. The deps are set before any machine
// exists, which is how the daemon sets them (newSupervisor, then run's own Deps).
func pacedLaneTree(t *testing.T, log *slog.Logger) (*laneTree, *wakepace.Limiter) {
	t.Helper()
	tree := startLaneTree(t, log)
	limiter := pacedWakes()
	tree.runner.supervisor.deps.Clock = tree.clock
	tree.runner.supervisor.deps.WakePace = supervise.WakePace{Reserve: limiter.Reserve, Rate: testWakeRate, Burst: testWakeBurst}
	for range waveSize {
		limiter.Reserve(tree.clock.Now())
	}
	return tree, limiter
}

// waitingWakeOf is the wake the pacing holds over the issue's role, as the machine publishes it.
func waitingWakeOf(t *testing.T, tree *laneTree, role claim.Role) (*supervise.Machine, supervise.WakeWaiting) {
	t.Helper()
	token := mustClaimToken(t, tree.root.Key, role)
	machine, ok := tree.runner.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	wake := machine.View().WakeWaiting
	if wake == nil {
		t.Fatalf("the pacing holds no wake over %s, want the launch its start raised", token)
	}
	return machine, *wake
}

// The issue's lane runs on while the wake its start raised still waits: the start row finishes
// rather than holding the lane until the claim's turn, the suspend that follows it runs on the
// same lane and is decided there and then, and no row of the issue is ever held long enough to
// raise the daemon's held-row warning — two minutes of lease ticks pass with the wake still
// waiting and nothing is logged. The suspend of that claim is decided where it lands, since the
// claim records no process while its wake waits, and the wake goes with it.
//
// The negative control is this same wait held inside the machine's decision, where the gated
// runtime holds the architect's start: the suspend is then not decided until that start returns,
// and the issue's lane logs the row held behind it
// (TestARunningLifecycleRowLogsEachRowHeldBehindItOnceItHasWaitedLong pins that shape).
func TestALaneRunsOnWhileTheClaimsWakeWaits(t *testing.T) {
	var logged syncBuffer
	tree, _ := pacedLaneTree(t, slog.New(slog.NewJSONHandler(&logged, nil)))

	// Where the clock stands when the start runs is the moment the gate reserves from, so the wait
	// the wake's timer was armed for is its turn less this.
	reservedAt := tree.clock.Now()
	tree.commit(t, "architect-start", tree.start(t, claim.RoleArchitect))
	testwait.Eventually(t, "the start row to finish while the wake it raised waits", func() bool {
		return outboxRows(t, tree.pool) == 0
	})
	machine, wake := waitingWakeOf(t, tree, claim.RoleArchitect)
	if wake.Kind != "launch" || wake.At.Sub(tree.clock.Now()) < testWakeInterval {
		t.Fatalf("the wake the start raised is %+v, want a launch at least one interval out from %s", wake, tree.clock.Now())
	}
	if spawned := spawnedRoles(tree.rt); len(spawned) != 0 {
		t.Fatalf("the runtime launched %v while the wake waits, want nothing", spawned)
	}

	// A row of another lane of the same issue — the board lane's status write — runs while that
	// claim's wake still waits: the lanes of an issue are not held behind one another, and none of
	// them is held behind a wake.
	tree.commit(t, "board-status", mustOutboxRow(t, tree.root.Key,
		record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}, tree.clock.Now()))
	testwait.Eventually(t, "the board row of the same issue to run while the wake waits", func() bool {
		return tree.client.count() == 1 && outboxRows(t, tree.pool) == 0
	})

	// Two minutes of lease ticks with the wake still waiting — the harness holds every timer, the
	// wake's own among them — and no row of the issue is held behind a running one, so nothing is
	// logged for an operator to look at.
	for range int(heldWarnAfter/(outboxLease/3)) + 1 {
		tree.clock.advance(outboxLease / 3)
		tree.clock.fire(outboxLease / 3)
	}
	if held := heldWarnings(t, logged.String()); len(held) != 0 {
		t.Fatalf("the lane logged %v held behind a running row, want none: no row waits on a wake's turn", held)
	}
	if rows := outboxRows(t, tree.pool); rows != 0 {
		t.Fatalf("%d rows are left while the wake waits, want none: each row of the issue ran where it stood", rows)
	}
	if _, still := waitingWakeOf(t, tree, claim.RoleArchitect); still.At != wake.At {
		t.Fatalf("the wake is due at %s after two minutes of ticks, want the turn it already held, %s", still.At, wake.At)
	}

	// The suspend of that same claim: the machine decides it the moment the row runs, and the row
	// finishes there. The claim records no process while its wake waits, so the suspension has no
	// process to protect and nothing to wait for (holdSuspendUntilReady): it suspends the claim
	// where it arrives — written, so a restart relaunches nothing — and takes the wake with it,
	// rather than answering the wait every durable caller retries on and landing at a turn tens of
	// minutes out.
	suspend := mustOutboxRow(t, tree.root.Key, record.SuperviseRequest{Op: "suspend", Tree: tree.root.Tree,
		Role: claim.RoleArchitect, Generation: tree.root.Generation}, tree.clock.Now())
	tree.commit(t, "architect-suspend", suspend)
	testwait.Eventually(t, "the suspend row to finish with the claim whose wake waited", func() bool {
		return outboxRows(t, tree.pool) == 0
	})
	if c := machine.Claim(); c.State != supervise.StateSuspended || c.SuspensionHeld {
		t.Fatalf("the claim whose wake waited is %s with suspensionHeld %v, want %s with nothing held",
			c.State, c.SuspensionHeld, supervise.StateSuspended)
	}
	if still := machine.View().WakeWaiting; still != nil {
		t.Fatalf("the pacing holds %+v over the suspended claim, want the wake gone with the suspension", *still)
	}

	// The moment the wake would have had: its timer went with the suspension, so nothing fires at
	// it and nothing is launched.
	if fired := tree.clock.fire(wake.At.Sub(reservedAt)); fired != 0 {
		t.Fatalf("%d timers fired at the turn the dropped wake held, want none: the suspension disarmed it", fired)
	}
	if c := machine.Claim(); c.State != supervise.StateSuspended {
		t.Fatalf("the suspended claim is %s at the turn its wake held, want it left %s", c.State, supervise.StateSuspended)
	}
	if spawned := spawnedRoles(tree.rt); len(spawned) != 0 {
		t.Fatalf("the runtime launched %v at the suspended claim's turn, want nothing", spawned)
	}
}

// A second start of a claim whose wake the pacing already holds takes no second token and starts
// no second launch: the wake keeps the turn it had, the limiter's next release is one interval
// after that turn rather than two, and the runtime is asked for nothing. The sweep of a wave
// re-posts such a start every tick, so a second token each time would charge the whole queue the
// rate again.
//
// The negative control is the same wave with the pacing off (the rate at 0): the first start then
// launches at once, there is no wake for a second start to meet, and
// TestWithNoPacingTheBootWaveGoesOutInOneTick holds that shape for the launches themselves.
func TestASecondStartOfAWaitingClaimTakesNoTokenAndStartsNoSecondLaunch(t *testing.T) {
	tree, limiter := pacedLaneTree(t, nil)

	tree.commit(t, "architect-start", tree.start(t, claim.RoleArchitect))
	testwait.Eventually(t, "the start row to finish while the wake it raised waits", func() bool {
		return outboxRows(t, tree.pool) == 0
	})
	_, wake := waitingWakeOf(t, tree, claim.RoleArchitect)

	tree.commit(t, "architect-start-again", tree.start(t, claim.RoleArchitect))
	testwait.Eventually(t, "the second start row to finish", func() bool { return outboxRows(t, tree.pool) == 0 })

	if spawned := spawnedRoles(tree.rt); len(spawned) != 0 {
		t.Fatalf("the second start launched %v, want nothing: the claim is already launching", spawned)
	}
	if _, still := waitingWakeOf(t, tree, claim.RoleArchitect); still.At != wake.At {
		t.Fatalf("the wake is due at %s after the second start, want the turn it already held, %s", still.At, wake.At)
	}
	// The next wake of the whole daemon is one interval past this claim's turn: a second token
	// would have pushed it a further interval out.
	next := limiter.Reserve(tree.clock.Now())
	if gap := next.Sub(wake.At); gap < testWakeInterval || gap >= 2*testWakeInterval {
		t.Fatalf("the next wake of the daemon is %s past this claim's turn, want at least %s and less than %s: the second start took a token",
			gap, testWakeInterval, 2*testWakeInterval)
	}
}

// noticeRowOf is the notice row of one issue, and whether the outbox holds one.
func noticeRowOf(t *testing.T, pool *pgxpool.Pool, issue string) (noticeRow, bool) {
	t.Helper()
	var row noticeRow
	// A notice the pacing reserved no moment for carries no paced_at at all (omitzero).
	var pacedAt *time.Time
	err := pool.QueryRow(context.Background(), `select id, (payload->>'paced_at')::timestamptz, created_at, next_at
		from outbox where kind = $1 and issue = $2 order by id`, string(record.OutboxKindNotice), issue).Scan(&row.id, &pacedAt, &row.createdAt, &row.nextAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return noticeRow{}, false
	}
	if err != nil {
		t.Fatalf("read the notice row of %s: %v", issue, err)
	}
	if pacedAt != nil {
		row.pacedAt = pacedAt.UTC()
	}
	row.createdAt, row.nextAt = row.createdAt.UTC(), row.nextAt.UTC()
	return row, true
}

// A ready of an architect whose own catch-up waits in a queue of forty leaves that catch-up
// exactly where the queue holds it — the ready releases the notices that waited for the architect
// to come back, and this wake is waiting for a turn instead — and the minute that turn falls in
// still holds no more than the rate plus the burst. A later notice of the same tree, written while
// the catch-up waits, is published after it and never before it, so the architect is woken once
// and reads the later notice in the turn that wake starts.
//
// The negative control is the pacing reverted for this lane, the engine given no Reserve
// (TestWithNoPacingTheCatchUpWaveIsWrittenDueAtOnce): the catch-up is then due at once, the ready
// has nothing to leave where it was, and the architect's minute holds the whole wave.
func TestAReadyAndALaterNoticeLeaveThePacedCatchUpWhereTheQueueHoldsIt(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	now := wakeOrigin
	publisher := &holderPublisher{}
	engine := wakeEngine(records, pacedWakes(), func() time.Time { return now })
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		notices: publisher, trees: outboxTreeStore(t, pool), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{engine, admit.New(records, engine, 2, "legion", quietLogger())},
		now:      func() time.Time { return now }}

	for i := 1; i <= waveSize; i++ {
		key := fmt.Sprintf("LEGION-%d", i)
		runningArchitect(t, pool, records, sup, key)
		if err := runner.execute(ctx, startRow(t, key, wakeOrigin)); err != nil {
			t.Fatalf("start the running architect of %s: %v", key, err)
		}
	}

	// An architect from the middle of the queue, whose turn is minutes out.
	const woken = "LEGION-20"
	catchUp, ok := noticeRowOf(t, pool, woken)
	if !ok || !catchUp.paced() || !catchUp.nextAt.After(now.Add(time.Minute)) {
		t.Fatalf("the catch-up of %s is %+v, want one the pacing holds more than a minute out", woken, catchUp)
	}
	architect := mustClaimToken(t, woken, claim.RoleArchitect)

	if err := runner.releaseWaiting(ctx, architect); err != nil {
		t.Fatalf("release the notices waiting for %s: %v", architect, err)
	}
	if after, _ := noticeRowOf(t, pool, woken); !after.nextAt.Equal(catchUp.nextAt) {
		t.Fatalf("the catch-up of %s is due at %s after its architect's ready, want the turn the queue holds it at, %s",
			woken, after.nextAt, catchUp.nextAt)
	}
	rows := noticeRows(t, pool)
	moments := make([]time.Time, 0, len(rows))
	for _, row := range rows {
		moments = append(moments, row.nextAt)
	}
	assertSpread(t, "the queue after the architect's ready", moments, wakeOrigin)

	// The later notice of that tree, written while the catch-up waits and due at once.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, woken, record.Notice{Kind: "phase-finished", Role: claim.RoleArchitect,
		Phase: phase.Planning, Summary: "the planner finished"}, now))
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox while the catch-up waits: %v", err)
	}
	if got := publishedTo(t, publisher, woken); len(got) != 0 {
		t.Fatalf("published %v to the architect of %s while its catch-up waits, want nothing: it is woken once, by the catch-up", got, woken)
	}

	now = catchUp.nextAt
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the outbox at the catch-up's moment: %v", err)
	}
	if got := publishedTo(t, publisher, woken); fmt.Sprint(got) != "[catch-up phase-finished]" {
		t.Fatalf("published %v to the architect of %s, want the catch-up and then the notice written behind it", got, woken)
	}
}

// publishedTo is the kind of each notice published to the issue's architect, in order.
func publishedTo(t *testing.T, publisher *holderPublisher, issue string) []string {
	t.Helper()
	_, delivered := publisher.snapshot()
	var kinds []string
	for _, published := range delivered {
		if published.topic != architectTopic(t, issue) {
			continue
		}
		notice, ok := published.payload.(record.Notice)
		if !ok {
			t.Fatalf("the architect of %s was published %T, want a notice", issue, published.payload)
		}
		kinds = append(kinds, string(notice.Kind))
	}
	return kinds
}
