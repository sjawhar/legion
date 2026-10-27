package ghnotify

import "github.com/sjawhar/envoy/internal/metrics"

// The polls_total results.
const (
	resultOK           = "ok"
	resultNotModified  = "not_modified"
	resultError        = "error"
	resultUnauthorized = "unauthorized"
	resultRateLimited  = "rate_limited"
	resultRateFloor    = "rate_floor"
)

// The threads_dropped_total reasons.
const (
	reasonSkippedOwner = "skipped_owner"
	reasonUnmapped     = "unmapped"
	reasonUnreadable   = "unreadable"
)

// pollerMetrics are the poller's series on the listener's registry, so /metrics serves them beside
// the listener's own.
type pollerMetrics struct {
	polls         *metrics.Counter
	events        *metrics.Counter
	dropped       *metrics.Counter
	rateRemaining *metrics.Gauge
	pollInterval  *metrics.Gauge
	lastSuccess   *metrics.Gauge
}

func newPollerMetrics(reg *metrics.Registry) *pollerMetrics {
	return &pollerMetrics{
		polls: reg.NewCounter("envoy_github_notifications_polls_total",
			"GitHub notification polls, by result."),
		events: reg.NewCounter("envoy_github_notifications_events_total",
			"Envelopes the GitHub notifications poller published that the stream stored, by webhook event."),
		dropped: reg.NewCounter("envoy_github_notifications_threads_dropped_total",
			"GitHub notification threads the poller published nothing for, by reason."),
		rateRemaining: reg.NewGauge("envoy_github_notifications_rate_remaining",
			"X-RateLimit-Remaining of GitHub's latest answer to the notifications poller."),
		pollInterval: reg.NewGauge("envoy_github_notifications_poll_interval_seconds",
			"Seconds until the GitHub notifications poller's next poll."),
		lastSuccess: reg.NewGauge("envoy_github_notifications_last_success_timestamp_seconds",
			"Unix time of the GitHub notifications poller's last successful poll."),
	}
}
