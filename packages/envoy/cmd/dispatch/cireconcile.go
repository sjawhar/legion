package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// reviewerAppID is DISPATCH_REVIEWER_APP_ID as a reconcile pass takes it: the text of the number
// GitHub's own check run objects carry under app.id, which the pass compares a verdict check's
// App against (contracts.ReviewerVerdictDecision). Unset it is empty, and the pass then records
// no `tester` or `architect` check run at all. Anything but a positive decimal number refuses
// startup naming the variable, as DISPATCH_APP_ID's own reader does (auth.LoadAppFromEnv).
func reviewerAppID(env settingValues) (string, error) {
	raw := strings.TrimSpace(env.get("DISPATCH_REVIEWER_APP_ID"))
	if raw == "" {
		return "", nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return "", fmt.Errorf("DISPATCH_REVIEWER_APP_ID=%q (expected the review App's numeric ID)", raw)
	}
	// The number as GitHub spells it, so a value written with a leading zero still matches the
	// app.id of the checks the review App posted.
	return strconv.FormatUint(id, 10), nil
}

// ciReconciler builds the CI reconcile pass for the App whose key Dispatch holds, writing through
// the listener's own CI record bucket. It returns nil when the pass cannot run: no NATS to write
// its records to, or no App private key to sign the reads with. The bucket is bound by the first
// pass that finds the client connected, not here, exactly as the webhook redelivery sweep's state
// is (webhookSweeper): the client connects in the background (serverBus), and a bus that is down
// at boot must not stop the boot; a pass before then reads GitHub nothing and says so. Its App
// client obeys the GitHub rate limits every process shares, in the database's github_rate_limits,
// since GitHub budgets the App rather than a process.
func ciReconciler(natsClient *bus.Client, database *store.Store, app *auth.AppConfig, apiBase, reviewer string) (*cireconcile.Reconciler, error) {
	if natsClient == nil {
		return nil, nil
	}
	limits := ratelimit.New(database.Pool)
	github, err := githubapp.New(app, apiBase, githubapp.WithLimits(limits))
	if err != nil || github == nil {
		return nil, err
	}
	// cistore's writer logs its KV calls through this logger; the pass's own lines go to the
	// server's default logger, as every other loop's do.
	logger := logging.New("ci-reconcile")
	open := func() (cireconcile.Writer, error) {
		if !natsClient.Connected() {
			return nil, errors.New("NATS is not connected, so the CI record writer cannot be bound yet")
		}
		writer, err := cistore.OpenWriter(natsClient.Conn, logger)
		if err != nil {
			return nil, err
		}
		return writer, nil
	}
	return &cireconcile.Reconciler{
		Pool:          database.Pool,
		GitHub:        github,
		Limits:        limits,
		Open:          open,
		ReviewerAppID: reviewer,
		Logger:        slog.Default(),
		Pace:          cireconcile.RequestPace,
	}, nil
}
