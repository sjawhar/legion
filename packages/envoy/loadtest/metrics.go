package main

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Dispatch's Prometheus endpoint (DISPATCH_METRICS_ADDR, GET /metrics). The 5 s sampler scrapes it
// and keeps every dispatch_* series in its sample (sample.Metrics); summarizeMetrics reduces the
// in-window samples to the Pools and admission table.

// metricsPrefix is the prefix of the series a sample keeps.
const metricsPrefix = "dispatch_"

// metricKey is a series' key in sample.Metrics: its name, then its labels sorted by name,
// name{a="1",b="2"}, each value escaped as the text format escapes it; the name alone when it has
// no label.
func metricKey(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(labels)) {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + `="` + labelEscaper.Replace(labels[k]) + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// parseMetrics reads Prometheus's text exposition format and keeps the samples of the series
// whose name starts with prefix, by metricKey. Comment and blank lines are skipped; a sample's
// timestamp is ignored. A line that is not a sample is an error.
func parseMetrics(r io.Reader, prefix string) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, err := parseMetricLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if strings.HasPrefix(name, prefix) {
			out[metricKey(name, labels)] = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseMetricLine reads one sample line: name{labels} value [timestamp].
func parseMetricLine(line string) (name string, labels map[string]string, value float64, err error) {
	end := strings.IndexAny(line, "{ \t")
	if end <= 0 {
		return "", nil, 0, fmt.Errorf("%q is not a sample", line)
	}
	name, rest := line[:end], line[end:]
	if rest[0] == '{' {
		if labels, rest, err = parseLabels(rest[1:]); err != nil {
			return "", nil, 0, fmt.Errorf("%q: %w", line, err)
		}
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 {
		return "", nil, 0, fmt.Errorf("%q has no value or more than a value and a timestamp", line)
	}
	if value, err = strconv.ParseFloat(fields[0], 64); err != nil {
		return "", nil, 0, fmt.Errorf("%q: value: %w", line, err)
	}
	return name, labels, value, nil
}

// parseLabels reads the labels after a series' "{" up to its "}", and returns what follows it.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, "", fmt.Errorf("a label has no name=")
		}
		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t")
		if !strings.HasPrefix(s, `"`) {
			return nil, "", fmt.Errorf("label %s has no quoted value", key)
		}
		var v strings.Builder
		i, closed := 1, false
		for ; i < len(s); i++ {
			c := s[i]
			if c == '"' {
				closed = true
				break
			}
			if c == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					v.WriteByte('\n')
				default:
					v.WriteByte(s[i])
				}
				continue
			}
			v.WriteByte(c)
		}
		if !closed {
			return nil, "", fmt.Errorf("label %s has an unclosed value", key)
		}
		labels[key] = v.String()
		s = strings.TrimLeft(s[i+1:], " \t")
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "}"):
		default:
			return nil, "", fmt.Errorf("label %s is followed by neither , nor }", key)
		}
	}
}

// splitMetricKey is the name and labels of a metricKey.
func splitMetricKey(key string) (string, map[string]string) {
	name, rest, ok := strings.Cut(key, "{")
	if !ok {
		return key, nil
	}
	labels, _, err := parseLabels(rest)
	if err != nil {
		return name, nil
	}
	return name, labels
}

// scrapeMetrics GETs base/metrics and keeps its dispatch_* series; an error when the GET fails,
// answers other than 200, does not parse or holds no dispatch_* series (a Dispatch build without
// the endpoint).
func scrapeMetrics(base string) (map[string]float64, error) {
	url := base + "/metrics"
	body, err := getOK(shortClient, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	m, err := parseMetrics(body, metricsPrefix)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(url), err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("GET %s: no %s* series", redactURL(url), metricsPrefix)
	}
	return m, nil
}

// histBucket is one cumulative bucket of a histogram: the observations at or below le.
type histBucket struct {
	le    float64
	count float64
}

// histogramPct is the upper bound of the bucket that holds the p-th percentile (the lower nearest
// rank, as pct) of the observations that cumulative buckets count: +Inf when it lies past the last
// finite bound. false when the buckets count no observation.
func histogramPct(buckets []histBucket, p float64) (float64, bool) {
	bs := slices.Clone(buckets)
	sort.Slice(bs, func(i, j int) bool { return bs[i].le < bs[j].le })
	if len(bs) == 0 {
		return 0, false
	}
	total := bs[len(bs)-1].count
	if total <= 0 {
		return 0, false
	}
	rank := math.Floor(p/100*(total-1)) + 1
	for _, b := range bs {
		if b.count >= rank {
			return b.le, true
		}
	}
	return math.Inf(1), true
}

// metricsSummary is Dispatch's /metrics over the window. Maxima are over the in-window samples
// whose scrape worked; deltas are from the first of them to the last, and need two. A value whose
// series no scrape held is absent (null).
type metricsSummary struct {
	// Scraped counts the in-window samples whose scrape worked, Skipped those whose scrape failed
	// (a Dispatch build without the endpoint, or no -metrics under -allow-foreign).
	Scraped int `json:"scraped"`
	Skipped int `json:"skipped"`
	// Pools holds each database pool, by its pool label.
	Pools map[string]poolMetrics `json:"pools,omitempty"`
	// CheckoutsPerRequest is the requests pool's checkouts (the delta of
	// dispatch_db_pool_acquire_seconds_count) per in-window request or write that got an HTTP
	// status.
	CheckoutsPerRequest *float64 `json:"checkouts_per_request"`
	// HTTPQueuedMax and HTTPInFlightMax are dispatch_http_queued and dispatch_http_in_flight, by
	// class (read, write).
	HTTPQueuedMax   map[string]float64 `json:"http_queued_max,omitempty"`
	HTTPInFlightMax map[string]float64 `json:"http_in_flight_max,omitempty"`
	// HTTPRefused is the delta of dispatch_http_refused_total, by reason (queue_full,
	// wait_exceeded), both classes summed.
	HTTPRefused           map[string]float64 `json:"http_refused,omitempty"`
	DocWriterQueuedMax    *float64           `json:"doc_writer_queued_max"`
	SettlementsRunningMax *float64           `json:"settlements_running_max"`
	SettlementsWaitingMax *float64           `json:"settlements_waiting_max"`
	// LogDropped is the delta of dispatch_log_dropped_total, every level summed.
	LogDropped *float64 `json:"log_dropped"`
}

// poolMetrics is one database pool over the window.
type poolMetrics struct {
	WaitingMax *float64 `json:"waiting_max"` // dispatch_db_pool_waiting
	InUseMax   *float64 `json:"in_use_max"`  // dispatch_db_pool_in_use_conns
	// AcquireP99Ms is the upper bound, in ms, of the dispatch_db_pool_acquire_seconds bucket that
	// holds the 99th percentile of the window's acquires (the bucket deltas); null with no acquire,
	// or when that bucket is +Inf: AcquireP99AboveMs is then the last finite bound, in ms.
	AcquireP99Ms      *float64 `json:"acquire_p99_ms"`
	AcquireP99AboveMs *float64 `json:"acquire_p99_above_ms,omitempty"`
	// Checkouts is the delta of dispatch_db_pool_acquire_seconds_count.
	Checkouts *float64 `json:"checkouts"`
}

// summarizeMetrics reduces the in-window samples' metrics; answered is the in-window requests and
// writes that got an HTTP status.
func summarizeMetrics(ss []sample, answered int) metricsSummary {
	var out metricsSummary
	var ok []sample
	for _, s := range ss {
		if !s.InWindow {
			continue
		}
		if slices.Contains(s.FailedReads, readMetrics) || s.Metrics == nil {
			out.Skipped++
			continue
		}
		out.Scraped++
		ok = append(ok, s)
	}
	if len(ok) == 0 {
		return out
	}
	first, last := ok[0].Metrics, ok[len(ok)-1].Metrics
	deltas := len(ok) >= 2
	maxOf := func(keep func(name string, labels map[string]string) (string, bool)) map[string]float64 {
		m := map[string]float64{}
		for _, s := range ok {
			for key, v := range s.Metrics {
				name, labels := splitMetricKey(key)
				if group, keepIt := keep(name, labels); keepIt {
					if old, seen := m[group]; !seen || v > old {
						m[group] = v
					}
				}
			}
		}
		return m
	}
	// deltaOf sums, by group, last minus first of each kept series; a series the first sample
	// lacks counts from 0 (a pool opened during the window).
	deltaOf := func(keep func(name string, labels map[string]string) (string, bool)) map[string]float64 {
		m := map[string]float64{}
		if !deltas {
			return m
		}
		for key, v := range last {
			name, labels := splitMetricKey(key)
			if group, keepIt := keep(name, labels); keepIt {
				m[group] += v - first[key]
			}
		}
		return m
	}
	by := func(metric, label string) func(string, map[string]string) (string, bool) {
		return func(name string, labels map[string]string) (string, bool) {
			if name != metric {
				return "", false
			}
			return labels[label], true
		}
	}
	ptr := func(m map[string]float64, k string) *float64 {
		if v, ok := m[k]; ok {
			return &v
		}
		return nil
	}

	waiting := maxOf(by("dispatch_db_pool_waiting", "pool"))
	inUse := maxOf(by("dispatch_db_pool_in_use_conns", "pool"))
	checkouts := deltaOf(by("dispatch_db_pool_acquire_seconds_count", "pool"))
	buckets := map[string][]histBucket{}
	if deltas {
		for key, v := range last {
			name, labels := splitMetricKey(key)
			if name != "dispatch_db_pool_acquire_seconds_bucket" {
				continue
			}
			le, err := strconv.ParseFloat(labels["le"], 64)
			if err != nil {
				continue
			}
			buckets[labels["pool"]] = append(buckets[labels["pool"]], histBucket{le: le, count: v - first[key]})
		}
	}
	pools := map[string]bool{}
	for _, m := range []map[string]float64{waiting, inUse, checkouts} {
		for p := range m {
			pools[p] = true
		}
	}
	for p := range buckets {
		pools[p] = true
	}
	if len(pools) > 0 {
		out.Pools = map[string]poolMetrics{}
	}
	for p := range pools {
		pm := poolMetrics{WaitingMax: ptr(waiting, p), InUseMax: ptr(inUse, p), Checkouts: ptr(checkouts, p)}
		if le, found := histogramPct(buckets[p], 99); found {
			if math.IsInf(le, 1) {
				finiteMax := 0.0
				for _, b := range buckets[p] {
					if !math.IsInf(b.le, 1) {
						finiteMax = max(finiteMax, b.le)
					}
				}
				above := finiteMax * 1000
				pm.AcquireP99AboveMs = &above
			} else {
				ms := le * 1000
				pm.AcquireP99Ms = &ms
			}
		}
		out.Pools[p] = pm
	}
	if c, has := checkouts["requests"]; has && answered > 0 {
		v := c / float64(answered)
		out.CheckoutsPerRequest = &v
	}
	nonEmpty := func(m map[string]float64) map[string]float64 {
		if len(m) == 0 {
			return nil
		}
		return m
	}
	out.HTTPQueuedMax = nonEmpty(maxOf(by("dispatch_http_queued", "class")))
	out.HTTPInFlightMax = nonEmpty(maxOf(by("dispatch_http_in_flight", "class")))
	out.HTTPRefused = nonEmpty(deltaOf(by("dispatch_http_refused_total", "reason")))
	single := func(metric string) func(string, map[string]string) (string, bool) {
		return func(name string, _ map[string]string) (string, bool) { return "", name == metric }
	}
	out.DocWriterQueuedMax = ptr(maxOf(single("dispatch_doc_writer_queued")), "")
	out.SettlementsRunningMax = ptr(maxOf(single("dispatch_settlements_running")), "")
	out.SettlementsWaitingMax = ptr(maxOf(single("dispatch_settlements_waiting")), "")
	out.LogDropped = ptr(deltaOf(single("dispatch_log_dropped_total")), "")
	return out
}
