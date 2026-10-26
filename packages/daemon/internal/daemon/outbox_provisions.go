package daemon

import (
	"context"
	"sync"
)

// provisionConcurrency is how many workspace checkouts the outbox's provisions run at once
// (provisionLocks.slot, workspace.Request.CheckoutSlot). Under tmux each provision checks out the
// issue's whole working copy on the daemon's host — one project's repository is 80,975 files and
// 1.5 GB, about 20 s to check out on a loaded build host — and a daemon that takes over a project's
// store provisions a workspace for every claim it relaunches, hundreds at once, which one at a time
// takes hours. The checkouts write only their own directories and run side by side
// (workspace.Request.StoreLock); sixteen keep a host's disk busy without a few hundred writers
// contending for it. Only the checkout takes a place: the rest of a provision is ordered by its
// repository's store lock, and a provision that waited on that lock while it held a place would
// keep every other repository's checkouts waiting behind it.
const provisionConcurrency = 16

// provisionLocks orders the outbox's workspace provisions and removals. At most
// provisionConcurrency checkouts run at once (slot). Provisions and removals of one issue take
// turns (issues, by workspace directory): two launches that both found a workspace missing or not
// checked out provision it once, and a removal never runs under its issue's provision. Each
// repository's shared clone has one store lock (stores, by clone directory), which
// workspace.Provision holds for every command on the clone but the checkout, and workspace.Remove
// for every step but the delete of the workspace's files. A provision takes its issue's turn, then
// its store lock, and lets the lock go before it waits for a checkout place; a removal takes its
// issue's turn, then its store lock. Nothing that holds a place waits on a store lock or a turn,
// and nothing that holds a store lock waits on a turn or a place, so no two wait on each other.
// The zero value is ready to use.
type provisionLocks struct {
	mu     sync.Mutex
	slots  chan struct{}
	issues keyedLocks
	stores keyedLocks
}

// slot waits for one of the provisionConcurrency checkout places, or for ctx to end, and returns
// the release of the place it took: a workspace.Request's CheckoutSlot.
func (p *provisionLocks) slot(ctx context.Context) (func(), error) {
	p.mu.Lock()
	if p.slots == nil {
		p.slots = make(chan struct{}, provisionConcurrency)
	}
	slots := p.slots
	p.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keyedLocks is one mutex per key, kept only while someone holds or awaits it. The zero value is
// ready to use.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

// keyedLock is one key's mutex and the number of its current users.
type keyedLock struct {
	sync.Mutex
	users int
}

// take is key's mutex, not locked, and the release that ends this use of it: once a key's last
// user releases it, the key's entry goes.
func (k *keyedLocks) take(key string) (*keyedLock, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	lock := k.locks[key]
	if lock == nil {
		lock = &keyedLock{}
		k.locks[key] = lock
	}
	lock.users++
	return lock, func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if lock.users--; lock.users == 0 {
			delete(k.locks, key)
		}
	}
}
