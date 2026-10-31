// Command loadgen is the Dispatch load-test harness: it seeds a local stack through Dispatch's
// API, runs one load step (sessions on NATS, an event generator, a request generator, dashboard
// live sockets, samplers and profiles) and renders the tables of one or more steps. stack.sh,
// run-step.sh and run-series.sh drive it; every endpoint and token comes from the stack's
// stack.json (guard.go).
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

const usage = `usage: loadgen <command> [flags]

commands:
  proxy            TCP latency proxy toward Postgres
  seed             seed 4 projects, 1,000 issues and 8,000 comments through Dispatch; writes seed.json
  seed-refs        add production-shaped dispatch:// references to that seed: 1,000 cited documents
  refs-shape       check the references answers of the LGA issues against production's shape
  refs-fetch       write a TSV of every seeded issue's references answer (status, ETag, sha256, bytes)
  run              one load step against the stack in stack.json; writes summary.json and profiles
  plan-docs        print, without a stack, the documents a step's in-window edits and uploads write
                   the most versions to (the run's own plan of the same flags, seed.json)
  tables           render TABLES.md and pprof tops from step directories
  seed-finish      after seed: wait for the outbox and the block-index backfill, vacuum, install the commit-time trigger
  cgroup-merge     reduce a cluster sampler's cgroup.jsonl to a step directory's cgroup.json
  curves           CURVES.md, curves.csv, KNEES.md, refine.txt and TREND.md from cluster step directories
  codepaths        CODEPATHS.md and codepaths.json: a replay step's CPU and memory by code path beside the outage
  series-sessions  print the session count of the series step at a load factor
  trigger-sql      print the commit-time trigger SQL (commit-trigger.sql)
  box-busy         print the box's busy fraction over the next 5 s, from /proc/stat
  replay-fetch     read a project's state at -at and its writes to -until from a Dispatch, read-only;
                   writes a replay bundle (DISPATCH_URL, DISPATCH_TOKEN_FILE)
  replay-pack      pack a replay bundle as deterministic zstd tar parts
  replay-unpack    unpack replay-pack's parts into a directory
  replay-verify-bundle
                   check every file of a bundle against bundle.meta.json's sha256; print counts
  replay-seed      recreate a replay bundle's state on the stack through Dispatch's API, resumably
                   (loadtest.replay_ids maps each logged id to the local one)
  replay-check-seed
                   compare a replay seed's documents with the bundle; exit 1 on a difference
  replay-checksum  print row counts and an md5 of every version's markdown and comment body
  replay-logical-dump
                   write a replay seed's documents and comments with every uuid and timestamp masked
  replay-plan      print, without a stack, the actions and requests a replay run would send (run's
                   -replay-* flags and -rng-seed), as JSON counts

Run "loadgen <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "proxy":
		err = runProxy(os.Args[2:])
	case "seed":
		err = runSeed(os.Args[2:])
	case "seed-refs":
		err = runSeedRefs(os.Args[2:])
	case "refs-shape":
		err = runRefsShape(os.Args[2:])
	case "refs-fetch":
		err = runRefsFetch(os.Args[2:])
	case "run":
		err = runLoad(os.Args[2:])
	case "plan-docs":
		err = runPlanDocs(os.Args[2:])
	case "tables":
		err = runTables(os.Args[2:])
	case "seed-finish":
		err = runSeedFinish(os.Args[2:])
	case "cgroup-merge":
		err = runCgroupMerge(os.Args[2:])
	case "curves":
		err = runCurves(os.Args[2:])
	case "codepaths":
		err = runCodepaths(os.Args[2:])
	case "series-sessions":
		err = runSeriesSessions(os.Args[2:])
	case "replay-fetch":
		err = runReplayFetch(os.Args[2:])
	case "replay-pack":
		err = runReplayPack(os.Args[2:])
	case "replay-unpack":
		err = runReplayUnpack(os.Args[2:])
	case "replay-verify-bundle":
		err = runReplayVerifyBundle(os.Args[2:])
	case "replay-seed":
		err = runReplaySeed(os.Args[2:])
	case "replay-check-seed":
		err = runReplayCheckSeed(os.Args[2:])
	case "replay-checksum":
		err = runReplayChecksum(os.Args[2:])
	case "replay-logical-dump":
		err = runReplayLogicalDump(os.Args[2:])
	case "replay-plan":
		err = runReplayPlan(os.Args[2:])
	case "trigger-sql":
		fmt.Print(commitTriggerSQL)
	case "box-busy":
		busy, ok := boxBusy(context.Background(), boxBusyBeforeWindow)
		if !ok {
			err = fmt.Errorf("/proc/stat gave no ticks")
		} else {
			fmt.Printf("%.4f\n", busy)
		}
	default:
		fmt.Fprintf(os.Stderr, "loadgen: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// loadtestRoot is where the stack keeps all its state: $LOADTEST_ROOT, or
// ${TMPDIR:-/tmp}/dispatch-loadtest as stack.sh defaults it.
func loadtestRoot() string {
	if root := os.Getenv("LOADTEST_ROOT"); root != "" {
		return root
	}
	return filepath.Join(os.TempDir(), "dispatch-loadtest")
}

// seriesSessions is the session count of the series step at load factor f: piecewise linear
// through the spec's three points (100 at 0.25x, 400 at 1x, 1,000 at 10x), 400·f up to 1x and
// 400 + 600·(f−1)/9 above it.
func seriesSessions(f float64) int {
	if f <= 1 {
		return int(math.Round(400 * f))
	}
	return int(math.Round(400 + 600*(f-1)/9))
}

func runSeriesSessions(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: loadgen series-sessions <factor>")
	}
	f, err := strconv.ParseFloat(args[0], 64)
	if err != nil || !finite(f) || f <= 0 {
		return fmt.Errorf("factor %q is not a positive number", args[0])
	}
	fmt.Println(seriesSessions(f))
	return nil
}
