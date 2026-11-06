package main

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The listener's refilling answer (cmd/listener writeListRefilling), which every refused list read
// must carry exactly.
const (
	refillingMessage    = "envoy: the agent list is refilling; try again in a moment"
	refillingCode       = "refilling"
	refillingRetryAfter = "5"
)

// expectation is what a list must hold around one read of its bucket: before, the records the
// copy held when the read began; after, the records the bucket holds, which the copy holds exactly
// once the read has completed.
type expectation struct {
	before, after map[string]recordSet
}

func newExpectation(before, after string) (expectation, error) {
	b, err := parseSets(before)
	if err != nil {
		return expectation{}, err
	}
	a, err := parseSets(after)
	if err != nil {
		return expectation{}, err
	}
	e := expectation{before: map[string]recordSet{}, after: map[string]recordSet{}}
	for _, set := range b {
		e.before[set.name] = set
	}
	for _, set := range a {
		e.after[set.name] = set
	}
	return e, nil
}

func (e expectation) names() []string {
	var names []string
	for name := range e.before {
		names = append(names, name)
	}
	for name := range e.after {
		if _, ok := e.before[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func variantIn(sets map[string]recordSet, name string, index int) (byte, bool) {
	set, ok := sets[name]
	if !ok || index >= set.count {
		return 0, false
	}
	return byte('0' + set.variant), true
}

func (e expectation) total(sets map[string]recordSet) int {
	n := 0
	for _, set := range sets {
		n += set.count
	}
	return n
}

// The three periods of a read a 200 can fall in.
const (
	beforeCompletion = iota // it ended before the read completed: the copy held every record of before
	acrossCompletion        // it may have been answered before or after: the records both hold are there
	afterCompletion         // it began after the read completed: the copy holds exactly after
)

// counts is a count per set, printed as set×n.
type counts map[string]int

func (c counts) String() string {
	var parts []string
	for name, n := range c {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", name, n))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (c counts) any() bool {
	for _, n := range c {
		if n > 0 {
			return true
		}
	}
	return false
}

// rowProblem names what is wrong with a 200 in period, or "" when nothing is.
func (e expectation) rowProblem(o observation, period int) string {
	missing, kept, stale, wrong := counts{}, counts{}, counts{}, counts{}
	for _, name := range e.names() {
		chars := o.decoded[name]
		size := max(e.before[name].count, e.after[name].count)
		for i := range size {
			c := byte('.')
			if i < len(chars) {
				c = chars[i]
			}
			bv, inBefore := variantIn(e.before, name, i)
			av, inAfter := variantIn(e.after, name, i)
			present := c != '.'
			if present && !(inBefore && c == bv) && !(inAfter && c == av) {
				wrong[name]++
				continue
			}
			switch period {
			case beforeCompletion:
				if inBefore && !present {
					missing[name]++
				}
			case acrossCompletion:
				if inBefore && inAfter && !present {
					missing[name]++
				}
			case afterCompletion:
				switch {
				case inAfter && !present:
					missing[name]++
				case inAfter && c != av:
					stale[name]++
				case !inAfter && present:
					kept[name]++
				}
			}
		}
	}
	var parts []string
	if missing.any() {
		parts = append(parts, "missing "+missing.String())
	}
	if kept.any() {
		parts = append(parts, "still listing "+kept.String())
	}
	if stale.any() {
		parts = append(parts, "old values of "+stale.String())
	}
	if wrong.any() {
		parts = append(parts, "values no seeded record has for "+wrong.String())
	}
	if o.Unknowns > 0 {
		parts = append(parts, fmt.Sprintf("%d rows the rig never seeded (%s)", o.Unknowns, strings.Join(o.Unknown, " ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("a 200 of %d rows %s", o.Rows, strings.Join(parts, "; "))
}

// regressed counts the records of after that o listed at their after value and next no longer
// does: a record the read delivered never goes back.
func (e expectation) regressed(o, next observation) counts {
	back := counts{}
	for name, set := range e.after {
		was, now := o.decoded[name], next.decoded[name]
		for i := range set.count {
			want := byte('0' + set.variant)
			if i < len(was) && was[i] == want && (i >= len(now) || now[i] != want) {
				back[name]++
			}
		}
	}
	return back
}

// partial reports whether o lists some record of after not yet at its after value.
func (e expectation) partial(o observation) bool {
	for name, set := range e.after {
		chars := o.decoded[name]
		for i := range set.count {
			if i >= len(chars) || chars[i] != byte('0'+set.variant) {
				return true
			}
		}
	}
	return false
}

// verdict is a judge's finding: problems make it FAIL, an invalid reason INVALID, neither PASS.
type verdict struct {
	invalid  string
	problems []string
	notes    []string
}

func (v *verdict) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *verdict) print() {
	switch {
	case len(v.problems) > 0:
		more := ""
		if len(v.problems) > 1 {
			more = fmt.Sprintf(" (and %d more: %s)", len(v.problems)-1, strings.Join(v.problems[1:min(len(v.problems), 4)], "; "))
		}
		fmt.Printf("FAIL %s%s\n", v.problems[0], more)
	case v.invalid != "":
		fmt.Printf("INVALID %s\n", v.invalid)
	default:
		fmt.Printf("PASS %s\n", strings.Join(v.notes, "; "))
	}
}

func ms(d int64) string { return fmt.Sprintf("%dms", d/int64(time.Millisecond)) }

// at names o's time relative to origin.
func at(o observation, origin int64) string {
	return fmt.Sprintf("the poll at %+.3fs", float64(o.Start-origin)/float64(time.Second))
}

func answer(o observation) string {
	if o.Status == 0 {
		return "no answer (" + o.Transport + ")"
	}
	text := fmt.Sprintf("%d", o.Status)
	if o.Error != "" || o.Code != "" {
		text += fmt.Sprintf(" %q code=%q", o.Error, o.Code)
	}
	if o.Body != "" {
		text += " " + o.Body
	}
	if o.RetryAfter != "" {
		text += " Retry-After=" + o.RetryAfter
	}
	return text
}

func refilling(o observation) bool {
	return o.Status == http.StatusServiceUnavailable && o.Error == refillingMessage && o.Code == refillingCode && o.RetryAfter == refillingRetryAfter
}

// complete reports whether o is a 200 listing exactly the records of sets.
func complete(o observation, sets map[string]recordSet) bool {
	if o.Status != http.StatusOK || o.Transport != "" {
		return false
	}
	return expectation{before: sets, after: sets}.rowProblem(o, afterCompletion) == "" && o.Rows == expectation{}.total(sets)
}

func runJudge(args []string) error {
	if len(args) == 0 {
		return errors.New("judge reread|interrupt|boot|outage|recreate-live")
	}
	judges := map[string]func([]string) (verdict, error){
		"reread":        judgeReread,
		"interrupt":     judgeInterrupt,
		"boot":          judgeBoot,
		"outage":        judgeOutage,
		"recreate-live": judgeRecreateLive,
	}
	judge, ok := judges[args[0]]
	if !ok {
		return fmt.Errorf("judge %q: want reread, interrupt, boot, outage or recreate-live", args[0])
	}
	v, err := judge(args[1:])
	if err != nil {
		return err
	}
	v.print()
	return nil
}

func firstLine(lines []logLine, after int64, match func(logLine) bool) (logLine, bool) {
	for _, line := range lines {
		if line.at >= after && match(line) {
			return line, true
		}
	}
	return logLine{}, false
}

// The kvwatch lines about a recreated bucket. The watcher's stream probe (or the gate on a record,
// or the check before a read completes) logs foundLine when it finds the stream replaced under it,
// and every watch that installs onto a stream other than the one read before logs installLine as
// the read of the new bucket begins: after foundLine for a recreate the watcher found itself, alone
// for one a Rewatch read first (the self-health rebuild after Check, or the reconnect hook).
func foundLine(registry string) func(logLine) bool {
	return func(l logLine) bool {
		return strings.HasPrefix(l.msg, registry+" bucket was recreated under its watcher")
	}
}

func installLine(registry string) func(logLine) bool {
	return func(l logLine) bool { return strings.HasPrefix(l.msg, registry+" bucket was recreated;") }
}

// installText is installLine's text, for the messages that name it.
func installText(registry string) string { return registry + " bucket was recreated;" }

func warmUpLine(registry, outcome string) func(logLine) bool {
	return func(l logLine) bool {
		return l.msg == registry+" cache warm-up" && (outcome == "" || l.outcome == outcome)
	}
}

// judgeList applies an expectation to every answer of one list around a read that completed at
// completed (and, after a failed read, at last, when the sweep may come from a later read):
// every answer is a 200, nothing of before is missing until the read completed, the records both
// hold never are, nothing the read delivered goes back, and once settle has passed since the last
// completion the list is exactly after.
func judgeList(v *verdict, label string, obs []observation, e expectation, origin, completed, last int64, settle time.Duration) (after int) {
	var previous *observation
	for i := range obs {
		o := obs[i]
		if o.Status != http.StatusOK || o.Transport != "" {
			v.fail("%s: %s answered %s", label, at(o, origin), answer(o))
			continue
		}
		period := acrossCompletion
		switch {
		case o.End < completed:
			period = beforeCompletion
		case o.Start > last+int64(settle):
			period = afterCompletion
			after++
		}
		if problem := e.rowProblem(o, period); problem != "" {
			v.fail("%s: %s got %s", label, at(o, origin), problem)
		}
		if previous != nil {
			if back := e.regressed(*previous, o); back.any() {
				v.fail("%s: %s no longer listed at their new value records an earlier poll had: %s", label, at(o, origin), back)
			}
		}
		previous = &obs[i]
	}
	return after
}
