package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Under tmux the daemon provisions every issue's workspace on its own host, each a jj workspace of
// its repository's one shared clone, and a project of one repository may have hundreds to
// provision at once — every claim a daemon taking over a store relaunches. Provisions of
// different issues run side by side; each is handed its repository's one store lock
// (workspace.Request.StoreLock), which workspace.Provision holds for every command on the clone,
// so those never overlap, and lets go for the checkout, and the wait for one of the
// provisionConcurrency checkout places (workspace.Request.CheckoutSlot), which it holds for the
// checkout alone.

// storeStep is a stand-in for the commands a provision runs on the shared clone: under the
// request's store lock, counting how many run at once.
func storeStep(t *testing.T, request workspace.Request, inStore, worst *atomic.Int32) {
	t.Helper()
	if request.StoreLock == nil {
		t.Errorf("the provision of %s was handed no store lock", request.Issue)
		return
	}
	request.StoreLock.Lock()
	defer request.StoreLock.Unlock()
	now := inStore.Add(1)
	defer inStore.Add(-1)
	for seen := worst.Load(); now > seen && !worst.CompareAndSwap(seen, now); seen = worst.Load() {
	}
	time.Sleep(10 * time.Millisecond)
}

// Two issues of one repository provision at the same time: the second starts while the first is
// still provisioning (each checkout waits for the other's to begin), and their store steps never
// overlap, since both are handed the repository's one lock.
func TestProvisionsOfTwoIssuesOverlapAndTheirStoreStepsDoNot(t *testing.T) {
	var inStore, worst atomic.Int32
	var checkouts atomic.Int32
	overlapped := make(chan struct{})
	var once sync.Once
	locks := map[string]sync.Locker{}
	var locksMu sync.Mutex
	r := &outbox{
		stateDir: t.TempDir(), tokens: outboxTokens{}, log: quietLogger(),
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			locksMu.Lock()
			locks[request.Issue] = request.StoreLock
			locksMu.Unlock()
			storeStep(t, request, &inStore, &worst)
			if checkouts.Add(1) == 2 {
				once.Do(func() { close(overlapped) })
			}
			select {
			case <-overlapped:
			case <-time.After(20 * time.Second):
				return workspace.Workspace{}, fmt.Errorf("the provision of %s never overlapped another's", request.Issue)
			}
			storeStep(t, request, &inStore, &worst)
			return workspace.Workspace{}, nil
		},
	}
	repo := ghrepo.MustParse("acme/widgets")
	var wg sync.WaitGroup
	for _, key := range []string{"WIDGETS-1", "WIDGETS-2"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := r.provisionIn(context.Background(), key, repo, "main", false); err != nil {
				t.Errorf("provision %s: %v", key, err)
			}
		}(key)
	}
	wg.Wait()
	if got := worst.Load(); got != 1 {
		t.Errorf("%d store steps ran at once, want one at a time", got)
	}
	if locks["WIDGETS-1"] == nil || locks["WIDGETS-1"] != locks["WIDGETS-2"] {
		t.Errorf("the two issues of one repository were handed store locks %p and %p, want the repository's one", locks["WIDGETS-1"], locks["WIDGETS-2"])
	}

	// Another repository's clone is another store: its provision is handed another lock.
	var other sync.Locker
	r.provision = func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
		other = request.StoreLock
		return workspace.Workspace{}, nil
	}
	if err := r.provisionIn(context.Background(), "GADGETS-1", ghrepo.MustParse("acme/gadgets"), "main", false); err != nil {
		t.Fatal(err)
	}
	if other == nil || other == locks["WIDGETS-1"] {
		t.Errorf("another repository's provision was handed store lock %p, want one of its own (widgets' is %p)", other, locks["WIDGETS-1"])
	}
}

// At most provisionConcurrency checkouts run at once: one more waits for a running one to end. Each
// provision is handed the wait for a place, which it takes around its checkout.
func TestCheckoutsAreBoundedByTheirConcurrency(t *testing.T) {
	var running, most atomic.Int32
	release := make(chan struct{})
	r := &outbox{
		stateDir: t.TempDir(), tokens: outboxTokens{}, log: quietLogger(),
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			if request.CheckoutSlot == nil {
				return workspace.Workspace{}, fmt.Errorf("the provision of %s was handed no checkout place to wait for", request.Issue)
			}
			done, err := request.CheckoutSlot(ctx)
			if err != nil {
				return workspace.Workspace{}, err
			}
			defer done()
			now := running.Add(1)
			defer running.Add(-1)
			for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
			}
			<-release
			return workspace.Workspace{}, nil
		},
	}
	repo := ghrepo.MustParse("acme/widgets")
	var wg sync.WaitGroup
	for i := range provisionConcurrency + 1 {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := r.provisionIn(context.Background(), key, repo, "main", false); err != nil {
				t.Errorf("provision %s: %v", key, err)
			}
		}(fmt.Sprintf("WIDGETS-%d", i+1))
	}
	deadline := time.Now().Add(20 * time.Second)
	for running.Load() < provisionConcurrency && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := running.Load(); got != provisionConcurrency {
		t.Errorf("%d checkouts run at once with %d asked for, want %d", got, provisionConcurrency+1, provisionConcurrency)
	}
	close(release)
	wg.Wait()
	if got := most.Load(); got != provisionConcurrency {
		t.Errorf("at most %d checkouts ran at once, want %d", got, provisionConcurrency)
	}
}

// A provision waiting on its repository's store lock holds no checkout place: with one provision
// of a repository holding its store lock and provisionConcurrency more waiting on it, a provision of
// another repository still takes a place, checks out and finishes.
func TestAProvisionOfAnotherRepositoryIsNotBlockedByProvisionsWaitingOnOneStoreLock(t *testing.T) {
	widgets, gadgets := ghrepo.MustParse("acme/widgets"), ghrepo.MustParse("acme/gadgets")
	var waiting atomic.Int32
	hold := make(chan struct{})
	r := &outbox{
		stateDir: t.TempDir(), tokens: outboxTokens{}, log: quietLogger(),
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			if request.StoreLock == nil || request.CheckoutSlot == nil {
				return workspace.Workspace{}, fmt.Errorf("the provision of %s was handed store lock %v and a checkout place %t, want both", request.Issue, request.StoreLock, request.CheckoutSlot != nil)
			}
			// The commands on the shared clone, under its store lock, which the first provision of
			// acme/widgets holds until the test lets it go.
			if request.Repo == widgets {
				waiting.Add(1)
			}
			request.StoreLock.Lock()
			if request.Repo == widgets {
				<-hold
			}
			request.StoreLock.Unlock()
			// The checkout, in a place.
			done, err := request.CheckoutSlot(ctx)
			if err != nil {
				return workspace.Workspace{}, err
			}
			done()
			return workspace.Workspace{}, nil
		},
	}
	var wg sync.WaitGroup
	for i := range provisionConcurrency + 1 {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := r.provisionIn(context.Background(), key, widgets, "main", false); err != nil {
				t.Errorf("provision %s: %v", key, err)
			}
		}(fmt.Sprintf("WIDGETS-%d", i+1))
	}
	deadline := time.Now().Add(20 * time.Second)
	for waiting.Load() < provisionConcurrency+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	other := make(chan error, 1)
	go func() { other <- r.provisionIn(context.Background(), "GADGETS-1", gadgets, "main", false) }()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("provision of acme/gadgets: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("a provision of acme/gadgets waited 10s behind %d provisions of acme/widgets (%d of them waiting on its store lock), want it to check out at once", provisionConcurrency+1, waiting.Load()-1)
	}
	close(hold)
	wg.Wait()
}

// Two launches of one issue that both found its workspace missing (ensureWorkspace) provision it
// once: provisions of one issue take turns, and the second finds the first's workspace there.
func TestTwoLaunchesThatFindOneWorkspaceMissingProvisionItOnce(t *testing.T) {
	var provisions atomic.Int32
	stateDir := t.TempDir()
	repo := ghrepo.MustParse("acme/widgets")
	r := &outbox{
		stateDir: stateDir, tokens: outboxTokens{}, log: quietLogger(),
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisions.Add(1)
			time.Sleep(50 * time.Millisecond)
			located, err := workspace.Location(request.StateDir, request.Repo, request.Issue)
			if err != nil {
				return workspace.Workspace{}, err
			}
			return located, os.MkdirAll(located.Dir, 0o700)
		},
		// The stand-in provision leaves the workspace checked out.
		checkedOut: func(context.Context, workspace.Workspace) (bool, error) { return true, nil },
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.provisionIn(context.Background(), "WIDGETS-7", repo, "main", true); err != nil {
				t.Errorf("provision of a missing workspace: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := provisions.Load(); got != 1 {
		t.Errorf("two launches provisioned the missing workspace %d times, want once", got)
	}
}

// A provision whose checkout failed leaves the issue's workspace directory registered with no files
// (workspace.Provision registers it, then checks it out). The next launch of a claim of the issue —
// the machine's relaunch after the charged failure, a resume, a retry — reads, under the issue's
// turn, that the checkout never finished and provisions again, which finishes it, rather than
// start the pane in an empty working copy. A launch that then finds the workspace whole provisions
// nothing.
func TestALaunchProvisionsAWorkspaceWhoseCheckoutFailed(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	stateDir := t.TempDir()
	repo := ghrepo.MustParse("acme/widgets")
	located, err := workspace.Location(stateDir, repo, "S1-2")
	if err != nil {
		t.Fatal(err)
	}
	checkoutFailed := errors.New("command failed (exit 1): jj sparse reset: No space left on device")
	provisions, checkedOut := 0, false
	var r *outbox
	r = &outbox{
		stateDir: stateDir, tokens: outboxTokens{}, log: quietLogger(),
		supervisor: &supervisor{deps: supervise.Deps{Runtime: fake.NewRuntime()}},
		provision: func(_ context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisions++
			// The registration makes the workspace's directory; the checkout writes its files.
			if err := os.MkdirAll(located.Dir, 0o700); err != nil {
				return workspace.Workspace{}, err
			}
			if provisions == 1 {
				return workspace.Workspace{}, fmt.Errorf("check out workspace %s: %w", located.Dir, checkoutFailed)
			}
			checkedOut = true
			return located, nil
		},
		checkedOut: func(_ context.Context, working workspace.Workspace) (bool, error) {
			if working != located {
				t.Errorf("read the checkout of %#v, want the issue's workspace %#v", working, located)
			}
			turn, release := r.provisions.issues.take(located.Dir)
			defer release()
			if turn.TryLock() {
				turn.Unlock()
				t.Error("the launch read the workspace's checkout outside its issue's turn")
			}
			return checkedOut, nil
		},
	}
	// The issue's first provision, a new claim's, registers the workspace and fails to check it out.
	if err := r.provisionIn(context.Background(), "S1-2", repo, "main", false); !errors.Is(err, checkoutFailed) {
		t.Fatalf("the first provision = %v, want its checkout's failure", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "s1", prompts: composer, repositories: noRepositoriesView,
		repository: func(context.Context, string) (ghrepo.Repository, string, error) {
			return repo, "main", nil
		},
		hostWorkspaces:  true,
		ensureWorkspace: r.ensureWorkspace,
	}
	token, err := claim.NewToken("s1", "S1-2", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	relaunched := supervise.Claim{Token: token, Issue: "S1-2", Tree: "S1-1", Role: claim.RoleImplementer, Session: "ses-1"}
	if _, err := s.SpawnSpec(context.Background(), relaunched); err != nil {
		t.Fatalf("SpawnSpec after the failed checkout: %v", err)
	}
	if provisions != 2 || !checkedOut {
		t.Fatalf("the launch after the failed checkout left %d provisions in all and the checkout finished=%t, want a second provision that finished it", provisions, checkedOut)
	}
	if _, err := s.SpawnSpec(context.Background(), relaunched); err != nil {
		t.Fatalf("SpawnSpec into the whole workspace: %v", err)
	}
	if provisions != 2 {
		t.Errorf("a launch into the whole workspace provisioned it again (%d provisions in all), want none", provisions)
	}
}

// hostWorkspace makes issue's workspace under stateDir as a provision leaves it: a jj workspace of
// the repository's shared clone, checked out whole (workspace.CheckedOut). A launch under a runtime
// that leaves workspaces to the daemon's host (the fake runtime's default, as tmux) then finds it
// whole and provisions nothing (specs.ensureWorkspace), so a test about something else never
// reaches GitHub. The jj it runs reads none of the configuration of the user who runs the tests.
func hostWorkspace(t *testing.T, stateDir, repo, issue string) {
	t.Helper()
	located, err := workspace.Location(stateDir, ghrepo.MustParse(repo), issue)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	jj := func(args ...string) {
		t.Helper()
		command := exec.Command("jj", args...)
		command.Dir = stateDir
		command.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+home, "JJ_CONFIG="+filepath.Join(home, "no-user-config.toml"),
			"JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("jj %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	for _, dir := range []string{filepath.Dir(located.Clone), filepath.Dir(located.Dir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(located.Clone, ".jj")); errors.Is(err, os.ErrNotExist) {
		jj("git", "init", "--no-colocate", located.Clone)
	} else if err != nil {
		t.Fatal(err)
	}
	jj("workspace", "add", "--name", filepath.Base(located.Dir), located.Dir, "-R", located.Clone)
}
