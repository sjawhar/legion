package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// scheduleSegment is one segment of a -schedule: a load factor held for a duration. The factor
// multiplies the step's -events and -requests.
type scheduleSegment struct {
	Factor    float64 `json:"factor"`
	DurationS float64 `json:"duration_s"`
}

func (s scheduleSegment) duration() time.Duration {
	return time.Duration(math.Round(s.DurationS * float64(time.Second)))
}

// schedule is a parsed -schedule, the segments of the window in order; empty when the step has
// none, and then the window holds the rates as given.
type schedule []scheduleSegment

// scheduleError refuses one item of a -schedule.
type scheduleError struct {
	Item   string
	Reason string
}

func (e *scheduleError) Error() string {
	return fmt.Sprintf("-schedule: %q %s; give <factor>x:<duration>,..., such as 1x:60s,5x:60s,1x:120s", e.Item, e.Reason)
}

// parseSchedule reads "<factor>x:<duration>,...": each factor a number more than 0 followed by
// x, each duration a Go duration more than 0. An empty string is no schedule.
func parseSchedule(s string) (schedule, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out schedule
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		factorText, durText, ok := strings.Cut(item, ":")
		if !ok {
			return nil, &scheduleError{Item: item, Reason: "is not <factor>x:<duration>"}
		}
		factorText, durText = strings.TrimSpace(factorText), strings.TrimSpace(durText)
		number, isFactor := strings.CutSuffix(factorText, "x")
		if !isFactor {
			return nil, &scheduleError{Item: item, Reason: fmt.Sprintf("has factor %q, which does not end in x", factorText)}
		}
		f, err := strconv.ParseFloat(number, 64)
		if err != nil || !finite(f) || f <= 0 {
			return nil, &scheduleError{Item: item, Reason: fmt.Sprintf("has factor %q, which is not a number more than 0", number)}
		}
		d, err := time.ParseDuration(durText)
		if err != nil || d <= 0 {
			return nil, &scheduleError{Item: item, Reason: fmt.Sprintf("has duration %q, which is not a duration more than 0", durText)}
		}
		out = append(out, scheduleSegment{Factor: f, DurationS: d.Seconds()})
	}
	return out, nil
}

// String is the schedule as -schedule takes it, with each duration in seconds.
func (s schedule) String() string {
	items := make([]string, len(s))
	for i, seg := range s {
		items[i] = fmt.Sprintf("%gx:%gs", seg.Factor, seg.DurationS)
	}
	return strings.Join(items, ",")
}

// total is the window the schedule fills.
func (s schedule) total() time.Duration {
	var t time.Duration
	for _, seg := range s {
		t += seg.duration()
	}
	return t
}

// meanFactor is the time-weighted mean factor over the window; 1 with no schedule.
func (s schedule) meanFactor() float64 {
	var sum, total float64
	for _, seg := range s {
		sum += seg.Factor * seg.DurationS
		total += seg.DurationS
	}
	if total <= 0 {
		return 1
	}
	return sum / total
}

// maxFactor is the highest factor; 1 with no schedule.
func (s schedule) maxFactor() float64 {
	if len(s) == 0 {
		return 1
	}
	m := 0.0
	for _, seg := range s {
		m = max(m, seg.Factor)
	}
	return m
}

// rateSegment is one piece of an open loop's rate plan: from `from` until the next segment's
// from, arrivals come at rate per second. The last segment has no end.
type rateSegment struct {
	from time.Time
	rate float64
}

// ratePlan is the open loop's rate plan for a base (1x) rate: the warm-up at 1x from start, then
// each schedule segment from the window start on at its factor. With no schedule it is the base
// rate from start on, so the generator draws the arrivals it draws without -schedule.
func ratePlan(base float64, start, winStart time.Time, s schedule) []rateSegment {
	plan := []rateSegment{{from: start, rate: base}}
	at := winStart
	for _, seg := range s {
		plan = append(plan, rateSegment{from: at, rate: base * seg.Factor})
		at = at.Add(seg.duration())
	}
	return plan
}

// nextArrival is the arrival after t of an inhomogeneous Poisson process whose rate the plan
// gives, from e, an exponential draw of mean 1: the time at which the plan's integrated rate from
// t reaches e. In a single segment of rate r it is t + e/r, the homogeneous schedule's step.
// The second result is false when no arrival comes (every rate from t on is 0).
func nextArrival(plan []rateSegment, t time.Time, e float64) (time.Time, bool) {
	i := 0
	for i+1 < len(plan) && !plan[i+1].from.After(t) {
		i++
	}
	for ; ; i++ {
		rate := plan[i].rate
		if i == len(plan)-1 {
			if rate <= 0 {
				return time.Time{}, false
			}
			return t.Add(time.Duration(e / rate * float64(time.Second))), true
		}
		end := plan[i+1].from
		if rate > 0 {
			if next := t.Add(time.Duration(e / rate * float64(time.Second))); next.Before(end) {
				return next, true
			}
			e = max(0, e-rate*end.Sub(t).Seconds())
		}
		t = end
	}
}
