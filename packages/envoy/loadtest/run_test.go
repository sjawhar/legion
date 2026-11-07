package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
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
	openLoop(context.Background(), seed, 1, ratePlan(rate, start, winStart, nil), winStart, winEnd, func(r *rand.Rand, _ time.Time, inWindow bool) {
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
	const commitsErr = `read loadtest.event_commits (is commit-trigger.sql installed?): ERROR: relation "loadtest.event_commits" does not exist (SQLSTATE 42P01)`
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
