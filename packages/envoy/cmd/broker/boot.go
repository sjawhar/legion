package main

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
)

// brokerBoot is what the boot sequence does its work through: each collaborator a function field,
// so that a test drives the sequence - the first policy load, the reread listener, the wait on
// its first LISTEN, the catch-up load and the bind - against fakes of its own and reads back the
// order it ran them in. A row makes real only what the thing it pins is about: ordering_test.go's
// two boot rows each bind 127.0.0.1:0, and the catch-up row drives a real reread listener on a
// real Postgres, because the race it pins is one Postgres itself decides. main fills every field
// from the configuration it has loaded; boot calls each one exactly where main used to do that
// work itself.
type brokerBoot struct {
	// LoadPolicy takes the first whole-namespace policy load and starts the five-minute reload
	// behind it, on the serving context boot hands it. In production:
	// policy.NewCurrent(serving, loader, policyRefresh).
	LoadPolicy func(ctx context.Context) (*policy.Current, error)
	// StartListener starts the receiving half of the reread fan-out on ctx and returns at once,
	// the listener redialing behind it for the life of the process. current is the live policy
	// LoadPolicy answered, the one a reread refreshes a single name in: boot hands it over
	// rather than leaving a caller to carry it out of its own LoadPolicy, so nothing here
	// depends on the order boot calls these in. onConnected is what it calls once each LISTEN
	// statement has returned; the first of those calls is what the wait below is for. In
	// production: go (&reread.Listener{...}).Run(serving).
	StartListener func(ctx context.Context, current *policy.Current, onConnected func())
	// Listen binds the address callers reach this broker on. In production:
	// net.Listen("tcp", cfg.ListenAddr).
	Listen func() (net.Listener, error)
	// ListenAirlock binds the airlock's proxy port, and is nil for a deployment that configures
	// no rules file and so has no airlock. In production: net.Listen("tcp",
	// cfg.AirlockListenAddr).
	ListenAirlock func() (net.Listener, error)
	// ListenWait bounds how long the bind waits for that first LISTEN. In production:
	// rereadListenWait, whose own comment says what the bound is for and what reaching it costs.
	ListenWait time.Duration
	// Catchup takes the catch-up load, the one more whole-namespace load the boot takes once the
	// LISTEN is in place, into current, the same policy StartListener is given. In production:
	// current.Refresh.
	Catchup func(ctx context.Context, current *policy.Current) error
	// Log is where every line this sequence logs goes. In production: slog.Default().
	Log *slog.Logger
}

// booted is what a finished boot hands back: a bound socket, the live policy behind it, and the
// context the work that outlives the drain runs on, with the one call that ends it. A boot that
// was signalled before it had bound anything answers the zero booted and a nil error, so its
// caller reads a nil Serving as "there is nothing to serve, and nothing to stop".
type booted struct {
	// Serving is the context the work that has to outlive the drain runs on: the five-minute
	// policy reload and the reread listener under it. It is nil for a boot that returned early.
	Serving context.Context
	// EndServing ends that work, and is the stop sequence's to call once srv.Shutdown has
	// returned. boot calls it itself on every path that answers no Serving.
	EndServing context.CancelFunc
	// Policy is the live policy the first load built and the catch-up reloaded.
	Policy *policy.Current
	// Listener is the bound socket, which nothing is serving yet.
	Listener net.Listener
	// AirlockListener is the airlock proxy port's bound socket, nil when there is no airlock.
	// Nothing is serving it yet either.
	AirlockListener net.Listener
}

// boot takes the broker from its first policy load to a bound socket, in the one order that
// holds: the policy before the listener that rereads into it, that listener's first LISTEN before
// the catch-up load which pairs with it, and both before the bind, so that an address the load
// balancer can reach is an address a retag has already reached.
//
// It owns the serving context. It makes it here, cancels it itself on every error and on both
// early returns, and hands it to its caller otherwise, where the stop sequence ends it once
// srv.Shutdown has returned. ctx is the signal's own context, read only for whether a signal has
// arrived: nothing this sequence runs is cancelled by it.
func boot(ctx context.Context, b brokerBoot) (booted, error) {
	// The context the work that has to outlive the drain runs on: the five-minute policy reload
	// below and the reread listener under it. ctx is signal.NotifyContext's, which the first
	// SIGTERM cancels, and a pod goes on being sent new requests for the whole of its drain —
	// that is what the drain is for — so for that window it is still one of the pods a reread has
	// to reach, and a retag that narrows who may have a secret has to be in force there too.
	// context.WithoutCancel keeps every value ctx carries and drops only its cancellation;
	// EndServing, called once srv.Shutdown has returned - having answered the last request, or
	// having spent its 60-second allowance - is what ends these loops instead. Everything that
	// legitimately stops at the signal — the request sweeper main starts — stays on ctx.
	serving, endServing := context.WithCancel(context.WithoutCancel(ctx))

	current, err := b.LoadPolicy(serving)
	if err != nil {
		endServing()
		return booted{}, err
	}
	// A signal that arrived while that first load ran leaves nothing to drain. The load runs on
	// serving and so does not end early, which is deliberate, but a process signalled before it
	// has bound anything has no caller to answer and no in-flight request to finish: returning
	// here is the whole of such a process's shutdown. Binding a socket and then running a drain
	// and a shutdown budget for a server that never served would hold the pod open for the whole
	// drain and serve nobody - the shutdown budget itself is a maximum rather than a wait, since
	// srv.Shutdown with nothing in flight returns at once, so what is saved here is the drain,
	// and what is avoided is a pod in an EndpointSlice that answers no caller.
	if ctx.Err() != nil {
		b.Log.Info("broker: signalled while starting up, before it had bound anything", "stage", "the first policy load")
		endServing()
		return booted{}, nil
	}

	// firstListen carries the reread listener's first successful LISTEN to the bind below, which
	// waits for it: a pod the load balancer can reach is a pod a retag has to reach, so a process
	// that bound before its own LISTEN was in place would spend those milliseconds serving
	// traffic while a peer's reread of a secret it holds goes to a channel nobody on this pod is
	// on yet. That wait orders the bind against the LISTEN and nothing else. It does nothing for
	// a notification committed before the LISTEN, while the first policy load above was reading
	// the namespace - the load was taken before the subscription existed - and the catch-up load
	// after the wait is what covers that one. OnConnected is what makes the wait mean "listening"
	// rather than nearly that: pglisten runs it after the LISTEN statement has succeeded, where a
	// hook on the dial would fire with the LISTEN still in flight. The token is sent without
	// blocking and the channel is never drained again, so every redial behind the boot costs the
	// listener nothing.
	firstListen := make(chan struct{}, 1)
	b.StartListener(serving, current, func() {
		select {
		case firstListen <- struct{}{}:
		default:
		}
	})
	// The wait that pairs with it, and the whole of what it promises: either this pod is listening
	// on the channel before the bind takes the socket callers reach it on, or ListenWait has run
	// out. A pod whose dial or whose LISTEN failed waits the bound out and then serves anyway,
	// the listener redialing behind it, so nothing here can refuse a boot; the bound is what
	// keeps a Postgres that takes the connection and never answers from holding one.
	select {
	case <-firstListen:
		// The catch-up: one more whole-namespace load into the live policy, taken now that the
		// LISTEN is in place, so that the subscription exists before the read whose result it is
		// meant to invalidate. Postgres delivers nothing to a session for a NOTIFY committed before
		// that session's LISTEN, and the listener's onConnected deliberately catches nothing up, so
		// without this load the following is lost on this pod until its five-minute reload, with
		// nothing logged: B starts and begins its first load; A serves a reread of a narrowing
		// retag and commits its notification, which no session of B's is listening for; B's
		// LISTEN succeeds. With this load, B then reads the namespace again here and binds with
		// the retag in force. A notification committed after the LISTEN is delivered instead; the
		// RefreshOne it drives and this Refresh take the policy's one writer lock, and Refresh
		// re-describes every name a reread changed within the store's ListLag (Current's recent
		// anchors), so a reread that lands first is not undone by this load's listing. What this
		// load reads is the store's listing, as every whole-namespace load does, so a store whose
		// listing lags a write by up to its ListLag (Store.ListLag) can still show it the tags
		// from before the retag. That exposure is the store's rather than this ordering's - the
		// alternative shapes of this fix all share it, since a retag committed before any
		// subscription is only ever visible through a listing - and it is tracked, with the two
		// ways to close it and their costs, at dispatch://INFRA-375.
		//
		// It costs one extra namespace read per boot, and nothing after: the whole listing - on
		// Secrets Manager its ListSecrets pages and at most one KMS alias lookup, on Secret
		// Manager those pages and one GetSecretVersion per secret.
		//
		// A failed load does not refuse the boot. The first load succeeded, so the policy is
		// live and this is a catch-up; the line says what failing it loses, and it is named
		// apart from the "broker: <operation> failed" lines, which stand for a request answered
		// 500 or 503. It runs on serving, as the first load does and for the same reasons, so
		// what the first load's comment writes down about one signal holds here too: a store
		// that never answers holds the boot until a second signal or the kubelet's SIGKILL.
		if err := b.Catchup(serving, current); err != nil {
			b.Log.Warn("broker: serving without the boot's catch-up policy load, so a retag another pod served while this one was starting may not be in force here until its five-minute reload", "error", err)
		}
		// The same check as the one after the first load, for the same reason: a process
		// signalled before it has bound anything has nothing to drain.
		if ctx.Err() != nil {
			b.Log.Info("broker: signalled while starting up, before it had bound anything", "stage", "the catch-up policy load")
			endServing()
			return booted{}, nil
		}
	case <-time.After(b.ListenWait):
		// No catch-up here. With no LISTEN in place there is no subscription to order a read
		// after, so a second namespace read would not close the window, only move where it
		// starts: a notification sent before the listener connects is lost on this pod whatever
		// is read now, as this warning says, and its five-minute reload is what catches it up.
		b.Log.Warn("broker: binding before the reread listener has had an answer", "waited", b.ListenWait)
	}

	listener, err := b.Listen()
	if err != nil {
		endServing()
		return booted{}, err
	}
	// The proxy port, bound here rather than beside its own server below, and inside the boot
	// rather than after it: a pod that cannot bind it - an address another process holds, a
	// port a misconfiguration repeats - fails its boot instead of serving an API whose releases
	// hand out placeholders for a proxy nobody is listening on. The API's socket is closed
	// again on that path, since a refused boot must leave no address held.
	var airlockListener net.Listener
	if b.ListenAirlock != nil {
		airlockListener, err = b.ListenAirlock()
		if err != nil {
			listener.Close()
			endServing()
			return booted{}, err
		}
	}
	return booted{Serving: serving, EndServing: endServing, Policy: current, Listener: listener, AirlockListener: airlockListener}, nil
}

// listenTCP is brokerBoot.Listen in production: the bind on addr, which boot runs synchronously,
// right after every guard that can still refuse to boot has already run (config, the port-0
// public-URL guard, migrations, the first policy load) — the only way any caller, dev-broker.sh
// included, can learn which process holds an address is the line main logs once this exact Listen
// call has already succeeded. With a shared fixed
// dev port, a losing instance's own readiness curl could see a different, already-running
// instance's healthz answer and report "ready" pointing at the wrong broker; splitting Listen
// from Serve and logging only after a real bind closes that regardless of how the address is
// chosen, and dev-broker.sh's BROKER_LISTEN_ADDR=127.0.0.1:0 makes a same-port collision between
// two dev instances impossible in the first place — the kernel never hands out an address a live
// listener already holds.
func listenTCP(addr string) func() (net.Listener, error) {
	return func() (net.Listener, error) { return net.Listen("tcp", addr) }
}

// brokerStop is what the stop sequence does its work through, the same shape brokerBoot has and
// for the same reason: a test drives the order these run in with no server to shut down and no
// store to close.
type brokerStop struct {
	// Drain is how long the broker goes on answering requests after the signal, failing
	// readiness throughout. In production: BROKER_DRAIN_SECONDS.
	Drain time.Duration
	// DrainNow ends the drain at once; the watchdog hands a second signal over on it.
	DrainNow <-chan struct{}
	// Draining fails GET /healthz. In production: the atomic flag api.Deps.Draining reads.
	Draining func(bool)
	// Shutdown finishes the in-flight requests. In production: srv.Shutdown.
	Shutdown func(ctx context.Context) error
	// ShutdownAirlock drains the airlock's proxy port, and is nil for a deployment with no
	// airlock. In production: airlockproxy.Server.Shutdown.
	ShutdownAirlock func(ctx context.Context) error
	// EndServing ends the work that outlived the drain. In production: booted.EndServing.
	EndServing func()
	// CloseStore releases the database pools. In production: store.Store.Close.
	CloseStore func()
	// Log is where every line this sequence logs goes. In production: slog.Default().
	Log *slog.Logger
}

// stop takes the broker from the signal it has already read to a process with nothing of its own
// left running: the drain, the shutdown, and then - in that order, which is a dependency - the
// end of the work that outlived the drain and the close of the store behind it.
//
// ctx is what the shutdown's own budget hangs off, and it is context.Background() in production:
// nothing in a stop may be cancelled by the signal that began it, least of all the shutdown that
// is finishing the last requests.
func stop(ctx context.Context, s brokerStop) {
	// The drain comes first and the listener stays open through it, rather than the reverse: a pod
	// leaves its Service's EndpointSlice concurrently with the kubelet beginning termination, not
	// before it, so for a while after SIGTERM the kubelet is still routing new connections here.
	// Failing readiness now — GET /healthz answers 503 SHUTTING_DOWN from this point, while every
	// other route is served exactly as before — and closing the listener only once the drain has
	// run out is what keeps those connections from being refused by a socket that is already gone.
	s.Log.Info("broker draining", "drain", wholeSeconds(s.Drain))
	s.Draining(true)
	if s.Drain > 0 {
		// A second signal ends the drain at once: an operator, or a kubelet that has decided this
		// pod has to be gone now, must not have to wait out a drain whose whole purpose has
		// already been served. ctx is no help here — signal.NotifyContext cancelled main's with
		// the first signal — so the second one arrives on DrainNow, handed over by the watchdog at
		// the top of main once it has found serveStarted closed. A zero drain takes neither this
		// select nor a timer, which would be a wait on something that never fires.
		timer := time.NewTimer(s.Drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.DrainNow:
			s.Log.Info("broker: a second signal ended the drain early")
		}
	}
	s.Log.Info("broker shutting down")
	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	// Both ports drain out of the one budget, and beside each other rather than one after the
	// other: a command streaming an answer through the proxy and a caller waiting on the API
	// are each owed the whole of the shutdown's allowance, and a sequence would hand the second
	// of them whatever the first left. The proxy's own drain closes every tunnel it still
	// holds, which http.Server.Shutdown does not.
	airlockDrained := make(chan error, 1)
	if s.ShutdownAirlock != nil {
		go func() { airlockDrained <- s.ShutdownAirlock(shutdownCtx) }()
	} else {
		airlockDrained <- nil
	}
	if err := s.Shutdown(shutdownCtx); err != nil {
		s.Log.Warn("broker: shutdown", "error", err)
	}
	if err := <-airlockDrained; err != nil {
		s.Log.Warn("broker: airlock proxy shutdown", "error", err)
	}
	// Shutdown has returned, either having answered the last request or having spent the 60
	// seconds above, so the work kept alive through the drain has nothing left to serve: this is
	// where the reread listener and the five-minute policy reload stop.
	//
	// The order is a dependency, and it is not the one the pool makes: st.Listener hands pglisten
	// a connection hijacked out of the pool (store.go, conn.Hijack), so closing the store never
	// touches it and would not end a LISTEN. What ends it is pglisten's own deferred
	// close of that connection when its loop returns, which EndServing asks for by cancelling
	// serving. EndServing only asks: nothing here waits for that goroutine, and the process
	// exits with the cancellation delivered and the close in flight, which costs nothing because
	// Postgres ends a session whose connection goes with the process. The order still matters for
	// the reload and the pools: EndServing stops the loop that would otherwise read through a
	// pool CloseStore is about to shut, and CloseStore releases the two pools behind it, each of
	// whose health checks is a goroutine of its own.
	s.EndServing()
	s.CloseStore()
}
