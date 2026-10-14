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

// StartIntakes runs Intake and JobIntake until ctx is cancelled, each on a bus client of its own
// that connect opens, and closes each client once its intake has stopped. The returned channel
// closes once both have. It connects both before it returns, and returns the error of a connect
// that fails with neither intake started. Without the GitHub App neither intake reads anything, so
// neither connects.
//
// The intakes cannot share a client: a bus.Client keeps one JetStream subscription, and the second
// intake's bind would unsubscribe the first's (bus.Client.Subscribe). Nothing binds that durable
// again, so it would stay on the server unbound, its messages pending, until the process restarts.
func StartIntakes(ctx context.Context, connect func() (*bus.Client, error), pool *store.Pool, github *githubapp.Client) (<-chan struct{}, error) {
	stopped := make(chan struct{})
	if github == nil {
		slog.Info("dispatch delivery: no GitHub App key — the intakes are idle")
		close(stopped)
		return stopped, nil
	}
	intakeClient, err := connect()
	if err != nil {
		return nil, fmt.Errorf("connect the intake: %w", err)
	}
	jobClient, err := connect()
	if err != nil {
		intakeClient.Close()
		return nil, fmt.Errorf("connect the job intake: %w", err)
	}
	var running sync.WaitGroup
	running.Go(func() {
		defer intakeClient.Close()
		NewIntake(intakeClient, pool, github).Run(ctx)
	})
	running.Go(func() {
		defer jobClient.Close()
		NewJobIntake(jobClient, pool, github).Run(ctx)
	})
	go func() {
		running.Wait()
		close(stopped)
	}()
	return stopped, nil
}
