package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// heldLock is a Request.StoreLock that knows whether it is held, so a runner can read, at each
// command, whether provisioning ran it under the lock.
type heldLock struct {
	mu   sync.Mutex
	held atomic.Bool
}

func (l *heldLock) Lock()   { l.mu.Lock(); l.held.Store(true) }
func (l *heldLock) Unlock() { l.held.Store(false); l.mu.Unlock() }

// lockReadingRunner runs every command through run and records, per command, whether lock was
// held as it started.
type lockReadingRunner struct {
	*recordingRunner
	lock *heldLock
	mu   sync.Mutex
	seen []string
}

func (r *lockReadingRunner) Run(ctx context.Context, command Command) (Result, error) {
	r.mu.Lock()
	r.seen = append(r.seen, fmt.Sprintf("%v %s", r.lock.held.Load(), strings.Join(command.Argv, " ")))
	r.mu.Unlock()
	return r.recordingRunner.Run(ctx, command)
}

// isCheckout is the one command that writes an issue's own working copy: `jj sparse reset` in the
// workspace, which `jj workspace add --sparse-patterns empty` registered with no files.
func isCheckout(argv []string) bool {
	return len(argv) > 2 && argv[0] == "jj" && argv[1] == "sparse" && argv[2] == "reset"
}

// Every command provisioning runs on the repository's shared clone runs under the request's
// StoreLock — its clone, fetch, bookmark reads and writes, the workspace's registration (`jj
// workspace add`, which writes no file of it), git worktree bookkeeping and configuration — and the
// lock is let go only while `jj sparse reset` writes the issue's own working copy, the step whose
// time follows the repository's size, so the checkouts of several issues can run side by side while
// nothing else of the clone ever does. Provision returns with the lock released, on the first
// workspace of a clone, on another issue's, and on a workspace that already exists, which it checks
// out no more.
func TestProvisionHoldsTheStoreLockForEverythingButTheCheckout(t *testing.T) {
	lock := &heldLock{}
	run := &lockReadingRunner{recordingRunner: newLocalRunner(t), lock: lock}
	req := provisionRequest(t)
	req.StoreLock = lock
	for _, issue := range []string{"WIDGETS-42", "WIDGETS-43", "WIDGETS-42"} {
		req.Issue = issue
		if _, err := Provision(context.Background(), run, req); err != nil {
			t.Fatalf("Provision %s: %v", issue, err)
		}
		if lock.held.Load() || !lock.mu.TryLock() {
			t.Fatalf("Provision %s returned with the store lock held", issue)
		}
		lock.mu.Unlock()
	}
	adds, checkouts := 0, 0
	for _, line := range run.seen {
		held, argv, _ := strings.Cut(line, " ")
		words := strings.Fields(argv)
		checkout := isCheckout(words)
		if checkout {
			checkouts++
		}
		if commandWith(words, "jj", "workspace", "add") {
			adds++
			if !strings.Contains(argv, "--sparse-patterns empty") {
				t.Errorf("ran %q, want the workspace registered with no files (--sparse-patterns empty)", argv)
			}
		}
		if checkout == (held == "true") {
			t.Errorf("ran %q with the store lock held=%s; want it held for every command but the checkout", argv, held)
		}
	}
	if adds != 2 || checkouts != 2 {
		t.Errorf("ran jj workspace add %d times and the checkout %d, want each twice (the third provision found its workspace whole)", adds, checkouts)
	}
}

// A workspace whose checkout never finished — registered with no files, the daemon stopped before
// `jj sparse reset` wrote them — gets its files from the next provision, which finds the directory
// and would otherwise leave the pane an empty working copy.
func TestProvisionFinishesACheckoutThatNeverRan(t *testing.T) {
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "main", "kept.txt")
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	runSetup(t, first.Dir, "jj", "sparse", "set", "--clear", "-R", first.Dir)
	if entries, _ := os.ReadDir(first.Dir); len(entries) > 2 {
		t.Fatalf("the emptied workspace still holds %d entries", len(entries))
	}
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("Provision of the half-made workspace: %v", err)
	}
	if listed := strings.TrimSpace(runSetup(t, first.Dir, "jj", "sparse", "list", "-R", first.Dir)); listed != "." {
		t.Errorf("the workspace's sparse patterns are %q after the next provision, want the whole tree (.)", listed)
	}
	if _, err := os.Stat(filepath.Join(first.Dir, "kept.txt")); err != nil {
		t.Errorf("the workspace's files are not back: %v", err)
	}
}

// failingCheckoutRunner fails the first checkout (`jj sparse reset`) it is asked to run, as a jj
// error, a full disk or a daemon stopped mid-checkout does, and runs every other command.
type failingCheckoutRunner struct {
	*recordingRunner
	failed atomic.Bool
}

func (r *failingCheckoutRunner) Run(ctx context.Context, command Command) (Result, error) {
	if isCheckout(command.Argv) && r.failed.CompareAndSwap(false, true) {
		return Result{ExitCode: 1, Stderr: "Error: No space left on device (os error 28)"}, nil
	}
	return r.recordingRunner.Run(ctx, command)
}

// A checkout that fails leaves the workspace registered with no files: Provision returns the
// failure, CheckedOut reads the workspace as not checked out, and the next Provision checks it out.
func TestTheProvisionAfterAFailedCheckoutFinishesIt(t *testing.T) {
	base := newLocalRunner(t)
	pushRemoteBranch(t, base.remote, "main", "kept.txt")
	run := &failingCheckoutRunner{recordingRunner: base}
	req := provisionRequest(t)
	if _, err := Provision(context.Background(), run, req); err == nil || !strings.Contains(err.Error(), "check out workspace") || !strings.Contains(err.Error(), "No space left on device") {
		t.Fatalf("Provision with a failing checkout = %v, want the checkout's failure", err)
	}
	working, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(working.Dir, "kept.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the workspace whose checkout failed holds the tree's files (%v), want none", err)
	}
	if whole, err := CheckedOut(context.Background(), run, working); err != nil || whole {
		t.Fatalf("CheckedOut of the workspace whose checkout failed = %t, %v; want false", whole, err)
	}
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("Provision after the failed checkout: %v", err)
	}
	if whole, err := CheckedOut(context.Background(), run, working); err != nil || !whole {
		t.Errorf("CheckedOut after the next provision = %t, %v; want true", whole, err)
	}
	if _, err := os.Stat(filepath.Join(working.Dir, "kept.txt")); err != nil {
		t.Errorf("the workspace's files are not there after the next provision: %v", err)
	}
}

// CheckedOut reads a workspace's sparse patterns without snapshotting its working copy, which a
// pane may be writing: a file the pane wrote is not recorded in the repository's log.
func TestCheckedOutNeverSnapshotsTheWorkingCopy(t *testing.T) {
	run := newLocalRunner(t)
	working, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(working.Dir, "pane-edit.txt"), []byte("written by the pane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := cloneSnapshots(t, working.Clone)
	if whole, err := CheckedOut(context.Background(), run, working); err != nil || !whole {
		t.Fatalf("CheckedOut of a provisioned workspace = %t, %v; want true", whole, err)
	}
	if after := cloneSnapshots(t, working.Clone); after != before {
		t.Errorf("CheckedOut snapshotted the working copy (%d snapshots, then %d)", before, after)
	}
}

// placeReadingRunner records, per command, whether the store lock and a checkout place were held
// as it started.
type placeReadingRunner struct {
	*recordingRunner
	lock  *heldLock
	place *atomic.Bool
	mu    sync.Mutex
	seen  []placeRead
}

type placeRead struct {
	argv         string
	store, place bool
}

func (r *placeReadingRunner) Run(ctx context.Context, command Command) (Result, error) {
	r.mu.Lock()
	r.seen = append(r.seen, placeRead{argv: strings.Join(command.Argv, " "), store: r.lock.held.Load(), place: r.place.Load()})
	r.mu.Unlock()
	return r.recordingRunner.Run(ctx, command)
}

// A provision holds one of the request's checkout places (CheckoutSlot) around its checkout alone:
// it waits for a place only once it has let the store lock go, runs nothing else while it holds
// one, and lets it go before it returns; a provision of a workspace already checked out takes
// none. A wait for a place that ends with its context fails the provision, naming the wait, with
// the workspace registered and not checked out, which the next provision checks out.
func TestProvisionHoldsACheckoutPlaceAroundTheCheckoutAlone(t *testing.T) {
	lock := &heldLock{}
	var place atomic.Bool
	places := 0
	run := &placeReadingRunner{recordingRunner: newLocalRunner(t), lock: lock, place: &place}
	req := provisionRequest(t)
	req.StoreLock = lock
	req.CheckoutSlot = func(context.Context) (func(), error) {
		if lock.held.Load() {
			t.Error("Provision waited for a checkout place holding the store lock")
		}
		if !place.CompareAndSwap(false, true) {
			t.Error("Provision took a second checkout place")
		}
		places++
		return func() { place.Store(false) }, nil
	}
	for _, issue := range []string{"WIDGETS-42", "WIDGETS-42"} {
		req.Issue = issue
		if _, err := Provision(context.Background(), run, req); err != nil {
			t.Fatalf("Provision %s: %v", issue, err)
		}
		if place.Load() {
			t.Fatalf("Provision %s returned holding its checkout place", issue)
		}
	}
	if places != 1 {
		t.Errorf("two provisions of one issue took %d checkout places, want one (the second found the workspace checked out)", places)
	}
	for _, read := range run.seen {
		checkout := isCheckout(strings.Fields(read.argv))
		if read.place != checkout || (checkout && read.store) {
			t.Errorf("ran %q holding the store lock=%t and a checkout place=%t; want the place for the checkout alone, without the lock", read.argv, read.store, read.place)
		}
	}

	req.Issue = "WIDGETS-43"
	req.CheckoutSlot = func(context.Context) (func(), error) { return nil, context.Canceled }
	if _, err := Provision(context.Background(), run, req); err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "wait for a checkout's place") {
		t.Fatalf("Provision whose wait for a place ended = %v, want the wait's end named", err)
	}
	if lock.held.Load() {
		t.Fatal("Provision whose wait for a place ended returned with the store lock held")
	}
	working, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if whole, err := CheckedOut(context.Background(), run, working); err != nil || whole {
		t.Fatalf("CheckedOut of the workspace that never had a place = %t, %v; want false", whole, err)
	}
	req.CheckoutSlot = nil
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("Provision after the wait for a place ended: %v", err)
	}
	if whole, err := CheckedOut(context.Background(), run, working); err != nil || !whole {
		t.Errorf("CheckedOut after the next provision = %t, %v; want true", whole, err)
	}
}

// unlockWatch is a store lock that runs onUnlock each time it is let go, before it is free.
type unlockWatch struct {
	heldLock
	onUnlock func()
}

func (l *unlockWatch) Unlock() { l.onUnlock(); l.heldLock.Unlock() }

// Remove holds the repository's store lock for every command on the shared clone and the rename
// aside, and lets it go before it deletes the workspace's files, whose time follows the
// workspace's size: when the lock is let go the renamed-aside directory is still there, and once
// Remove returns it is gone, with the workspace, and the lock is free.
func TestRemoveLetsTheStoreLockGoBeforeItDeletesTheWorkspace(t *testing.T) {
	base := newLocalRunner(t)
	working, err := Provision(context.Background(), base, provisionRequest(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	lock := &unlockWatch{}
	var asideAtUnlock []bool
	lock.onUnlock = func() {
		_, err := os.Stat(removingPath(working.Dir))
		asideAtUnlock = append(asideAtUnlock, err == nil)
	}
	run := &lockReadingRunner{recordingRunner: base, lock: &lock.heldLock}
	if err := Remove(context.Background(), run, working, lock); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(asideAtUnlock) != 1 || !asideAtUnlock[0] {
		t.Errorf("Remove let the store lock go %d times, the aside directory there each time: %v; want once, before the delete", len(asideAtUnlock), asideAtUnlock)
	}
	if len(run.seen) == 0 {
		t.Fatal("Remove ran no command on the shared clone")
	}
	for _, line := range run.seen {
		if held, argv, _ := strings.Cut(line, " "); held != "true" {
			t.Errorf("Remove ran %q with the store lock let go", argv)
		}
	}
	for _, dir := range []string{working.Dir, removingPath(working.Dir)} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after Remove: %v", dir, err)
		}
	}
	if lock.held.Load() || !lock.mu.TryLock() {
		t.Fatal("Remove returned with the store lock held")
	}
	lock.mu.Unlock()
}

// overlapRunner holds the first checkout until a second one starts, so two provisions' checkouts
// are seen to run at once, and counts the other commands that run at once.
type overlapRunner struct {
	*recordingRunner
	checkouts, store, worstStore atomic.Int32
	overlapped                   chan struct{}
	once                         sync.Once
}

func (r *overlapRunner) Run(ctx context.Context, command Command) (Result, error) {
	if !isCheckout(command.Argv) {
		now := r.store.Add(1)
		defer r.store.Add(-1)
		for worst := r.worstStore.Load(); now > worst && !r.worstStore.CompareAndSwap(worst, now); worst = r.worstStore.Load() {
		}
		return r.recordingRunner.Run(ctx, command)
	}
	if r.checkouts.Add(1) == 2 {
		r.once.Do(func() { close(r.overlapped) })
	}
	select {
	case <-r.overlapped:
	case <-time.After(20 * time.Second):
		return Result{}, fmt.Errorf("the checkouts never overlapped: %s waited 20s for another issue's checkout", strings.Join(command.Argv, " "))
	}
	return r.recordingRunner.Run(ctx, command)
}

// Two issues of one repository provisioned at once share its clone: their checkouts run side by
// side, and no two of the clone's other commands ever do. Both workspaces are then whole, and the
// clone knows both.
func TestTwoIssuesProvisionedAtOnceOverlapOnlyInTheirCheckouts(t *testing.T) {
	lock := &sync.Mutex{}
	base := newLocalRunner(t)
	req := provisionRequest(t)
	req.StoreLock = lock
	// The clone exists first, as it does once a repository's first issue ran.
	req.Issue = "WIDGETS-41"
	if _, err := Provision(context.Background(), base, req); err != nil {
		t.Fatalf("Provision of the first issue: %v", err)
	}
	run := &overlapRunner{recordingRunner: base, overlapped: make(chan struct{})}

	var wg sync.WaitGroup
	results := make(map[string]Workspace)
	var resultsMu sync.Mutex
	for _, issue := range []string{"WIDGETS-42", "WIDGETS-43"} {
		wg.Add(1)
		go func(issue string) {
			defer wg.Done()
			mine := req
			mine.Issue = issue
			working, err := Provision(context.Background(), run, mine)
			if err != nil {
				t.Errorf("Provision %s: %v", issue, err)
				return
			}
			resultsMu.Lock()
			results[issue] = working
			resultsMu.Unlock()
		}(issue)
	}
	wg.Wait()
	if worst := run.worstStore.Load(); worst != 1 {
		t.Errorf("%d of the clone's commands ran at once, want one at a time", worst)
	}
	listed := runSetup(t, req.StateDir, "jj", "workspace", "list", "--ignore-working-copy", "-R", results["WIDGETS-42"].Clone)
	for issue, working := range results {
		if !strings.Contains(listed, strings.ToLower(issue)+":") {
			t.Errorf("the clone's workspaces %q do not list %s", listed, issue)
		}
		if bookmark, at := commitOf(t, working.Clone, working.Bookmark), commitOf(t, working.Dir, "@"); bookmark != at {
			t.Errorf("%s's bookmark is on %s, want its working copy %s", issue, bookmark, at)
		}
		runSetup(t, working.Dir, "jj", "status", "-R", working.Dir)
	}
}

// A host provision runs its fetch under the store lock, so it fetches only what the issue needs —
// the repository's default branch and the issue's own branch, each by its exact name — never every
// branch GitHub has: another issue's branch pushed since is not brought, and the issue's own branch,
// made on GitHub before its workspace, is.
func TestAHostProvisionFetchesOnlyTheBranchesItNeeds(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	req.Issue = "WIDGETS-41"
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision of the first issue: %v", err)
	}
	own := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "own.txt")
	pushRemoteBranch(t, run.remote, "legion/WIDGETS-99", "other.txt")
	before := len(run.Calls())
	req.Issue = "WIDGETS-42"
	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Provision of WIDGETS-42: %v", err)
	}
	var fetch Command
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "jj", "git", "fetch") {
			fetch = call
		}
	}
	for _, branch := range []string{"main", "legion/WIDGETS-42"} {
		if !slices.Contains(fetch.Argv, "exact:"+jjString(branch)) {
			t.Errorf("the fetch %q does not name %s exactly", fetch.Argv, branch)
		}
	}
	if parent := commitOf(t, working.Dir, "@-"); parent != own {
		t.Errorf("WIDGETS-42's workspace starts at %s, want its own branch's %s", parent, own)
	}
	if listed := runSetup(t, first.Clone, "jj", "bookmark", "list", "--all-remotes", "--ignore-working-copy", "-R", first.Clone); strings.Contains(listed, "WIDGETS-99") {
		t.Errorf("the fetch brought another issue's branch: %q", listed)
	}
}
