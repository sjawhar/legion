package main

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// afterRestart is the span after a restart that the restart table and the post-restart comment
// latencies cover, and restartBucket the step of the per-interval CPU in it.
const (
	afterRestart  = 60 * time.Second
	restartBucket = 5 * time.Second
)

// restartSummary is the restart of Dispatch at -restart-at into the window and Dispatch's and
// Postgres's CPU in the minute after it.
type restartSummary struct {
	// AtS is -restart-at: the seconds into the window the restart was due.
	AtS float64 `json:"at_s"`
	// At is when stack.sh restart-dispatch started (it stops Dispatch at once); Done when it
	// returned, with Dispatch's /healthz ok again or a failure.
	At        time.Time `json:"at"`
	Done      time.Time `json:"done"`
	DurationS float64   `json:"duration_s"`
	OldPID    int       `json:"old_pid"`
	NewPID    int       `json:"new_pid"`
	Error     string    `json:"error,omitempty"`
	// DispatchCPUS and PostgresCPUS are the CPU-seconds from At to At + 60 s; null when the samples
	// do not cover that span. The per-5-s lists cover it from At in 5 s steps, up to the last step
	// the samples cover. Dispatch's ticks run on across its old and new pid (cpuStitch).
	DispatchCPUS      *float64  `json:"dispatch_cpu_s_60s"`
	DispatchCPUSPer5s []float64 `json:"dispatch_cpu_s_per_5s"`
	PostgresCPUS      *float64  `json:"postgres_cpu_s_60s"`
	PostgresCPUSPer5s []float64 `json:"postgres_cpu_s_per_5s"`
}

// restarter runs `stack.sh restart-dispatch` once, at a set time.
type restarter struct {
	stackSh string
	at      time.Time
	begun   atomic.Bool
	done    chan struct{}
	sum     restartSummary
}

// run waits for the restart time, then restarts Dispatch through stack.sh under the run's lock
// token (inherited in loadgen's environment). The sampler takes a sample at once, follows the old
// pid's CPU to its exit, and does not count Dispatch's absence during the restart as an exit.
func (r *restarter) run(ctx context.Context, smp *sampler, lg *log.Logger) {
	defer close(r.done)
	if !sleepUntil(ctx, r.at) {
		r.sum.Error = "the step ended before the restart was due"
		return
	}
	old := smp.currentPIDs()["dispatch"]
	r.sum.OldPID = old
	smp.setRestarting("dispatch")
	smp.sampleNow()
	r.sum.At = time.Now()
	r.begun.Store(true)
	go smp.followExit("dispatch", old)
	lg.Printf("restart: stack.sh restart-dispatch (dispatch pid %d)", old)
	out, err := exec.CommandContext(ctx, "bash", r.stackSh, "restart-dispatch").CombinedOutput()
	r.sum.Done = time.Now()
	r.sum.DurationS = r.sum.Done.Sub(r.sum.At).Seconds()
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		lg.Printf("restart: %s", line)
	}
	smp.refreshPIDs()
	r.sum.NewPID = smp.currentPIDs()["dispatch"]
	smp.setRestarting("")
	if err != nil {
		r.sum.Error = "stack.sh restart-dispatch: " + err.Error()
		lg.Printf("restart: %s", r.sum.Error)
		return
	}
	lg.Printf("restart: dispatch pid %d -> %d in %.1f s", old, r.sum.NewPID, r.sum.DurationS)
}

// fillCPU sets the CPU columns from the step's samples.
func (r *restartSummary) fillCPU(ss []sample) {
	if r.At.IsZero() {
		return
	}
	r.DispatchCPUS, r.DispatchCPUSPer5s = cpuAfter(ss, "dispatch", r.At)
	r.PostgresCPUS, r.PostgresCPUSPer5s = cpuAfter(ss, "postgres", r.At)
}

// cpuAfter is name's CPU-seconds over afterRestart from at, and per restartBucket from at.
func cpuAfter(ss []sample, name string, at time.Time) (*float64, []float64) {
	var total *float64
	if v, ok := cpuSecondsBetween(ss, name, at, at.Add(afterRestart)); ok {
		total = &v
	}
	per := []float64{}
	for from := at; from.Before(at.Add(afterRestart)); from = from.Add(restartBucket) {
		v, ok := cpuSecondsBetween(ss, name, from, from.Add(restartBucket))
		if !ok {
			break
		}
		per = append(per, v)
	}
	return total, per
}

// cpuSecondsBetween is name's CPU-seconds from from to to, from its cumulative ticks interpolated
// linearly between the two samples around each end; false when the samples do not reach both ends
// or lack the process there.
func cpuSecondsBetween(ss []sample, name string, from, to time.Time) (float64, bool) {
	a, okA := ticksAt(ss, name, from)
	b, okB := ticksAt(ss, name, to)
	if !okA || !okB {
		return 0, false
	}
	return (b - a) / clkTck, true
}

// ticksAt is name's cumulative CPU ticks at t, interpolated between the samples just before and
// just after it.
func ticksAt(ss []sample, name string, t time.Time) (float64, bool) {
	for i := 1; i < len(ss); i++ {
		p, q := ss[i-1], ss[i]
		if t.Before(p.At) || t.After(q.At) {
			continue
		}
		tp, okP := p.CPUTicks[name]
		tq, okQ := q.CPUTicks[name]
		if !okP || !okQ {
			return 0, false
		}
		span := q.At.Sub(p.At)
		if span <= 0 {
			return tq, true
		}
		return tp + (tq-tp)*float64(t.Sub(p.At))/float64(span), true
	}
	return 0, false
}
