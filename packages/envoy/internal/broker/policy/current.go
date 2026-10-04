package policy

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"
)

// listLag is how stale ListSecrets may be: AWS documents "might not reflect changes from the last
// five minutes" (ListSecrets API reference). Refresh re-describes every name RefreshOne touched
// within it, so a lagging listing never overwrites what a single-name read settled.
const listLag = 5 * time.Minute

// Current is the live policy. The first load must succeed; a later load that fails logs
// LoadFailedMessage and keeps the previous set, so a Secrets Manager outage never turns into
// deny-everything. A secret refused for its own tags leaves only that secret out.
type Current struct {
	loader Loader
	set    atomic.Pointer[Set]
	// mu is held by every writer, Refresh and RefreshOne, from its read of Secrets Manager to its
	// Store, so neither stores a set built on one the other has since replaced. It guards recent.
	mu sync.Mutex
	// recent is when RefreshOne last reread each name, until listLag has passed.
	recent map[string]time.Time
	now    func() time.Time
}

// NewCurrent loads the policy once, failing when that load fails, and then reloads it every
// interval until ctx ends.
func NewCurrent(ctx context.Context, loader Loader, every time.Duration) (*Current, error) {
	c := &Current{loader: loader, recent: map[string]time.Time{}, now: time.Now}
	if err := c.Refresh(ctx); err != nil {
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Refresh(ctx); err != nil {
					slog.Error(LoadFailedMessage, "error", err)
				}
			}
		}
	}()
	return c, nil
}

// Refresh loads the policy now and, when the load succeeds, makes it the live one. A name
// RefreshOne reread within listLag is read again alone and kept as that read finds it, since the
// full listing may not show the change yet; any failed read fails the refresh.
func (c *Current) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	set, err := c.loader.Load(ctx)
	if err != nil {
		return err
	}
	if len(c.recent) == 0 {
		c.set.Store(set)
		return nil
	}
	now := c.now()
	for name, at := range c.recent {
		if now.Sub(at) > listLag {
			delete(c.recent, name)
			continue
		}
		lk, err := c.loader.LoadOne(ctx, name)
		if err != nil {
			return err
		}
		merge(set.Secrets, name, lk)
	}
	c.set.Store(NewSet(set.Secrets))
	return nil
}

// RefreshOne rereads name alone (Loader.LoadOne) and merges the answer into the live policy: a
// served secret replaces or adds name, and a refused or absent one takes name out. Every other
// name stays as the live policy had it. A failed read leaves the live policy as it was. Refresh
// keeps the answer for listLag.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	lk, err := c.loader.LoadOne(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	secrets := maps.Clone(c.set.Load().Secrets)
	merge(secrets, name, lk)
	c.set.Store(NewSet(secrets))
	c.recent[name] = c.now()
	return lk, nil
}

// merge sets name in secrets as lk found it: the served secret, or no entry.
func merge(secrets map[string]Secret, name string, lk Lookup) {
	if lk.Served {
		secrets[name] = lk.Secret
	} else {
		delete(secrets, name)
	}
}

// Get answers the live policy.
func (c *Current) Get() *Set { return c.set.Load() }
