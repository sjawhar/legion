package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// runtimeHolder is the process runtime, Sandbox or tmux (config.HolderRuntime): everything a
// launch reads, and the pod shapes the Sandbox runtime caches when it is built
// (runtime/sandbox's podShape). It is a runtime.Runtime itself, held by the machines' Deps, the
// supervision and the API's tree releaser, so a rebuild reaches all of them behind one pointer
// and takes no pod down: the new runtime observes and adopts the processes the old one was
// supervising, which are the cluster's and the tmux server's, not the runtime's.
//
// Observe is the one call whose answer outlives it: supervision ranges over one channel for its
// whole life. The holder owns that channel and feeds it from whichever runtime is current, so a
// rebuild moves the feed across without supervision noticing.
//
// Every other call is forwarded through enter, which counts it against the runtime it runs on:
// a rebuild ends the runtime it replaced only once the calls inside it have returned, within
// that runtime's own launch bound and a margin (runtimeDrainBound), so a Spawn or a Resume
// already waiting on that runtime's informers finishes on informers that are still watching.
type runtimeHolder struct {
	current atomic.Pointer[runtime.Runtime]
	// build makes a runtime from a configuration and answers the stop that ends it: the Sandbox
	// runtime's informers, its cache and credential refreshers and the tmux runtime's sweeps run
	// on a context of that runtime's own, so a rebuild can end the one it replaces rather than
	// leave it watching the cluster for the daemon's whole life.
	build func(cfg config.Config) (runtime.Runtime, context.CancelFunc, error)
	log   *slog.Logger

	// mu guards the observation feed and the runtime the holder is serving from: out is the
	// channel supervision ranges over, once it has asked for one, observing the context it asked
	// on, and stopFeed ends the feed from the runtime being replaced. feeds numbers the feeds, so
	// the one a rebuild replaced knows not to close the channel the new one is writing to, and
	// closed says the channel is already closed. serving is the runtime every forwarded call
	// enters, with the stop that ends it.
	mu       sync.Mutex
	out      chan runtime.Observation
	watching context.Context
	stopFeed context.CancelFunc
	serving  *servedRuntime
	feeds    uint64
	closed   bool
}

// servedRuntime is one runtime of the holder's while it is the one calls run on: the runtime
// itself, whose launch bound is how long a rebuild waits for the calls inside it, the stop that
// ends everything that runtime runs of its own, how many calls are inside it now, and the
// channel the last of those calls closes once it has returned.
type servedRuntime struct {
	rt      runtime.Runtime
	stop    context.CancelFunc
	calls   int
	drained chan struct{}
}

var _ runtime.Runtime = (*runtimeHolder)(nil)

// runtimeHolding is the runtime behind its holder as the daemon holds it: the runtime itself,
// and the holder the settings observer registers and rebuilds.
//
// newRuntimeHolder answers one of two shapes, because a Go type either implements an interface
// or it does not, and three readers of `deps.Runtime` ask it for one the core runtime.Runtime
// does not carry: a closed issue's own resources (runtime.IssueSuspender, outbox's
// record.IssueSuspend and its release save), the unpushed work of a released workspace
// (runtime.WorkspaceBundler) and whether an issue's pod runs the configured image
// (issuePodImages, which attachIdleRelease asks before it starts the idle tick). The holder in
// front of a runtime that answers them answers them too; the one in front of a runtime that
// answers none of them answers none, so a tmux deployment's readers take the same miss they
// took before the holder existed.
type runtimeHolding interface {
	runtime.Runtime
	rebuilt
}

// heldRuntime is the runtime a holder holds now, whichever shape the holder has (sandboxOf).
type heldRuntime interface {
	get() runtime.Runtime
}

// newRuntimeHolder holds rt, which stop ends, and builds its replacements with build.
func newRuntimeHolder(rt runtime.Runtime, stop context.CancelFunc, log *slog.Logger, build func(cfg config.Config) (runtime.Runtime, context.CancelFunc, error)) runtimeHolding {
	if log == nil {
		log = slog.Default()
	}
	h := &runtimeHolder{build: build, log: log, serving: &servedRuntime{rt: rt, stop: stop}}
	h.current.Store(&rt)
	if answersIssueResources(rt) {
		return &issueResourceHolder{runtimeHolder: h}
	}
	return h
}

// answersIssueResources is whether rt answers any of the three interfaces an issue-resource
// holder forwards. The Sandbox runtime answers all three and the tmux runtime none, and a
// runtime that answered some of them is held by that holder too: each of its three methods
// answers a runtime that does not carry the interface exactly as the reader's own missed type
// assertion did (issueResourceHolder).
//
// It is read once, as the holder is built, and the shape it chooses is sound only because the
// runtime's kind cannot change under a running daemon: `runtime`, `runtime.kubernetes` and
// `runtime.tmux` are ClassFixed rows (config/classes.go), so a rebuild builds the same kind
// again from its launch keys. A reader who reclassed one of those rows to `rebuild` would leave
// a daemon that booted on tmux holding a plain holder in front of a Sandbox runtime, with all
// three behaviours off and no test failing; such a change belongs with a shape chosen per call.
func answersIssueResources(rt runtime.Runtime) bool {
	if _, suspends := rt.(runtime.IssueSuspender); suspends {
		return true
	}
	if _, bundles := rt.(runtime.WorkspaceBundler); bundles {
		return true
	}
	_, reads := rt.(issuePodImages)
	return reads
}

func (h *runtimeHolder) holder() string { return config.HolderRuntime }

// rebuild builds the runtime again from the configuration that has just settled, moves
// supervision's feed onto it, swaps it in, and only then ends the one it replaced: everything
// that runtime runs of its own — the Sandbox runtime's three informers and its cache and
// credential refreshers — ends with it, so a deployment that changes a `runtime.*` key ten
// times is watching the cluster once and not ten times over.
//
// A call already inside the runtime being replaced holds that end back, up to that runtime's
// own drain bound (runtimeDrainBound): the informers a Spawn's await and a Resume's awaitNewPod
// poll (runtime/sandbox's await, awaitNewPod) are the replaced runtime's, and a launch left
// polling a store nothing refreshes fails at its boot timeout and spends one of the claim's
// launch failures (supervise.Limits.LaunchFailures). Past the bound the runtime is ended anyway
// and the call sees the cancelled context, as the NATS rebuild closes a connection whose intake
// never settled.
//
// A failure changes nothing: the runtime that was built is stopped, the feed stays on the
// runtime the daemon is supervising with, and the rebuilder logs what could not be built.
func (h *runtimeHolder) rebuild(cfg *config.Config) error {
	next, stopNext, err := h.build(*cfg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if h.out != nil {
		// The feed moves to the new runtime before the old one's ends, so no observation is
		// missed between them; a duplicate observation of one process is what every probe
		// already answers.
		previous := h.stopFeed
		h.feeds++
		if err := h.feed(next); err != nil {
			h.feeds--
			h.mu.Unlock()
			stopNext()
			return err
		}
		if previous != nil {
			previous()
		}
	}
	h.current.Store(&next)
	replaced := h.serving
	h.serving = &servedRuntime{rt: next, stop: stopNext}
	var draining chan struct{}
	if replaced.calls > 0 {
		replaced.drained = make(chan struct{})
		draining = replaced.drained
	}
	h.mu.Unlock()
	if draining != nil {
		bound := runtimeDrainBound(replaced.rt)
		select {
		case <-draining:
		case <-time.After(bound):
			h.log.Warn("legion settings: a call was still inside the runtime being replaced; the daemon ends that runtime anyway and the call sees its context cancelled",
				"holder", config.HolderRuntime, "waited", bound)
		}
	}
	if replaced.stop != nil {
		replaced.stop()
	}
	return nil
}

// runtimeDrainBound is how long a rebuild waits for the calls inside the runtime it replaced to
// return before it ends that runtime anyway: that runtime's own LaunchBound — the longest one
// Spawn or Resume try may rightfully run on its account, the Sandbox runtime's being
// `worker_boot_timeout_seconds` several times over plus its provision bound and its calls
// (runtime/sandbox's launchBound) — with drainBound as the margin for the Suspend, Release or
// ReplacePod that is no launch at all. A runtime that states no bound, whose calls wait on
// nothing of the kind (tmux, whose pane start is a local command), is waited drainBound.
//
// It is read from the runtime being replaced and not from the configuration, because that
// runtime's bound is the one its calls were started under: `worker_boot_timeout_seconds` is the
// runtime's own to rebuild, so the change being applied here may be the one that raised it, and
// a bound shorter than the work inside the old runtime would stop that runtime under a launch
// that had not yet had its turn.
func runtimeDrainBound(rt runtime.Runtime) time.Duration {
	if launch := rt.LaunchBound(); launch > 0 {
		return launch + drainBound
	}
	return drainBound
}

func (h *runtimeHolder) get() runtime.Runtime { return *h.current.Load() }

// enter is the runtime this call runs on and the end of the call, which every forwarded call
// defers: the rebuild that replaces the runtime between the two waits for this call before it
// ends the runtime the call is inside (rebuild).
func (h *runtimeHolder) enter() (runtime.Runtime, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rt, serving := h.get(), h.serving
	serving.calls++
	return rt, func() { h.left(serving) }
}

// left records that one call of serving has returned, and tells the rebuild waiting on it when
// it was the last.
func (h *runtimeHolder) left(serving *servedRuntime) {
	h.mu.Lock()
	defer h.mu.Unlock()
	serving.calls--
	if serving.calls == 0 && serving.drained != nil {
		close(serving.drained)
		serving.drained = nil
	}
}

// Observe is supervision's one channel of process observations, fed from whichever runtime is
// current. The caller's context is the feed's life; a second call answers the same channel.
func (h *runtimeHolder) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.out != nil {
		return h.out, nil
	}
	h.out, h.watching = make(chan runtime.Observation), ctx
	if err := h.feed(h.get()); err != nil {
		h.out, h.watching = nil, nil
		return nil, err
	}
	return h.out, nil
}

// feed starts forwarding rt's observations into the holder's channel until the rebuild that
// replaces rt, or the context Observe was given, ends. The caller holds mu.
//
// The last feed closes the channel as it ends, as a runtime's own Observe channel closes when
// its context ends: supervision ranges over it, and a channel that never closes would hold the
// daemon's stop until its budget ran out. A feed a rebuild replaced closes nothing — the feed
// that replaced it is writing to that same channel.
func (h *runtimeHolder) feed(rt runtime.Runtime) error {
	feeding, stop := context.WithCancel(h.watching)
	observations, err := rt.Observe(feeding)
	if err != nil {
		stop()
		return err
	}
	h.stopFeed = stop
	mine := h.feeds
	go func() {
		defer h.ended(mine)
		defer stop()
		for {
			select {
			case <-feeding.Done():
				return
			case observation, open := <-observations:
				if !open {
					return
				}
				select {
				case h.out <- observation:
				case <-feeding.Done():
					return
				}
			}
		}
	}()
	return nil
}

// ended closes the observation channel once the feed numbered mine was the last one.
func (h *runtimeHolder) ended(mine uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds != mine || h.closed {
		return
	}
	h.closed = true
	close(h.out)
}

func (h *runtimeHolder) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	rt, left := h.enter()
	defer left()
	return rt.Spawn(ctx, spec)
}

func (h *runtimeHolder) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	rt, left := h.enter()
	defer left()
	return rt.Resume(ctx, prev, spec)
}

func (h *runtimeHolder) Suspend(ctx context.Context, loc runtime.Locator) error {
	rt, left := h.enter()
	defer left()
	return rt.Suspend(ctx, loc)
}

func (h *runtimeHolder) Release(ctx context.Context, k runtime.Known) error {
	rt, left := h.enter()
	defer left()
	return rt.Release(ctx, k)
}

func (h *runtimeHolder) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	rt, left := h.enter()
	defer left()
	return rt.Probe(ctx, loc)
}

func (h *runtimeHolder) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	rt, left := h.enter()
	defer left()
	return rt.ReconcileOrphans(ctx, known, grace)
}

func (h *runtimeHolder) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	rt, left := h.enter()
	defer left()
	return rt.AdoptWorkingCopy(ctx, loc, id)
}

func (h *runtimeHolder) ProvisionsWorkspaces() bool { return h.get().ProvisionsWorkspaces() }

func (h *runtimeHolder) ProvisionBound() time.Duration { return h.get().ProvisionBound() }

func (h *runtimeHolder) LaunchBound() time.Duration { return h.get().LaunchBound() }

func (h *runtimeHolder) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	rt, left := h.enter()
	defer left()
	return rt.PodDrift(ctx, k)
}

func (h *runtimeHolder) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	rt, left := h.enter()
	defer left()
	return rt.ReplacePod(ctx, k, due)
}

func (h *runtimeHolder) RescueUnpushed(ctx context.Context, k runtime.Known, request runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
	rt, left := h.enter()
	defer left()
	return rt.RescueUnpushed(ctx, k, request)
}

func (h *runtimeHolder) CleanupTree(ctx context.Context, tree string) error {
	rt, left := h.enter()
	defer left()
	return rt.CleanupTree(ctx, tree)
}

// issueResourceHolder is the holder in front of a runtime that answers what a deployment with
// resources of its own per issue is asked: the Sandbox runtime, whose Sandbox, pod and disk a
// closed issue's suspension deletes, whose shim bundles the unpushed work of a released
// workspace, and which knows whether an issue's pod runs the image the configuration names.
// Three readers of `deps.Runtime` ask for each by a type assertion (outbox's record.IssueSuspend
// and bundleIssueWorkspace, workflow's attachIdleRelease), and a holder that answered none of
// them took those three behaviours out of every Kubernetes deployment while every other test of
// this package passed.
//
// Each call enters the runtime it runs on as every other forwarded call does, so a rebuild waits
// for it rather than ending the runtime under it. A runtime that does not carry the interface —
// which the `runtime` selector being fixed for the process makes impossible under a rebuild, and
// which a runtime answering two of the three would be — is answered exactly as the reader's own
// missed assertion answered: nothing to delete, nothing to bundle, no pod image read.
type issueResourceHolder struct {
	*runtimeHolder
}

var (
	_ runtimeHolding           = (*issueResourceHolder)(nil)
	_ runtime.IssueSuspender   = (*issueResourceHolder)(nil)
	_ runtime.WorkspaceBundler = (*issueResourceHolder)(nil)
	_ issuePodImages           = (*issueResourceHolder)(nil)
)

// SuspendIssue deletes the issue's own resources through the runtime that holds them, under the
// authorization the caller passes: the Sandbox, the pod and the disk of a closed issue.
func (h *issueResourceHolder) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	rt, left := h.enter()
	defer left()
	suspender, suspends := rt.(runtime.IssueSuspender)
	if !suspends {
		return nil
	}
	return suspender.SuspendIssue(ctx, issue, tree, authorize)
}

// BundleWorkspace asks the live role's process to bundle the unpushed work of the issue's
// workspace. A runtime that bundles none answers the error the reader turns into the release's
// own hold, as its missed assertion did.
func (h *issueResourceHolder) BundleWorkspace(ctx context.Context, issue string, role claim.Role) (runtime.WorkspaceBundle, error) {
	rt, left := h.enter()
	defer left()
	bundler, bundles := rt.(runtime.WorkspaceBundler)
	if !bundles {
		return runtime.WorkspaceBundle{}, errors.New("the runtime bundles no workspace")
	}
	return bundler.BundleWorkspace(ctx, issue, role)
}

// IssuePodRunsConfiguredImage is whether the issue's pod runs the image the configuration names.
// It takes no context and reaches no cluster — the Sandbox runtime answers it from the pod it
// already watches — so it reads the runtime the holder holds now rather than entering it.
func (h *issueResourceHolder) IssuePodRunsConfiguredImage(issue string) bool {
	images, reads := h.get().(issuePodImages)
	return reads && images.IssuePodRunsConfiguredImage(issue)
}
