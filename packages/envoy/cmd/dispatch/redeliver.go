package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
)

// redeliveryPace spaces redelivery requests a second apart, as GitHub's REST best practices ask of
// a large number of POSTs: 60 a minute, 300 of the 900 points a minute GitHub's secondary limit
// allows an endpoint.
const redeliveryPace = time.Second

// webhookSweeper builds the redelivery sweep for the App whose key Dispatch holds. It returns nil
// when there is no key to sign with (the endpoints take only an App JWT) or no NATS for its
// state.
func webhookSweeper(natsClient *bus.Client, app *auth.AppConfig, apiBase string) (*redeliver.Sweeper, error) {
	if natsClient == nil {
		return nil, nil
	}
	github, err := githubapp.New(app, apiBase)
	if err != nil || github == nil {
		return nil, err
	}
	state, err := redeliver.OpenState(natsClient.JS())
	if err != nil {
		return nil, err
	}
	return &redeliver.Sweeper{GitHub: github, State: state, Logger: slog.Default(), Pace: redeliveryPace}, nil
}

// redeliverWebhooks is the operator command over the same sweep: it lists the App webhook's
// failed deliveries since --since and, unless --dry-run, redelivers them under the running
// sweep's rules and records. It leaves the running sweep's cursor alone.
func redeliverWebhooks(ctx context.Context, args []string, out io.Writer) int {
	flags := flag.NewFlagSet("redeliver-webhooks", flag.ContinueOnError)
	flags.SetOutput(out)
	since := flags.Duration("since", 0, "list failed deliveries this far back (GitHub keeps 72h)")
	dryRun := flags.Bool("dry-run", false, "decide every delivery without requesting a redelivery or writing state")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *since <= 0 {
		fmt.Fprintln(out, "redeliver-webhooks: --since is required, for example --since 72h")
		return 2
	}
	envoyConfig, err := config.Load(config.LoadOptions{})
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: load envoy config: %v\n", err)
		return 1
	}
	natsClient, err := bus.Connect(envoyConfig.NatsURLs)
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: connect NATS: %v\n", err)
		return 1
	}
	defer natsClient.Close()
	dataDir, err := defaultDataDir()
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: resolve data dir: %v\n", err)
		return 1
	}
	app, _, err := loadAppCredentials(dataDir)
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: load app credentials: %v\n", err)
		return 1
	}
	sweeper, err := webhookSweeper(natsClient, app, strings.TrimSpace(os.Getenv("DISPATCH_GITHUB_API_BASE")))
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: %v\n", err)
		return 1
	}
	if sweeper == nil {
		fmt.Fprintln(out, "redeliver-webhooks: no GitHub App private key (DISPATCH_APP_PEM_B64) to sign with")
		return 1
	}
	sweeper.Logger = slog.New(slog.NewTextHandler(out, nil))
	report, err := sweeper.Sweep(ctx, redeliver.Options{Since: time.Now().Add(-*since), DryRun: *dryRun})
	writeRedeliveryReport(out, report)
	if err != nil {
		fmt.Fprintf(out, "redeliver-webhooks: %v\n", err)
		return 1
	}
	if !report.RateLimitedUntil.IsZero() {
		fmt.Fprintf(out, "redeliver-webhooks: GitHub rate-limited the App; no sweep asks it anything before %s\n", report.RateLimitedUntil.Format(time.RFC3339))
		return 1
	}
	return 0
}

func writeRedeliveryReport(out io.Writer, report redeliver.Report) {
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "DELIVERED_AT\tGUID\tDELIVERY_ID\tEVENT\tREPOSITORY_ID\tSTATUS\tATTEMPTS\tOUTCOME")
	for _, decision := range report.Decisions {
		event := decision.Event
		if decision.Action != "" {
			event += "." + decision.Action
		}
		fmt.Fprintf(table, "%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\n",
			decision.DeliveredAt.Format(time.RFC3339), decision.GUID, decision.DeliveryID, event,
			decision.RepositoryID, decision.StatusCode, decision.Attempts, decision.Outcome)
	}
	_ = table.Flush()
	fmt.Fprintf(out, "redeliver-webhooks: since=%s failed_attempts=%d deliveries=%d", report.Since.Format(time.RFC3339), report.Listed, len(report.Decisions))
	for _, outcome := range []redeliver.Outcome{
		redeliver.Redelivered, redeliver.WouldRedeliver, redeliver.RequestRefused, redeliver.RateLimited,
		redeliver.Waiting, redeliver.Pending, redeliver.Delivered, redeliver.Terminal, redeliver.Exhausted,
		redeliver.Closed, redeliver.ClaimedElsewhere,
	} {
		if n := report.Count(outcome); n > 0 {
			fmt.Fprintf(out, " %s=%d", outcome, n)
		}
	}
	if !report.Complete {
		fmt.Fprint(out, " complete=false")
	}
	if !report.RateLimitedUntil.IsZero() {
		fmt.Fprintf(out, " rate_limited_until=%s", report.RateLimitedUntil.Format(time.RFC3339))
	}
	fmt.Fprintln(out)
}
