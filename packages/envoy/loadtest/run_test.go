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

// TestAForeignRunsMetaHoldsNoToken: under -allow-foreign the tokens are flags, and meta.json
// records the run's arguments; it holds neither token, whichever spelling gave it.
func TestAForeignRunsMetaHoldsNoToken(t *testing.T) {
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	const agent, listener = "agent-token-7f3a", "listener-token-9c1e"
	for _, args := range [][]string{
		{"-allow-foreign", "-token", agent, "-listener-token", listener, "-name", "-token", "-pg", "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"},
		{"-allow-foreign", "-token=" + agent, "-listener-token=" + listener, "-pg", "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"},
		{"--allow-foreign", "--token", agent, "--listener-token", listener, "-pg", "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"},
		{"--allow-foreign=true", "--token=" + agent, "--listener-token=" + listener, "-pg=postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"},
	} {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		var ef endpointFlags
		ef.register(fs)
		name := fs.String("name", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if ef.token != agent || ef.listenerToken != listener || !ef.allowForeign {
			t.Fatalf("%q parsed to token %q, listener token %q: the test does not pass the tokens", args, ef.token, ef.listenerToken)
		}
		var logged bytes.Buffer
		cfg := runConfig{
			name: *name, allowForeign: true, boxBusyBefore: 0, args: redactArgs(fs, args),
			ep: endpoints{pg: ef.pg, token: ef.token, listenerToken: ef.listenerToken, pids: map[string]int{}},
		}
		meta, err := json.Marshal(collectMeta(context.Background(), cfg, log.New(&logged, "", 0)))
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{agent, listener} {
			if strings.Contains(string(meta), token) || strings.Contains(logged.String(), token) {
				t.Fatalf("%q: meta.json or the log holds a token:\n%s\n%s", args, meta, logged.String())
			}
		}
		if len(cfg.args) != len(args) {
			t.Fatalf("%q recorded as %q: arguments lost", args, cfg.args)
		}
		for i, a := range cfg.args {
			if a != args[i] && !strings.HasSuffix(a, redacted) {
				t.Fatalf("%q recorded as %q: an argument that is not a token changed", args, cfg.args)
			}
		}
	}
}
