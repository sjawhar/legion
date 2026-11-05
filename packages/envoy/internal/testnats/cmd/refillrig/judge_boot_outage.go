package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	"time"
)

// metricsIn finds the metrics scrapes of obs inside [from, to).
func metricsIn(obs []observation, from, to int64) []observation {
	var out []observation
	for _, o := range obs {
		if o.Start >= from && o.End < to {
			out = append(out, o)
		}
	}
	return out
}

func gauge(o observation, list string) *int64 {
	if list == "sessions" {
		return o.Sessions
	}
	return o.Interests
}

func gaugeName(list string) string { return "envoy_active_" + list }

// judgeBoot judges a listener's start on a full bucket (proof P3a): each list answers the
// refilling 503 or a complete 200, never a 200 short of the bucket, and its gauge has a sample only
// when, and equal to what, the list answers.
func judgeBoot(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge boot", flag.ContinueOnError)
	sessionsPath := flags.String("sessions-obs", "", "GET /v1/sessions polls")
	interestsPath := flags.String("interests-obs", "", "GET /v1/interests/ polls")
	metricsPath := flags.String("metrics-obs", "", "/metrics polls")
	spec := flags.String("sets", "", "sets both buckets hold")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	sets, err := newExpectation(*spec, *spec)
	if err != nil {
		return v, err
	}
	metrics, err := readObservations(*metricsPath)
	if err != nil {
		return v, err
	}
	total := int64(sets.total(sets.after))
	metricsAnswered := false
	for _, m := range metrics {
		switch {
		case m.Status == 0 && !metricsAnswered:
			// The listener has not bound its port yet.
		case m.Status != http.StatusOK:
			v.fail("/metrics: a scrape answered %s", answer(m))
		default:
			metricsAnswered = true
		}
	}
	for _, list := range []string{"sessions", "interests"} {
		path := map[string]string{"sessions": *sessionsPath, "interests": *interestsPath}[list]
		obs, err := readObservations(path)
		if err != nil {
			return v, err
		}
		if len(obs) == 0 {
			v.invalid = "no " + list + " poll"
			continue
		}
		origin := obs[0].Start
		answered := false
		refused, whole := 0, 0
		var lastRefused, firstWhole int64 = 0, math.MaxInt64
		for _, o := range obs {
			switch {
			case o.Status == 0 && !answered:
				// The listener has not bound its port yet.
			case refilling(o):
				answered = true
				refused++
				lastRefused = max(lastRefused, o.Start)
			case complete(o, sets.after):
				answered = true
				whole++
				firstWhole = min(firstWhole, o.End)
			default:
				answered = true
				problem := answer(o)
				if o.Status == http.StatusOK {
					problem = sets.rowProblem(o, afterCompletion)
				}
				v.fail("GET %s: %s answered neither the refilling 503 nor the whole bucket: %s", listPaths[list], at(o, origin), problem)
			}
		}
		unsampled := 0
		for _, m := range metrics {
			value := gauge(m, list)
			switch {
			case m.Status != http.StatusOK:
			case value == nil && m.End < lastRefused:
				unsampled++
			case value != nil && *value != total:
				v.fail("%s: %s had the sample %d, and the bucket holds %d", gaugeName(list), at(m, origin), *value, total)
			case value != nil && m.End < lastRefused:
				v.fail("%s: %s had a sample while GET %s still answered 503", gaugeName(list), at(m, origin), listPaths[list])
			case value == nil && m.Start > firstWhole:
				v.fail("%s: %s had no sample once GET %s answered the whole bucket", gaugeName(list), at(m, origin), listPaths[list])
			}
		}
		if refused == 0 || whole == 0 {
			v.invalid = fmt.Sprintf("GET %s answered the refilling 503 %d times and the whole bucket %d times: the polls missed the boot", listPaths[list], refused, whole)
		}
		v.notes = append(v.notes, fmt.Sprintf("GET %s: %d refilling 503s then %d complete 200s of %d records; %s had no sample in the %d scrapes before its list's last 503 and %d after its first complete 200",
			listPaths[list], refused, whole, total, gaugeName(list), unsampled, sampledAfter(metrics, list, firstWhole, total)))
	}
	return v, nil
}

// sampledAfter counts the scrapes begun after from whose gauge for list has the sample total.
func sampledAfter(metrics []observation, list string, from, total int64) int {
	n := 0
	for _, m := range metrics {
		if value := gauge(m, list); m.Start > from && value != nil && *value == total {
			n++
		}
	}
	return n
}

// judgeOutage judges a NATS outage past the stale bound and the recovery after it (proof P3b):
// each list serves its copy within the bound, answers the refilling 503 past it with no gauge
// sample, and once NATS is back answers a complete 200 within the recovery limit, its gauge equal
// to its length.
func judgeOutage(args []string) (verdict, error) {
	flags := flag.NewFlagSet("judge outage", flag.ContinueOnError)
	sessionsPath := flags.String("sessions-obs", "", "GET /v1/sessions polls")
	interestsPath := flags.String("interests-obs", "", "GET /v1/interests/ polls")
	metricsPath := flags.String("metrics-obs", "", "/metrics polls")
	spec := flags.String("sets", "", "sets both buckets hold")
	down := flags.Int64("down-ns", 0, "when NATS was stopped")
	restarted := flags.Int64("restart-ns", 0, "when NATS was started again")
	seeded := flags.Int64("seeded-ns", 0, "when the records were written")
	bound := flags.Duration("bound", 2*time.Minute, "the stale bound")
	margin := flags.Duration("margin", 5*time.Second, "slack around the bound")
	within := flags.Duration("within", 30*time.Second, "how soon after NATS is back the list must be whole")
	ttl := flags.Duration("ttl", 5*time.Minute, "the session bucket's record lifetime")
	var v verdict
	if err := flags.Parse(args); err != nil {
		return v, err
	}
	if *restarted-*seeded > int64(*ttl-30*time.Second) {
		v.invalid = fmt.Sprintf("NATS came back %s after the records were written, too close to their %s lifetime for the list to be whole", time.Duration(*restarted-*seeded).Round(time.Second), *ttl)
		return v, nil
	}
	sets, err := newExpectation(*spec, *spec)
	if err != nil {
		return v, err
	}
	metrics, err := readObservations(*metricsPath)
	if err != nil {
		return v, err
	}
	total := int64(sets.total(sets.after))
	refuseFrom := *down + int64(*bound+*margin)
	serveUntil := *down + int64(*bound-*margin)
	for _, list := range []string{"sessions", "interests"} {
		path := map[string]string{"sessions": *sessionsPath, "interests": *interestsPath}[list]
		obs, err := readObservations(path)
		if err != nil {
			return v, err
		}
		served, refused, firstRefused := 0, 0, int64(0)
		var recovered int64
		for _, o := range obs {
			switch {
			case o.End < serveUntil:
				if !complete(o, sets.after) {
					v.fail("GET %s: %s, %s after NATS stopped and within the bound, answered %s instead of its copy", listPaths[list], at(o, *down), time.Duration(o.Start-*down).Round(time.Second), describe(o, sets))
				} else {
					served++
				}
			case o.Start > refuseFrom && o.End < *restarted:
				if !refilling(o) {
					v.fail("GET %s: %s, %s after NATS stopped and past the %s bound, answered %s instead of the refilling 503", listPaths[list], at(o, *down), time.Duration(o.Start-*down).Round(time.Second), *bound, describe(o, sets))
				} else {
					refused++
				}
			case o.Start > *restarted:
				switch {
				case complete(o, sets.after):
					if recovered == 0 {
						recovered = o.End
					}
				case refilling(o) && recovered == 0:
				default:
					v.fail("GET %s: %s, after NATS came back, answered %s", listPaths[list], at(o, *down), describe(o, sets))
				}
			}
			if refilling(o) && firstRefused == 0 && o.Start > *down {
				firstRefused = o.Start
			}
		}
		if refused == 0 || served == 0 {
			v.invalid = fmt.Sprintf("GET %s: %d polls within the bound and %d past it: the polls missed the outage", listPaths[list], served, refused)
		}
		switch {
		case recovered == 0:
			v.fail("GET %s never answered the whole bucket after NATS came back", listPaths[list])
		case recovered-*restarted > int64(*within):
			v.fail("GET %s answered the whole bucket %s after NATS came back, later than %s", listPaths[list], time.Duration(recovered-*restarted).Round(time.Millisecond), *within)
		}
		for _, m := range metrics {
			value := gauge(m, list)
			switch {
			case m.Start > refuseFrom && m.End < *restarted:
				if value != nil {
					v.fail("%s: %s, past the bound with NATS down, had the sample %d", gaugeName(list), at(m, *down), *value)
				}
			case recovered != 0 && m.Start > recovered:
				if m.Status != http.StatusOK || value == nil || *value != total {
					v.fail("%s: %s, after GET %s answered %d records, had %s", gaugeName(list), at(m, *down), listPaths[list], total, sampleText(m, value))
				}
			}
		}
		v.notes = append(v.notes, fmt.Sprintf("GET %s: its copy for %d polls within the bound, the refilling 503 from %s after NATS stopped (%d polls past the bound), whole again %s after NATS came back",
			listPaths[list], served, time.Duration(firstRefused-*down).Round(time.Second), refused, time.Duration(recovered-*restarted).Round(time.Millisecond)))
	}
	unsampled := 0
	for _, m := range metricsIn(metrics, refuseFrom, *restarted) {
		if m.Status == http.StatusOK && m.Sessions == nil && m.Interests == nil {
			unsampled++
		}
	}
	if unsampled == 0 && v.invalid == "" {
		v.invalid = "no /metrics scrape fell past the bound while NATS was down"
	}
	v.notes = append(v.notes, fmt.Sprintf("%d /metrics scrapes past the bound had neither gauge's sample; after recovery both equal %d", unsampled, total))
	return v, nil
}

func describe(o observation, sets expectation) string {
	if o.Status == http.StatusOK && o.Transport == "" {
		if problem := sets.rowProblem(o, afterCompletion); problem != "" {
			return problem
		}
		return fmt.Sprintf("a 200 of all %d records", o.Rows)
	}
	return answer(o)
}

func sampleText(m observation, value *int64) string {
	if m.Status != http.StatusOK {
		return answer(m)
	}
	if value == nil {
		return "no sample"
	}
	return fmt.Sprintf("the sample %d", *value)
}
