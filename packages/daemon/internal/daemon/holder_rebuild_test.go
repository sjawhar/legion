package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// A NATS rebuild ends the old connection's intake and waits for it to settle before it closes
// that connection: an ack or a nak of a fact in flight goes to the connection it was delivered
// on, never to a closed one. Without the wait the rebuild cancelled the run and closed the
// connection in the same breath, and whatever the handlers were holding was acked into nothing.
//
// The holder is the daemon's own (workflowRuntime.connect), so the wait is the one production
// runs: the connection and its consumers are opened outside every lock, the handover runs under
// the fact lock, and only then is the intake this rebuild replaced ended and drained.
func TestANatsRebuildWaitsForTheIntakeItEndedToSettle(t *testing.T) {
	url := testnats.URL(t)
	probe, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect to the test NATS server: %v", err)
	}
	t.Cleanup(probe.Close)
	js, err := jetstream.New(probe)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	testnats.CreateStream(t, js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	w := grantWorkflow(t, &syncBuffer{})
	cfg := daemonGrantConfig(url)
	if err := w.connect(t.Context(), cfg, newRebuilder(w.log, config.Settled(cfg)), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatalf("connect the workflow's NATS: %v", err)
	}
	t.Cleanup(w.stop)
	_, running, _, settled := w.nats.begin(t.Context())
	// The intake loop: its Run returns once the messages it holds are settled, which this test
	// holds until it has seen the rebuild wait for it.
	release := make(chan struct{})
	go func() {
		<-running.Done()
		<-release
		close(settled)
	}()

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- w.nats.rebuild(&cfg) }()

	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild returned before the intake it ended had settled: %v", err)
	case <-time.After(time.Second):
	}
	close(release)
	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rebuild did not return once the intake settled")
	}
}

// A runtime rebuild stops the runtime it replaced: the Sandbox runtime's three informers and its
// cache and credential refreshers run on a context of that runtime's own, so a daemon whose
// `runtime.*` keys change ten times watches the cluster once and not ten times over.
func TestARuntimeRebuildStopsTheRuntimeItReplaced(t *testing.T) {
	replaced, stopReplaced := context.WithCancel(t.Context())
	var built []context.Context
	holder := newRuntimeHolder(fake.NewRuntime(), stopReplaced, quietLogger(), func(config.Config) (runtime.Runtime, context.CancelFunc, error) {
		living, stop := context.WithCancel(t.Context())
		built = append(built, living)
		return fake.NewRuntime(), stop, nil
	})

	if err := holder.rebuild(&config.Config{}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if replaced.Err() == nil {
		t.Fatal("the runtime the rebuild replaced is still running: its informers and refreshers outlive it")
	}
	if len(built) != 1 || built[0].Err() != nil {
		t.Fatalf("the runtime the rebuild built is not running (%d built)", len(built))
	}
	// A build that fails changes nothing, and stops nothing the daemon is supervising with.
	if err := holder.rebuild(&config.Config{}); err != nil {
		t.Fatalf("the second rebuild: %v", err)
	}
	if built[0].Err() == nil {
		t.Fatal("the second rebuild left the runtime it replaced running")
	}
}

// A runtime a rebuild could not build is stopped where it was built, and the runtime the daemon
// is supervising with keeps running.
func TestARuntimeRebuildThatFailsStopsNothingTheDaemonIsSupervisingWith(t *testing.T) {
	serving, stopServing := context.WithCancel(t.Context())
	holder := newRuntimeHolder(fake.NewRuntime(), stopServing, quietLogger(), func(config.Config) (runtime.Runtime, context.CancelFunc, error) {
		return nil, nil, errors.New("the cluster credential cannot be read")
	})

	if err := holder.rebuild(&config.Config{}); err == nil {
		t.Fatal("the rebuild answered no error, want the build's own")
	}
	if serving.Err() != nil {
		t.Fatal("a failed rebuild stopped the runtime the daemon is supervising with")
	}
}

// blockingRuntime is a runtime whose Spawn is still inside it when the rebuild comes, as a
// Sandbox Spawn is while it waits on the informers for its pod (runtime/sandbox's await).
type blockingRuntime struct {
	runtime.Runtime
	entered chan struct{}
	release chan struct{}
}

func (r *blockingRuntime) Spawn(ctx context.Context, _ runtime.SpawnSpec) (runtime.Locator, error) {
	close(r.entered)
	<-r.release
	return runtime.Locator{}, ctx.Err()
}

// A runtime rebuild ends the runtime it replaced only once the calls inside it have returned. A
// Spawn or a Resume already in that runtime polls its informers until its pod is there
// (runtime/sandbox's await, awaitNewPod); ending the runtime under it freezes that store, so the
// launch waits out its boot timeout, fails, and spends one of the claim's launch failures
// (supervise.Limits.LaunchFailures) for a settings change that took no pod down.
func TestARuntimeRebuildWaitsForTheCallsInsideTheRuntimeItReplaced(t *testing.T) {
	spawning := &blockingRuntime{Runtime: fake.NewRuntime(), entered: make(chan struct{}), release: make(chan struct{})}
	replaced, stopReplaced := context.WithCancel(t.Context())
	holder := newRuntimeHolder(spawning, stopReplaced, quietLogger(), func(config.Config) (runtime.Runtime, context.CancelFunc, error) {
		_, stop := context.WithCancel(t.Context())
		return fake.NewRuntime(), stop, nil
	})
	launched := make(chan error, 1)
	go func() {
		_, err := holder.Spawn(t.Context(), runtime.SpawnSpec{})
		launched <- err
	}()
	<-spawning.entered

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- holder.rebuild(&config.Config{}) }()

	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild returned while a launch was still inside the runtime it replaced: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if replaced.Err() != nil {
		t.Fatal("the rebuild ended the runtime a launch was still inside: its informers stop and the launch polls a store nothing refreshes until its boot timeout")
	}
	close(spawning.release)

	select {
	case err := <-rebuilt:
		if err != nil {
			t.Fatalf("rebuild: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the rebuild did not return once the launch had left the runtime it replaced")
	}
	if replaced.Err() == nil {
		t.Fatal("the rebuild left the runtime it replaced running after its last call returned")
	}
	if err := <-launched; err != nil {
		t.Fatalf("the launch inside the replaced runtime = %v, want it answered on a runtime that was still running", err)
	}
}

// The wait a rebuild gives the calls inside the runtime it replaced is that runtime's own
// launch bound and a margin, never a fixed two minutes. One Sandbox launch try may run up to
// LaunchBound — `worker_boot_timeout_seconds` several times over plus the provision bound
// (runtime/sandbox's launchBound) — and that timeout defaults to 120 s and is the runtime's own
// to rebuild, so the bound to wait is the one the replaced runtime was built with and no caller
// can raise it under a launch: a drain bounded by the two minutes the NATS intake is given would
// stop the old runtime under a Spawn, Resume or ReplacePod that was still rightfully inside it,
// leaving the call a cancelled context and the claim one of its launch failures.
func TestTheRuntimeDrainBoundIsTheReplacedRuntimesLaunchBoundAndAMargin(t *testing.T) {
	launching := fake.NewRuntime()
	launching.TryBound = 25 * time.Minute

	if bound := runtimeDrainBound(launching); bound != launching.TryBound+drainBound {
		t.Fatalf("a rebuild waits %s for the calls inside a runtime whose launch bound is %s, want that bound and the %s margin",
			bound, launching.TryBound, drainBound)
	}
	// A runtime that states no bound — a tmux pane start is a local command with no cluster
	// wait — is waited the margin alone.
	if bound := runtimeDrainBound(fake.NewRuntime()); bound != drainBound {
		t.Fatalf("a rebuild waits %s for the calls inside a runtime that states no launch bound, want %s", bound, drainBound)
	}
}

// The App token manager is built again only when its own App credentials moved. A rebuild closes
// the manager it replaces — every lease it granted is dropped and a `token_command` still
// waiting on a person's approval is killed — and the file watcher wakes on every watched
// directory, the one the kubelet rewrites the projected token in among them, so a manager
// rebuilt at every wake would do that routinely for nothing.
func TestARereadRebuildsTheAppTokenManagerOnlyWhenItsAppCredentialsMoved(t *testing.T) {
	cfg := &config.Config{GitHubApps: config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: "implement-key"},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: "review-key"},
	}}
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	held := &closingTokens{}
	rebuild.register(newAppsHolder(held))

	// A wake of the watcher with the App credentials where they were.
	rebuild.rereadCredentials(cfg)

	if held.closed {
		t.Fatal("a reread over unmoved App credentials closed the manager the daemon holds: every lease dropped and every token_command killed")
	}

	cfg.GitHubApps.Implement.PrivateKey = "rotated-implement-key"
	rebuild.rereadCredentials(cfg)

	if !held.closed {
		t.Fatal("a reread after the App private key moved left the manager built on the old key serving")
	}
}

// One changed credential file is one rebuild. The watcher's settle applies the change — the
// holder its row names is built again — and then asks every credential holder to read its
// credentials again (the daemon's watcher.Settled, rereadCredentials): with the digest recorded
// where the reread decided rather than where the holder was built, that reread found the file
// moved from what it last recorded and built the holder a second time, replacing the Dispatch
// client twice over for one change.
func TestAChangedCredentialFileRebuildsItsHolderOnce(t *testing.T) {
	dir := t.TempDir()
	held, routed := filepath.Join(dir, "dispatch-token"), filepath.Join(dir, "dispatch-token-rotated")
	for file, contents := range map[string]string{held: "bearer\n", routed: "new-bearer\n"} {
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	cfg := &config.Config{DispatchTokenFile: held}
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	client := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderDispatchClient},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.DispatchTokenFile) },
	}
	rebuild.register(client)

	// The change: the daemon is running on the file the operator named.
	cfg.DispatchTokenFile = routed
	rebuild.observe(cfg, []config.Change{changeOfPath(t, "dispatch_token_file")})
	// And the reread the settle runs behind it, over credentials nothing has moved since.
	rebuild.rereadCredentials(cfg)

	if client.built != 1 {
		t.Fatalf("the Dispatch client was built %d times for one changed dispatch_token_file, want once", client.built)
	}
}

// countingApps is the App credential holder with its rebuilds counted: the holder itself, so
// what a reread compares is the manager's own credentials (appsHolder.credentials).
type countingApps struct {
	*appsHolder
	built int
}

func (h *countingApps) rebuild(cfg *config.Config) error {
	h.built++
	return h.appsHolder.rebuild(cfg)
}

// A route change of an App credential is built once, and the next wake of the file watcher —
// the kubelet refreshing a projected token in a watched directory, which is no credential of
// the manager's at all — leaves the manager that change built serving. Built again there, that
// rebuild would close the manager that has served since the change: every lease it granted
// dropped, and a `token_command` still waiting on a person's approval killed.
func TestARouteChangeOfAnAppCredentialIsNotBuiltAgainAtTheNextWake(t *testing.T) {
	cfg := &config.Config{GitHubApps: config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: "implement-key"},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: "review-key"},
	}}
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	apps := &countingApps{appsHolder: newAppsHolder(&closingTokens{})}
	rebuild.register(apps)

	cfg.GitHubApps.Implement.PrivateKey = "rotated-implement-key"
	rebuild.observe(cfg, []config.Change{changeOfPath(t, "github_apps.implement.private_key")})
	if apps.built != 1 {
		t.Fatalf("the App manager was built %d times by the change of its own private key, want once", apps.built)
	}

	rebuild.rereadCredentials(cfg)

	if apps.built != 1 {
		t.Fatalf("the App manager was built %d times, want the change's one: the wake's rebuild closes the manager that has served since, dropping its leases and killing a token_command waiting on a person", apps.built)
	}
}

// A rebuild that failed records nothing, so the next reread tries the credentials it could not
// build on again: a seed rotated while NATS is unreachable is reached at the next wake of the
// watcher, and not left for a restart. Recorded before the rebuild, the new seed counted as
// built on and the daemon stayed on the connection it had with nothing to try it again.
func TestARereadRebuildThatFailedIsTriedAgainAtTheNextReread(t *testing.T) {
	seedFile := filepath.Join(t.TempDir(), "nats-seed")
	if err := os.WriteFile(seedFile, []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("write the seed: %v", err)
	}
	cfg := &config.Config{NatsNkeySeedFile: seedFile}
	rebuild := newRebuilder(quietLogger(), config.Settled(*cfg))
	natsClient := &countingCredentialHolder{
		countingHolder: countingHolder{name: config.HolderNATS, fails: errors.New("connect Envoy NATS: no servers available")},
		reads:          func(cfg *config.Config) string { return credentialDigest(cfg.NatsNkeySeedFile) },
	}
	rebuild.register(natsClient)

	// The seed is rotated in place, which names no setting, and the rebuild on it fails.
	if err := os.WriteFile(seedFile, []byte("rotated-seed\n"), 0o600); err != nil {
		t.Fatalf("rotate the seed: %v", err)
	}
	rebuild.rereadCredentials(cfg)
	if natsClient.built != 1 {
		t.Fatalf("the rotated seed was tried %d times, want once", natsClient.built)
	}

	// The next wake of the watcher, with the seed where the rotation left it, tries it again.
	rebuild.rereadCredentials(cfg)
	if natsClient.built != 2 {
		t.Fatalf("the seed the daemon could not connect with was tried %d times, want it tried again at the next reread", natsClient.built)
	}

	// Once the rebuild succeeds the holder is recorded on that seed, and the wake after it
	// builds nothing.
	natsClient.fails = nil
	rebuild.rereadCredentials(cfg)
	rebuild.rereadCredentials(cfg)
	if natsClient.built != 3 {
		t.Fatalf("NATS was built %d times, want three: the reread after the rebuild that worked rebuilt it over a seed that had not moved since", natsClient.built)
	}
}

// changeOfPath is one applied change of the path, as an apply reports it to the rebuild
// observer: the row's own holder and class (config.ClassOf).
func changeOfPath(t *testing.T, path string) config.Change {
	t.Helper()
	row, known := config.ClassOf(path)
	if !known {
		t.Fatalf("the class table holds no %s row", path)
	}
	return config.Change{Path: path, Holder: row.Holder, Class: row.Class, Outcome: row.Class.Outcome()}
}

// `repository_renames` is held by four things — the API server's settings, the project's
// repository mapping, the intake consumers and the gh bundles the runtime's launches hand each
// role — so a change of it builds all four again. The operator is told the API server was
// rebuilt, which is the one holder the row names (config.Row.Holder); the other three are its
// row's Also, and a reader left on the boot's value would compare two names of one repository as
// two repositories.
func TestAChangedRepositoryRenamesRebuildsEveryHolderOfIt(t *testing.T) {
	rebuild := newRebuilder(quietLogger(), config.Settled(config.Config{}))
	holders := map[string]*countingHolder{}
	for _, name := range []string{
		config.HolderAPIServer, config.HolderRepositoryMapping, config.HolderNATS, config.HolderRuntime,
		config.HolderDispatchClient,
	} {
		holders[name] = &countingHolder{name: name}
		rebuild.register(holders[name])
	}

	row, known := config.ClassOf("repository_renames")
	if !known {
		t.Fatal("the class table holds no repository_renames row")
	}
	rebuild.observe(&config.Config{}, []config.Change{{
		Path: "repository_renames", Holder: row.Holder, Class: row.Class, Outcome: row.Class.Outcome(),
	}})

	for _, name := range []string{
		config.HolderAPIServer, config.HolderRepositoryMapping, config.HolderNATS, config.HolderRuntime,
	} {
		if holders[name].built != 1 {
			t.Errorf("%s was built %d times by a changed repository_renames, want once", name, holders[name].built)
		}
	}
	if holders[config.HolderDispatchClient].built != 0 {
		t.Errorf("the Dispatch client was built %d times by a changed repository_renames, want never",
			holders[config.HolderDispatchClient].built)
	}
}

// The mapping holder is what carries that change into the mapping: every later comparison of two
// repository names reads the equivalence the change settled, not the one the daemon booted on.
func TestTheMappingHolderMovesTheRepositoryEquivalence(t *testing.T) {
	repositories := newRepositories("DEMO", ghrepo.Renames{})
	if repositories.same("sjawhar/legion", "cybermonklabs/legion") {
		t.Fatal("the mapping already calls the two names one repository, so this proves nothing")
	}

	holder := newMappingHolder(repositories)
	renames := ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))
	if err := holder.rebuild(&config.Config{RepositoryRenames: renames}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if !repositories.same("sjawhar/legion", "cybermonklabs/legion") {
		t.Fatal("the mapping still compares names by the equivalence the daemon booted on")
	}
}

// closingTokens is an App token manager that records its Close, as appauth.Manager's does for a
// token_command still waiting on a grant.
type closingTokens struct {
	appauth.Tokens
	closed bool
}

func (t *closingTokens) Close() { t.closed = true }

// The daemon's stop closes the App token manager, so a token_command waiting on a person's
// approval does not outlive the daemon. The workflow holds the holder, not the manager, so the
// holder is what must answer Close: without it the stop closed nothing at all.
func TestTheStopClosesTheAppTokenManagerThroughItsHolder(t *testing.T) {
	tokens := &closingTokens{}
	holder := newAppsHolder(tokens)

	closeTokens(holder)

	if !tokens.closed {
		t.Fatal("the daemon's stop left the App token manager open")
	}
}
