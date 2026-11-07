package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// arrivals runs openLoop over times in the past, so it never sleeps, and records each arrival:
// whether it is in the window, and the plan that fire draws from the stream after it.
func arrivals(seed uint64, rate float64, start, winStart, winEnd time.Time) (in []bool, plans []uint64) {
	openLoop(context.Background(), seed, 1, rate, start, winStart, winEnd, func(r *rand.Rand, inWindow bool) {
		in = append(in, inWindow)
		plans = append(plans, r.Uint64())
	})
	return in, plans
}

func TestOpenLoopSchedule(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	winStart, winEnd := start.Add(40*time.Second), start.Add(100*time.Second)
	const rate = 100.0

	in, plans := arrivals(7, rate, start, winStart, winEnd)
	againIn, againPlans := arrivals(7, rate, start, winStart, winEnd)
	if !slices.Equal(in, againIn) || !slices.Equal(plans, againPlans) {
		t.Fatal("one seed gave two schedules")
	}
	if _, otherPlans := arrivals(8, rate, start, winStart, winEnd); slices.Equal(plans, otherPlans) {
		t.Fatal("another seed gave the same schedule")
	}

	// A Poisson schedule: about rate arrivals per second from start to the window end, and the
	// window's share of them about its share of that time.
	span := winEnd.Sub(start).Seconds()
	if want := rate * span; math.Abs(float64(len(in))-want) > 0.05*want {
		t.Fatalf("%d arrivals over %.0f s at %g/s, want about %.0f", len(in), span, rate, want)
	}
	inWindow := 0
	for _, x := range in {
		if x {
			inWindow++
		}
	}
	share, want := float64(inWindow)/float64(len(in)), winEnd.Sub(winStart).Seconds()/span
	if math.Abs(share-want) > 0.03 {
		t.Fatalf("%.3f of the arrivals are in the window, want about %.3f", share, want)
	}

	if in, _ := arrivals(7, 0, start, winStart, winEnd); len(in) != 0 {
		t.Fatalf("a rate of 0 fired %d arrivals", len(in))
	}
}

// TestAForeignRunsMetaHoldsNoTokenOrPassword: under -allow-foreign the tokens and the URLs are
// flags, and meta.json records them as the normalized flag list: each token as <redacted>, each URL
// without its userinfo, the user too, since nats.go takes a user with no password as the auth
// token; a comma-separated -nats server list loses every element's, and -pg is recorded from pgx's
// own parse as its hosts, ports and database alone. Neither meta.json nor the log holds a token, a
// password or a NATS token user.
func TestAForeignRunsMetaHoldsNoTokenOrPassword(t *testing.T) {
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	isolatePG(t)
	const agent, listener, password, natsToken = "agent-token-7f3a", "listener-token-9c1e", "url-password-51d2", "nats-token-4b2e"
	pg, pgRecorded := "postgres://127.0.0.1:1/dispatch?sslmode=disable", "postgres://127.0.0.1:1/dispatch"
	foreignPG := func(pg string) []string {
		return []string{"-allow-foreign", "-token", agent, "-listener-token", listener, "-pg", pg}
	}
	recordedPG := func(pg string) []string {
		return []string{"-allow-foreign=true", "-listener-token=" + redacted, "-pg=" + pg, "-token=" + redacted}
	}
	for _, c := range []struct {
		args, want []string
		// Fragments of a case's own credentials, found in no output. meta.json and the log
		// also hold text this machine supplies: the temporary root's path, the CPU model, the
		// kernel release, and the login name and the resolver's address in pgx's connect
		// error. Only the root's path changes between runs, in one run of decimal digits -
		// os.MkdirTemp writes a uint32, so at most ten of them, between the test's name and a
		// "/" - so every fragment here holds a character that is not a digit, or more than
		// ten digits. A shorter run of digits, "123", matched that path by chance and failed
		// the test at random on any pull request.
		secrets []string
	}{
		{
			args: []string{"-allow-foreign", "-token", agent, "--listener-token=" + listener, "-name", "-token", "-pg", pg},
			want: []string{"-allow-foreign=true", "-listener-token=" + redacted, "-name=-token", "-pg=" + pgRecorded, "-token=" + redacted},
		},
		{
			args: []string{
				"-allow-foreign", "-token", agent, "-listener-token", listener,
				"-url", "http://agent:" + password + "@127.0.0.1:1", "-listener", "http://agent:" + password + "@127.0.0.1:2",
				"-nats", "nats://loadgen:" + password + "@127.0.0.1:3", "-nats-mon", "http://monitor:" + password + "@127.0.0.1:4",
				"-pprof", "http://pprof:" + password + "@127.0.0.1:5",
				"-pg", "postgres://postgres:" + password + "@127.0.0.1:1/dispatch?sslmode=disable&password=" + password,
			},
			want: []string{
				"-allow-foreign=true", "-listener=http://127.0.0.1:2", "-listener-token=" + redacted,
				"-nats=nats://127.0.0.1:3", "-nats-mon=http://127.0.0.1:4", "-pg=" + pgRecorded,
				"-pprof=http://127.0.0.1:5", "-token=" + redacted, "-url=http://127.0.0.1:1",
			},
		},
		{
			// A -nats URL whose user is the NATS token.
			args: []string{"-allow-foreign", "-token", agent, "-listener-token", listener, "-nats", "nats://" + natsToken + "@127.0.0.1:3"},
			want: []string{"-allow-foreign=true", "-listener-token=" + redacted, "-nats=nats://127.0.0.1:3", "-token=" + redacted},
		},
		{
			// A -nats server list and a -pg host list, credentials in more than the first element.
			args: []string{
				"-allow-foreign", "-token", agent, "-listener-token", listener,
				"-nats", "nats://" + natsToken + "@127.0.0.1:3,nats://loadgen:" + password + "@127.0.0.1:6",
				"-pg", "postgres://postgres:" + password + "@127.0.0.1:1,127.0.0.1:2/dispatch?sslmode=disable",
			},
			want: []string{
				"-allow-foreign=true", "-listener-token=" + redacted, "-nats=nats://127.0.0.1:3,nats://127.0.0.1:6",
				"-pg=postgres://127.0.0.1:1,127.0.0.1:2/dispatch", "-token=" + redacted,
			},
		},
		{
			// A -pg password or password parameter with a comma, which pgx reads whole: the
			// value is one URL, not a list, and no fragment of the password is recorded. The
			// password starts with digits, which net/url reads as a port, so a redactor built
			// on net/url parses this value and records the fragment instead of refusing it.
			args:    foreignPG("postgres://u:58302917406,zx-9k@h/db"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"58302917406", "zx-9k"},
		},
		{
			args:    foreignPG("postgres://h/db?password=Qx7-vv,wk3-tt"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"Qx7-vv", "wk3-tt"},
		},
		{
			args:    foreignPG("postgres://u:zq7-vk3:63019458277,mn5-jd@h/db"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"zq7-vk3", "63019458277", "mn5-jd"},
		},
		{
			// pgx ends the userinfo at the first @ before any /, so a # or ? in the password
			// stays in it, where net/url would read a fragment or a query.
			args:    foreignPG("postgres://u:#hush-6rw3@h/db"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"hush-6rw3"},
		},
		{
			args:    foreignPG("postgres://postgres:?Hunter2-vq8z@h:5432/dispatch"),
			want:    recordedPG("postgres://h:5432/dispatch"),
			secrets: []string{"Hunter2-vq8z"},
		},
		{
			args:    foreignPG("postgres://u:74120596338#vr4-mq7@h/db"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"74120596338", "vr4-mq7"},
		},
		{
			// pgx splits the query at & alone and reads no fragment, so ; and # stay in the
			// password parameter; sslpassword is a secret too.
			args:    foreignPG("postgres://h/db?sslmode=disable&password=ws4-kd9;jt6-pb"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"ws4-kd9", "jt6-pb"},
		},
		{
			args:    foreignPG("postgres://h/db?password=rn2-hq5#tb8-xw"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"rn2-hq5", "tb8-xw"},
		},
		{
			args:    foreignPG("postgres://h/db?sslpassword=keypass-m4vq"),
			want:    recordedPG("postgres://h:5432/db"),
			secrets: []string{"keypass-m4vq"},
		},
	} {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		var ef endpointFlags
		ef.register(fs)
		name := fs.String("name", "", "")
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if ef.token != agent || ef.listenerToken != listener || !ef.allowForeign {
			t.Fatalf("%q parsed to token %q, listener token %q: the test does not pass the tokens", c.args, ef.token, ef.listenerToken)
		}
		var logged bytes.Buffer
		cfg := runConfig{
			name: *name, allowForeign: true, boxBusyBefore: 0, args: recordedArgs(fs),
			ep: endpoints{pg: ef.pg, token: ef.token, listenerToken: ef.listenerToken, pids: map[string]int{}},
		}
		meta, err := json.Marshal(collectMeta(context.Background(), cfg, log.New(&logged, "", 0)))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range append([]string{agent, listener, password, natsToken}, c.secrets...) {
			if strings.Contains(string(meta), secret) || strings.Contains(logged.String(), secret) {
				t.Errorf("%q: meta.json or the log holds %q, a token or a password:\n%s\n%s", c.args, secret, meta, logged.String())
				break
			}
		}
		if !slices.Equal(cfg.args, c.want) {
			t.Errorf("%q recorded as %q, want %q", c.args, cfg.args, c.want)
		}
	}
}

// TestStepFailsWhenItsCommitRowsWereNotRead: the committed, receipt and delivery columns depend on
// the commit rows, so a step whose read of them failed fails, naming it; a failed read of the other
// probes marks its own table and does not fail the step.
func TestStepFailsWhenItsCommitRowsWereNotRead(t *testing.T) {
	const commitsErr = `read public.loadtest_event_commits (is commit-trigger.sql installed?): ERROR: relation "public.loadtest_event_commits" does not exist (SQLSTATE 42P01)`
	err := stepFailure(summary{Events: eventsSummary{CommitsError: commitsErr}})
	if err == nil {
		t.Fatal("a step whose commit rows were not read does not fail")
	}
	if !strings.Contains(err.Error(), commitsErr) || !strings.Contains(err.Error(), "events.commits_error") {
		t.Fatalf("error %q does not name the commit-row read and its summary.json key", err)
	}

	gone := stackExit{Name: "dispatch", PID: 4242, FirstSeenGone: time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)}
	both := stepFailure(summary{Events: eventsSummary{CommitsError: commitsErr}, StackExits: []stackExit{gone}})
	if both == nil || !strings.Contains(both.Error(), "pid 4242") || !strings.Contains(both.Error(), commitsErr) {
		t.Fatalf("error %v does not name both the stack exit and the commit-row read", both)
	}

	otherReads := summary{
		Resources:       &resourcesSummary{PGReadErrors: 5, PprofReadErrors: 2, XIDReadError: "xid probe: failed to connect"},
		CommitOrderLock: lockSummary{Errors: 1},
		UngrantedLocks:  ungrantedSummary{Errors: 160},
	}
	if err := stepFailure(otherReads); err != nil {
		t.Fatalf("failed reads that feed no committed, receipt or delivery column fail the step: %v", err)
	}
}

// TestMetaRecordsAFailedPostgresVersionRead: meta.json's postgres_version is empty when it could not
// be read, and postgres_version_error says why, so an empty version is never a silent gap.
func TestMetaRecordsAFailedPostgresVersionRead(t *testing.T) {
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	var logged bytes.Buffer
	cfg := runConfig{boxBusyBefore: 0, ep: endpoints{pg: "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable", pids: map[string]int{}}}
	meta := collectMeta(context.Background(), cfg, log.New(&logged, "", 0))
	if meta.PostgresVersion != "" || meta.PostgresVersionError == "" {
		t.Fatalf("postgres_version %q, postgres_version_error %q: want empty and the error", meta.PostgresVersion, meta.PostgresVersionError)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"postgres_version_error":`) {
		t.Fatalf("meta.json lacks postgres_version_error:\n%s", b)
	}
}

// TestMetaRecordsTheDispatchBuildOfEveryStep: a step's meta.json names the Dispatch binary it ran
// (bin/envoy-dispatch.json), not only the checkout's commit, so a step run with
// LOADTEST_DISPATCH_COMMIT names the Dispatch it measured. A synthetic step records it too.
func TestMetaRecordsTheDispatchBuildOfEveryStep(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOADTEST_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	const build = `{"dispatch_commit":"35efd326","dispatch_binary_sha256":"ab12","go_version":"go1.27.1","newest_migration":134}`
	if err := os.WriteFile(filepath.Join(root, "bin", "envoy-dispatch.json"), []byte(build), 0o644); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	cfg := runConfig{buildCommit: "checkout99", boxBusyBefore: 0, ep: endpoints{pg: unreachablePG, pids: map[string]int{}}}
	meta := collectMeta(context.Background(), cfg, log.New(&logged, "", 0))
	if meta.BuildCommit != "checkout99" {
		t.Fatalf("build_commit %q: want the checkout's commit", meta.BuildCommit)
	}
	if meta.DispatchCommit != "35efd326" || meta.DispatchBinarySHA256 != "ab12" {
		t.Fatalf("dispatch_commit %q, dispatch_binary_sha256 %q: want the build record's", meta.DispatchCommit, meta.DispatchBinarySHA256)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"dispatch_build":`) || !strings.Contains(string(b), `"go_version":"go1.27.1"`) {
		t.Fatalf("meta.json lacks the whole build record:\n%s", b)
	}
	// No build record: the fields are empty and the log says so, and the step is not stopped.
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	logged.Reset()
	meta = collectMeta(context.Background(), cfg, log.New(&logged, "", 0))
	if meta.DispatchCommit != "" || meta.DispatchBuild != nil || !strings.Contains(logged.String(), "no Dispatch build record") {
		t.Fatalf("without the record: dispatch_commit %q, log %q", meta.DispatchCommit, logged.String())
	}
}

// TestSummarizeRequestsCodesAndTolerated: a non-2xx answer is counted by status and error code,
// and one its action expected (the CLI's own 404) is tolerated, not a failure.
func TestSummarizeRequestsCodesAndTolerated(t *testing.T) {
	read, edit := "GET /api/v1/projects/{project}/architecture-sources [issue-create]", "POST /api/v1/artifacts/{id}/edits [edit]"
	s := summarizeRequests([]reqRec{
		{route: read, status: 404, code: "NOT_FOUND", tolerated: true},
		{route: edit, status: 200},
		{route: edit, status: 400, code: "TARGET_SPANS_BLOCKS"},
		{route: edit, status: 404, code: "TARGET_NOT_FOUND"},
		{route: edit, status: 502},
		{route: edit, transport: true},
	}, 0, 0, 1)
	r, e := s.Routes[read], s.Routes[edit]
	if r.Failures != 0 || r.Tolerated != 1 || r.Codes["404 NOT_FOUND"] != 1 {
		t.Fatalf("tolerated read: %+v", r)
	}
	want := map[string]int{"400 TARGET_SPANS_BLOCKS": 1, "404 TARGET_NOT_FOUND": 1, "502": 1}
	if e.Failures != 4 || e.Tolerated != 0 || !maps.Equal(e.Codes, want) || s.OK2xx != 1 {
		t.Fatalf("edit: %+v, ok_2xx %d", e, s.OK2xx)
	}
	if code := (callResult{status: 400, body: []byte(`{"error":"x","code":"INVALID_OP"}`)}).errorCode(); code != "INVALID_OP" {
		t.Fatalf("errorCode %q", code)
	}
}

// TestLoopStepPlansTheSameWritesAndReadsTwice: a loop step's writes and reads are a pure function
// of its flags and -rng-seed, as a seed.json step's are (TestOpenLoopSchedule): one seed gives one
// plan of writes and one of reads, and another seed gives another.
func TestLoopStepPlansTheSameWritesAndReadsTwice(t *testing.T) {
	ls := loopTestSeed(t)
	mix, err := parseEventMix("spec-loop-2026-11-03")
	if err != nil {
		t.Fatal(err)
	}
	routes, err := loadRouteMix(specLoopRouteMixName)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		sessions: 3, events: 60, requests: 90, warmup: 10 * time.Second, duration: 30 * time.Second,
		mix: mix, routes: routes, human: 0.1, rngSeed: 5, loop: ls, loopSeedPath: "loopseed.seed.json",
	}
	plan := func(c runConfig) ([]eventPlan, []string) {
		st := newStep(c, ls.seedFile(), log.New(io.Discard, "", 0), 0)
		// Any start gives the same plan, as plan-docs relies on (planVersionDocs).
		st.openWindow(time.Time{})
		var writes []eventPlan
		st.planWrites(st.pops[0], func(p eventPlan, in bool) {
			if in {
				writes = append(writes, p)
			}
		})
		var reads []string
		openLoopPlan(c.rngSeed, requestStream, c.requests, st.start, st.winStart, st.winEnd, func(r *rand.Rand, _ time.Time, in bool) bool {
			_, path := st.requestPath(st.pops[0], r)
			if in {
				reads = append(reads, path)
			}
			return true
		})
		return writes, reads
	}
	writes, reads := plan(cfg)
	if len(writes) == 0 || len(reads) == 0 {
		t.Fatalf("the step plans %d writes and %d reads", len(writes), len(reads))
	}
	again, againReads := plan(cfg)
	if !slices.Equal(writes, again) || !slices.Equal(reads, againReads) {
		t.Fatal("one -rng-seed gave two plans")
	}
	other := cfg
	other.rngSeed = 6
	otherWrites, otherReads := plan(other)
	if slices.Equal(writes, otherWrites) || slices.Equal(reads, otherReads) {
		t.Fatal("another -rng-seed gave the same plan")
	}
	// What it planned is the loop's: the three kinds, on proposals the kind can be sent to, and
	// reads of the loop's own records with no placeholder left in them.
	for _, p := range writes {
		if p.kind != kindVerdict && p.kind != kindRequirement && p.kind != kindStatus {
			t.Fatalf("the loop mix planned a %s write", p.kind)
		}
		if p.issue < 0 || p.issue >= len(ls.Proposals) {
			t.Fatalf("a write landed on proposal %d of %d", p.issue, len(ls.Proposals))
		}
	}
	for _, path := range reads {
		if strings.ContainsAny(path, "{}") || !strings.HasPrefix(path, "/api/v1/") {
			t.Fatalf("the step planned the read %q", path)
		}
	}
}

// TestLoopSummaryConfigNamesItsSeed: a loop step's summary.json says which seed it ran against and
// which plan that seed was exported from; a step without one carries neither key.
func TestLoopSummaryConfigNamesItsSeed(t *testing.T) {
	ls := loopTestSeed(t)
	cfg := runConfig{loop: ls, loopSeedPath: "/loadtest/loopseed.seed.json"}
	if cfg.loopPlanID() != ls.PlanID {
		t.Fatalf("the step names plan %q, want %q", cfg.loopPlanID(), ls.PlanID)
	}
	if id := (runConfig{}).loopPlanID(); id != "" {
		t.Fatalf("a step without a loop seed names plan %q", id)
	}
	b, err := json.Marshal(summaryConfig{LoopSeed: cfg.loopSeedPath, LoopPlanID: cfg.loopPlanID()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"loop_seed":"/loadtest/loopseed.seed.json"`) ||
		!strings.Contains(string(b), `"loop_plan_id":"`+ls.PlanID+`"`) {
		t.Fatalf("summary.json's config does not name the seed it ran against:\n%s", b)
	}
	plain, err := json.Marshal(summaryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "loop_seed") || strings.Contains(string(plain), "loop_plan_id") {
		t.Fatalf("a step without a loop seed records one:\n%s", plain)
	}
}

// TestLoopSummaryConfigRecordsItsFactorAndItsGenerator: a step given -loop-factor and
// -generator-role records both, so loadgen curves draws the step at the factor of its grid it
// stood at and can tell whether its own generator was starved; a step given neither carries
// neither key, and the curves read it by its rates as they always have.
func TestLoopSummaryConfigRecordsItsFactorAndItsGenerator(t *testing.T) {
	b, err := json.Marshal(summaryConfig{LoopFactor: 6, GeneratorRole: "vm-gen1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"loop_factor":6`) || !strings.Contains(string(b), `"generator_role":"vm-gen1"`) {
		t.Fatalf("summary.json's config does not say which factor the step stood at, or which machine sent its load:\n%s", b)
	}
	plain, err := json.Marshal(summaryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "loop_factor") || strings.Contains(string(plain), "generator_role") {
		t.Fatalf("a step given neither flag records one:\n%s", plain)
	}
	if got := configFactor(summaryConfig{LoopFactor: 6, RequestsPerS: 60}); got != 6 {
		t.Fatalf("the curves read the recorded step at factor %v, want 6", got)
	}
}

// TestMetaRecordsTheLoopListenersWithoutCredentials: -loop-listeners is a list of listener URLs,
// and meta.json records every element of it without its userinfo, as it records the endpoint
// flags (TestAForeignRunsMetaHoldsNoTokenOrPassword).
func TestMetaRecordsTheLoopListenersWithoutCredentials(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.String("loop-listeners", "", "")
	if err := fs.Parse([]string{"-loop-listeners", "http://agent:listener-password-4f1c@127.0.0.1:1, http://127.0.0.1:2"}); err != nil {
		t.Fatal(err)
	}
	got := recordedArgs(fs)
	want := []string{"-loop-listeners=http://127.0.0.1:1,http://127.0.0.1:2"}
	if !slices.Equal(got, want) {
		t.Fatalf("recorded %q, want %q", got, want)
	}
}

// productionKinds are the five write kinds of a seed.json population, which the loop's seed
// serves none of but the plain comment.
var productionKinds = []string{kindComment, kindAnchored, kindReply, kindEdit, kindUpload}

// twoPopulationStep is a step driven at both seeds: production's own load from seed.json with
// the production mix and the production routes loadgen holds embedded, and the spec loop's from
// the loop seed with the loop mix and the loop's own route mix. It is the step vm.sh runs at a
// factor: -events and -requests are the totals, -baseline-events and -baseline-requests the
// portion production's own population sends.
func twoPopulationStep(t *testing.T, sf seedFile, ls *loopSeed) (*step, runConfig) {
	t.Helper()
	mix, err := parseEventMix(defaultEventMix)
	if err != nil {
		t.Fatal(err)
	}
	loopMix, err := parseEventMix(loopEventMix)
	if err != nil {
		t.Fatal(err)
	}
	loopRoutes, err := loadRouteMix(specLoopRouteMixName)
	if err != nil {
		t.Fatal(err)
	}
	baselineRoutes, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		bothSeeds: true, seedPath: "/loadtest/seed.json", loopSeedPath: "/loadtest/loopseed.seed.json", loop: ls,
		sessions: 4, events: 40, requests: 100, baselineEvents: 4, baselineRequests: 60,
		warmup: 10 * time.Second, duration: 30 * time.Second,
		mix: mix, loopMix: loopMix, routes: loopRoutes, baselineRoutes: baselineRoutes,
		human: 0.1, rngSeed: 5,
	}
	st := newStep(cfg, sf, log.New(io.Discard, "", 0), 0)
	st.openWindow(time.Time{})
	return st, cfg
}

// stepPlan is what one population of a step plans in its window: its in-window writes and the
// paths of its in-window reads.
func stepPlan(st *step, p *population) ([]eventPlan, []string) {
	var writes []eventPlan
	st.planWrites(p, func(plan eventPlan, in bool) {
		if in {
			writes = append(writes, plan)
		}
	})
	var reads []string
	openLoopPlan(st.cfg.rngSeed, p.requestStream, p.requests, st.start, st.winStart, st.winEnd,
		func(r *rand.Rand, _ time.Time, in bool) bool {
			_, path := st.requestPath(p, r)
			if in {
				reads = append(reads, path)
			}
			return true
		})
	return writes, reads
}

// TestBothSeedsPlanEachPopulationFromItsOwnSeed: a step given -seed with -loop-seed drives two
// populations. Production's own draws its writes from seed.json's issues with -event-mix and its
// reads over the embedded production routes; the loop's draws its writes from the loop seed's
// proposals with -loop-event-mix and its reads over -route-mix. Neither kind nor issue of one
// population is ever drawn for the other, and -events and -requests are the totals the portion
// splits.
func TestBothSeedsPlanEachPopulationFromItsOwnSeed(t *testing.T) {
	sf, ls := planFixtureSeed(8), loopTestSeed(t)
	st, cfg := twoPopulationStep(t, sf, ls)
	if len(st.pops) != 2 || st.pops[0].name != populationBaseline || st.pops[1].name != populationLoop {
		t.Fatalf("the step drives %d populations, want the baseline then the loop", len(st.pops))
	}
	base, loop := st.pops[0], st.pops[1]
	if base.events != 4 || base.requests != 60 || loop.events != 36 || loop.requests != 40 {
		t.Fatalf("baseline %g events/s and %g req/s, loop %g and %g: want the portion and the remainder of %g and %g",
			base.events, base.requests, loop.events, loop.requests, cfg.events, cfg.requests)
	}
	if base.mix.Name != defaultEventMix || loop.mix.Name != loopEventMix {
		t.Fatalf("baseline mix %q, loop mix %q", base.mix.Name, loop.mix.Name)
	}
	if !strings.HasPrefix(base.routes.Source, defaultRouteMixName) || loop.routes.Source != specLoopRouteMixName {
		t.Fatalf("baseline routes %q, want the embedded %s; loop routes %q", base.routes.Source, defaultRouteMixName, loop.routes.Source)
	}
	if base.seedPath != cfg.seedPath || loop.seedPath != cfg.loopSeedPath {
		t.Fatalf("baseline seed %q, loop seed %q", base.seedPath, loop.seedPath)
	}

	baseWrites, baseReads := stepPlan(st, base)
	loopWrites, loopReads := stepPlan(st, loop)
	if len(baseWrites) == 0 || len(loopWrites) == 0 || len(baseReads) == 0 || len(loopReads) == 0 {
		t.Fatalf("the step plans %d and %d writes, %d and %d reads", len(baseWrites), len(loopWrites), len(baseReads), len(loopReads))
	}
	for _, p := range baseWrites {
		if !slices.Contains(productionKinds, p.kind) {
			t.Fatalf("the baseline population planned a %s write, which only the loop seed serves", p.kind)
		}
		if key := base.sf.Issues[p.issue].Key; !strings.HasPrefix(key, "LGA-") {
			t.Fatalf("a baseline write landed on issue %q, which is not seed.json's", key)
		}
	}
	for _, p := range loopWrites {
		if p.kind != kindVerdict && p.kind != kindRequirement && p.kind != kindStatus {
			t.Fatalf("the loop population planned a %s write, which seed.json's mix sends", p.kind)
		}
		if key := loop.sf.Issues[p.issue].Key; !strings.HasPrefix(key, "LOOP-") {
			t.Fatalf("a loop write landed on issue %q, which is not a proposal of the loop seed", key)
		}
	}
	for _, path := range baseReads {
		if strings.Contains(path, "LOOP") || strings.ContainsAny(path, "{}") {
			t.Fatalf("the baseline population planned the read %q", path)
		}
	}
	for _, path := range loopReads {
		if strings.Contains(path, "LGA") || strings.ContainsAny(path, "{}") {
			t.Fatalf("the loop population planned the read %q", path)
		}
	}

	// The negative control: production's own population draws exactly what a step of seed.json
	// alone draws at those rates, on streams 1 and 2, so adding the loop beside it changes
	// neither its schedule nor its draws.
	alone := runConfig{sessions: cfg.sessions, events: cfg.baselineEvents, requests: cfg.baselineRequests,
		warmup: cfg.warmup, duration: cfg.duration, mix: cfg.mix, routes: cfg.baselineRoutes,
		human: cfg.human, rngSeed: cfg.rngSeed, seedPath: cfg.seedPath}
	one := newStep(alone, sf, log.New(io.Discard, "", 0), 0)
	one.openWindow(time.Time{})
	if len(one.pops) != 1 || one.pops[0].eventStream != eventStream || one.pops[0].requestStream != requestStream {
		t.Fatalf("a step given one seed drives %d populations", len(one.pops))
	}
	aloneWrites, aloneReads := stepPlan(one, one.pops[0])
	if !slices.Equal(baseWrites, aloneWrites) || !slices.Equal(baseReads, aloneReads) {
		t.Fatalf("the baseline population of a two-population step planned %d writes and %d reads, and a step of seed.json alone at its rates planned %d and %d",
			len(baseWrites), len(baseReads), len(aloneWrites), len(aloneReads))
	}
}

// TestOneSeedDrivesOnePopulation: a step given one seed drives one population at the whole of
// -events and -requests, on streams 1 and 2, whichever seed it is, and its summary carries no
// populations key: the two-population split changes nothing for it.
func TestOneSeedDrivesOnePopulation(t *testing.T) {
	sf, ls := planFixtureSeed(8), loopTestSeed(t)
	mix, err := parseEventMix(defaultEventMix)
	if err != nil {
		t.Fatal(err)
	}
	loopMix, err := parseEventMix(loopEventMix)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		cfg  runConfig
		sf   seedFile
		want string
	}{
		{name: "seed.json alone", sf: sf, want: populationBaseline,
			cfg: runConfig{sessions: 4, events: 40, requests: 100, warmup: time.Second, duration: 10 * time.Second,
				mix: mix, routes: routes, rngSeed: 1, seedPath: "/loadtest/seed.json"}},
		{name: "the loop seed alone", sf: ls.seedFile(), want: populationLoop,
			cfg: runConfig{sessions: 4, events: 40, requests: 100, warmup: time.Second, duration: 10 * time.Second,
				mix: loopMix, routes: routes, rngSeed: 1, loop: ls, loopSeedPath: "/loadtest/loopseed.seed.json"}},
	} {
		st := newStep(c.cfg, c.sf, log.New(io.Discard, "", 0), 0)
		if len(st.pops) != 1 {
			t.Fatalf("%s: the step drives %d populations", c.name, len(st.pops))
		}
		p := st.pops[0]
		if p.name != c.want || p.events != c.cfg.events || p.requests != c.cfg.requests {
			t.Fatalf("%s: population %q at %g events/s and %g req/s, want %q at the step's whole rates",
				c.name, p.name, p.events, p.requests, c.want)
		}
		if p.eventStream != eventStream || p.requestStream != requestStream {
			t.Fatalf("%s: the population draws on streams %d and %d, want 1 and 2", c.name, p.eventStream, p.requestStream)
		}
		if p.mix.Name != c.cfg.mix.Name || p.routes.Source != c.cfg.routes.Source {
			t.Fatalf("%s: the population draws with mix %q and routes %q", c.name, p.mix.Name, p.routes.Source)
		}
	}
	b, err := json.Marshal(summary{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "populations") {
		t.Fatalf("a step that drove one population records populations:\n%s", b)
	}
}

// twoSeedFiles writes production's own seed and the loop's in a temporary directory, as a step
// reads them from the machine it runs on, and answers their paths.
func twoSeedFiles(t *testing.T) (seedPath, loopPath string) {
	t.Helper()
	dir := t.TempDir()
	seedPath = filepath.Join(dir, "seed.json")
	loopPath = filepath.Join(dir, loopSeedFileName)
	write := func(path string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(seedPath, planFixtureSeed(8))
	write(loopPath, &loopSeed{
		Plan: "loopseed.plan.json", PlanID: strings.Repeat("ab", 32) + ".7", Project: "LOOP",
		ThreadsPerProposal: loopThreadSample, SearchTerms: []string{"alpha", "beta", "gamma"},
		StatusTerms: loopStatusNames[:], Proposals: loopTestSeedProposals(),
	})
	return seedPath, loopPath
}

// TestTwoPopulationFlagsAreRefusedByName: every input a two-population step cannot be run with
// is refused before the first connection, naming the flag - a baseline portion above the step's
// total, a -baseline-* without both seeds, a -loop-event-mix without the loop seed, and a mix
// one population's seed cannot serve - and the step the driver runs is refused by none of them.
func TestTwoPopulationFlagsAreRefusedByName(t *testing.T) {
	isolatePG(t)
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	seedPath, loopPath := twoSeedFiles(t)
	out := filepath.Join(t.TempDir(), "step")
	base := []string{
		"-allow-foreign", "-url", "http://127.0.0.1:1", "-listener", "http://127.0.0.1:1",
		"-nats", "nats://127.0.0.1:1", "-nats-mon", "http://127.0.0.1:1", "-pprof", "http://127.0.0.1:1",
		"-pg", "postgres://postgres@127.0.0.1:1/dispatch", "-token", "t", "-listener-token", "l",
		"-sessions", "4", "-out", out,
	}
	both := []string{"-seed", seedPath, "-loop-seed", loopPath, "-route-mix", specLoopRouteMixName}
	args := func(rest ...string) []string { return append(append(slices.Clone(base), both...), rest...) }
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{name: "a baseline portion above the step's events", want: "-baseline-events 40 is more than -events 4",
			args: args("-events", "4", "-requests", "100", "-baseline-events", "40", "-baseline-requests", "60")},
		{name: "a baseline portion above the step's requests", want: "-baseline-requests 100 is more than -requests 60",
			args: args("-events", "40", "-requests", "60", "-baseline-events", "4", "-baseline-requests", "100")},
		{name: "a two-population step with no portion", want: "-baseline-events and -baseline-requests are required",
			args: args("-events", "40", "-requests", "100")},
		{name: "a portion without -seed", want: "-baseline-events applies only with -seed and -loop-seed together",
			args: append(append(slices.Clone(base), "-loop-seed", loopPath, "-event-mix", loopEventMix),
				"-events", "40", "-requests", "100", "-baseline-events", "4")},
		{name: "both portions without a loop seed", want: "-baseline-events and -baseline-requests apply only with -seed and -loop-seed together",
			args: append(append(slices.Clone(base), "-seed", seedPath),
				"-events", "40", "-requests", "100", "-baseline-events", "4", "-baseline-requests", "60")},
		{name: "a loop mix without a loop seed", want: "-loop-event-mix applies only with -loop-seed",
			args: append(append(slices.Clone(base), "-seed", seedPath, "-loop-event-mix", loopEventMix),
				"-events", "40", "-requests", "100")},
		{name: "the loop's mix on production's seed", want: "seed.json holds no review page to write a verdict on",
			args: args("-events", "40", "-requests", "100", "-baseline-events", "4", "-baseline-requests", "60",
				"-event-mix", loopEventMix)},
		{name: "production's mix on the loop seed", want: "a loop seed holds nothing a anchored write reads",
			args: args("-events", "40", "-requests", "100", "-baseline-events", "4", "-baseline-requests", "60",
				"-loop-event-mix", defaultEventMix)},
	} {
		err := runLoad(c.args)
		if err == nil {
			t.Errorf("%s was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s was refused with %q, which does not name %q", c.name, err, c.want)
		}
	}

	// The negative control: the step vm.sh runs at a factor is refused by none of these. It
	// reads both seeds, checks each mix against its own, and gets as far as the stack guard,
	// which no endpoint of this test answers.
	err := runLoad(args("-events", "40", "-requests", "100", "-baseline-events", "4", "-baseline-requests", "60"))
	if err == nil {
		t.Fatal("a two-population step reached a stack no endpoint of this test serves")
	}
	for _, refusal := range []string{"-baseline-events", "-baseline-requests", "-loop-event-mix", "-seed applies only", "event mix"} {
		if strings.Contains(err.Error(), refusal) {
			t.Fatalf("the step the driver runs was refused: %v", err)
		}
	}
}

// TestSummaryCountsEachPopulationAndBoth: a step that drove both populations counts each one's
// writes, receipts and requests on its own, under populations, and the two together in the
// step's own events and requests, which is what the pass rule reads (curves.go). Each
// population's join reads its own writes alone, so neither counts the other's.
func TestSummaryCountsEachPopulationAndBoth(t *testing.T) {
	sf, ls := planFixtureSeed(8), loopTestSeed(t)
	st, _ := twoPopulationStep(t, sf, ls)
	basePop, loopPop := st.pops[0], st.pops[1]
	const durS = 10.0
	record := func(p *population, ws []writeRec, reqs []reqRec, dropped int64) {
		p.writes = newCollector[writeRec](len(ws))
		for _, w := range ws {
			p.writes.add(w)
		}
		p.reqs = &requestLoad{recs: newCollector[reqRec](len(reqs))}
		for _, r := range reqs {
			p.reqs.recs.add(r)
		}
		p.reqs.dropped.Add(dropped)
	}
	const browse, review = "browser GET /api/v1/issues/{key}", "loop GET /api/v1/artifacts/{review_page_artifact_id}/text"
	record(basePop, []writeRec{
		{Marker: 1, Kind: kindAnchored, Issue: "LGA-001", InWindow: true, Sent: 990 * msNs, Arrival: 1000 * msNs, Response: 1100 * msNs, Status: 201},
		{Marker: 2, Kind: kindEdit, Issue: "LGA-002", InWindow: true, Sent: 1990 * msNs, Arrival: 2000 * msNs, Response: 2100 * msNs, Status: 500},
	}, []reqRec{
		{route: browse, status: 200, latency: 5 * time.Millisecond},
		{route: browse, status: 200, latency: 7 * time.Millisecond},
	}, 1)
	record(loopPop, []writeRec{
		{Marker: 3, Kind: kindVerdict, Issue: "LOOP-1", InWindow: true, Sent: 2990 * msNs, Arrival: 3000 * msNs, Response: 3100 * msNs, Status: 200},
	}, []reqRec{
		{route: review, status: 200, latency: 9 * time.Millisecond},
	}, 2)
	commits := []commitRow{
		{EventID: 10, Type: evCommentCreated, Marker: 1, CommitNs: 1050 * msNs},
		{EventID: 30, Type: evArtifactVersion, Marker: 3, CommitNs: 3050 * msNs},
	}
	receipts := []receipt{
		{EventID: 10, Recv: 1200 * msNs}, {EventID: 10, Recv: 1300 * msNs},
		{EventID: 30, Recv: 3200 * msNs}, {EventID: 30, Recv: 3300 * msNs}, {EventID: 30, Recv: 3400 * msNs},
	}
	watchers := map[string]int{"LGA-001": 2, "LGA-002": 2, "LOOP-1": 3}

	ws, reqs, dropped := st.sent()
	if len(ws) != 3 || len(reqs) != 3 || dropped != 3 {
		t.Fatalf("the step sent %d writes and %d requests, %d dropped: want both populations' together", len(ws), len(reqs), dropped)
	}
	both := joinDelivery(ws, commits, receipts, watchers, durS)
	bothReqs := summarizeRequests(reqs, dropped, st.cfg.requests, durS)
	pops := st.populationSummaries(commits, receipts, watchers, nil, durS)
	if len(pops) != 2 || pops[0].Name != populationBaseline || pops[1].Name != populationLoop {
		t.Fatalf("the step recorded %d populations", len(pops))
	}
	base, loop := pops[0], pops[1]
	if base.EventsPerS != basePop.events || base.RequestsPerS != basePop.requests || base.Seed != basePop.seedPath ||
		base.EventMix.Name != defaultEventMix || loop.EventMix.Name != loopEventMix || loop.Seed != loopPop.seedPath {
		t.Fatalf("the populations are recorded as %+v and %+v", base, loop)
	}
	for _, c := range []struct {
		what             string
		base, loop, both int
	}{
		{"offered", base.Events.Offered, loop.Events.Offered, both.Offered},
		{"committed", base.Events.Committed, loop.Events.Committed, both.Committed},
		{"receipts", base.Events.IssueTopicReceipts, loop.Events.IssueTopicReceipts, both.IssueTopicReceipts},
		{"expected receipts", base.Events.ExpectedIssueTopicReceipts, loop.Events.ExpectedIssueTopicReceipts, both.ExpectedIssueTopicReceipts},
		{"write errors", base.Events.WriteErrors.Non2xx, loop.Events.WriteErrors.Non2xx, both.WriteErrors.Non2xx},
		{"requests sent", base.Requests.Sent, loop.Requests.Sent, bothReqs.Sent},
		{"requests 2xx", base.Requests.OK2xx, loop.Requests.OK2xx, bothReqs.OK2xx},
		{"requests dropped", base.Requests.DroppedInflightCap, loop.Requests.DroppedInflightCap, bothReqs.DroppedInflightCap},
	} {
		if c.base+c.loop != c.both {
			t.Errorf("%s: the baseline's %d and the loop's %d do not add up to the step's %d", c.what, c.base, c.loop, c.both)
		}
	}
	if base.Events.Offered != 2 || loop.Events.Offered != 1 || base.Events.Committed != 1 || loop.Events.Committed != 1 {
		t.Fatalf("the baseline offered %d and committed %d, the loop %d and %d", base.Events.Offered, base.Events.Committed, loop.Events.Offered, loop.Events.Committed)
	}
	if base.Events.IssueTopicReceipts != 2 || loop.Events.IssueTopicReceipts != 3 {
		t.Fatalf("the baseline took %d receipts and the loop %d: neither takes the other's", base.Events.IssueTopicReceipts, loop.Events.IssueTopicReceipts)
	}
	if _, ok := base.Events.ByKind[kindVerdict]; ok {
		t.Errorf("production's own population counted a verdict: %+v", base.Events.ByKind)
	}
	if _, ok := loop.Events.ByKind[kindAnchored]; ok {
		t.Errorf("the loop population counted an anchored write: %+v", loop.Events.ByKind)
	}
	if _, ok := base.Requests.Routes[review]; ok {
		t.Errorf("production's own population counted a review read: %+v", base.Requests.Routes)
	}
	if _, ok := loop.Requests.Routes[browse]; ok {
		t.Errorf("the loop population counted a dashboard read: %+v", loop.Requests.Routes)
	}
	b, err := json.Marshal(summary{Populations: pops})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"populations":[{"name":"baseline"`) || !strings.Contains(string(b), `"name":"loop"`) {
		t.Fatalf("summary.json does not carry each population:\n%s", b)
	}
}
