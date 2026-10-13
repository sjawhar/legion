package daemon

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// gateLaunch wraps store so its CheckLaunch refuses first when check does, then asks the real
// store, when check is non-nil: a test's way to refuse a launch the way a reserved tree cleanup
// or a transient store error would (checkLaunch, internal/daemon/plugin_test.go), without a
// lifecycle this pod has none of to reserve. A nil check wraps store with nothing, so a
// production daemon's every launch reaches its real store's own CheckLaunch untouched.
func gateLaunch(store supervise.Store, check func(context.Context, supervise.Claim) error) supervise.Store {
	if check == nil {
		return store
	}
	return launchGatedStore{Store: store, check: check}
}

type launchGatedStore struct {
	supervise.Store
	check func(context.Context, supervise.Claim) error
}

func (s launchGatedStore) CheckLaunch(ctx context.Context, c supervise.Claim) error {
	if err := s.check(ctx, c); err != nil {
		return err
	}
	return s.Store.CheckLaunch(ctx, c)
}
