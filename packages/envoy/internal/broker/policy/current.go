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
// RefreshOne reread within listLag of the moment this reload began - before its listing was
// fetched - is read again alone and kept as that read finds it, since the full listing may not
// show the change yet; any failed read fails the refresh.
func (c *Current) Refresh(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// The window is measured from before the listing is fetched: a listing read inside it may lag
	// a reread however long Load takes to return.
	now := c.now()
	set, err := c.loader.Load(ctx)
	if err != nil {
		return err
	}
	if len(c.recent) == 0 {
		c.set.Store(set)
		return nil
	}
	live := c.set.Load().Secrets
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
		// A change this read catches that no reread did - a console delete, a tag that now refuses
		// the secret - is as new to the listing as a reread's answer, so it is kept for listLag from
		// now; the live set, not this listing, is what it changed.
		if was, served := live[name]; served != lk.Served || was != lk.Secret {
			c.recent[name] = c.now()
		}
	}
	c.set.Store(NewSet(set.Secrets))
	return nil
}

// RefreshOne rereads name alone (Loader.LoadOne) and merges the answer into the live policy: a
// served secret replaces or adds name, and a refused or absent one takes name out. Every other
// name stays as the live policy had it. A failed read leaves the live policy as it was. Refresh
// keeps the answer for listLag, except an absent answer for a name the live policy did not serve:
// that changed nothing, and anyone may ask for a reread, so keeping it would let invented names
// each cost every reload in the next listLag a DescribeSecret under mu. A served name found absent
// is kept, so a lagging listing cannot bring back a deleted secret; a secret created and deleted
// before any read served it can still show from a lagging listing until listLag has passed.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	if err := c.CheckName(name); err != nil {
		return Lookup{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lk, err := c.loader.LoadOne(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	live := c.set.Load().Secrets
	if _, served := live[name]; !served && lk.Reason == ReasonAbsent {
		return lk, nil
	}
	secrets := maps.Clone(live)
	merge(secrets, name, lk)
	c.set.Store(NewSet(secrets))
	c.recent[name] = c.now()
	return lk, nil
}

// CheckName answers ErrNameInvalid for a name no secret under the prefix can carry (free text, or
// one whose Secrets Manager name would pass its length limit) and nil for any other, by the rule
// LoadOne applies, so a caller refuses such a name before it waits on mu or spends a reread.
func (c *Current) CheckName(name string) error {
	_, _, err := c.loader.secretName(name)
	return err
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
