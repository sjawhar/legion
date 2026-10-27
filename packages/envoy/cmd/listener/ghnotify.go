package main

// The listener's half of the GitHub notifications poller (internal/ghnotify): starting it when its
// token is set, stopping it with the listener, and reporting its health on /healthz. The poller
// itself knows nothing of the listener; it is handed a publisher, a logger and the metrics
// registry, and it answers Health().

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/ghnotify"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

const (
	// notificationsPollerStartedLine is logged once, when the poller starts. A listener without
	// ENVOY_GITHUB_NOTIFICATIONS_TOKEN never logs it and sends GitHub nothing.
	notificationsPollerStartedLine = "github notifications poller started"
	// notificationsPollerRefusedLine is logged once, at ERROR, when the token is set but the
	// configuration cannot start a poller -- a lookback past 48 hours, which could republish
	// events older than the stream's 72-hour duplicate window. The listener serves webhooks as
	// before.
	notificationsPollerRefusedLine = "github notifications poller not started"
	// notificationsLookbackVar is named in that line so the value an operator must correct is in
	// the record, whatever the package's own error says.
	notificationsLookbackVar = "ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK"
)

// startNotificationsPoller starts the GitHub notifications poller when
// ENVOY_GITHUB_NOTIFICATIONS_TOKEN holds a value, and returns it with the function that stops it.
// It answers (nil, a no-op stop) for a listener with no token, which is every listener but the one
// its deployment gives the token to: no goroutine, no GitHub request, and no github_notifications
// field on /healthz.
//
// The poller publishes through publisher, which is the listener's own bus client, so it reaches
// the stream the webhook route publishes to and no other. It registers its metrics on reg, the
// registry /metrics serves.
//
// Stopping cancels the poller's context and does not wait for Run to return: the listener's
// shutdown budget is already spent by the HTTP drain, the live topics lane and the NATS drain
// (shutdownListener), and a poll still in flight is covered by that NATS drain, which waits for
// what is in flight and fails a publish issued after it.
func startNotificationsPoller(publisher ghnotify.Publisher, reg *metrics.Registry, logger *logging.Logger, getenv func(string) string) (*ghnotify.Poller, func()) {
	noPoller := func() {}
	poller, cfg, err := buildNotificationsPoller(publisher, reg, logger, getenv)
	switch {
	case err != nil:
		// One refusal for both halves of the build: ErrLookbackTooLong comes back from either
		// LoadConfig or New, and an operator reads the same line whichever refused.
		logger.Error(notificationsPollerRefusedLine,
			slog.String(notificationsLookbackVar, getenv(notificationsLookbackVar)),
			slog.String("error", err.Error()))
		return nil, noPoller
	case poller == nil:
		return nil, noPoller
	}
	ctx, cancel := context.WithCancel(context.Background())
	go poller.Run(ctx)
	logger.Info(notificationsPollerStartedLine, slog.Int("skip_owners", len(cfg.SkipOwners)))
	return poller, cancel
}

// buildNotificationsPoller reads the configuration and builds the poller. It answers a nil poller
// for a listener that holds no token, which runs none.
func buildNotificationsPoller(publisher ghnotify.Publisher, reg *metrics.Registry, logger *logging.Logger, getenv func(string) string) (*ghnotify.Poller, ghnotify.Config, error) {
	cfg, enabled, err := ghnotify.LoadConfig(getenv)
	if err != nil || !enabled {
		return nil, cfg, err
	}
	poller, err := ghnotify.New(cfg, publisher, logger, reg)
	if err != nil {
		return nil, cfg, err
	}
	return poller, cfg, nil
}

// githubNotificationsHealth is one poller's health as /healthz reports it: the time of its last
// successful poll (absent until it has had one), how many threads it could not read in the last 24
// hours, and "degraded" once it has gone ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER without a
// successful poll. It is a report and nothing else: a degraded poller leaves the listener's own
// status healthy and never counts toward its self-health termination, since every webhook, every
// delivery and every /v1 call is served as before.
func githubNotificationsHealth(health ghnotify.Health) map[string]interface{} {
	status := "healthy"
	if health.Degraded {
		status = "degraded"
	}
	field := map[string]interface{}{"status": status, "unreadable_24h": health.Unreadable24h}
	if !health.LastSuccess.IsZero() {
		field["last_success"] = health.LastSuccess.UTC().Format(time.RFC3339)
	}
	return field
}
