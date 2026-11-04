package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A NATS rebuild ends the old connection's intake and waits for it to settle before it closes
// that connection: an ack or a nak of a fact in flight goes to the connection it was delivered
// on, never to a closed one. Without the wait the rebuild cancelled the run and closed the
// connection in the same breath, and whatever the handlers were holding was acked into nothing.
func TestANatsRebuildWaitsForTheIntakeItEndedToSettle(t *testing.T) {
	holder := newNATSHolder(nil, nil, quietLogger(), func(config.Config) (*nats.Conn, *intake.Consumers, error) {
		return nil, nil, nil
	})
	_, running, _, settled := holder.begin(t.Context())
	// The intake loop: its Run returns once the messages it holds are settled, which this test
	// holds until it has seen the rebuild wait for it.
	release := make(chan struct{})
	go func() {
		<-running.Done()
		<-release
		close(settled)
	}()

	rebuilt := make(chan error, 1)
	go func() { rebuilt <- holder.rebuild(&config.Config{}) }()

	select {
	case err := <-rebuilt:
		t.Fatalf("the rebuild returned before the intake it ended had settled: %v", err)
	case <-time.After(100 * time.Millisecond):
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
	holder := newRuntimeHolder(fake.NewRuntime(), stopReplaced, func(config.Config) (runtime.Runtime, context.CancelFunc, error) {
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
	holder := newRuntimeHolder(fake.NewRuntime(), stopServing, func(config.Config) (runtime.Runtime, context.CancelFunc, error) {
		return nil, nil, errors.New("the cluster credential cannot be read")
	})

	if err := holder.rebuild(&config.Config{}); err == nil {
		t.Fatal("the rebuild answered no error, want the build's own")
	}
	if serving.Err() != nil {
		t.Fatal("a failed rebuild stopped the runtime the daemon is supervising with")
	}
}

// `repository_renames` is held by four things — the API server's settings, the project's
// repository mapping, the intake consumers and the gh bundles the runtime's launches hand each
// role — so a change of it builds all four again. The operator is told the API server was
// rebuilt, which is the one holder the row names (config.Row.Holder); the other three are its
// row's Also, and a reader left on the boot's value would compare two names of one repository as
// two repositories.
func TestAChangedRepositoryRenamesRebuildsEveryHolderOfIt(t *testing.T) {
	rebuild := newRebuilder(quietLogger())
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
