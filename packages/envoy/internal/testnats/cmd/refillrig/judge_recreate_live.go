package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// judgeRecreateLive judges a bucket recreated under a live watcher well before the self-health
// monitor's next tick, so that tick's Check cannot be what reads it: the watcher must find the
// recreate itself (its stream probe) and complete its read of the new bucket before that tick, with
// no self-health failure or rebuild on the way. Every 200 must list each record of the old copy
// until that read completes, the renewed session's included, whose old record would have expired
// inside the re-read: after the finding, which stops the copy following, and before the read's
// completion. A 200 answered while the listener still claimed its copy follows the bucket
// (before any line says it stopped) must not list records only the new bucket has while it lacks
// others the new bucket held: that is a consumer nats.go moved onto the new stream, resuming past
// the records its old sequence covers. After the read a 200 lists every record the read could have
// delivered, and once the read and the fill are both done, exactly the new bucket.
func judgeRecreateLive(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge recreate-live", flag.ContinueOnError)
	path := flags.String("log", "", "listener log")
	offset := flags.Int64("offset", 0, "log offset the shape began at")
	registry := flags.String("registry", "session registry", "the cache's log name")
	obsPath := flags.String("obs", "", "the list's polls")
	before := flags.String("before", "", "sets the old bucket held")
	after := flags.String("after", "", "sets the new bucket holds")
	fillPath := flags.String("fill-log", "", "the fill log recreate wrote")
	ready := flags.Int64("ready-ns", 0, "the listener's ready line, from which its self-health monitor ticks")
	interval := flags.Duration("tick", 30*time.Second, "the self-health monitor's interval")
	renewed := flags.String("renewed", "", "the set whose old record would expire inside the read and which the new bucket renews")
	renewedFrom := flags.Int64("renewed-from-ns", 0, "when the rig began writing the renewed set's old record")
	renewedTo := flags.Int64("renewed-to-ns", 0, "when that write was acknowledged")
	ttl := flags.Duration("ttl", 0, "the session bucket's MaxAge")
	shape := flags.String("shape", "", "shorter or longer: the old stream against the new one")
	settle := flags.Duration("settle", time.Second, "how long a record stored in a followed bucket may take to be listed")
	margin := flags.Duration("scan-margin", 250*time.Millisecond, "how long before the read's install line a record must have been stored to be in its scan")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	if *ready == 0 || *ttl <= 0 || *renewed == "" || *renewedFrom == 0 || *renewedTo == 0 {
		return v, errors.New("-ready-ns, -ttl, -renewed, -renewed-from-ns and -renewed-to-ns are required")
	}
	e, err := newExpectation(*before, *after)
	if err != nil {
		return v, err
	}
	fill, err := readFillLog(*fillPath)
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
	invalid := func(format string, args ...any) {
		if v.invalid == "" {
			v.invalid = fmt.Sprintf(format, args...)
		}
	}
	origin := fill.Deleted
	since := func(t int64) string { return fmt.Sprintf("%+.3fs", float64(t-origin)/float64(time.Second)) }
	tick := *ready + ((origin-*ready)/int64(*interval)+1)*int64(*interval)
	if (*shape == "shorter" && fill.OldLastSeq >= fill.NewLastSeq) || (*shape == "longer" && fill.OldLastSeq <= fill.NewLastSeq) {
		invalid("the old stream ended at sequence %d and the new one at %d: not the %s old stream this shape needs", fill.OldLastSeq, fill.NewLastSeq, *shape)
	}

	found, hasFound := firstLine(lines, origin, foundLine(*registry))
	install, hasInstall := firstLine(lines, origin, installLine(*registry))
	var completed logLine
	hasCompleted := false
	readEnd := int64(math.MaxInt64)
	if hasInstall {
		if completed, hasCompleted = firstLine(lines, install.at, warmUpLine(*registry, "completed")); hasCompleted {
			readEnd = completed.at
		}
	}
	health, hasHealth := firstLine(lines, origin, func(l logLine) bool {
		return l.msg == "self-health probe failed" || l.msg == "self-health rebuild started"
	})
	hasHealth = hasHealth && health.at <= readEnd
	healthText := ""
	if hasHealth {
		healthText = fmt.Sprintf(" (the self-health monitor's %q at %s)", health.msg, since(health.at))
	}
	// stopped is when a line first said the copy no longer follows its bucket: until then the
	// listener claims its copy is the bucket.
	stopped := int64(math.MaxInt64)
	if line, ok := firstLine(lines, origin, func(l logLine) bool {
		return foundLine(*registry)(l) || installLine(*registry)(l) ||
			strings.HasPrefix(l.msg, *registry+" bucket was deleted under its watcher") ||
			l.msg == *registry+" watcher stopped" || strings.Contains(l.msg, "envoy nats disconnected")
	}); ok {
		stopped = line.at
	}

	switch {
	case !hasFound:
		v.fail("the watcher never found the bucket recreated under it: no %q line", *registry+" bucket was recreated under its watcher")
	case found.at >= tick:
		v.fail("the watcher found the recreated bucket only at %s, after the self-health tick at %s", since(found.at), since(tick))
	}
	switch {
	case !hasInstall:
		v.fail("no read of the new bucket began: no %q line", installText(*registry))
	case install.at >= tick:
		v.fail("the read of the new bucket began only at %s, after the self-health tick at %s%s", since(install.at), since(tick), healthText)
	case !hasCompleted:
		v.fail("the read of the new bucket that began at %s never completed", since(install.at))
	case completed.at >= tick:
		invalid("the self-health tick at %s fell inside the read of the new bucket, from %s to %s: the run missed its window", since(tick), since(install.at), since(completed.at))
	case hasHealth:
		v.fail("the self-health monitor took part before the read of the new bucket completed: %q at %s", health.msg, since(health.at))
	}

	// The copy must keep the renewed session's old record past its expiry only when it had stopped
	// following its bucket by then: a copy still following prunes it, as the old bucket would have.
	expiresFrom, expiresBy := *renewedFrom+int64(*ttl), *renewedTo+int64(*ttl)
	renewedKept := stopped < expiresFrom
	if !renewedKept {
		invalid("the copy still claimed to follow its bucket when %s's old record could expire at %s: the run missed its window", *renewed, since(expiresFrom))
	}

	settleNs := int64(*settle)
	scanBy := int64(math.MinInt64)
	if hasInstall {
		scanBy = install.at - int64(*margin)
	}
	whole := int64(math.MaxInt64)
	if hasCompleted {
		whole = max(readEnd, fill.Filled) + settleNs
	}
	var wrong, claimed pollRun
	inside, partial, keptRenewed, exact := 0, 0, 0, 0
	var previous *observation
	for i := range obs {
		o := obs[i]
		if o.Status != http.StatusOK || o.Transport != "" {
			v.fail("GET /v1/sessions: %s answered %s", at(o, origin), answer(o))
			continue
		}
		r := newLiveRows()
		for _, name := range e.names() {
			chars := o.decoded[name]
			for index := range max(e.before[name].count, e.after[name].count) {
				c := charAt(chars, index)
				bv, inBefore := variantIn(e.before, name, index)
				av, inAfter := variantIn(e.after, name, index)
				present := c != '.'
				if present && !(inBefore && c == bv) && !(inAfter && c == av) {
					r.wrong[name]++
					continue
				}
				acked := int64(math.MaxInt64)
				if inAfter {
					acked = fill.acked(name, index)
				}
				// held: the new bucket held the record long enough before o for a copy following
				// it to list it; scanned: it held it when the read of the new bucket began.
				held, scanned := acked < o.Start-settleNs, acked < scanBy
				if present && inAfter && c == av && !(inBefore && bv == av) {
					r.fresh[name]++
				}
				if held && !(present && c == av) {
					r.lacking[name]++
				}
				oldCopy := inBefore && (name != *renewed || renewedKept || o.End < expiresFrom)
				// required: o must list the record. Until the read completed the copy kept every
				// record of the old copy; a poll that may have been answered on either side of the
				// sweep holds the ones the read scanned; after it, every record the read scanned or
				// the bucket held long enough to arrive as a live update.
				var required bool
				switch {
				case o.End < readEnd:
					required = oldCopy
				case o.Start <= readEnd:
					required = oldCopy && scanned
				default:
					required = scanned || held
					if o.Start > readEnd+settleNs {
						if present && inAfter && c != av {
							r.stale[name]++
						}
						if present && !inAfter {
							r.kept[name]++
						}
					}
				}
				if required && !present {
					r.missing[name]++
				}
				// A record listed at its new value never goes back to its old one, nor away while o
				// must list it. (A record of the old copy the read did not scan is swept when the
				// read completes and comes back with its live update.)
				if previous != nil && inAfter && charAt(previous.decoded[name], index) == av && c != av && (present || required) {
					r.back[name]++
				}
			}
		}
		if problem := r.problem(o); problem != "" {
			wrong.add(o, problem)
		}
		previous = &obs[i]
		if o.Start >= origin && o.End < stopped && r.fresh.any() && r.lacking.any() {
			claimed.add(o, fmt.Sprintf("records only the new bucket has (%s) while it lacked %s the bucket held", r.fresh, r.lacking))
		}
		if hasFound && o.Start >= found.at && o.End <= readEnd {
			inside++
			if r.fresh.any() && e.partial(o) {
				partial++
			}
		}
		if renewedKept && o.Start > expiresBy && o.End < readEnd {
			keptRenewed++
		}
		if o.Start > whole && complete(o, e.after) {
			exact++
		}
	}
	if claimed.n > 0 {
		v.fail("GET /v1/sessions listed records of the new bucket while the listener still claimed its copy follows the bucket (no line said it had stopped) and lacked records the bucket held, in %d polls from %s to %s; the first: %s",
			claimed.n, since(claimed.first.Start), since(claimed.last.Start), claimed.what)
	}
	if wrong.n > 0 {
		v.fail("GET /v1/sessions answered wrong rows in %d polls from %s to %s; the first: %s", wrong.n, since(wrong.first.Start), since(wrong.last.Start), wrong.what)
	}
	if !hasFound || !hasCompleted {
		return v, nil
	}
	if inside == 0 {
		invalid("no poll fell between the watcher's finding at %s and the read's completion at %s", since(found.at), since(readEnd))
	}
	if renewedKept && keptRenewed == 0 {
		invalid("no poll fell between %s's old record's expiry at %s and the read's completion at %s", *renewed, since(expiresBy), since(readEnd))
	}
	if exact == 0 {
		invalid("no poll came once the read and the fill were both done (%s)", since(whole))
	}
	v.notes = append(v.notes,
		fmt.Sprintf("the old stream ended at sequence %d, the new one at %d", fill.OldLastSeq, fill.NewLastSeq),
		fmt.Sprintf("the watcher's probe found the recreate at %s and its read of the new bucket ran from %s to %s (%d records), before the self-health tick at %s, with no self-health failure or rebuild",
			since(found.at), since(install.at), since(readEnd), completed.entries, since(tick)),
		fmt.Sprintf("%d of %d polls fell between the finding and the read's completion, %d of them with the new bucket partly delivered; every poll listed every record of the old copy until the read completed, and none listed the new bucket's records while the copy claimed to follow",
			inside, len(obs), partial),
		fmt.Sprintf("%s's old record would have expired at %s, after the finding and before the read completed, and the %d polls from then to the read's completion kept it", *renewed, since(expiresBy), keptRenewed),
		fmt.Sprintf("%d polls once the read and the fill were done listed exactly the new bucket", exact))
	return v, nil
}

// liveRows is what one 200 got wrong under judgeRecreateLive's rules, and what it shows of the new
// bucket: fresh counts the records listed at a value only the new bucket has, lacking the records
// the new bucket held by the poll that it does not list at their new value.
type liveRows struct {
	missing, stale, kept, wrong, back, fresh, lacking counts
}

func newLiveRows() liveRows {
	return liveRows{counts{}, counts{}, counts{}, counts{}, counts{}, counts{}, counts{}}
}

func (r liveRows) problem(o observation) string {
	var parts []string
	if r.missing.any() {
		parts = append(parts, "missing "+r.missing.String())
	}
	if r.kept.any() {
		parts = append(parts, "still listing "+r.kept.String())
	}
	if r.stale.any() {
		parts = append(parts, "old values of "+r.stale.String())
	}
	if r.wrong.any() {
		parts = append(parts, "values no seeded record has for "+r.wrong.String())
	}
	if r.back.any() {
		parts = append(parts, "no longer listing at their new value records an earlier poll had: "+r.back.String())
	}
	if o.Unknowns > 0 {
		parts = append(parts, fmt.Sprintf("%d rows the rig never seeded (%s)", o.Unknowns, strings.Join(o.Unknown, " ")))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("a 200 of %d rows %s", o.Rows, strings.Join(parts, "; "))
}

// pollRun gathers the polls one rule found wrong, so a verdict names their span and the first.
type pollRun struct {
	n           int
	first, last observation
	what        string
}

func (p *pollRun) add(o observation, what string) {
	if p.n == 0 {
		p.first, p.what = o, what
	}
	p.last = o
	p.n++
}

// charAt is a poll's character for the record at index, '.' past the end.
func charAt(chars []byte, index int) byte {
	if index < len(chars) {
		return chars[index]
	}
	return '.'
}
