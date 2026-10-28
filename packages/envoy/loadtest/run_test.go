package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"log"
	"math"
	"math/rand/v2"
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
// without its password (its user stays). Neither meta.json nor the log holds a token or a password.
func TestAForeignRunsMetaHoldsNoTokenOrPassword(t *testing.T) {
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	const agent, listener, password = "agent-token-7f3a", "listener-token-9c1e", "url-password-51d2"
	pg := "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"
	for _, c := range []struct {
		args, want []string
	}{
		{
			args: []string{"-allow-foreign", "-token", agent, "--listener-token=" + listener, "-name", "-token", "-pg", pg},
			want: []string{"-allow-foreign=true", "-listener-token=" + redacted, "-name=-token", "-pg=" + pg, "-token=" + redacted},
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
				"-allow-foreign=true", "-listener=http://agent@127.0.0.1:2", "-listener-token=" + redacted,
				"-nats=nats://loadgen@127.0.0.1:3", "-nats-mon=http://monitor@127.0.0.1:4", "-pg=" + pg,
				"-pprof=http://pprof@127.0.0.1:5", "-token=" + redacted, "-url=http://agent@127.0.0.1:1",
			},
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
		for _, secret := range []string{agent, listener, password} {
			if strings.Contains(string(meta), secret) || strings.Contains(logged.String(), secret) {
				t.Fatalf("%q: meta.json or the log holds a token or a password:\n%s\n%s", c.args, meta, logged.String())
			}
		}
		if !slices.Equal(cfg.args, c.want) {
			t.Fatalf("%q recorded as %q, want %q", c.args, cfg.args, c.want)
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
