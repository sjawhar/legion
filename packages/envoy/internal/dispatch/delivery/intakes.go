package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
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
// started. Without the GitHub App neither intake reads anything, so neither connects. broker is the
// server's event broker, through which Intake appends issue.deployed (RecordDeployedEvents).
//
// Both intakes route by one SettingsCache, which reads delivery_settings at start and again each
// time settingsWake is signalled -- in the server, the shared listener's wake on SettingsChannel --
// so no message and no timer reads the row. A nil settingsWake reads it once.
//
// The returned stop, called once ctx is cancelled, waits for both intakes' Runs and the cache's,
// then closes both clients. Each Run returns only once it has stopped delivering and released its
// hold (heldDurable.stop): a message in its handler then finishes and is acknowledged before stop
// closes the client, the messages behind it are handed back to NATS, and the next process takes
// the durable over at once rather than at the hold's lapse. Each Run's stop has one deadline,
// intakeStopBudget after ctx ends (stopContext), which a take-over under way at that moment
// spends rather than extends: the handlers still running then are given up and their messages
// handed back too, and the Runs return promptly after it. So intakeStopBudget from ctx's end
// bounds how long a shutdown spends on the intakes, and stop adds no deadline of its own. Every
// handler of Intake's has returned by the time stop does, so a caller that closes the pool after
// stop returns, as cmd/dispatch's shutdown does, never closes it under one's writes. A second
// call returns once the first has.
//
// The intakes cannot share a client: a bus.Client holds one JetStream subscription, and refuses the
// second intake's subscribe with bus.ErrSubscriptionHeld.
//
// Each intake delivers from its durable only while it holds that durable's hold
// (intakeHoldName), and changes the durable only under that durable's own consumer lease
// (consumerLease), both taken and the hold renewed on the pool's lease connections, so that of
// every envoy-dispatch process one delivers from each durable at a time -- and so that neither
// intake's take-over can block the other's, which one lease over both durables did (the
// measurement is on consumerLease). A pool that cannot open those connections is returned as the
// error, with neither intake started: an intake that could take neither would deliver nothing.
func StartIntakes(ctx context.Context, connect func(name string) (*bus.Client, error), pool *store.Pool, github *githubapp.Client, broker *events.Broker, settingsWake <-chan struct{}) (stop func(), err error) {
	if github == nil {
		slog.Info("dispatch delivery: no GitHub App key — the intakes are idle")
		return func() {}, nil
	}
	leases, err := pool.Leases()
	if err != nil {
		return nil, fmt.Errorf("open the lease pool for the intakes: %w", err)
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
	settings := newSettingsCache(pool, settingsWake)
	intake := newIntake(intakeClient, pool, leases, github, broker, settings)
	var running sync.WaitGroup
	running.Go(func() { settings.Run(ctx) })
	running.Go(func() { intake.Run(ctx) })
	running.Go(func() { newJobIntake(jobClient, pool, leases, github, realJobClock{}, settings).Run(ctx) })
	var stopped sync.Once
	return func() {
		stopped.Do(func() {
			running.Wait()
			intakeClient.Close()
			jobClient.Close()
		})
	}, nil
}
