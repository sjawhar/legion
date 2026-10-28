package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// trendFrom and trendTo are the minutes of the hour's window that the trend fits: from
// minute 5, inclusive, past the warm-up, to minute 60, exclusive.
const (
	trendFrom = 5
	trendTo   = 60
)

// trendPoint is one minute's mean of a series, by its minute index from window_start.
type trendPoint struct {
	minute int
	v      float64
}

// trendFit is the ordinary least-squares line through a series' minute means: n points, their
// mean, and the slope per hour with its 95% interval [lo, hi] (Student t, n−2 degrees of freedom).
type trendFit struct {
	n                   int
	mean, slope, lo, hi float64
}

// fitTrend fits the points against their time in hours; false with fewer than 3 points.
func fitTrend(points []trendPoint) (trendFit, bool) {
	n := float64(len(points))
	if len(points) < 3 {
		return trendFit{}, false
	}
	hours := func(p trendPoint) float64 { return float64(p.minute) / 60 }
	var sx, sy float64
	for _, p := range points {
		sx += hours(p)
		sy += p.v
	}
	mx, my := sx/n, sy/n
	var sxx, sxy float64
	for _, p := range points {
		dx := hours(p) - mx
		sxx += dx * dx
		sxy += dx * (p.v - my)
	}
	if sxx == 0 {
		return trendFit{}, false
	}
	slope := sxy / sxx
	var sse float64
	for _, p := range points {
		r := p.v - (my + slope*(hours(p)-mx))
		sse += r * r
	}
	half := studentT975(n-2) * math.Sqrt(sse/(n-2)/sxx)
	return trendFit{n: len(points), mean: my, slope: slope, lo: slope - half, hi: slope + half}, true
}

// upward says the series trends upward: the interval's lower bound above 0, and the slope above
// 5% of the mean per hour.
func (f trendFit) upward() bool { return f.lo > 0 && f.slope > 0.05*f.mean }

// studentT975 is the 0.975 quantile of Student's t with df degrees of freedom, the half-width
// factor of a two-sided 95% interval: the bisection of its CDF.
func studentT975(df float64) float64 {
	lo, hi := 0.0, 1000.0
	for range 100 {
		mid := (lo + hi) / 2
		if studentTCDF(mid, df) < 0.975 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// studentTCDF is P(T ≤ t) for t ≥ 0 with df degrees of freedom: 1 − I_x(df/2, 1/2)/2 at
// x = df/(df+t²).
func studentTCDF(t, df float64) float64 {
	return 1 - 0.5*regIncBeta(df/(df+t*t), df/2, 0.5)
}

// regIncBeta is the regularized incomplete beta function I_x(a, b), by its continued fraction
// (betaCF) on the side of x where it converges fast.
func regIncBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(x, a, b) / a
	}
	return 1 - front*betaCF(1-x, b, a)/b
}

// betaCF evaluates the incomplete beta function's continued fraction by the modified Lentz
// method.
func betaCF(x, a, b float64) float64 {
	const tiny = 1e-300
	clamp := func(v float64) float64 {
		if math.Abs(v) < tiny {
			return tiny
		}
		return v
	}
	c, d := 1.0, 1/clamp(1-(a+b)*x/(a+1))
	h := d
	for m := 1.0; m <= 300; m++ {
		even := m * (b - m) * x / ((a + 2*m - 1) * (a + 2*m))
		d = 1 / clamp(1+even*d)
		c = clamp(1 + even/c)
		h *= d * c
		odd := -(a + m) * (a + b + m) * x / ((a + 2*m) * (a + 2*m + 1))
		d = 1 / clamp(1+odd*d)
		c = clamp(1 + odd/c)
		step := d * c
		h *= step
		if math.Abs(step-1) < 1e-15 {
			break
		}
	}
	return h
}

// trendSeries is one row of TREND.md: its name, the decimals of its numbers, and its minute
// means; why says why it has none.
type trendSeries struct {
	name   string
	d      int
	points []trendPoint
	why    string
}

// hourSeries are the hour step's three series over minutes trendFrom to trendTo: Dispatch's cores
// and working set from cgroup.json's minutes (a null minute is left out), and its Go heap from
// samples.json (hourHeap).
func hourSeries(s *curveStep) []trendSeries {
	cores := trendSeries{name: "Dispatch cores", d: 3}
	wset := trendSeries{name: "Dispatch working set MiB", d: 1}
	r := s.role("dispatch")
	switch {
	case s.cgroup == nil:
		cores.why = "no cgroup.json"
	case r == nil:
		cores.why = "cgroup.json has no dispatch role"
	case r.Minutes == nil:
		cores.why = "cgroup.json's dispatch role has no minutes"
	default:
		for m := trendFrom; m < min(trendTo, len(r.Minutes)); m++ {
			if mm := r.Minutes[m]; mm != nil {
				cores.points = append(cores.points, trendPoint{m, mm.Cores})
				wset.points = append(wset.points, trendPoint{m, mib(mm.WorkingSetBytesMean)})
			}
		}
	}
	wset.why = cores.why
	return []trendSeries{cores, wset, hourHeap(s)}
}

// The names of the hour's heap series: the heap profile's in-use bytes, or an older run's
// runtime.MemStats HeapInuse, a different metric.
const (
	trendHeapLive     = "Go heap live MiB (heap profile)"
	trendHeapMemStats = "MemStats HeapInuse MiB (older run)"
)

// hourHeap is the hour step's 1-minute means of its Go heap, MiB, over the heap reads of
// samples.json that worked and ended in the window, each in the minute it ended (sample.HeapAt):
// the heap profile's in-use bytes, or, in an older run's samples.json (one with `heap_inuse`),
// runtime.MemStats HeapInuse under its own name. Its why says why there are none.
func hourHeap(s *curveStep) trendSeries {
	heap := trendSeries{name: trendHeapLive, d: 1}
	if s.meta == nil || s.meta.WindowStart.IsZero() {
		heap.why = "no window_start in meta.json"
		return heap
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "samples.json"))
	if err != nil {
		heap.why = "samples.json: " + err.Error()
		return heap
	}
	var ss []sample
	if err := json.Unmarshal(b, &ss); err != nil {
		heap.why = "samples.json: " + err.Error()
		return heap
	}
	value := func(x sample) float64 { return x.HeapLiveBytes }
	if slices.ContainsFunc(ss, func(x sample) bool { return x.MemStatsHeapInuse > 0 }) {
		heap.name = trendHeapMemStats
		value = func(x sample) float64 { return x.MemStatsHeapInuse }
	}
	var sum [trendTo]float64
	var n [trendTo]int
	for _, x := range ss {
		// An older samples.json read the heap in the sample itself: it has no heap_at.
		at, in := x.HeapAt, x.HeapInWindow
		if at.IsZero() {
			at, in = x.At, x.InWindow
		}
		if !in || value(x) <= 0 || slices.Contains(x.FailedReads, readHeap) {
			continue
		}
		m := int(at.Sub(s.meta.WindowStart) / time.Minute)
		if at.Before(s.meta.WindowStart) || m < trendFrom || m >= trendTo {
			continue
		}
		sum[m] += value(x)
		n[m]++
	}
	for m := trendFrom; m < trendTo; m++ {
		if n[m] > 0 {
			heap.points = append(heap.points, trendPoint{m, mib(sum[m] / float64(n[m]))})
		}
	}
	return heap
}

// renderTrend is TREND.md over the hour steps, "" when there is none.
func renderTrend(steps []*curveStep) string {
	var hours []*curveStep
	for _, s := range steps {
		if s.kind == kindHour {
			hours = append(hours, s)
		}
	}
	if len(hours) == 0 {
		return ""
	}
	var md markdown
	md.line("# Trend over the hour\n")
	md.line("Each row is a series of 1-minute means over minutes %d to %d of the hour step's window (minute %d inclusive to %d exclusive, past the warm-up): Dispatch's cores and working set from cgroup.json's `minutes` (a minute with a gap over %s in the cgroup samples is left out), and its Go heap from samples.json's in-window heap reads: `%s` is the heap profile's in-use bytes, a sampled estimate of the live heap; `%s` is an older run's runtime.MemStats HeapInuse, a different metric. The slope is the ordinary least-squares slope per hour with its 95%% interval (Student t, n − 2 degrees of freedom), and as a share of the series' mean per hour.\n",
		trendFrom, trendTo, trendFrom, trendTo, seconds(cgroupMaxGap), trendHeapLive, trendHeapMemStats)
	md.line("Rule: a series is upward when the interval's lower bound is above 0 and the slope is above 5%% of the mean per hour. This is reported as measured; nothing here judges a step pass or fail.")
	for _, s := range hours {
		md.line("\n## %s\n", s.label())
		if s.failed {
			md.line("The step failed (%s), so the trend takes no number from it.", strings.Join(s.failedKinds, ", "))
			continue
		}
		md.header("series", "minutes", "mean", "slope per hour", "95% interval", "slope % of mean per hour", "upward", "note")
		for _, ts := range hourSeries(s) {
			fit, ok := fitTrend(ts.points)
			note := ts.why
			if !ok {
				if note == "" {
					note = fmt.Sprintf("%d minutes with a value; a fit needs 3", len(ts.points))
				}
				md.row(ts.name, num(float64(len(ts.points)), 0), "-", "-", "-", "-", "-", mdCell(note))
				continue
			}
			share := "-"
			if fit.mean != 0 {
				share = num(100*fit.slope/fit.mean, 1) + "%"
			}
			md.row(ts.name, num(float64(fit.n), 0), num(fit.mean, ts.d), num(fit.slope, ts.d),
				num(fit.lo, ts.d)+" to "+num(fit.hi, ts.d), share, yesNo(fit.upward()), mdEscape(note))
		}
	}
	return md.String()
}
