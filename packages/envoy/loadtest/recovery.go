package main

import (
	"time"
)

// recoveryWindow is the length of a recovery window, and the thresholds of a recovered one: at
// least recoveryMinSuccess of its requests 2xx, at least recoveryMinCommitted of its offered
// events committed, and its request p99 at most recoveryMaxP99Ratio times the reference.
const (
	recoveryWindow       = 10 * time.Second
	recoveryMinSuccess   = 0.99
	recoveryMinCommitted = 0.95
	recoveryMaxP99Ratio  = 2.0
	// recoveryRefWindows is how many of the last windows give the reference p99 when
	// -recovery-ref-p99 is not given.
	recoveryRefWindows = 3
)

// The sources of a recovery's reference p99.
const (
	recoveryRefFlag = "-recovery-ref-p99"
	recoveryRefLast = "last 3 windows"
)

// recoverySummary is how the step recovered after the -schedule's last drop: the start of its last
// segment.
type recoverySummary struct {
	Schedule string `json:"schedule"`
	// DropS is the drop's offset into the window, in seconds; FactorBefore and FactorAfter are the
	// factors of the last two segments.
	DropS        float64 `json:"drop_s"`
	FactorBefore float64 `json:"factor_before"`
	FactorAfter  float64 `json:"factor_after"`
	WindowS      float64 `json:"window_s"`
	// RefP99Ms is the reference request p99 and RefSource where it came from: -recovery-ref-p99, or
	// the requests of the last three windows; null when those windows hold no request.
	RefP99Ms  *float64 `json:"ref_p99_ms"`
	RefSource string   `json:"ref_source"`
	// RecoverySeconds is the time from the drop to the start of the first window from which every
	// window is recovered; null when the last window is not.
	RecoverySeconds *float64       `json:"recovery_seconds"`
	Windows         []recoveryStep `json:"windows"`
}

// recoveryStep is one whole 10 s window after the drop. Requests count by their start, writes by
// their scheduled arrival.
type recoveryStep struct {
	StartS     float64 `json:"start_s"` // after the drop
	Requests   int     `json:"requests"`
	RequestsOK int     `json:"requests_2xx"`
	ReqMs      stat    `json:"req_ms"` // request latency, failures included
	Offered    int     `json:"offered"`
	Committed  int     `json:"committed"`
	// Recovered is set when the window meets every criterion; a criterion with nothing to measure
	// (no request, no offered event, no reference p99) is met.
	Recovered bool `json:"recovered"`
}

// computeRecovery measures the recovery after the last drop of s, a schedule of two segments or
// more over [winStart, winEnd). The windows are the whole 10 s windows from the drop to the window
// end; a remainder shorter than 10 s is not counted. reqs are the requests sent in the window,
// writes the step's writes and commits the run's commit rows (joinDelivery's committed rule).
// refP99 is -recovery-ref-p99, nil to take the reference from the last three windows.
func computeRecovery(s schedule, winStart, winEnd time.Time, reqs []reqRec, writes []writeRec, commits []commitRow, refP99 *float64) *recoverySummary {
	if len(s) < 2 {
		return nil
	}
	var dropOffset time.Duration
	for _, seg := range s[:len(s)-1] {
		dropOffset += seg.duration()
	}
	drop := winStart.Add(dropOffset)
	n := int(winEnd.Sub(drop) / recoveryWindow)
	out := &recoverySummary{
		Schedule: s.String(), DropS: dropOffset.Seconds(), FactorBefore: s[len(s)-2].Factor, FactorAfter: s[len(s)-1].Factor,
		WindowS: recoveryWindow.Seconds(), Windows: make([]recoveryStep, n),
	}
	index := func(at int64) int {
		if at < drop.UnixNano() {
			return -1
		}
		k := int((at - drop.UnixNano()) / int64(recoveryWindow))
		if k >= n {
			return -1
		}
		return k
	}
	latencies := make([][]float64, n)
	for _, r := range reqs {
		k := index(r.at)
		if k < 0 {
			continue
		}
		w := &out.Windows[k]
		w.Requests++
		if !r.transport && r.status/100 == 2 {
			w.RequestsOK++
		}
		latencies[k] = append(latencies[k], float64(r.latency)/1e6)
	}
	byMarker := commitsByMarker(commits)
	for _, wr := range writes {
		k := index(wr.Scheduled)
		if !wr.InWindow || k < 0 {
			continue
		}
		out.Windows[k].Offered++
		if primaryCommit(byMarker[wr.Marker], primaryEventType[wr.Kind]) != nil {
			out.Windows[k].Committed++
		}
	}
	for k := range out.Windows {
		out.Windows[k].StartS = float64(k) * recoveryWindow.Seconds()
		out.Windows[k].ReqMs = mkStat(latencies[k])
	}

	out.RefSource = recoveryRefFlag
	out.RefP99Ms = refP99
	if refP99 == nil {
		out.RefSource = recoveryRefLast
		var last []float64
		for k := max(0, n-recoveryRefWindows); k < n; k++ {
			last = append(last, latencies[k]...)
		}
		if ref := mkStat(last); ref.N > 0 {
			out.RefP99Ms = &ref.P99
		}
	}

	for k := range out.Windows {
		w := &out.Windows[k]
		success := w.Requests == 0 || float64(w.RequestsOK) >= recoveryMinSuccess*float64(w.Requests)
		committed := w.Offered == 0 || float64(w.Committed) >= recoveryMinCommitted*float64(w.Offered)
		fast := w.ReqMs.N == 0 || out.RefP99Ms == nil || w.ReqMs.P99 <= recoveryMaxP99Ratio**out.RefP99Ms
		w.Recovered = success && committed && fast
	}
	if n > 0 && out.Windows[n-1].Recovered {
		k := n - 1
		for k > 0 && out.Windows[k-1].Recovered {
			k--
		}
		secs := out.Windows[k].StartS
		out.RecoverySeconds = &secs
	}
	return out
}
