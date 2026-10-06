package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ControllerStartMessage is the controller's first prompt at every start: `legion controller start`
// passes it to the operator's Oh My Pi at launch, and the daemon delivers it to its own controller
// (`controller: daemon`) each time that controller reports ready, so every start and restart runs
// the skill's start procedure with nothing typed, where the plugin alone would leave the session
// idle until a wake.
const ControllerStartMessage = "Legion controller start: follow skill://legion-controller's start procedure now (\"What happened before you started\"), then end the turn."

// The waits before a failed or retired controller is retried: the first (plan.controllerRetry,
// controllerRetryFirst unless a test replaces it), then doubled at each retry that fails again, up
// to the last. A controller that reaches ready resets them.
const (
	controllerRetryFirst = time.Minute
	controllerRetryMax   = 30 * time.Minute
)

// controllerKeeper keeps the daemon's own controller running (`controller: daemon`), in place of
// watchController's line about the operator's. The controller is one claim, on the controller role
// with no issue, which its machine supervises as any claim's: a death relaunches the same session,
// within the launch failure budget. What the machine leaves to its caller is the keeper's: the claim
// is created when the store holds none, launched while queued, resumed when suspended, and, when
// its budget ran out (failed) or its agent ended it (retired), retried with fresh budgets after a
// wait that doubles while it keeps failing. At each ready it hands the controller the start message.
type controllerKeeper struct {
	supervisor *supervisor
	// project is the project token the claim is filed under.
	project string
	// ctx bounds each decision the keeper hands a machine: supervision's lifetime, as a machine's
	// own work is bounded.
	ctx context.Context
	log *slog.Logger
	now func() time.Time
	// retryFirst is the first wait before a failed or retired controller is retried.
	retryFirst time.Duration
	// retryAt is when a failed or retired controller is next retried, zero while none is waiting,
	// and backoff the wait that set it.
	retryAt time.Time
	backoff time.Duration
}

func newControllerKeeper(ctx context.Context, sup *supervisor, project string, retryFirst time.Duration, log *slog.Logger) *controllerKeeper {
	return &controllerKeeper{supervisor: sup, project: project, ctx: ctx, log: log, now: time.Now, retryFirst: retryFirst}
}

func (k *controllerKeeper) token() claim.Token { return claim.ControllerToken(k.project) }

// run keeps the controller at once and then every interval until ctx is done.
func (k *controllerKeeper) run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := k.keep(); err != nil && ctx.Err() == nil {
			k.log.Error("controller: keep the daemon's controller running", "claim", k.token(), "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// keep is one look at the controller's claim, and the one request its state calls for.
func (k *controllerKeeper) keep() error {
	token := k.token()
	m, ok := k.supervisor.Machine(token)
	if !ok {
		created, _, err := k.supervisor.Create(k.ctx, supervise.Claim{
			Token: token, Project: k.project, Role: claim.RoleController, State: supervise.StateQueued,
		}, "")
		if err != nil {
			return fmt.Errorf("create the controller's claim: %w", err)
		}
		k.log.Info("controller: the daemon launches this project's controller", "claim", token)
		m = created
	}
	var request supervise.Event
	switch state := m.Claim().State; state {
	case supervise.StateQueued:
		request = supervise.RequestSpawn{Claim: token}
	case supervise.StateSuspended:
		request = supervise.RequestResume{Claim: token}
	case supervise.StateFailed, supervise.StateRetired:
		now := k.now()
		if k.retryAt.IsZero() {
			k.backoff = k.retryFirst
			k.retryAt = now.Add(k.backoff)
			k.log.Error("controller: the daemon's controller stopped; retrying it with fresh budgets", "claim", token,
				"state", state, "retryAt", k.retryAt)
			return nil
		}
		if now.Before(k.retryAt) {
			return nil
		}
		k.backoff = min(2*k.backoff, controllerRetryMax)
		k.retryAt = now.Add(k.backoff)
		k.log.Warn("controller: retrying the daemon's controller", "claim", token, "state", state, "nextRetryAt", k.retryAt)
		request = supervise.RequestRetry{Claim: token}
	case supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		k.retryAt, k.backoff = time.Time{}, 0
		return nil
	default:
		// Launching, connected, registered, or a launch the previous daemon left uncertain: the
		// machine and boot's reconciliation own those.
		return nil
	}
	if err := m.Handle(k.ctx, request); err != nil {
		return fmt.Errorf("%T for the controller's claim: %w", request, err)
	}
	return nil
}

// ready hands the controller the start message once its agent says it is ready, at every launch, so
// each one runs the skill's start procedure before any wake. A start message still pending from an
// earlier launch — its turn interrupted by the death this launch replaced — is sent on this ready
// instead, so none is queued behind it.
func (k *controllerKeeper) ready(c supervise.Claim) {
	m, ok := k.supervisor.Machine(c.Token)
	if !ok {
		return
	}
	err := m.Handle(k.ctx, supervise.RequestDeliver{Claim: c.Token, Task: ControllerStartMessage})
	switch {
	case err == nil:
	case errors.Is(err, supervise.ErrDeliveryPending):
		k.log.Info("controller: the start message waits behind the controller's pending delivery", "claim", c.Token)
	default:
		k.log.Error("controller: hand the daemon's controller its start message", "claim", c.Token, "error", err)
	}
}

// claimReadyHook is the ready route's hook: the daemon's controller's ready goes to its keeper, and
// every other claim's to the workflow's (nil for a daemon with no workflow).
func claimReadyHook(keeper *controllerKeeper, workflow func(supervise.Claim)) func(supervise.Claim) {
	if keeper == nil {
		return workflow
	}
	return func(c supervise.Claim) {
		if c.Role == claim.RoleController {
			keeper.ready(c)
			return
		}
		if workflow != nil {
			workflow(c)
		}
	}
}
