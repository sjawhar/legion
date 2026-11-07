// packages/envoy/cmd/broker/ordering_test.go
//
// The broker's boot and its stop are two orders, and each step of each one is where it is
// because the step before it has to have happened first. boot.go holds both as functions whose
// collaborators are fields (brokerBoot, brokerStop), so every row here drives the real sequence
// against collaborators of its own and reads back the order the sequence ran them in. The three
// stop rows need nothing real at all; the two boot rows each bind 127.0.0.1:0, since the bind is
// one of the steps they order, and the first of them needs a real Postgres besides, because the
// race it pins is one Postgres itself decides. Each row's comment names the one edit to boot.go
// that turns that row red, which is how these five are measured.
//
// No row waits on the clock to decide an order: the sequences call each collaborator on one
// goroutine, so a step recorded as it runs is the order itself, and the one row that does read a
// duration to decide anything (the drain) reads it one way only - a timer never fires early,
// however loaded the machine is. The two durations beside it are hang guards, read only where a
// wait on an event would otherwise hang: the bound the bind row hands boot for a wait that is
// already over when it begins (bootListenWait), and the one the catch-up row hands it for a wait
// that is real but reachable by nothing a Postgres that answers at all does (listenHangGuard).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/reread"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// The steps the rows below record, written as an operator reads them in a failure message.
const (
	theFirstLoad               = "the first policy load"
	theCatchUpReturned         = "the catch-up policy load returned"
	theBind                    = "the bind"
	theAirlockBind             = "the airlock proxy port's bind"
	theShutdownBegan           = "the shutdown began"
	theShutdownReturned        = "the shutdown returned"
	theAirlockShutdownReturned = "the airlock proxy's shutdown returned"
	theServingEnded            = "the serving context ended"
	theStoreClosed             = "the store closed"
)

// steps is the order one row's collaborators ran in. It is a record rather than a set of
// timestamps: boot and stop call each collaborator on the one goroutine, so what ran first ran
// first, and nothing here has to hold a tolerance a loaded machine could break. The mutex is for
// the rows whose steps run on a goroutine of their own.
type steps struct {
	mu   sync.Mutex
	seen []string
}

// record notes that step has just run.
func (s *steps) record(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, step)
}

// order answers what has run so far, in the order it ran.
func (s *steps) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// is reports whether the steps recorded are exactly want.
func (s *steps) is(want ...string) bool { return slices.Equal(s.order(), want) }

// has reports whether step has run, which a fake reads to fail the moment a step it depends on
// has already happened - a dependency the recorded order alone cannot show, since a sequence
// that ran that step on a goroutine of its own could still record the expected order.
func (s *steps) has(step string) bool { return slices.Contains(s.order(), step) }

// String renders the order for a failure message.
func (s *steps) String() string { return strings.Join(s.order(), " -> ") }

// orderingLog is the logger these rows give boot and stop. Both sequences log what they do; what
// they log is pinned elsewhere, and a row here reads the order rather than the lines.
func orderingLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// bootListenWait is what the bind row hands boot for the bound on its wait for the first LISTEN.
// That row lets the bound decide nothing, because it has the LISTEN in hand before the wait
// begins: its StartListener answers onConnected in the call itself, so what this names there is a
// hang guard on a wait that is already over. The catch-up row's wait is a real one, against a
// real dial and a real LISTEN on a real Postgres, and it hands boot listenHangGuard instead: it
// was these thirty seconds until dispatch://INFRA-1248, and a merge-queue runner whose listener
// was redialing on pglisten's backoff reached them, which failed that row for its own setup
// rather than for the catch-up it is about.
const bootListenWait = 30 * time.Second

// listenHangGuard is what the catch-up row hands boot for that same bound. There the wait is the
// whole of what the row reads back, so the bound has to be out of reach of an ordinary slow dial
// and reachable by a hang alone: that LISTEN is one dial and one statement on a local Postgres,
// milliseconds of work, and a listener that has to redial climbs pglisten's backoff - a second,
// doubling to thirty - so five minutes is several of those ladders and nothing a Postgres that
// answers at all approaches. A row that did reach it would have boot bind on the arm that skips
// the catch-up, and would fail on the read of that LISTEN below, naming the LISTEN that never
// came, rather than passing for the wrong reason or hanging until go test's own deadline.
const listenHangGuard = 5 * time.Minute

// stopDrain is the drain the stop rows run. It is long enough to be told apart from no drain at
// all and short enough to cost each row a twentieth of a second.
const stopDrain = 50 * time.Millisecond

// retaggedWhileBooting is the secret the catch-up row narrows while the booting process is held
// inside its first policy load. The name is this row's alone, so no other test's reread on the
// shared test server can name it.
const retaggedWhileBooting = "RETAGGED_WHILE_BOOTING"

// narrowedOwner is who the secret belongs to after the retag: one person, at human tier, in
// place of shared at agent tier. That is the narrowing direction - the one where a pod serving
// the tags from before the retag hands out a secret the retag has taken away.
const narrowedOwner = "ada@example.com"

// TestBootLoadsThePolicyAgainAfterTheFirstListen is the catch-up, and the race it exists for.
//
// A booting pod reads the whole namespace once, and only then does its reread listener LISTEN.
// Postgres delivers nothing to a session for a notification committed before that session's
// LISTEN, so a retag another pod served in that window reaches this pod through neither the
// first load (taken before the retag) nor the fan-out (committed before the LISTEN). The
// catch-up - one more whole-namespace load, taken once the LISTEN is in place - is the only
// thing that closes it, and this row holds the process exactly there: the first load has read
// the secret store and is held; the retag and a real NOTIFY on a real Postgres are both
// committed; then the load is released and the LISTEN happens. What the booted process serves
// must be the narrowed tags.
//
// Nothing in the row orders that catch-up: its StartListener returns at once, as brokerBoot's
// contract says and as production does, and boot's own wait on the first LISTEN is the only
// thing holding the catch-up and the bind behind that LISTEN. The row reads the order back once
// boot has returned.
//
// The named revert: remove the Catchup call from boot.
func TestBootLoadsThePolicyAgainAfterTheFirstListen(t *testing.T) {
	st := storetest.Open(t)
	secretStore := secrets.NewLocal(policytest.Secret(retaggedWhileBooting, policy.OwnerShared, policy.TierAgent, "v1"))
	loader := policytest.Loader(secretStore)

	readTheStore := make(chan struct{})    // the first load has read the secret store
	releaseTheLoad := make(chan struct{})  // the row has committed the retag and the notification
	listening := make(chan struct{})       // the listener's first LISTEN has succeeded
	listenerStopped := make(chan struct{}) // its Run has returned
	closeListening := sync.OnceFunc(func() { close(listening) })

	// What the boot answers, carried back off its own goroutine.
	type finished struct {
		started booted
		err     error
	}
	done := make(chan finished, 1)
	go func() {
		started, err := boot(t.Context(), brokerBoot{
			// The first load, held open once it has read the store. Holding it here rather than
			// inside the store's own calls is what the race needs: the whole namespace has been
			// read, at the tags from before the retag, and nothing of this process is listening
			// yet.
			LoadPolicy: func(ctx context.Context) (*policy.Current, error) {
				cur, err := policy.NewCurrent(ctx, loader, time.Hour)
				close(readTheStore)
				<-releaseTheLoad
				return cur, err
			},
			// The real receiving half of the fan-out on a real database, wired as main wires it,
			// so the LISTEN is a LISTEN and onConnected runs only after that statement has
			// succeeded. Its sender id differs from the one the row notifies under, so nothing
			// here is dropped as this pod's own reread: if Postgres did deliver the
			// notification, this listener would reread the secret and the row would pass with
			// the catch-up gone, which is why the revert run is measured too.
			//
			// It starts that listener and returns at once, which is what brokerBoot's contract
			// asks of it and what production does, so boot's own wait on the first LISTEN is the
			// only thing ordering the catch-up and the bind behind that LISTEN - the order this
			// row exists to read back. A fake that held its return until the LISTEN had happened
			// would do that ordering itself, and the read after the boot could then never fail,
			// however boot was written.
			//
			// OnConnected closes listening before it hands boot its token, so by the time boot's
			// wait returns through firstListen this row's own channel is already closed and the
			// read below is exact rather than a race with boot's next statement. Closing it is
			// once-only because every redial behind the boot runs OnConnected again.
			StartListener: func(ctx context.Context, current *policy.Current, onConnected func()) {
				go func() {
					defer close(listenerStopped)
					(&reread.Listener{
						Sender: "this-pod",
						Dial:   st.Listener,
						Refresh: func(ctx context.Context, name string) error {
							_, err := current.RefreshOne(ctx, name)
							return err
						},
						OnConnected: func() {
							closeListening()
							onConnected()
						},
					}).Run(ctx)
				}()
			},
			Listen: func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
			// A hang guard rather than a deadline this row spends: an ordinary slow dial stays
			// well inside it and leaves boot's wait to do the ordering, and only a Postgres that
			// never answers reaches it.
			ListenWait: listenHangGuard,
			Catchup:    func(ctx context.Context, current *policy.Current) error { return current.Refresh(ctx) },
			Log:        orderingLog(),
		})
		done <- finished{started, err}
	}()

	<-readTheStore
	// Both of these commit while the first load is held, which puts both of them before this
	// process's LISTEN: the retag in the secret store, and the notification the pod that served
	// that retag sends. This is the state a second pod's boot really meets during a rolling
	// deploy.
	secretStore.Put(policytest.Secret(retaggedWhileBooting, narrowedOwner, policy.TierHuman, "v1"))
	if err := reread.Notifier(st.Pool, "the-pod-that-served-the-retag")(t.Context(), retaggedWhileBooting); err != nil {
		t.Fatalf("commit the other pod's reread notification: %v", err)
	}
	close(releaseTheLoad)

	answer := <-done
	started, err := answer.started, answer.err
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if started.Serving == nil {
		t.Fatal("boot answered no serving context: it returned early, so nothing below is about the catch-up")
	}
	t.Cleanup(func() {
		started.EndServing()
		<-listenerStopped
	})
	defer started.Listener.Close()

	// The order boot ran in, read now that boot has returned. This channel is closed inside that
	// LISTEN's own OnConnected, ahead of the token boot's wait takes, so a boot that waited for
	// the LISTEN before its catch-up and its bind has left it closed here. Finding it open means
	// the boot bound with no LISTEN in place - nothing started that listener, or it never
	// connected within the bound the row hands boot - which is the arm that skips the catch-up by
	// design, and nothing below it would be about the catch-up.
	select {
	case <-listening:
	default:
		t.Fatalf("the boot bound its address without its reread listener's first LISTEN: either nothing started that listener, or it did not connect within the %v boot waits for it, so the boot took the arm that skips the catch-up by design and this row proves nothing about the catch-up", listenHangGuard)
	}
	served, ok := started.Policy.Get().Secrets[retaggedWhileBooting]
	if !ok {
		t.Fatalf("the booted broker serves no %s at all; want it served as owner=%q tier=%q", retaggedWhileBooting, narrowedOwner, policy.TierHuman)
	}
	if served.Owner != narrowedOwner || served.Tier != policy.TierHuman {
		t.Fatalf("the booted broker serves %s as owner=%q tier=%q, the tags from before the retag; want owner=%q tier=%q. A retag committed while this process took its first policy load reaches it through nothing else: the load had already read the store, and Postgres delivers no notification committed before this process's LISTEN. The catch-up load after that LISTEN is what reads it, and without it this pod hands out a secret the retag took away until its five-minute reload",
			retaggedWhileBooting, served.Owner, served.Tier, narrowedOwner, policy.TierHuman)
	}
}

// TestBootBindsOnlyAfterTheCatchUp is the other half of that ordering: an address the load
// balancer can reach is an address a retag has already reached. The bind is the moment this pod
// becomes one of the pods a caller is sent to, so it comes after the catch-up load has returned,
// never beside it and never before it.
//
// The named revert: move the bind above the catch-up (Listen called before Catchup returns, the
// catch-up kept).
func TestBootBindsOnlyAfterTheCatchUp(t *testing.T) {
	var ran steps
	started, err := boot(t.Context(), brokerBoot{
		LoadPolicy: func(context.Context) (*policy.Current, error) {
			ran.record(theFirstLoad)
			return nil, nil
		},
		// The first LISTEN, answered at once: this row is about what the boot does after it, not
		// about the wait itself.
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen: func() (net.Listener, error) {
			ran.record(theBind)
			return net.Listen("tcp", "127.0.0.1:0")
		},
		ListenWait: bootListenWait,
		Catchup: func(context.Context, *policy.Current) error {
			ran.record(theCatchUpReturned)
			return nil
		},
		Log: orderingLog(),
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if started.Serving == nil {
		t.Fatal("boot answered no serving context: it returned early, and nothing below is about the order of a finished boot")
	}
	t.Cleanup(started.EndServing)
	defer started.Listener.Close()

	if !ran.is(theFirstLoad, theCatchUpReturned, theBind) {
		t.Fatalf("the boot ran %s; want %s. A pod that binds before its catch-up load has returned is a pod the load balancer sends callers to while it still serves the tags it read before the catch-up, which is the whole window the catch-up exists to close",
			&ran, strings.Join([]string{theFirstLoad, theCatchUpReturned, theBind}, " -> "))
	}
}

// TestStopDrainsBeforeShutdown pins the first of the stop's three orderings. A pod leaves its
// Service's EndpointSlice at about the moment the kubelet starts terminating it, not before, so
// for a while after SIGTERM new connections still arrive here. The drain is that window: the
// broker fails readiness and goes on answering. A shutdown taken at the signal would cut those
// callers off.
//
// The named revert: call Shutdown at the first signal, before the drain.
func TestStopDrainsBeforeShutdown(t *testing.T) {
	var shutdownRan bool
	var shutdownAfter time.Duration
	began := time.Now()
	stop(t.Context(), brokerStop{
		Drain:    stopDrain,
		DrainNow: make(chan struct{}), // no second signal: this row is about the whole drain
		Draining: func(bool) {},
		Shutdown: func(context.Context) error {
			shutdownRan, shutdownAfter = true, time.Since(began)
			return nil
		},
		EndServing: func() {},
		CloseStore: func() {},
		Log:        orderingLog(),
	})
	if !shutdownRan {
		t.Fatal("the stop never shut the server down: the requests in flight at the signal were neither answered nor refused")
	}
	// One-sided on purpose: a Go timer fires at or after its deadline and never before it, so a
	// slow machine can only make this longer. A shutdown that reads as earlier than the drain
	// can only be a shutdown that did not wait for it.
	if shutdownAfter < stopDrain {
		t.Fatalf("the broker shut down %v after the signal, before its %v drain had run out; want the shutdown only once the drain has. A pod that shuts down at the signal cuts the callers the kubelet is still routing to it, which is exactly what the drain is for",
			shutdownAfter, stopDrain)
	}
}

// TestStopEndsServingOnlyAfterShutdownReturns pins the second. The reread listener and the
// five-minute policy reload run on the serving context, and they have to outlive the drain: a
// draining pod is still being sent requests, so it is still a pod a peer's retag has to reach.
// Shutdown returning - the last request answered, or the shutdown's own budget spent - is what
// says there is nothing left to serve.
//
// The named revert: call EndServing at the first signal.
func TestStopEndsServingOnlyAfterShutdownReturns(t *testing.T) {
	var ran steps
	stop(t.Context(), brokerStop{
		Drain:    stopDrain,
		DrainNow: make(chan struct{}),
		Draining: func(bool) {},
		// Both ends of the shutdown are recorded, so the order says "after it returned" and not
		// merely "after it was called". The fake also asks whether the serving context has
		// already ended, as TestStopClosesTheStoreOnlyAfterServingEnds asks inside its
		// CloseStore: a stop that ended it on a goroutine of its own, before or during the
		// shutdown, would leave the three records below in the order this row wants.
		Shutdown: func(context.Context) error {
			ran.record(theShutdownBegan)
			if ran.has(theServingEnded) {
				t.Error("the stop ended the serving context before its shutdown returned: the reread listener and the five-minute policy reload stop under requests the broker is still answering, so the last callers of a draining pod are served tags a retag has already changed")
			}
			ran.record(theShutdownReturned)
			return nil
		},
		EndServing: func() { ran.record(theServingEnded) },
		CloseStore: func() {},
		Log:        orderingLog(),
	})
	if !ran.is(theShutdownBegan, theShutdownReturned, theServingEnded) {
		t.Fatalf("the stop ran %s; want %s. Ending the serving context before the shutdown returns stops the reread listener and the policy reload under requests the broker is still answering, so the last callers of a draining pod are served tags a retag has already changed",
			&ran, strings.Join([]string{theShutdownBegan, theShutdownReturned, theServingEnded}, " -> "))
	}
}

// TestStopClosesTheStoreOnlyAfterServingEnds pins the third. Closing the store releases the two
// pools, and the work on the serving context reads through them: the five-minute reload and the
// reread listener's own rereads. Ending that work first is what makes the close safe, so this
// row's store refuses to be closed while the serving context is still live.
//
// The named revert: call CloseStore before EndServing.
func TestStopClosesTheStoreOnlyAfterServingEnds(t *testing.T) {
	serving, endServing := context.WithCancel(context.Background())
	defer endServing()
	var ran steps
	stop(t.Context(), brokerStop{
		Drain:    stopDrain,
		DrainNow: make(chan struct{}),
		Draining: func(bool) {},
		Shutdown: func(context.Context) error { return nil },
		EndServing: func() {
			ran.record(theServingEnded)
			endServing()
		},
		CloseStore: func() {
			ran.record(theStoreClosed)
			if serving.Err() == nil {
				t.Error("the stop closed the store while the serving context was still live: the five-minute policy reload and the reread listener's rereads read through the pools this close is releasing, so each of them is left reading a pool that is shutting under it")
			}
		},
		Log: orderingLog(),
	})
	if !ran.is(theServingEnded, theStoreClosed) {
		t.Fatalf("the stop ran %s; want %s", &ran, strings.Join([]string{theServingEnded, theStoreClosed}, " -> "))
	}
}

// TestBootBindsTheAirlockPortAfterTheAPIAndInsideTheBoot pins where the second socket is taken.
// A deployment with an airlock releases placeholders and a proxy URL in place of values, so a
// pod that served its API while its proxy port was unbound would hand out credentials nothing
// can be done with: the command's HTTPS_PROXY would refuse every connection, and the session
// would read it as the service being down. Binding it inside the boot, after the API's bind and
// before anything serves either, makes a proxy port that cannot be taken a boot that fails.
//
// The named revert: bind the airlock in main, after boot has returned.
func TestBootBindsTheAirlockPortAfterTheAPIAndInsideTheBoot(t *testing.T) {
	var ran steps
	started, err := boot(t.Context(), brokerBoot{
		LoadPolicy: func(context.Context) (*policy.Current, error) {
			ran.record(theFirstLoad)
			return nil, nil
		},
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen: func() (net.Listener, error) {
			ran.record(theBind)
			return net.Listen("tcp", "127.0.0.1:0")
		},
		ListenAirlock: func() (net.Listener, error) {
			ran.record(theAirlockBind)
			return net.Listen("tcp", "127.0.0.1:0")
		},
		ListenWait: bootListenWait,
		Catchup: func(context.Context, *policy.Current) error {
			ran.record(theCatchUpReturned)
			return nil
		},
		Log: orderingLog(),
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if started.Serving == nil {
		t.Fatal("boot answered no serving context: it returned early, and nothing below is about the order of a finished boot")
	}
	t.Cleanup(started.EndServing)
	defer started.Listener.Close()
	if started.AirlockListener == nil {
		t.Fatal("the boot answered no airlock listener, so there is no proxy port for main to serve")
	}
	defer started.AirlockListener.Close()

	want := []string{theFirstLoad, theCatchUpReturned, theBind, theAirlockBind}
	if !ran.is(want...) {
		t.Fatalf("the boot ran %s; want %s", &ran, strings.Join(want, " -> "))
	}
}

// TestBootRefusesWhenTheAirlockPortCannotBeBound is the other half of that: a proxy port the pod
// cannot take is a boot that fails, not an API that serves without it. The API's own socket goes
// with it, since a refused boot must leave no address held - another process is about to be
// started in this one's place and would find the address taken.
//
// The named revert: log the airlock bind's error and return the booted API listener anyway.
func TestBootRefusesWhenTheAirlockPortCannotBeBound(t *testing.T) {
	var apiListener net.Listener
	refusal := errors.New("the proxy port is already held")
	started, err := boot(t.Context(), brokerBoot{
		LoadPolicy:    func(context.Context) (*policy.Current, error) { return nil, nil },
		StartListener: func(_ context.Context, _ *policy.Current, onConnected func()) { onConnected() },
		Listen: func() (net.Listener, error) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			apiListener = listener
			return listener, err
		},
		ListenAirlock: func() (net.Listener, error) { return nil, refusal },
		ListenWait:    bootListenWait,
		Catchup:       func(context.Context, *policy.Current) error { return nil },
		Log:           orderingLog(),
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("boot with an unbindable proxy port = %v; want the bind's own refusal", err)
	}
	if started.Serving != nil || started.Listener != nil || started.AirlockListener != nil {
		t.Fatalf("the refused boot answered %+v; want nothing to serve and nothing to stop", started)
	}
	if apiListener == nil {
		t.Fatal("the boot never reached the API's bind, so this row says nothing about what it left behind")
	}
	if err := apiListener.Close(); err == nil {
		t.Error("the refused boot left the API's socket open: the pod that replaces this one finds the address held")
	}
}

// TestStopDrainsTheAirlockBesideTheAPI pins the two ports' drain. Both are this one process's,
// both have callers in flight at the signal - a command streaming an answer through the proxy is
// one - and both have to be finished before the work that outlived the drain stops, since the
// proxy reads the same policy and the same pools the API does. They drain beside each other out
// of the one shutdown budget rather than one after the other, so neither is handed only what the
// other left.
//
// The named revert: drop ShutdownAirlock from stop, or call it after EndServing.
func TestStopDrainsTheAirlockBesideTheAPI(t *testing.T) {
	var ran steps
	stop(t.Context(), brokerStop{
		Drain:    stopDrain,
		DrainNow: make(chan struct{}),
		Draining: func(bool) {},
		Shutdown: func(context.Context) error {
			ran.record(theShutdownBegan)
			ran.record(theShutdownReturned)
			return nil
		},
		ShutdownAirlock: func(context.Context) error {
			if ran.has(theServingEnded) {
				t.Error("the stop ended the serving context before the airlock had drained: a command still being answered through the proxy is answered out of the pools that close next")
			}
			ran.record(theAirlockShutdownReturned)
			return nil
		},
		EndServing: func() { ran.record(theServingEnded) },
		CloseStore: func() {},
		Log:        orderingLog(),
	})
	if !ran.has(theAirlockShutdownReturned) {
		t.Fatalf("the stop ran %s; it never drained the airlock's proxy port, so every tunnel it held is cut by the kubelet rather than closed", &ran)
	}
	order := ran.order()
	if len(order) == 0 || order[len(order)-1] != theServingEnded {
		t.Fatalf("the stop ran %s; want both shutdowns before %s", &ran, theServingEnded)
	}
}
