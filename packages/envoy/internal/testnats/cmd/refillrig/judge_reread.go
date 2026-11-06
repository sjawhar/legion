package main

import (
	"flag"
	"fmt"
	"net/http"
	"time"
)

// judgeReread judges one recreate of a bucket read again by the listener (proof P1): the recreated
// list's answers against before and after, and the other list's answers against its records, which
// nothing changed. The script stops the listener while it recreates and fills the bucket, so the
// read the listener begins once it runs again (its watcher's probe finds the recreate) reads the
// whole bucket; one that delivered less began before the fill was done, and the run is INVALID.
func judgeReread(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge reread", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the phase began at")
	registry := flags.String("registry", "", "the cache's log name: session registry or interest registry")
	obsPath := flags.String("obs", "", "the recreated bucket's list polls")
	before := flags.String("before", "", "sets the copy held before the read")
	after := flags.String("after", "", "sets the recreated bucket holds")
	steadyPath := flags.String("steady-obs", "", "the other list's polls")
	steady := flags.String("steady-sets", "", "sets the other list holds throughout")
	settle := flags.Duration("settle", time.Second, "time after the completion line by which the sweep is done")
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
		v.invalid = fmt.Sprintf("the log has no %q line after the recreate: the listener never read the new bucket", installText(*registry))
		return v, nil
	}
	end, ok := firstLine(lines, begin.at, warmUpLine(*registry, "completed"))
	if !ok {
		v.invalid = fmt.Sprintf("the log has no completed %q line after the recreate", *registry+" cache warm-up")
		return v, nil
	}
	if want := e.total(e.after); end.entries != want {
		v.invalid = fmt.Sprintf("the read delivered %d records and the recreated bucket holds %d: it began before the rig had filled the bucket", end.entries, want)
		return v, nil
	}
	if incomplete, ok := firstLine(lines, begin.at, warmUpLine(*registry, "timed out")); ok && incomplete.at <= end.at {
		v.invalid = "the read timed out before the read that completed: not the one uninterrupted read this scenario observes"
		return v, nil
	}
	judgeList(&v, *registry, obs, e, begin.at, end.at, end.at, *settle)
	inside, partial := 0, 0
	for _, o := range obs {
		if o.Status == http.StatusOK && o.Start >= begin.at+int64(time.Millisecond) && o.End <= end.at {
			inside++
			if e.partial(o) {
				partial++
			}
		}
	}
	if *steadyPath != "" {
		steadyObs, err := readObservations(*steadyPath)
		if err != nil {
			return v, err
		}
		sets, err := newExpectation(*steady, *steady)
		if err != nil {
			return v, err
		}
		judgeList(&v, "the other list", steadyObs, sets, begin.at, end.at, end.at, 0)
		v.notes = append(v.notes, fmt.Sprintf("the other list held its %d records in all %d polls", sets.total(sets.after), len(steadyObs)))
	}
	if len(v.problems) == 0 && inside == 0 {
		v.invalid = fmt.Sprintf("no poll fell inside the %s read (%d polls in all)", ms(end.at-begin.at), len(obs))
	}
	found := "no line says the watcher found the recreate itself"
	if line, ok := firstLine(lines, 0, foundLine(*registry)); ok && line.at <= begin.at {
		found = fmt.Sprintf("the watcher's probe found the recreate %s before the read began", ms(begin.at-line.at))
	}
	v.notes = append([]string{fmt.Sprintf("%d of %d polls fell inside the %s read of %d records, %d of them with the new records partly delivered; every record of the old copy held until the read completed, exactly the bucket's after it; %s",
		inside, len(obs), ms(end.at-begin.at), end.entries, partial, found)}, v.notes...)
	return v, nil
}
