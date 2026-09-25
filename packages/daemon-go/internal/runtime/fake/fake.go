// Package fake is the runtime boundary as a test double: it records every call, answers from a
// script, and plays observations into the sweep on demand.
//
// It exists because the things the supervisor does are mostly invisible in their return values —
// a suspend, a release, an orphan reconciliation all return only an error — and because the
// interesting orderings (an observation that arrives during a resume, a stale incarnation, a
// duplicated verdict) are not reachable by driving a real tmux server.
//
// A fake ignores its context except where the contract is about the context: only the sweep,
// whose channel closes when the context ends, reads one.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.Runtime = (*Runtime)(nil)

// Call is one method the caller called, with the arguments it called it with. A method that does
// not take a given argument leaves it zero.
type Call struct {
	Method  string
	Spec    runtime.SpawnSpec
	Locator runtime.Locator
	// Previous is Resume's previous incarnation; nil when the caller recorded none.
	Previous *runtime.Locator
	// Released is Release's claim; its Locator is nil for a claim released with no process.
	Released runtime.Known
	Known    []runtime.Known
	Grace    time.Duration
	Identity runtime.GitIdentity
}

// SpawnResult is one scripted answer to Spawn or Resume.
type SpawnResult struct {
	Locator runtime.Locator
	Err     error
}

// ProbeResult is one scripted answer to Probe: a verdict about the locator probed, or the
// runtime failing to answer at all. The fake fills the observation's locator and time in.
type ProbeResult struct {
	Kind   runtime.ObservationKind
	Detail string
	Err    error
}

// Runtime is the fake. The zero value is not usable; call NewRuntime.
type Runtime struct {
	// Launch is what ControllerLaunch answers. NewRuntime sets it to "daemon", the tmux answer.
	Launch runtime.ControllerLaunch
	// Now stamps the observations the fake mints. NewRuntime sets it to time.Now; a test with a
	// clock of its own replaces it before use.
	Now func() time.Time

	mu       sync.Mutex
	pending  *sync.Cond
	calls    []Call
	spawns   []SpawnResult
	resumes  []SpawnResult
	probes   []ProbeResult
	failures map[string]error
	queue    []runtime.Observation
	minted   int
}

// NewRuntime is a fake ready to record: no script, controller launched by the daemon, real time.
func NewRuntime() *Runtime {
	fake := &Runtime{
		Launch:   runtime.ControllerLaunchDaemon,
		Now:      time.Now,
		failures: map[string]error{},
	}
	fake.pending = sync.NewCond(&fake.mu)
	return fake
}

// ScriptSpawn queues answers to Spawn, one per call, in order. Spawn mints its own locator once
// the script runs out.
func (r *Runtime) ScriptSpawn(results ...SpawnResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spawns = append(r.spawns, results...)
}

// ScriptResume queues answers to Resume, one per call, in order.
func (r *Runtime) ScriptResume(results ...SpawnResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumes = append(r.resumes, results...)
}

// ScriptProbe queues answers to Probe, one per call, in order. An unscripted probe answers
// `Alive`: the uninteresting case stays out of the test.
func (r *Runtime) ScriptProbe(results ...ProbeResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, results...)
}

// Emit hands the sweep observations. They are queued, so a test may emit before `Observe` is
// ever called; one sweep consumes them, in the order they were emitted.
func (r *Runtime) Emit(observations ...runtime.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, observation := range observations {
		if observation.At.IsZero() {
			observation.At = r.Now()
		}
		r.queue = append(r.queue, observation)
	}
	r.pending.Broadcast()
}

// FailSuspend makes every later Suspend return err.
func (r *Runtime) FailSuspend(err error) { r.fail("Suspend", err) }

// FailRelease makes every later Release return err.
func (r *Runtime) FailRelease(err error) { r.fail("Release", err) }

// FailObserve makes every later Observe return err instead of a sweep.
func (r *Runtime) FailObserve(err error) { r.fail("Observe", err) }

// FailReconcileOrphans makes every later ReconcileOrphans return err.
func (r *Runtime) FailReconcileOrphans(err error) { r.fail("ReconcileOrphans", err) }

// FailAdoptWorkingCopy makes every later AdoptWorkingCopy return err.
func (r *Runtime) FailAdoptWorkingCopy(err error) { r.fail("AdoptWorkingCopy", err) }

func (r *Runtime) fail(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[method] = err
}

// Calls is every call made so far, in order.
func (r *Runtime) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// CallsOf is every call of one method, in order.
func (r *Runtime) CallsOf(method string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var of []Call
	for _, call := range r.calls {
		if call.Method == method {
			of = append(of, call)
		}
	}
	return of
}

// Methods is the order the calls came in, which is usually the whole assertion.
func (r *Runtime) Methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	methods := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		methods = append(methods, call.Method)
	}
	return methods
}

func (r *Runtime) Spawn(_ context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Spawn", Spec: spec})
	if len(r.spawns) > 0 {
		next := r.spawns[0]
		r.spawns = r.spawns[1:]
		return next.Locator, next.Err
	}
	return r.mint(spec.Claim), nil
}

func (r *Runtime) Resume(_ context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := Call{Method: "Resume", Spec: spec}
	if prev != nil {
		recorded := *prev
		call.Previous = &recorded
	}
	r.calls = append(r.calls, call)
	if len(r.resumes) > 0 {
		next := r.resumes[0]
		r.resumes = r.resumes[1:]
		return next.Locator, next.Err
	}
	return r.mint(spec.Claim), nil
}

func (r *Runtime) Suspend(_ context.Context, loc runtime.Locator) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Suspend", Locator: loc})
	return r.failures["Suspend"]
}

// Release records its claim, then refuses one whose locator disagrees with it, as every runtime
// does.
func (r *Runtime) Release(_ context.Context, k runtime.Known) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Release", Released: copyKnown(k)})
	if err := k.Validate(); err != nil {
		return err
	}
	return r.failures["Release"]
}

func (r *Runtime) Probe(_ context.Context, loc runtime.Locator) (runtime.Observation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Probe", Locator: loc})
	kind, detail := runtime.Alive, ""
	if len(r.probes) > 0 {
		next := r.probes[0]
		r.probes = r.probes[1:]
		if next.Err != nil {
			return runtime.Observation{}, next.Err
		}
		kind, detail = next.Kind, next.Detail
	}
	return runtime.Observation{Locator: loc, Kind: kind, At: r.Now(), Detail: detail}, nil
}

// Observe is the sweep. Every observation emitted, before or after this call, reaches the
// channel in order; the channel closes when ctx ends. One sweep at a time: two live sweeps would
// split the emissions between them, which no test wants.
func (r *Runtime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Method: "Observe"})
	failure := r.failures["Observe"]
	r.mu.Unlock()
	if failure != nil {
		return nil, failure
	}

	sweep := make(chan runtime.Observation)
	// The pump waits on the condition; the context ending has to wake it, or a sweep with
	// nothing queued would never notice that it is over.
	go func() {
		<-ctx.Done()
		r.mu.Lock()
		r.pending.Broadcast()
		r.mu.Unlock()
	}()
	go func() {
		defer close(sweep)
		for {
			r.mu.Lock()
			for len(r.queue) == 0 && ctx.Err() == nil {
				r.pending.Wait()
			}
			if ctx.Err() != nil {
				r.mu.Unlock()
				return
			}
			observation := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()
			select {
			case sweep <- observation:
			case <-ctx.Done():
				return
			}
		}
	}()
	return sweep, nil
}

// ReconcileOrphans records the known set, then refuses an entry whose locator disagrees with its
// claim, as every runtime does.
func (r *Runtime) ReconcileOrphans(_ context.Context, known []runtime.Known, grace time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recorded := make([]runtime.Known, len(known))
	for i, entry := range known {
		recorded[i] = copyKnown(entry)
	}
	r.calls = append(r.calls, Call{Method: "ReconcileOrphans", Known: recorded, Grace: grace})
	for _, entry := range known {
		if err := entry.Validate(); err != nil {
			return err
		}
	}
	return r.failures["ReconcileOrphans"]
}

// copyKnown is k with a locator of its own, so a caller that reuses its locator later does not
// rewrite what was recorded.
func copyKnown(k runtime.Known) runtime.Known {
	if k.Locator != nil {
		loc := *k.Locator
		k.Locator = &loc
	}
	return k
}

func (r *Runtime) AdoptWorkingCopy(_ context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "AdoptWorkingCopy", Locator: loc, Identity: id})
	return r.failures["AdoptWorkingCopy"]
}

func (r *Runtime) ControllerLaunch() runtime.ControllerLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "ControllerLaunch"})
	return r.Launch
}

// mint is the locator an unscripted spawn or resume hands back: tmux-shaped, because that is a
// shape the store round-trips and `Validate` accepts, and a fresh incarnation every time,
// because that is what starting a process gives you. Called with the lock held.
func (r *Runtime) mint(token claim.Token) runtime.Locator {
	r.minted++
	return runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       token,
		Incarnation: fmt.Sprintf("%d:%d", 31000+r.minted, 900000+r.minted),
		Tmux: &runtime.TmuxLocator{
			Window: fmt.Sprintf("@%d", r.minted),
			Pane:   fmt.Sprintf("%%%d", r.minted),
		},
	}
}
