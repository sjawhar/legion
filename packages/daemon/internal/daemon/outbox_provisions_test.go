package daemon

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Under tmux the daemon provisions every issue's workspace on its own host, each a jj workspace of
// its repository's one shared clone, and a project of one repository may have hundreds to
// provision at once — every claim a daemon taking over a store relaunches. Provisions of
// different issues run side by side, up to provisionConcurrency at once; each is handed its
// repository's one store lock (workspace.Request.StoreLock), which workspace.Provision holds for
// every command on the clone, so those never overlap, and lets go for the checkout.

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

// At most provisionConcurrency provisions run at once: one more waits for a running one to end.
func TestProvisionsAreBoundedByTheirConcurrency(t *testing.T) {
	var running, most atomic.Int32
	release := make(chan struct{})
	r := &outbox{
		stateDir: t.TempDir(), tokens: outboxTokens{}, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
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
		t.Errorf("%d provisions run at once with %d asked for, want %d", got, provisionConcurrency+1, provisionConcurrency)
	}
	close(release)
	wg.Wait()
	if got := most.Load(); got != provisionConcurrency {
		t.Errorf("at most %d provisions ran at once, want %d", got, provisionConcurrency)
	}
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

// hostWorkspace makes issue's workspace directory under stateDir, as a provision before it left it:
// a launch under a runtime that leaves workspaces to the daemon's host (the fake runtime's
// default, as tmux) then finds it there and provisions nothing (specs.ensureWorkspace), so a test
// about something else never reaches GitHub.
func hostWorkspace(t *testing.T, stateDir, repo, issue string) {
	t.Helper()
	located, err := workspace.Location(stateDir, ghrepo.MustParse(repo), issue)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(located.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
