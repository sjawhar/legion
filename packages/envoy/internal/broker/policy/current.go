package policy

import (
	"context"
	"log/slog"
	"maps"
	"sync/atomic"
	"time"
)

// Current is the live policy. The first load must succeed; a later load that fails logs
// LoadFailedMessage and keeps the previous set, so a Secrets Manager outage never turns into
// deny-everything. A secret refused for its own tags leaves only that secret out. Refresh
// re-describes each name it holds an anchor for (recent) for as long as the store's metadata may
// lag a write (Store.ListLag), so a lagging listing neither undoes what a single-name read settled
// nor undoes a change that reload's own re-describe caught.
type Current struct {
	loader Loader
	set    atomic.Pointer[Set]
	// mu is the writer lock, held by every writer, Refresh and RefreshOne, from its read of Secrets
	// Manager to its Store, so neither stores a set built on one the other has since replaced. It
	// guards recent. It is a one-slot channel so a writer waiting for it gives up when its context
	// ends (lock).
	mu chan struct{}
	// recent is, for each name a reread or a reload's own re-describe last changed the live answer
	// for, when it did: RefreshOne's reread and Refresh's own re-describe each record one only when
	// the answer differed from what the live set held, so a reread that confirms the live answer
	// leaves any earlier anchor in place and adds none. Each anchor lives until the store's ListLag
	// has passed.
	recent map[string]time.Time
	now    func() time.Time
}

// NewCurrent loads the policy once, failing when that load fails, and then reloads it every
// interval until ctx ends, each reload given that interval before it fails, so one stuck on a
// Secrets Manager that does not answer holds the writer lock no longer. A reload that fails while
// ctx is live logs LoadFailedMessage; one that fails once ctx has ended, the broker shutting down,
// is no failed load and logs nothing, since the deployment's alarm counts that line.
func NewCurrent(ctx context.Context, loader Loader, every time.Duration) (*Current, error) {
	c := &Current{loader: loader, mu: make(chan struct{}, 1), recent: map[string]time.Time{}, now: time.Now}
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
				reload, cancel := context.WithTimeout(ctx, every)
				err := c.Refresh(reload)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Error(LoadFailedMessage, "error", err)
				}
			}
		}
	}()
	return c, nil
}

// lock takes the writer lock, or answers ctx's error, holding nothing, once ctx has ended first.
func (c *Current) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock releases the writer lock lock took.
func (c *Current) unlock() { <-c.mu }

// Refresh loads the policy now and, when the load succeeds, makes it the live one. A name recent
// anchors within the store's ListLag of the moment this reload began - before its listing was fetched - is
// read again alone and kept as that read finds it, since the full listing may not show the change
// yet. recent anchors a name a reread settled, and one an earlier reload's own re-describe caught
// a change to. Any failed read fails the refresh.
func (c *Current) Refresh(ctx context.Context) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	// The window is measured from before the listing is fetched: a listing read inside it may lag
	// a reread however long Load takes to return.
	now := c.now()
	lag := c.loader.Store.ListLag()
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
		if now.Sub(at) > lag {
			delete(c.recent, name)
			continue
		}
		lk, err := c.loader.LoadOne(ctx, name)
		if err != nil {
			return err
		}
		merge(set.Secrets, name, lk)
		// A change this read catches that no reread did - a console delete, a tag that now refuses
		// the secret - is as new to the listing as a reread's answer, so it is kept for the store's
		// ListLag from now; the live set, not this listing, is what it changed.
		if was, served := live[name]; served != lk.Served || was != lk.Secret {
			c.recent[name] = c.now()
		}
	}
	c.set.Store(NewSet(set.Secrets))
	return nil
}

// RefreshOne rereads name alone (Loader.LoadOne) and merges the answer into the live policy: a
// served secret replaces or adds name, and a refused or absent one takes name out. Every other
// name stays as the live policy had it. A failed read leaves the live policy as it was, and a name
// no secret can carry is ErrNameInvalid before the writer lock (CheckName). Refresh keeps a reread
// for the store's ListLag only when it changed the live answer, as Refresh does for its own
// re-describes: a reread that confirms what the live policy already held settled nothing a lagging
// listing could undo, and anyone may ask for a reread, so anchoring it would let each one cost
// every reload in the next ListLag a describe under the writer lock (on Secret Manager, two
// metadata reads from a quota the whole project shares). A served name found absent is a change
// and is kept, so a lagging listing cannot bring back a deleted secret; a secret created and
// deleted before any read served it can still show from a lagging listing until ListLag has
// passed.
func (c *Current) RefreshOne(ctx context.Context, name string) (Lookup, error) {
	if err := c.CheckName(name); err != nil {
		return Lookup{}, err
	}
	if err := c.lock(ctx); err != nil {
		return Lookup{}, err
	}
	defer c.unlock()
	lk, err := c.loader.LoadOne(ctx, name)
	if err != nil {
		return Lookup{}, err
	}
	live := c.set.Load().Secrets
	if was, served := live[name]; served == lk.Served && was == lk.Secret {
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
