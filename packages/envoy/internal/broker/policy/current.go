package policy

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Current is the live policy. The first load must succeed; a later load that fails logs
// LoadFailedMessage and keeps the previous set, so a Secrets Manager outage never turns into
// deny-everything. A secret refused for its own tags leaves only that secret out.
type Current struct {
	loader Loader
	set    atomic.Pointer[Set]
}

// NewCurrent loads the policy once, failing when that load fails, and then reloads it every
// interval until ctx ends.
func NewCurrent(ctx context.Context, loader Loader, every time.Duration) (*Current, error) {
	c := &Current{loader: loader}
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

// Refresh loads the policy now and, when the load succeeds, makes it the live one.
func (c *Current) Refresh(ctx context.Context) error {
	set, err := c.loader.Load(ctx)
	if err != nil {
		return err
	}
	c.set.Store(set)
	return nil
}

// Get answers the live policy.
func (c *Current) Get() *Set { return c.set.Load() }
