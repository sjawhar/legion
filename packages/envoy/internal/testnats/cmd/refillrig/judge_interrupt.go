package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// judgeInterrupt judges a read of a recreated bucket the rig interrupted (proof P2): the read must
// have been under way at the interruption, end incomplete, remove nothing, and be followed by a
// complete read the listener began itself, which then sweeps.
func judgeInterrupt(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge interrupt", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the phase began at")
	registry := flags.String("registry", "session registry", "the cache's log name")
	obsPath := flags.String("obs", "", "the list's polls")
	before := flags.String("before", "", "sets the copy held before the read")
	after := flags.String("after", "", "sets the recreated bucket holds")
	interrupted := flags.Int64("interrupted-ns", 0, "when the rig interrupted NATS")
	mode := flags.String("mode", "stop", "stop (SIGSTOP past the idle timer) or restart (kill and start again)")
	settle := flags.Duration("settle", 2*time.Second, "time after the last completion line by which the sweep is done")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	e, err := newExpectation(*before, *after)
	if err != nil {
		return v, err
	}
	lines, err := readLog(*path, *offset)
	if err != nil {
		return v, err
	}
	obs, err := readObservations(*obsPath)
	if err != nil {
		return v, err
	}
	begin, ok := firstLine(lines, 0, installLine(*registry))
	if !ok {
		v.invalid = fmt.Sprintf("the log has no %q line: the rig never saw the read begin", installText(*registry))
		return v, nil
	}
	first, ok := firstLine(lines, begin.at, warmUpLine(*registry, ""))
	if ok && first.outcome == "completed" && first.at <= *interrupted {
		v.invalid = fmt.Sprintf("the scan completed (%d records in %s) before the interruption at +%s", first.entries, ms(first.at-begin.at), ms(*interrupted-begin.at))
		return v, nil
	}
	var outcomes []string
	if line, ok := firstLine(lines, begin.at, warmUpLine(*registry, "timed out")); ok {
		outcomes = append(outcomes, fmt.Sprintf("the scan timed out at +%s (%d records delivered)", ms(line.at-begin.at), line.entries))
	}
	if line, ok := firstLine(lines, begin.at, func(l logLine) bool { return l.msg == *registry+" watcher stopped" }); ok {
		outcomes = append(outcomes, fmt.Sprintf("the watcher ended before its scan did at +%s", ms(line.at-begin.at)))
	}
	// The bus logs through the default logger, whose time is to the second.
	if line, ok := firstLine(lines, begin.at-int64(time.Second), func(l logLine) bool { return strings.Contains(l.msg, "envoy nats disconnected") }); ok {
		outcomes = append(outcomes, fmt.Sprintf("the connection was lost at +%ds (the disconnect hook stopped the copy following; the reconnect hook read again)", (line.at-begin.at)/int64(time.Second)))
	}
	if line, ok := firstLine(lines, begin.at, func(l logLine) bool {
		return l.msg == *registry+" re-read of a timed-out scan failed; it is tried again"
	}); ok {
		outcomes = append(outcomes, fmt.Sprintf("a re-read failed at +%s and was tried again", ms(line.at-begin.at)))
	}
	completed, ok := firstLine(lines, *interrupted, warmUpLine(*registry, "completed"))
	if !ok {
		// No read completed after the interruption, so every poll of the run must still hold the
		// whole old copy: judge that too, so the verdict also says what the listener removed.
		judgeList(&v, *registry, obs, e, begin.at, math.MaxInt64, math.MaxInt64, 0)
		seen := strings.Join(outcomes, "; ")
		if seen == "" {
			seen = "the log shows no scan end, watcher end or disconnect either"
		}
		v.fail("no complete read of the bucket followed the interrupted one (%s): the listener did not read it again by itself", seen)
		return v, nil
	}
	if len(outcomes) == 0 {
		v.invalid = fmt.Sprintf("the %s interruption left the read whole: no timed-out scan, ended watcher or disconnect in the log before the read completed at +%s", *mode, ms(completed.at-begin.at))
		return v, nil
	}
	last := completed
	for _, line := range lines {
		if line.at > completed.at && warmUpLine(*registry, "completed")(line) {
			last = line
		}
	}
	afterSweep := judgeList(&v, *registry, obs, e, begin.at, completed.at, last.at, *settle)
	during := 0
	for _, o := range obs {
		if o.Status == http.StatusOK && o.Start >= *interrupted && o.End <= completed.at {
			during++
		}
	}
	if len(v.problems) == 0 {
		switch {
		case during == 0:
			v.invalid = "no poll fell between the interruption and the complete read"
		case afterSweep == 0:
			v.invalid = "no poll came after the sweep"
		}
	}
	v.notes = append(v.notes,
		fmt.Sprintf("interrupted by %s at +%s into the read: %s", *mode, ms(*interrupted-begin.at), strings.Join(outcomes, "; ")),
		fmt.Sprintf("%d polls during the incomplete read each held every record of the old copy", during),
		fmt.Sprintf("a complete read of %d records followed at +%s with no action of the rig's, and %d polls after it listed exactly the bucket", completed.entries, ms(completed.at-begin.at), afterSweep))
	return v, nil
}
