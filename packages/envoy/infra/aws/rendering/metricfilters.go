package rendering

import "fmt"

// The log metric filters both components register, and the alarms the listener pages with on its
// filters' metrics.

// MetricFilter is one log metric filter a component registers on its log group: the pattern of
// the log lines it counts and the metric it counts them into. `envoy-infra render metric-filters
// --json` prints a component's filters under these fields' JSON names.
type MetricFilter struct {
	LogicalName     string `json:"logicalName"`
	Pattern         string `json:"pattern"`
	MetricName      string `json:"metricName"`
	MetricNamespace string `json:"metricNamespace"`
}

// natsMetricFilters are the nats-server log lines a NatsService counts: each filter's name between
// the prefix and -filter, its pattern and the metric it counts into.
var natsMetricFilters = []struct{ name, pattern, metric string }{
	{"route-disconnect", `"Router connection closed"`, "RouteDisconnections"},
	{"slow-consumer", `"Slow Consumer Detected"`, "SlowConsumers"},
	{"jetstream-error", `?"JetStream out of" ?"JetStream failed to store" ?"JetStream being DISABLED" ?"NO quorum, stalled"`, "JetStreamErrors"},
	// nats-server's own log text for a refused credential and a denied subject.
	{"auth-error", `?"authentication error" ?"Subscription Violation" ?"Publish Violation"`, "AuthErrors"},
}

// NatsMetricFilters returns the log metric filters a NatsService whose
// metricFilterLogicalNamePrefix is prefix and whose metricNamespace is namespace registers, in the
// order it registers them: <prefix>-route-disconnect-filter, <prefix>-slow-consumer-filter,
// <prefix>-jetstream-error-filter and <prefix>-auth-error-filter.
func NatsMetricFilters(prefix, namespace string) []MetricFilter {
	filters := make([]MetricFilter, len(natsMetricFilters))
	for i, f := range natsMetricFilters {
		filters[i] = MetricFilter{
			LogicalName: prefix + "-" + f.name + "-filter", Pattern: f.pattern, MetricName: f.metric, MetricNamespace: namespace,
		}
	}
	return filters
}

// LogSignal is one kind of listener log line that pages: its filters count the line's shapes into
// one metric, and one alarm pages on that metric.
type LogSignal struct {
	alarm, metric string // the alarm is named after the component's name: <name>-<alarm>
	filters       []struct{ name, pattern string }
	description   string // the log group's name is written at %s
}

// ListenerLogSignals are the listener's log lines that page. Each alarm pages on one matching line
// in five minutes and delivers the OK transition too, so the channel sees the signal stop: the
// next period is 0 (lines arrived, none matched, the filters' default value) or missing (no lines,
// not breaching), and either clears it. A stopped listener is not these alarms' signal.
//
// The JSON lines are matched by field selectors and the plain lines are anchored on their date and
// time: a quoted or unanchored term would also match an INFO line whose payload quotes the phrase.
var ListenerLogSignals = []LogSignal{
	{
		// A JetStream publish that did not complete: the delivery path's slog JSON line
		// (cmd/listener/delivery.go) and a webhook handler's plain log line
		// (internal/webhook/webhook.go, deliveryFailed).
		alarm:  "publish-failure",
		metric: "PublishFailures",
		filters: []struct{ name, pattern string }{
			{"publish-failure-filter", `{ $.level = "ERROR" && $.msg = "listener exception publish failed" }`},
			{"webhook-publish-failure-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=failed*, ...]`},
		},
		description: "Envoy listener could not publish to JetStream (`listener exception publish failed` or " +
			"`<webhook> publish failed`, e.g. `nats: no response from stream`) — a delivery from the " +
			"Dispatch GitHub App (legion-envoy) waits for Dispatch's redelivery sweep, a Slack event is " +
			"retried by Slack three times with backoff and lost only if every attempt fails, any other " +
			"GitHub sender's delivery answered 503 is lost, and delivery-exception notices are lost; read " +
			"%s for the nats error.",
	},
	{
		// A delivery NATS can never take, answered 422 (internal/webhook/webhook.go's refusal
		// branch): one filter for each length of the action that logs it, matching `refused*` where
		// a failure says `failed`.
		alarm:  "webhook-refused",
		metric: "WebhookRefusals",
		filters: []struct{ name, pattern string }{
			{"webhook-publish-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=refused*, ...]`},
			{"ci-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=record, result=refused*, ...]`},
			{"ci-head-suite-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=head||w2=suite, w3=record, result=refused*, ...]`},
		},
		description: "Envoy listener refused a webhook delivery (`<source> publish refused`, `github ci record refused` " +
			"or `github ci suite record refused`, answered 422): the envelope is past what NATS takes or the " +
			"commit's CI record is past its bound, the same input is refused on every attempt, and Dispatch's " +
			"redelivery sweep does not retry a 4xx, so the delivery is lost (a refused CI record leaves its " +
			"commit overflowed, never settling). Read %s for the refusal's reason; the fix is " +
			"a cap in legion's normalizer or a CI bound, not the NATS server's limits.",
	},
	{
		// A start that dropped another deployment's overlapping stream subject: the WARN the bus
		// logs through Go's default handler (internal/bus/stream.go), so date, time and level
		// anchor it.
		alarm:  "stream-subject-dropped",
		metric: "StreamSubjectsDropped",
		filters: []struct{ name, pattern string }{
			{"stream-subject-dropped-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, level=WARN, w1=envoy, w2=nats, w3=stream, w4=subject, w5=replaced, w6=by, w7=an, w8=overlapping, w9=one, ...]`},
		},
		description: "An Envoy listener start dropped another deployment's `ENVOY_NOTIFICATIONS` subject " +
			"because it overlaps one of its own (`envoy nats stream subject replaced by an " +
			"overlapping one`) — that deployment receives nothing on the dropped subject until " +
			"it starts again; read %s for the dropped and kept subjects, then " +
			"reconcile the subject lists across the deployments.",
	},
	{
		// A start that refused its durable consumer and exits (cmd/listener/main.go).
		alarm:  "durable-refused",
		metric: "DurableRefusals",
		filters: []struct{ name, pattern string }{
			{"durable-refused-filter", `{ $.level = "ERROR" && $.msg = "subscribe refused, shutting down" }`},
		},
		description: "An Envoy listener start refused its durable consumer (`subscribe refused, shutting " +
			"down`): the durable carries an idle heartbeat or an ack policy other than explicit, " +
			"which NATS cannot change in place, so every replacement task exits 1 while the old task " +
			"keeps serving, until the circuit breaker rolls the deployment back and the apply fails. " +
			"Read %s for the " +
			"durable and the setting, then recreate the durable by the recipe in sjawhar/legion " +
			"packages/envoy/AGENTS.md (Operational notes), which keeps its cursor.",
	},
	{
		// A start that gave up binding its durable subscription and exits (cmd/listener/main.go,
		// after cmd/listener/durable.go's deadline).
		alarm:  "subscribe-exhausted",
		metric: "SubscribesExhausted",
		filters: []struct{ name, pattern string }{
			{"subscribe-exhausted-filter", `{ $.level = "ERROR" && $.msg = "subscribe failed after max attempts, shutting down" }`},
		},
		description: "An Envoy listener start gave up setting up its durable subscription 135 s after its first " +
			"attempt (`subscribe failed after max attempts, shutting down`). ECS had already stopped the " +
			"old task, so every replacement exits 1 the same way once its own bind gives up. Read the " +
			"line's `error` in %s: `consumer is already bound to a subscription` " +
			"means something still holds the durable's push binding (find it with `nats consumer " +
			"info ENVOY_NOTIFICATIONS listener-<machine id>`, push_bound, and stop it); any other " +
			"error is JetStream failing the consumer lookup, create or update.",
	},
}

// AlarmName is the signal's alarm's logical name in a WebhookListener named name.
func (s LogSignal) AlarmName(name string) string { return name + "-" + s.alarm }

// MetricName is the metric the signal's filters count into and its alarm pages on.
func (s LogSignal) MetricName() string { return s.metric }

// Description is the alarm's description for a listener logging to the log group logGroup.
func (s LogSignal) Description(logGroup string) string { return fmt.Sprintf(s.description, logGroup) }

// MetricFilters returns the signal's filters as a WebhookListener named name, whose
// metricNamespace is namespace, registers them: <name>-<filter's name>.
func (s LogSignal) MetricFilters(name, namespace string) []MetricFilter {
	filters := make([]MetricFilter, len(s.filters))
	for i, f := range s.filters {
		filters[i] = MetricFilter{LogicalName: name + "-" + f.name, Pattern: f.pattern, MetricName: s.metric, MetricNamespace: namespace}
	}
	return filters
}

// ListenerMetricFilters returns the log metric filters a WebhookListener named name, whose
// metricNamespace is namespace, registers, in the order it registers them.
func ListenerMetricFilters(name, namespace string) []MetricFilter {
	var filters []MetricFilter
	for _, s := range ListenerLogSignals {
		filters = append(filters, s.MetricFilters(name, namespace)...)
	}
	return filters
}
