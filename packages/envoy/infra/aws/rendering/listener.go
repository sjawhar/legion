package rendering

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/infra/core"
)

const (
	// ListenerPort is the port the listener serves in the task: its webhooks, /v1 and /healthz,
	// and the port the target group sends traffic to.
	ListenerPort = 9020
	// ListenerContainerName names the task's one container, which the service attaches to the
	// target group and whose log streams it prefixes.
	ListenerContainerName = "envoy-listener"
)

// supportedWebhooks are the webhooks lists this version takes, the lists whose container
// definitions the reference renders byte for byte: none, which leaves ENVOY_WEBHOOKS unset so the
// listener mounts no webhook route, and github then slack.
var supportedWebhooks = [][]string{{}, {"github", "slack"}}

// WebhookInput is one input of a WebhookListener that belongs to a webhook.
type WebhookInput struct {
	Webhook, Field string
	Given, Set     bool // given at all; given with a value the listener can use
}

// CheckWebhookInputs refuses, in order, each listed webhook's input that is not set and each
// unlisted webhook's input that is given, which would otherwise be dropped silently. Each refusal
// is a core.InputError.
func CheckWebhookInputs(webhooks []string, inputs []WebhookInput) error {
	for _, in := range inputs {
		listed := slices.Contains(webhooks, in.Webhook)
		if listed && !in.Set {
			return core.InputErrorf(in.Field, "%s is required with webhook %q", in.Field, in.Webhook)
		}
		if !listed && in.Given {
			return core.InputErrorf(in.Field, "%s is given for webhook %q, which webhooks does not list", in.Field, in.Webhook)
		}
	}
	return nil
}

// ListenerEnv holds the inputs of a WebhookListener that its container's non-secret environment
// is made of. The component checks it and writes its Variables into the container definitions;
// `envoy-infra render webhook-listener-env` checks and prints it the same way.
type ListenerEnv struct {
	MachineId      string
	NatsUrls       []string
	ListenHost     string
	HostBridge     string
	NatsReplicas   int
	Webhooks       []string
	ReviewerAppId  *string // written only with github
	MentionTrigger *string // written only with github
}

// Check refuses what the component refuses of these inputs before it registers anything: every
// one but NatsUrls, which may come from another resource and which Variables refuses. A webhook's
// input is checked against Webhooks before the list itself, so each refusal is the one its input
// earns: a listed webhook's missing input, an unlisted webhook's input, then a list this version
// does not take. Each refusal is a core.InputError.
func (e ListenerEnv) Check() error {
	set := func(v *string) bool { return v != nil && strings.TrimSpace(*v) != "" }
	if err := CheckWebhookInputs(e.Webhooks, []WebhookInput{
		// The listener refuses to start without it.
		{Webhook: "github", Field: "reviewerAppId", Given: e.ReviewerAppId != nil, Set: set(e.ReviewerAppId)},
		// The listener would fall back to a trigger of its own without it.
		{Webhook: "github", Field: "mentionTrigger", Given: e.MentionTrigger != nil, Set: set(e.MentionTrigger)},
	}); err != nil {
		return err
	}
	if !slices.ContainsFunc(supportedWebhooks, func(w []string) bool { return slices.Equal(e.Webhooks, w) }) {
		supported := make([]string, len(supportedWebhooks))
		for i, w := range supportedWebhooks {
			supported[i] = fmt.Sprintf("%q", w)
		}
		return core.InputErrorf("webhooks", "webhooks %q is not supported: this version takes %s", e.Webhooks, strings.Join(supported, " or "))
	}
	if err := e.config().Check(); err != nil {
		return err
	}
	// The listener binds 127.0.0.1 when ENVOY_LISTEN_HOST is blank, where the target group cannot
	// reach it.
	if strings.TrimSpace(e.ListenHost) == "" {
		return core.InputErrorf("listenHost", "listenHost is required: the listener binds 127.0.0.1 without it")
	}
	if e.NatsReplicas < 1 {
		return core.InputErrorf("natsReplicas", "natsReplicas %d is below 1", e.NatsReplicas)
	}
	return nil
}

// Variables returns the container's non-secret environment, as NAME=value in the order the
// container definitions list it: core.ListenerConfig.Env's, with the listen host after the port
// where the reference lists it, then the replicas and the webhooks: ENVOY_WEBHOOKS only when one is
// listed, and the GitHub variables only with github. It refuses NatsUrls as Env does, and expects
// the rest to have passed Check, which the component and the render command run first; it runs no
// check of Check's itself, so the container definitions are written for any webhook list.
func (e ListenerEnv) Variables() ([]string, error) {
	env, err := e.config().Env(e.NatsUrls)
	if err != nil {
		return nil, err
	}
	var variables []string
	for _, entry := range env {
		variables = append(variables, entry)
		if name, _, _ := strings.Cut(entry, "="); name == "PORT" {
			variables = append(variables, "ENVOY_LISTEN_HOST="+e.ListenHost)
		}
	}
	variables = append(variables, "ENVOY_NATS_REPLICAS="+strconv.Itoa(e.NatsReplicas))
	if len(e.Webhooks) > 0 {
		variables = append(variables, "ENVOY_WEBHOOKS="+strings.Join(e.Webhooks, ","))
	}
	if slices.Contains(e.Webhooks, "github") {
		variables = append(variables,
			"ENVOY_GITHUB_MENTION_TRIGGER="+*e.MentionTrigger,
			"ENVOY_REVIEWER_APP_ID="+*e.ReviewerAppId,
		)
	}
	return variables, nil
}

// config is the environment as core.ListenerConfig takes it, on the listener's fixed port.
func (e ListenerEnv) config() core.ListenerConfig {
	return core.ListenerConfig{MachineID: e.MachineId, Port: ListenerPort, HostBridge: e.HostBridge}
}

// SecretReadPolicy is the execution-role policy that reads the secret at arn into a container, as
// the reference's json.dumps writes it.
func SecretReadPolicy(arn string) string {
	return core.PyJSON(core.Object{
		{Key: "Version", Value: "2012-10-17"},
		{Key: "Statement", Value: []core.Object{{
			{Key: "Effect", Value: "Allow"},
			{Key: "Action", Value: []string{"secretsmanager:GetSecretValue"}},
			{Key: "Resource", Value: arn},
		}}},
	}, core.PyJSONOptions{EnsureASCII: true})
}

// ListenerContainer holds what a WebhookListener task's container takes from the component's
// inputs and children.
type ListenerContainer struct {
	Image string
	Env   ListenerEnv

	GitHubWebhookSecretArn string // written only with github
	SlackSigningSecretArn  string // written only with slack
	ApiTokenSecretArn      string
	SeedSecretArn          *string // nil injects no seed

	LogGroup string
	Region   string
}

// ListenerContainerDefinitions returns a WebhookListener task's container definitions, written as
// json.dumps writes the reference's list: its dict order, ensure_ascii on. The environment is the
// Env's Variables, and each webhook's secret is written only with its webhook.
func ListenerContainerDefinitions(c ListenerContainer) (string, error) {
	env, err := c.Env.Variables()
	if err != nil {
		return "", err
	}
	variables := make([]core.Object, len(env))
	for i, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		variables[i] = environment(name, value)
	}

	var secrets []core.Object
	if slices.Contains(c.Env.Webhooks, "github") {
		secrets = append(secrets, secret("ENVOY_GITHUB_WEBHOOK_SECRET", c.GitHubWebhookSecretArn))
	}
	if slices.Contains(c.Env.Webhooks, "slack") {
		secrets = append(secrets, secret("ENVOY_SLACK_SIGNING_SECRET", c.SlackSigningSecretArn))
	}
	secrets = append(secrets, secret("ENVOY_API_TOKEN", c.ApiTokenSecretArn))
	if c.SeedSecretArn != nil {
		secrets = append(secrets, secret(core.EnvNATSSeed, *c.SeedSecretArn))
	}

	listener := core.Object{
		{Key: "name", Value: ListenerContainerName},
		{Key: "image", Value: c.Image},
		{Key: "essential", Value: true},
		{Key: "command", Value: []string{"/usr/local/bin/envoy-listener"}},
		{Key: "portMappings", Value: []core.Object{portMapping(ListenerPort)}},
		{Key: "environment", Value: variables},
		{Key: "secrets", Value: secrets},
		// `hostname -i` is the task's ENI address in awsvpc mode, the interface the target group
		// probes, so a listener bound to loopback fails here too. It prints every address the
		// name resolves to; the first one keeps a dual-stack ENI from breaking the probe.
		{Key: "healthCheck", Value: healthCheck(
			fmt.Sprintf(`curl -sf "http://$(hostname -i | cut -d' ' -f1):%d/healthz" || exit 1`, ListenerPort), 30)},
		{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, ListenerContainerName)},
	}
	return core.PyJSON([]core.Object{listener}, core.PyJSONOptions{EnsureASCII: true}), nil
}

func secret(name, arn string) core.Object {
	return core.Object{{Key: "name", Value: name}, {Key: "valueFrom", Value: arn}}
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
