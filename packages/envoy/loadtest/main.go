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
  tables           render TABLES.md and pprof tops from step directories
  backlog          publish envelopes to the sessions' agent subjects (restart-step.sh); writes a JSON summary
  restart-report   the backlog and duplicate report of one restart run (restart-step.sh)
  series-sessions  print the session count of the series step at a load factor
  trigger-sql      print the commit-time trigger SQL (commit-trigger.sql)
  box-busy         print the box's busy fraction over the next 5 s, from /proc/stat

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
	case "tables":
		err = runTables(os.Args[2:])
	case "backlog":
		err = runBacklog(os.Args[2:])
	case "restart-report":
		err = runRestartReport(os.Args[2:])
	case "series-sessions":
		err = runSeriesSessions(os.Args[2:])
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
