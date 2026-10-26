// Command refillrig is the Go half of scripts/e2e-refill.sh, the rig that proves on a real listener
// and a real NATS server that the listener keeps its copy of a bucket while it reads the bucket
// again, refuses its two list routes while it cannot stand behind that copy, and publishes no
// active-sessions sample while it refuses (INFRA-261). The script builds, starts and signals the
// processes; this command does what a shell cannot do quickly or exactly:
//
//   - seed writes session and interest records straight into their buckets, and register writes
//     them through the listener's own registration route (POST /v1/interests/subscribe);
//   - recreate deletes a bucket and creates it again with the same stream config, then fills it,
//     and can write when each record of the fill was acknowledged, so a judge knows what the new
//     bucket held at any moment of a fill that a read overlapped;
//   - poll asks one list route, or /metrics, as fast as it answers until it is signalled, and writes
//     one line per answer saying which seeded records it held;
//   - await waits for a listener log line, and can signal a process the moment it appears (and
//     continue a stopped one a fixed time later);
//   - judge reads the polls beside the listener's own log lines and prints the scenario's verdict:
//     PASS, FAIL <reason>, or INVALID <reason> when the run missed the window it was meant to
//     observe, which is never a pass.
//
// Every record belongs to a named set: session id ses_refill_<set>_<index>, title
// "refill <set> <index> v<variant>" and interest topic notifications.refill.<set>.v<variant>, so a
// poll tells which set a row is from and which version of it the listener answered. A set is
// written on the command line as name=count[:variant] (variant 1 by default), several separated by
// commas.
//
// Each file holds one of those roles: main.go dispatches the commands; records.go names a set's
// records and builds them; seed.go writes them straight into a bucket (seed, and recreate's fill),
// register.go through the listener's route (register), and recreate.go recreates a bucket and
// writes its fill log; poll.go polls a route and reads the polls back; await.go reads the
// listener's log and waits on it. judge.go holds what every judge shares: the expectation a list
// is held to, the verdict and the log lines the judges look for. judge_reread.go,
// judge_interrupt.go, judge_boot_outage.go and judge_recreate_live.go hold one judge each, boot
// and outage together because they share their gauge checks.
//
// It is a test fixture, not a product, and lives under internal/testnats beside natsd, the server
// the script runs.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"seed":     runSeed,
		"register": runRegister,
		"recreate": runRecreate,
		"poll":     runPoll,
		"await":    runAwait,
		"judge":    runJudge,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "refillrig %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: refillrig seed|register|recreate|poll|await|judge [flags]; refillrig <command> -h lists a command's flags")
}
