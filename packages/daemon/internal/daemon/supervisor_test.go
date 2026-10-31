package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fixedClaimStore answers every boot token hash with the one claim it is given, or as unknown
// when found is false: a fake for tests that control exactly what ClaimByBootTokenHash, the
// durable Postgres fact helloResolver checks before ever waiting on restoration, returns.
type fixedClaimStore struct {
	claim supervise.Claim
	found bool
}

func (s fixedClaimStore) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return s.claim, s.found, nil
}

// An unknown boot token — no claim this daemon's store holds minted it — is rejected at once,
// bounded by its own resolve timeout, never held waiting for restoration: the worker-stream port
// is plaintext and reachable from any pod, so holding an unresolved connection with no deadline
// would let a garbage token pin a goroutine and a file descriptor for the whole boot wait, with no
// cap on how many a client could open.
func TestHelloResolverRejectsAnUnknownTokenAtOnceDuringTheBootWait(t *testing.T) {
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	resolve := sup.helloResolver(api.NewBootTokens(fixedClaimStore{found: false}), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("garbage-token")
		close(done)
	}()

	select {
	case <-done:
		// Rejected without ever waiting on restoration, which never closes in this test.
	case <-time.After(time.Second):
		t.Fatal("an unknown token was held rather than rejected at once: a garbage token must not pin a goroutine and a file descriptor for the whole boot wait")
	}
}

// A boot token a real claim minted — ClaimByBootTokenHash finds it — is held until restoration,
// not rejected: a live pane reconnecting while the boot's readiness gate still waits out an
// unreachable NATS or Dispatch (daemon.go's run, LEGION-580) waits on s.restored alone, with no
// deadline of its own, and is judged only once restoration closes it (supervision.start, once every
// stored claim has its machine, the boot orphan reconciliation has run and the terminal replay has
// applied what no daemon applied).
func TestHelloResolverHoldsAKnownTokenUntilRestoredRatherThanRejectingItAfterTheTimeout(t *testing.T) {
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	store := fixedClaimStore{found: true, claim: supervise.Claim{Token: claim.Token("a-claim"), Generation: 1}}
	resolve := sup.helloResolver(api.NewBootTokens(store), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("a-real-boot-token")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the resolver returned before restoration; a known token must be held, not rejected on the resolve timeout")
	case <-time.After(10 * resolveTimeout):
		// Comfortably longer than the resolve timeout: still blocked proves the wait is on
		// restoration, not that bound.
	}

	close(sup.restored)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the resolver never returned after restoration closed")
	}
}

// A hold on a known token is released, not left dangling, when the boot gives up rather than
// becoming ready: supervision.stop cancels s.ctx before the worker stream closes
// (daemon.go's run), so every held hello returns rather than leaking the goroutine and the
// connection it would otherwise pin forever.
func TestHelloResolverReleasesAKnownTokenWhenTheBootGivesUpRatherThanBecomingReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sup := newSupervisor(ctx, nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	store := fixedClaimStore{found: true, claim: supervise.Claim{Token: claim.Token("a-claim"), Generation: 1}}
	resolve := sup.helloResolver(api.NewBootTokens(store), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("a-real-boot-token")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the resolver returned before the boot gave up; a known token must be held until then")
	case <-time.After(10 * resolveTimeout):
		// Comfortably longer than the resolve timeout: still blocked proves the wait is on s.ctx,
		// not that bound.
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the resolver never returned after the boot's context was cancelled; a held hello would pin a goroutine and a connection forever")
	}
}

// flakyClaimStore fails its first read and answers every later read with its claim.
type flakyClaimStore struct {
	mu    sync.Mutex
	reads int
	claim supervise.Claim
}

func (s *flakyClaimStore) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.reads == 1 {
		return supervise.Claim{}, false, errors.New("read claim: context deadline exceeded")
	}
	return s.claim, true, nil
}

// A store that fails to read a hello's claim says nothing about the boot token, so the hello is not
// refused as an unknown one: the daemon closes the connection unacked and logs the failed read,
// and the shim's redial, which follows any hello it gets no ack for, is accepted once the store
// answers. The listener and the resolver are the daemon's own; the two dials are the shim's.
func TestAHelloWhoseClaimReadFailsOnceIsAcceptedOnTheRedialWithNoRefusal(t *testing.T) {
	const token = claim.Token("legion-test-legion-1-implementer")
	const bootToken = "the-pane-boot-token"
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	sup.machines[token] = &member{}
	close(sup.restored)
	store := &flakyClaimStore{claim: supervise.Claim{Token: token, Generation: 1}}

	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	listener, err := stream.Listen(ctx, "unix://"+filepath.Join(shortTempDir(t), "worker-stream.sock"),
		sup.helloResolver(api.NewBootTokens(store), time.Second),
		stream.Options{RPCTimeout: time.Second, Log: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	conn, err := net.Dial("unix", strings.TrimPrefix(listener.Addr(), "unix://"))
	if err != nil {
		t.Fatalf("dial the worker stream: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	first := &shim{t: t, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn)}
	first.send(shimwire.Hello2{BootToken: bootToken})
	first.closed()

	dialShim(t, listener.Addr(), bootToken)
	select {
	case event := <-listener.Events():
		if event != (stream.Hello{Claim: token, Generation: 1}) {
			t.Fatalf("the listener reported %#v, want the redial's hello", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the redial's hello never reached the listener's events")
	}

	logged := logs.String()
	if strings.Contains(logged, "rejected hello") {
		t.Fatalf("a hello the store could not resolve was refused:\n%s", logged)
	}
	if n := strings.Count(logged, "worker-stream: could not resolve a hello's boot token"); n != 1 ||
		!strings.Contains(logged, "read claim: context deadline exceeded") {
		t.Fatalf("the listener logged the failed read %d times, want once with the store's error:\n%s", n, logged)
	}
}

// relaunchingStore is a supervise.Store/api.BootTokenStore fake whose current row remembers only
// its latest PutClaim, exactly as a real claim row does: ClaimByBootTokenHash finds the hash of
// whichever generation was written last, never an earlier one.
type relaunchingStore struct {
	mu          sync.Mutex
	currentHash []byte
	current     supervise.Claim
}

func (s *relaunchingStore) ClaimByBootTokenHash(_ context.Context, hash []byte) (supervise.Claim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Equal(hash, s.currentHash) {
		return s.current, true, nil
	}
	return supervise.Claim{}, false, nil
}

func (s *relaunchingStore) PutClaim(_ context.Context, c supervise.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentHash = c.BootTokenHash
	s.current = c
	return nil
}

func (s *relaunchingStore) AdmitClaim(ctx context.Context, c supervise.Claim) (supervise.Claim, error) {
	return c, s.PutClaim(ctx, c)
}

func (s *relaunchingStore) CheckLaunch(context.Context, supervise.Claim) error { return nil }

func (s *relaunchingStore) PutDelivery(context.Context, claim.Token, supervise.Delivery) error {
	return nil
}

func (s *relaunchingStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, _ supervise.Delivery) error {
	return s.PutClaim(ctx, c)
}

func (s *relaunchingStore) RetireDelivery(ctx context.Context, c supervise.Claim, _ string) error {
	return s.PutClaim(ctx, c)
}

// A claim stored mid-launch across a restart (generation 1, no locator) is written at generation 2
// while a shim's hello for generation 1's own token is already held in helloResolver, resolved once
// before the hold started. Judging that hold by the first resolve alone would accept the old
// generation after its claim has already moved on, taking the stream slot the real generation-2
// shim's own hello then finds "already bound to a live stream". The fix resolves the token again
// once restoration ends and judges generation 1's hello by that second resolve. This process
// writes generation 1's claim before generation 2's however the claim reaches the restart:
// supervisor.restore rewrites one stored launching, or queued after a release, to
// StateLaunchUncertain, and one already stored StateLaunchUncertain is written by its release
// (supervise's ReleaseUncertainLaunch), before the launch writes generation 2. Every write goes
// through the Recording-wrapped store, which records generation 1's hash in this process's own
// BootTokens though it never minted it, so the second resolve comes back Stale, refused as "stale
// worker generation", and generation 2's own, freshly minted hello is accepted.
func TestHelloResolverResolvesAgainAfterRestorationSoARelaunchDuringTheHoldIsNotMissed(t *testing.T) {
	type result struct {
		stale bool
		known bool
		err   error
	}
	rewrite := func(ctx context.Context, recording supervise.Store, c supervise.Claim) error {
		c.State = supervise.StateLaunchUncertain
		return recording.PutClaim(ctx, c)
	}
	release := func(ctx context.Context, recording supervise.Store, c supervise.Claim) error {
		c.State = supervise.StateQueued
		return recording.PutClaim(ctx, c)
	}

	for _, tc := range []struct {
		name   string
		seeded supervise.ClaimState
		// first is this process's first write of generation 1's claim, through the Recording store,
		// before generation 2's.
		first func(ctx context.Context, recording supervise.Store, c supervise.Claim) error
	}{
		{name: "generation 1 stored launching: restore's rewrite records it", seeded: supervise.StateLaunching, first: rewrite},
		{name: "generation 1 stored queued after a release: restore's rewrite records it", seeded: supervise.StateQueued, first: rewrite},
		{name: "generation 1 stored launch_uncertain: its release records it", seeded: supervise.StateLaunchUncertain, first: release},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &relaunchingStore{}
			tokens := api.NewBootTokens(store)
			recording := tokens.Recording(store)

			const boot1, boot2 = "generation-1-token", "generation-2-token"
			ctx := context.Background()
			// A previous daemon's own row: this process has done no write for it yet, so its
			// BootTokens has recorded nothing. Seeded through the bare store, bypassing Recording,
			// so the write below is the only one that can record the hash.
			generation1 := supervise.Claim{
				Token: claim.Token("a-claim"), Generation: 1, BootTokenHash: supervise.HashBootToken(boot1),
				State: tc.seeded,
			}
			if err := store.PutClaim(ctx, generation1); err != nil {
				t.Fatalf("write generation 1's claim: %v", err)
			}

			sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
			sup.machines[claim.Token("a-claim")] = &member{}
			const resolveTimeout = 20 * time.Millisecond
			resolve := sup.helloResolver(tokens, resolveTimeout)

			done := make(chan result, 1)
			go func() {
				_, _, stale, known, err := resolve(boot1)
				done <- result{stale, known, err}
			}()

			select {
			case <-done:
				t.Fatal("the resolver returned before restoration; generation 1's hello must be held")
			case <-time.After(10 * resolveTimeout):
			}

			// This process's first write of generation 1's claim runs through the Recording-wrapped
			// store with generation 1's hash unchanged; then the relaunch writes generation 2's own
			// fresh token, before the held hello is ever judged.
			if err := tc.first(ctx, recording, generation1); err != nil {
				t.Fatalf("write generation 1's claim: %v", err)
			}
			if err := recording.PutClaim(ctx, supervise.Claim{
				Token: claim.Token("a-claim"), Generation: 2, BootTokenHash: supervise.HashBootToken(boot2),
			}); err != nil {
				t.Fatalf("write generation 2's claim: %v", err)
			}
			close(sup.restored)

			select {
			case got := <-done:
				if got.err != nil || !got.known || !got.stale {
					t.Fatalf("resolve(%q) = known %t, stale %t, err %v; want known and stale, no error",
						boot1, got.known, got.stale, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("the resolver never returned after restoration closed")
			}

			// Generation 2's own, freshly minted hello is accepted once restoration has already
			// closed.
			launched, generation, stale, known, err := resolve(boot2)
			if err != nil || !known || stale || generation != 2 || launched != claim.Token("a-claim") {
				t.Fatalf("resolve(%q) = (%q, %d, stale=%t, known=%t, %v), want (a-claim, 2, false, true, nil)", boot2, launched, generation, stale, known, err)
			}
		})
	}
}

// A registration in flight when the daemon's stop cuts its launch short — the process the launch
// started registered while the launch still waited on the runtime — is refused, and the claim is
// stored as the launch left it: launching at that generation with no process, no session and no
// capability. The next boot's restore finds it an unfinished launch, puts it back to
// launch_uncertain and relaunches it once its orphan reconciliation has run. A registration recorded
// instead would store the claim registered with no process, which restore keeps as it is, arming
// a registration deadline that has no process to probe or retire.
func TestARegistrationInFlightWhenTheStopCutsItsLaunchShortIsRestoredAsAnUnfinishedLaunch(t *testing.T) {
	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	store := &outboxClaimStore{}
	newBoot := func(rt *fake.Runtime, launches *stallingRuntime) *supervisor {
		sup := newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
		sup.deps = supervise.Deps{
			Runtime: rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
			Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
			Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
			PluginContract: api.DaemonAPIVersion,
		}
		if launches != nil {
			sup.deps.Runtime = launches
		}
		t.Cleanup(sup.stop)
		return sup
	}
	launches := newStallingRuntime(t, fake.NewRuntime(), false)
	launches.stalling.Store(true)
	first := newBoot(nil, launches)
	queued := supervise.Claim{Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer, State: supervise.StateQueued}
	if _, err := first.restore(context.Background(), []supervise.Claim{queued}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	m, _ := first.Machine(token)
	stopping, stop := context.WithCancel(context.Background())
	launched := make(chan error, 1)
	go func() { launched <- m.Handle(stopping, supervise.RequestSpawn{Claim: token}) }()
	select {
	case <-launches.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the launch never reached the runtime")
	}
	registered := make(chan error, 1)
	go func() {
		registered <- m.Handle(context.Background(), supervise.RequestRegister{
			Claim: token, Generation: 1, Session: "ses_late", SessionFile: "/sessions/late.jsonl",
			CapabilityHash: []byte("late"), PluginContract: api.DaemonAPIVersion,
		})
	}()
	stop()
	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cut-short launch returned %v, want its context's cancellation", err)
	}
	var refused *supervise.RefusedError
	if err := <-registered; !errors.As(err, &refused) {
		t.Fatalf("the registration in flight = %v, want it refused", err)
	}

	store.mu.Lock()
	stored := store.claims[token]
	store.mu.Unlock()
	if stored.State != supervise.StateLaunching || stored.Generation != 1 || stored.Locator != nil || stored.SessionFile != "" || len(stored.CapabilityHash) != 0 {
		t.Fatalf("the store holds %s at generation %d with locator %+v, session file %q and capability %x; want launching at 1 with no process, session or capability",
			stored.State, stored.Generation, stored.Locator, stored.SessionFile, stored.CapabilityHash)
	}

	next := newBoot(fake.NewRuntime(), nil)
	unfinished, err := next.restore(context.Background(), []supervise.Claim{stored})
	if err != nil {
		t.Fatalf("the next boot's restore: %v", err)
	}
	if len(unfinished) != 1 || unfinished[0] != token {
		t.Fatalf("the next boot's unfinished launches = %v, want %s", unfinished, token)
	}
	if restored, _ := next.Machine(token); restored.Claim().State != supervise.StateLaunchUncertain || restored.Claim().Locator != nil {
		t.Fatalf("the next boot restored %+v, want launch_uncertain with no process", restored.Claim())
	}
}

// A claim a daemon stored live past launching with no process recorded — registered, say, by a
// daemon whose fence let a registration through at a launch that recorded none — is an unfinished
// launch too: no process is known for it, so nothing could probe, retire or relaunch it. The boot's
// restore puts it back to launch_uncertain, in the store and in the claims the boot's later passes
// read, logs it with the state it was stored in, and the boot relaunches its session once its
// orphan reconciliation has run.
func TestALiveClaimStoredWithNoProcessIsRestoredAsAnUnfinishedLaunchAndRelaunched(t *testing.T) {
	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	for _, state := range []supervise.ClaimState{supervise.StateShimConnected, supervise.StateRegistered, supervise.StateReady,
		supervise.StateWorking, supervise.StateIdle} {
		t.Run(string(state), func(t *testing.T) {
			store := &outboxClaimStore{}
			logs := &syncBuffer{}
			enroller := &recordingEnroller{}
			sup := newSupervisor(context.Background(), nil, "legion", t.TempDir(), slog.New(slog.NewTextHandler(logs, nil)))
			rt := fake.NewRuntime()
			sup.deps = supervise.Deps{
				Runtime: rt, Conns: fake.NewConns(), Store: store, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
				Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
				Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
				PluginContract: api.DaemonAPIVersion,
				Secrets:        enroller,
			}
			t.Cleanup(sup.stop)
			enrollment := supervise.Enrollment{ID: "enr-" + string(state), Incarnation: "pod-" + string(state)}
			stored := supervise.Claim{Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer,
				Generation: 1, State: state, Session: "ses_impl", SessionFile: "/sessions/impl.jsonl", PluginContract: api.DaemonAPIVersion,
				CapabilityHash: []byte("known-registration-secret"), Enrollment: &enrollment}
			claims := []supervise.Claim{stored}

			unfinished, err := sup.restore(context.Background(), claims)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}

			if len(unfinished) != 1 || unfinished[0] != token {
				t.Fatalf("the unfinished launches = %v, want %s", unfinished, token)
			}
			m, _ := sup.Machine(token)
			if c := m.Claim(); c.State != supervise.StateLaunchUncertain || c.Locator != nil || len(c.CapabilityHash) != 0 || c.Enrollment != nil {
				t.Fatalf("restored %+v, want launch_uncertain with no process, capability or enrollment", c)
			}
			store.mu.Lock()
			written := store.claims[token]
			store.mu.Unlock()
			if written.State != supervise.StateLaunchUncertain || len(written.CapabilityHash) != 0 || written.Enrollment != nil {
				t.Fatalf("the store holds %+v, want launch_uncertain with no capability or enrollment", written)
			}
			if claims[0].State != supervise.StateLaunchUncertain || len(claims[0].CapabilityHash) != 0 || claims[0].Enrollment != nil {
				t.Fatalf("the boot's claims hold %+v, want launch_uncertain with no capability or enrollment for its later passes", claims[0])
			}

			testwait.Eventually(t, "the stale enrollment to be revoked", func() bool {
				return slices.Equal(enroller.revocations(), []string{enrollment.ID})
			})
			if got := logs.String(); !strings.Contains(got, "restoring a launch the previous daemon did not finish as launch_uncertain") ||
				!strings.Contains(got, "stored="+string(state)) {
				t.Errorf("logged %q, want the repair named with the state it was stored in", got)
			}

			s := &supervision{log: quietLogger(), supervisor: sup}
			if refused := s.launchUnfinished(unfinished); len(refused) != 0 {
				t.Fatalf("the relaunch refused %v", refused)
			}

			resumes := rt.CallsOf("Resume")
			if len(resumes) != 1 || resumes[0].Spec.Generation != 2 || resumes[0].Spec.ResumeSessionFile != "/sessions/impl.jsonl" {
				t.Fatalf("resumes = %+v, want the same session relaunched once at generation 2", resumes)
			}
			if c := m.Claim(); c.State != supervise.StateLaunching || c.Locator == nil || c.Generation != 2 {
				t.Fatalf("after the relaunch the claim is %s at generation %d with locator %+v, want launching at 2 with a process", c.State, c.Generation, c.Locator)
			}
		})
	}
}

// recordingEnroller makes restore prove that it revokes an enrollment belonging to a process it
// can no longer record before it makes that claim launch_uncertain.
type recordingEnroller struct {
	mu      sync.Mutex
	revoked []string
}

func (*recordingEnroller) Enroll(context.Context, supervise.PodEnrollment) (string, error) {
	return "", errors.New("unexpected enrollment")
}

func (e *recordingEnroller) Revoke(_ context.Context, enrollment string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = append(e.revoked, enrollment)
	return nil
}

func (e *recordingEnroller) revocations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.revoked)
}
