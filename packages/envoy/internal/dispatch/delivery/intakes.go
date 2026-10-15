package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The names StartIntakes asks connect for, one per intake, so the NATS server's connection
// listings tell each intake's connection from Dispatch's publisher and from every listener.
const (
	intakeConnectionName    = "dispatch-intake"
	jobIntakeConnectionName = "dispatch-job-intake"
)

// StartIntakes runs Intake and JobIntake until ctx is cancelled, each on a bus client of its own
// that connect opens under the name it is given (dispatch-intake, dispatch-job-intake). It connects
// both before it returns, and returns the error of a connect that fails with neither intake
// started. Without the GitHub App neither intake reads anything, so neither connects.
//
// The returned stop waits for both intakes' Runs, which return once ctx is cancelled, then closes
// both clients. It does not wait for a message still in an intake's handler: deliver runs on the
// subscription's delivery goroutine under a deadline of its own and acknowledges the message when
// it ends, which can be after Run has returned. cmd/dispatch defers stop beside its publisher's
// Close, so the clients stay open until the publisher closes, and such a message can be
// acknowledged while Dispatch shuts down.
//
// The intakes cannot share a client: a bus.Client holds one JetStream subscription, and refuses the
// second intake's bind with bus.ErrSubscriptionHeld.
func StartIntakes(ctx context.Context, connect func(name string) (*bus.Client, error), pool *store.Pool, github *githubapp.Client) (stop func(), err error) {
	if github == nil {
		slog.Info("dispatch delivery: no GitHub App key — the intakes are idle")
		return func() {}, nil
	}
	intakeClient, err := connect(intakeConnectionName)
	if err != nil {
		return nil, fmt.Errorf("connect the intake: %w", err)
	}
	jobClient, err := connect(jobIntakeConnectionName)
	if err != nil {
		intakeClient.Close()
		return nil, fmt.Errorf("connect the job intake: %w", err)
	}
	var running sync.WaitGroup
	running.Go(func() { newIntake(intakeClient, pool, github).Run(ctx) })
	running.Go(func() { newJobIntake(jobClient, pool, github, realJobClock{}).Run(ctx) })
	return sync.OnceFunc(func() {
		running.Wait()
		intakeClient.Close()
		jobClient.Close()
	}), nil
}
