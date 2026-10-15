package aws_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	webhookListenerType = "envoy-infra:aws:WebhookListener"
	rolePolicyType      = "aws:iam/rolePolicy:RolePolicy"
	metricAlarmType     = "aws:cloudwatch/metricAlarm:MetricAlarm"

	// listenerURN is the URN of the listener every mock test constructs, named example-listener.
	listenerURN = "urn:pulumi:s::p::" + webhookListenerType + "::example-listener"
	// constructedListenerURN is its URN under Construct, in the integration server's stack and project.
	constructedListenerURN = "urn:pulumi:stack::project::" + webhookListenerType + "::example-listener"

	listenerSecretArn = "arn:aws:secretsmanager:example-region-1:000000000000:secret:example/"
	alarmTopicArn     = "arn:aws:sns:example-region-1:000000000000:example-alarms"
)

// exampleListenerArgs are placeholder inputs. The container definitions' inputs are the goldens':
// the reference's own render from the same values, with a seed.
func exampleListenerArgs() *envoyaws.WebhookListenerArgs {
	return &envoyaws.WebhookListenerArgs{
		MachineID: "example-machine",
		NatsUrls:  pulumi.StringArray{pulumi.String("nats://example-bus.example-namespace.internal.example:4222")},
		Image:     "example.registry/envoy@sha256:" + strings.Repeat("0", 64),
		Region:    "example-region-1",

		ApiTokenSecretArn:       pulumi.String(listenerSecretArn + "api-token"),
		ApiTokenSecretVersionId: pulumi.String("example-token-version"),
		SeedSecretArn:           pulumi.String(listenerSecretArn + "nats-seed"),
		Webhooks:                []string{"github", "slack"},
		GitHubWebhookSecretArn:  pulumi.String(listenerSecretArn + "github-webhook"),
		SlackSigningSecretArn:   pulumi.String(listenerSecretArn + "slack-signing"),
		ReviewerAppId:           pulumi.StringRef("1234567"),

		ClusterArn:        pulumi.String("example-cluster-arn"),
		ExecutionRoleArn:  pulumi.String("example-execution-role-arn"),
		ExecutionRoleName: pulumi.String("example-execution-role"),
		TaskRoleArn:       pulumi.String("example-task-role-arn"),
		SubnetIds:         []pulumi.StringInput{pulumi.String("example-subnet-a"), pulumi.String("example-subnet-b")},
		SecurityGroupId:   pulumi.String("example-listener-sg"),
		TargetGroupArn:    pulumi.String("example-target-group-arn"),

		CloudMapNamespaceId:        pulumi.String("example-namespace-id"),
		CloudMapNamespaceName:      pulumi.String("example-namespace.internal.example"),
		CloudMapServiceName:        "example-hooks",
		CloudMapServiceLogicalName: "example-listener-cloudmap",

		Family:           "example-listener-family",
		LogGroupName:     "/ecs/envoy-listener",
		LogRetentionDays: 7,
		Cpu:              "256",
		Memory:           "512",

		ListenHost:     "0.0.0.0",
		HostBridge:     "127.0.0.1",
		NatsReplicas:   1,
		MentionTrigger: pulumi.StringRef("@legion"),

		AlarmTopicArn:   pulumi.String(alarmTopicArn),
		BaseTags:        map[string]string{"project": "example-project", "env": "example-env"},
		MetricNamespace: "example/EnvoyListener",
		NatsServiceArn:  pulumi.String("example-nats-service-arn"),
	}
}

// noWebhooks makes args a listener that serves no webhook: webhooks an empty list and none of a
// webhook's inputs given. With the seed kept, its container definitions are
// listener-no-webhooks-seed.golden, the reference's own render of the empty list.
func noWebhooks(a *envoyaws.WebhookListenerArgs) {
	a.Webhooks = []string{}
	a.GitHubWebhookSecretArn, a.SlackSigningSecretArn, a.ReviewerAppId, a.MentionTrigger = nil, nil, nil, nil
}

// exampleListenerOutputs are the outputs exampleListenerArgs give, under their schema names: in a
// run under mocks, and in a Construct preview whose inputs are known.
var exampleListenerOutputs = map[string]string{
	"serviceArn":        "arn:example:example-listener-service",
	"serviceName":       "example-listener-service-autonamed",
	"logGroupName":      "/ecs/envoy-listener",
	"taskDefinitionArn": "arn:example:example-listener-task",
	"internalUrl":       "http://example-hooks.example-namespace.internal.example:9020",
}

// runListener constructs a listener named example-listener with args, passing it an aws provider
// when withProvider is set, and returns what the mocks saw, its outputs and the run's error.
func runListener(t *testing.T, args *envoyaws.WebhookListenerArgs, withProvider bool) (*pulumitest.Mocks, map[string]string, error) {
	t.Helper()
	m := newMocks()
	var out map[string]string
	err := m.Run(func(ctx *pulumi.Context) error {
		var opts []pulumi.ResourceOption
		if withProvider {
			prov, err := aws.NewProvider(ctx, "example-aws", &aws.ProviderArgs{Region: pulumi.String("example-region-1")})
			if err != nil {
				return err
			}
			opts = append(opts, pulumi.Providers(prov))
		}
		l, err := envoyaws.NewWebhookListener(ctx, "example-listener", args, opts...)
		if err != nil {
			return err
		}
		out, err = awaitOutputs(ctx, map[string]pulumi.StringOutput{
			"serviceArn": l.ServiceArn, "serviceName": l.ServiceName, "logGroupName": l.LogGroupName,
			"taskDefinitionArn": l.TaskDefinitionArn, "internalUrl": l.InternalUrl,
		})
		return err
	})
	return m, out, err
}

// listenerSignals are the listener's log signals as the reference writes them: each alarm and the
// filters counting into its metric, by the part of their logical names after the component's name,
// and each alarm's description for a log group named /ecs/envoy-listener.
var listenerSignals = []struct {
	alarm, metric string
	filters       [][2]string // name suffix, pattern
	description   string
}{
	{
		alarm: "publish-failure", metric: "PublishFailures",
		filters: [][2]string{
			{"publish-failure-filter", `{ $.level = "ERROR" && $.msg = "listener exception publish failed" }`},
			{"webhook-publish-failure-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=failed*, ...]`},
		},
		description: "Envoy listener could not publish to JetStream (`listener exception publish failed` or `<webhook> publish failed`, e.g. `nats: no response from stream`) — a delivery from the Dispatch GitHub App (legion-envoy) waits for Dispatch's redelivery sweep, a Slack event is retried by Slack three times with backoff and lost only if every attempt fails, any other GitHub sender's delivery answered 503 is lost, and delivery-exception notices are lost; read /ecs/envoy-listener for the nats error.",
	},
	{
		alarm: "webhook-refused", metric: "WebhookRefusals",
		filters: [][2]string{
			{"webhook-publish-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github||source=slack||source=ghostwispr, verb=publish, result=refused*, ...]`},
			{"ci-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=record, result=refused*, ...]`},
			{"ci-head-suite-record-refused-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, source=github, w1=ci, w2=head||w2=suite, w3=record, result=refused*, ...]`},
		},
		description: "Envoy listener refused a webhook delivery (`<source> publish refused`, `github ci record refused` or `github ci suite record refused`, answered 422): the envelope is past what NATS takes or the commit's CI record is past its bound, the same input is refused on every attempt, and Dispatch's redelivery sweep does not retry a 4xx, so the delivery is lost (a refused CI record leaves its commit overflowed, never settling). Read /ecs/envoy-listener for the refusal's reason; the fix is a cap in legion's normalizer or a CI bound, not the NATS server's limits.",
	},
	{
		alarm: "stream-subject-dropped", metric: "StreamSubjectsDropped",
		filters: [][2]string{
			{"stream-subject-dropped-filter", `[date=%^\d{4}/\d{2}/\d{2}$%, time=%^\d{2}:\d{2}:\d{2}$%, level=WARN, w1=envoy, w2=nats, w3=stream, w4=subject, w5=replaced, w6=by, w7=an, w8=overlapping, w9=one, ...]`},
		},
		description: "An Envoy listener start dropped another deployment's `ENVOY_NOTIFICATIONS` subject because it overlaps one of its own (`envoy nats stream subject replaced by an overlapping one`) — that deployment receives nothing on the dropped subject until it starts again; read /ecs/envoy-listener for the dropped and kept subjects, then reconcile the subject lists across the deployments.",
	},
	{
		alarm: "durable-refused", metric: "DurableRefusals",
		filters: [][2]string{
			{"durable-refused-filter", `{ $.level = "ERROR" && $.msg = "subscribe refused, shutting down" }`},
		},
		description: "An Envoy listener start refused its durable consumer (`subscribe refused, shutting down`): the durable carries an idle heartbeat or an ack policy other than explicit, which NATS cannot change in place, so every replacement task exits 1 while the old task keeps serving, until the circuit breaker rolls the deployment back and the apply fails. Read /ecs/envoy-listener for the durable and the setting, then recreate the durable by the recipe in sjawhar/legion packages/envoy/AGENTS.md (Operational notes), which keeps its cursor.",
	},
	{
		alarm: "subscribe-exhausted", metric: "SubscribesExhausted",
		filters: [][2]string{
			{"subscribe-exhausted-filter", `{ $.level = "ERROR" && $.msg = "subscribe failed after max attempts, shutting down" }`},
		},
		description: "An Envoy listener start gave up setting up its durable subscription 135 s after its first attempt (`subscribe failed after max attempts, shutting down`). ECS had already stopped the old task, so every replacement exits 1 the same way once its own bind gives up. Read the line's `error` in /ecs/envoy-listener: `consumer is already bound to a subscription` means something still holds the durable's push binding (find it with `nats consumer info ENVOY_NOTIFICATIONS listener-<machine id>`, push_bound, and stop it); any other error is JetStream failing the consumer lookup, create or update.",
	},
}

// readPolicy is the execution-role policy that reads one secret, as the reference's json.dumps
// writes it.
func readPolicy(arn string) string {
	return `{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["secretsmanager:GetSecretValue"], "Resource": "` + arn + `"}]}`
}

// exampleListenerChildren are the children exampleListenerArgs register, with the seed when seed is
// set: the reference's resources with their inputs, each named from the component's name as the
// reference names it from envoy-listener.
func exampleListenerChildren(t *testing.T, seed bool) map[string]child {
	if seed {
		return listenerChildren(t, "listener-github-slack-seed.golden", true)
	}
	return listenerChildren(t, "listener-github-slack-no-seed.golden", false)
}

// listenerChildren are exampleListenerChildren with the task definition's container definitions
// the golden named golden: the webhooks list changes nothing else a listener registers.
func listenerChildren(t *testing.T, golden string, seed bool) map[string]child {
	children := map[string]child{
		"example-listener-log-group": {logGroupType, map[string]any{
			"name": "/ecs/envoy-listener", "retentionInDays": float64(7), "tags": tags("example-listener"),
		}},
		"example-listener-cloudmap": {cloudMapServiceType, map[string]any{
			"name":        "example-hooks",
			"namespaceId": "example-namespace-id",
			"dnsConfig": map[string]any{
				"namespaceId":   "example-namespace-id",
				"routingPolicy": "MULTIVALUE",
				"dnsRecords":    []any{map[string]any{"type": "A", "ttl": float64(10)}},
			},
			"tags": tags("example-listener"),
		}},
		"example-listener-execution-api-token-read": {rolePolicyType, map[string]any{
			"role": "example-execution-role", "policy": readPolicy(listenerSecretArn + "api-token"),
		}},
		"example-listener-task": {taskDefinitionType, map[string]any{
			"family":                  "example-listener-family",
			"cpu":                     "256",
			"memory":                  "512",
			"networkMode":             "awsvpc",
			"requiresCompatibilities": []any{"FARGATE"},
			"executionRoleArn":        "example-execution-role-arn",
			"taskRoleArn":             "example-task-role-arn",
			"containerDefinitions":    readGolden(t, golden),
			"tags":                    tags("example-listener"),
		}},
		"example-listener-service": {ecsServiceType, map[string]any{
			"cluster":                         "example-cluster-arn",
			"taskDefinition":                  "arn:example:example-listener-task",
			"desiredCount":                    float64(1),
			"launchType":                      "FARGATE",
			"availabilityZoneRebalancing":     "DISABLED",
			"deploymentMaximumPercent":        float64(200),
			"deploymentMinimumHealthyPercent": float64(100),
			"deploymentCircuitBreaker":        map[string]any{"enable": true, "rollback": true},
			"waitForSteadyState":              true,
			"healthCheckGracePeriodSeconds":   float64(120),
			"enableExecuteCommand":            true,
			"networkConfiguration": map[string]any{
				"subnets":        []any{"example-subnet-a", "example-subnet-b"},
				"securityGroups": []any{"example-listener-sg"},
				"assignPublicIp": false,
			},
			"loadBalancers": []any{map[string]any{
				"targetGroupArn": "example-target-group-arn", "containerName": "envoy-listener", "containerPort": float64(9020),
			}},
			"serviceRegistries": map[string]any{"registryArn": "arn:example:example-listener-cloudmap"},
			"tags":              tags("example-listener"),
		}},
	}
	if seed {
		children["example-listener-execution-nats-nkey-seed-read"] = child{rolePolicyType, map[string]any{
			"role": "example-execution-role", "policy": readPolicy(listenerSecretArn + "nats-seed"),
		}}
	}
	for _, s := range listenerSignals {
		for _, f := range s.filters {
			children["example-listener-"+f[0]] = child{metricFilterType, map[string]any{
				"logGroupName": "/ecs/envoy-listener",
				"pattern":      f[1],
				"metricTransformation": map[string]any{
					"name": s.metric, "namespace": "example/EnvoyListener", "value": "1", "defaultValue": "0",
				},
			}}
		}
		children["example-listener-"+s.alarm] = child{metricAlarmType, map[string]any{
			"alarmDescription":   s.description,
			"namespace":          "example/EnvoyListener",
			"metricName":         s.metric,
			"statistic":          "Sum",
			"period":             float64(300),
			"evaluationPeriods":  float64(1),
			"threshold":          float64(1),
			"comparisonOperator": "GreaterThanOrEqualToThreshold",
			"treatMissingData":   "notBreaching",
			"alarmActions":       []any{alarmTopicArn},
			"okActions":          []any{alarmTopicArn},
			"tags":               tags("example-listener"),
		}}
	}
	return children
}

func TestWebhookListenerRegistersTheReferenceChildren(t *testing.T) {
	for _, row := range []struct {
		name, golden string
		seed         bool
		mutate       func(*envoyaws.WebhookListenerArgs)
	}{
		{"seed true", "listener-github-slack-seed.golden", true, func(*envoyaws.WebhookListenerArgs) {}},
		{"seed false", "listener-github-slack-no-seed.golden", false, func(a *envoyaws.WebhookListenerArgs) { a.SeedSecretArn = nil }},
		{"no webhooks", "listener-no-webhooks-seed.golden", true, noWebhooks},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			row.mutate(args)
			m, out, err := runListener(t, args, true)
			require.NoError(t, err)

			require.Equal(t, "example-listener", m.One(t, webhookListenerType).Name)
			children := childrenOf(t, m)
			requireChildren(t, listenerChildren(t, row.golden, row.seed), children)
			for name, c := range children {
				require.Equal(t, listenerURN, c.RPC.GetParent(), "%s's parent is the component", name)
				require.Contains(t, c.RPC.GetProvider(), providerURN+"::", "%s uses the aws provider the component was given", name)
				require.Empty(t, c.RPC.GetAliases(), "%s is adopted from nothing", name)
			}
			require.Equal(t, exampleListenerOutputs, out)
		})
	}
}

// The service tag is derived from the component's name, as the reference derives it from
// envoy-listener; the role policies and the metric filters carry no tags.
func TestWebhookListenerTagsEachChildWithItsService(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), true)
	require.NoError(t, err)

	tagged := 0
	for name, c := range childrenOf(t, m) {
		switch c.Type {
		case rolePolicyType, metricFilterType:
			require.NotContains(t, c.Inputs, resource.PropertyKey("tags"), "%s carries no tags", name)
		default:
			require.Equal(t, tags("example-listener"), c.Inputs["tags"].Mappable(), "%s's tags", name)
			tagged++
		}
	}
	require.Equal(t, 9, tagged, "the log group, Cloud Map service, task definition, service and five alarms")
}

// The task definition comes after the policies its execution role reads the secrets with, and the
// service's create and update wait up to fifteen minutes for its rollout.
func TestWebhookListenerOrdersTheTaskDefinitionAfterItsPolicies(t *testing.T) {
	for _, seed := range []bool{true, false} {
		t.Run(fmt.Sprintf("seed %v", seed), func(t *testing.T) {
			args := exampleListenerArgs()
			policies := []string{"example-listener-execution-api-token-read"}
			if seed {
				policies = append(policies, "example-listener-execution-nats-nkey-seed-read")
			} else {
				args.SeedSecretArn = nil
			}
			m, _, err := runListener(t, args, true)
			require.NoError(t, err)

			var urns []string
			for _, name := range policies {
				urns = append(urns, "urn:pulumi:s::p::"+webhookListenerType+"$"+rolePolicyType+"::"+name)
			}
			require.Subset(t, m.One(t, taskDefinitionType).RPC.GetDependencies(), urns)
		})
	}
}

func TestWebhookListenerServiceWaitsFifteenMinutes(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), true)
	require.NoError(t, err)
	timeouts := m.One(t, ecsServiceType).RPC.GetCustomTimeouts()
	require.Equal(t, "15m", timeouts.GetCreate())
	require.Equal(t, "15m", timeouts.GetUpdate())
	require.Empty(t, timeouts.GetDelete())
}

// With adopt, each child takes over the root-level resource of its own name.
func TestWebhookListenerAdoptAliasesEachChildToItsRootLevelName(t *testing.T) {
	args := exampleListenerArgs()
	args.Adopt = true
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)
	requireAdopted(t, exampleListenerChildren(t, true), childrenOf(t, m))
}

// A listener that serves no webhook carries none of a webhook's settings or secrets: no
// ENVOY_WEBHOOKS, which the listener reads as no route to mount, no ENVOY_GITHUB_MENTION_TRIGGER
// or ENVOY_REVIEWER_APP_ID, and neither signing secret. Its API token and seed stay.
func TestWebhookListenerWithNoWebhooksWritesNoWebhookSetting(t *testing.T) {
	args := exampleListenerArgs()
	noWebhooks(args)
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)

	type entry struct {
		Name string `json:"name"`
	}
	var containers []struct {
		Environment []entry `json:"environment"`
		Secrets     []entry `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal([]byte(m.One(t, taskDefinitionType).Inputs["containerDefinitions"].StringValue()), &containers))
	require.Len(t, containers, 1)
	names := func(entries []entry) []string {
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}
	require.Equal(t, []string{
		"PORT", "ENVOY_LISTEN_HOST", "ENVOY_MACHINE_ID", "NATS_URLS", "ENVOY_ALLOW_REMOTE_NATS", "ENVOY_HOST_BRIDGE", "ENVOY_NATS_REPLICAS",
	}, names(containers[0].Environment))
	require.Equal(t, []string{"ENVOY_API_TOKEN", "NATS_NKEY_SEED"}, names(containers[0].Secrets))
}

// The alarms' descriptions send the reader to the log group the filters read, whatever its name.
func TestWebhookListenerAlarmsNameItsLogGroup(t *testing.T) {
	args := exampleListenerArgs()
	args.LogGroupName = "/ecs/example-hooks"
	m, _, err := runListener(t, args, true)
	require.NoError(t, err)

	alarms := 0
	for name, c := range childrenOf(t, m) {
		switch c.Type {
		case metricAlarmType:
			description := c.Inputs["alarmDescription"].StringValue()
			require.Contains(t, description, "/ecs/example-hooks", "%s's description", name)
			require.NotContains(t, description, "/ecs/envoy-listener", "%s's description", name)
			alarms++
		case metricFilterType:
			require.Equal(t, "/ecs/example-hooks", c.Inputs["logGroupName"].StringValue(), "%s's log group", name)
		}
	}
	require.Equal(t, 5, alarms)
}

// natsUrls may be known only in the apply, so core.ListenerConfig.Env refuses a bad one there and
// the refusal reaches the consumer as the run's error.
func TestWebhookListenerRefusesWhatTheListenerEnvironmentRefuses(t *testing.T) {
	args := exampleListenerArgs()
	args.NatsUrls = pulumi.StringArray{pulumi.String("nats://a.internal.example:4222,nats://b.internal.example:4222")}
	_, _, err := runListener(t, args, true)
	require.ErrorContains(t, err, "natsUrls[0] contains a comma")
}

// An optional ARN the consumer gives but that resolves to nothing is refused, naming its field,
// rather than leaving the container without it or naming an empty secret.
func TestWebhookListenerRefusesAGivenArnThatResolvesToNoValue(t *testing.T) {
	for _, row := range []struct {
		name  string
		value func(*string) *string
	}{
		{"nil", func(*string) *string { return nil }},
		{"empty", func(*string) *string { return pulumi.StringRef("") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			args.SeedSecretArn = pulumi.String(listenerSecretArn + "nats-seed").ToStringPtrOutput().
				ApplyT(row.value).(pulumi.StringPtrOutput)
			_, _, err := runListener(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: seedSecretArn resolved to no value")
		})
	}
}

func TestWebhookListenerRefusesWithoutAnAWSProviderBeforeRegistering(t *testing.T) {
	m, _, err := runListener(t, exampleListenerArgs(), false)
	require.ErrorContains(t, err, `envoy-infra:aws:WebhookListener needs a "aws" provider in its providers option`)
	require.Empty(t, m.Types())
}

// validate refuses each input NewWebhookListener cannot build from. The table runs against validate
// directly and holds the component's own refusals: the webhooks list itself, each webhook's secret
// ARN, and the service's. rendering.ListenerEnv.Check's refusals are tested in rendering; one row
// shows that validate runs it. TestWebhookListenerRefusesAnInvalidInputBeforeRegistering shows that
// the constructor runs validate first and prefixes the component's name.
func TestWebhookListenerValidate(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*envoyaws.WebhookListenerArgs)
		err    string
	}{
		// webhooks itself is required: a listener with no webhook gives [], and an omitted list is
		// never read as one.
		{
			name: "webhooks omitted",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.Webhooks = nil
			},
			err: `webhooks is required: give [] for a listener with no webhook`,
		},
		// A listed webhook's secret.
		{
			name:   "github without its secret",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.GitHubWebhookSecretArn = nil },
			err:    `githubWebhookSecretArn is required with webhook "github"`,
		},
		{
			name:   "slack without its secret",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.SlackSigningSecretArn = nil },
			err:    `slackSigningSecretArn is required with webhook "slack"`,
		},
		// A secret for a webhook that is not listed, which would otherwise be dropped silently.
		{
			name: "a github secret with github unlisted",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				a.Webhooks, a.ReviewerAppId, a.MentionTrigger = []string{"slack"}, nil, nil
			},
			err: `githubWebhookSecretArn is given for webhook "github", which webhooks does not list`,
		},
		{
			name:   "a slack secret with slack unlisted",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.Webhooks = []string{"github"} },
			err:    `slackSigningSecretArn is given for webhook "slack", which webhooks does not list`,
		},
		{
			name: "a github secret with no webhook",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.GitHubWebhookSecretArn = pulumi.String(listenerSecretArn + "github-webhook")
			},
			err: `githubWebhookSecretArn is given for webhook "github", which webhooks does not list`,
		},
		{
			name: "a slack secret with no webhook",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				noWebhooks(a)
				a.SlackSigningSecretArn = pulumi.String(listenerSecretArn + "slack-signing")
			},
			err: `slackSigningSecretArn is given for webhook "slack", which webhooks does not list`,
		},
		// The environment, through rendering.ListenerEnv.Check.
		{
			name: "a webhooks list this version does not take",
			mutate: func(a *envoyaws.WebhookListenerArgs) {
				a.Webhooks, a.SlackSigningSecretArn = []string{"github"}, nil
			},
			err: `webhooks ["github"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		// The service.
		{
			name:   "no subnet",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.SubnetIds = nil },
			err:    "subnetIds names no subnet",
		},
		{
			name:   "base tags holding service",
			mutate: func(a *envoyaws.WebhookListenerArgs) { a.BaseTags["service"] = "example" },
			err:    `baseTags holds "service"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleListenerArgs()
			row.mutate(args)
			require.ErrorContains(t, args.Validate(), row.err)
		})
	}
	require.NoError(t, exampleListenerArgs().Validate())
}

func TestWebhookListenerRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := exampleListenerArgs()
	args.SubnetIds = nil
	m, _, err := runListener(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: subnetIds names no subnet")
	require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
}

// without returns text with each part removed, requiring each to occur in it exactly once.
func without(t *testing.T, text string, parts ...string) string {
	t.Helper()
	for _, part := range parts {
		require.Equal(t, 1, strings.Count(text, part), "%q occurs once", part)
		text = strings.Replace(text, part, "", 1)
	}
	return text
}

// ENVOY_WEBHOOKS is written only when a webhook is listed, the GitHub variables only with github,
// and each webhook's secret only with its webhook. The component takes no webhook or github and
// slack, which the goldens pin through it; github or slack alone reaches only the container
// definitions, so this test calls them directly, on listener-github-slack-no-seed.golden's inputs.
// Every row gives both webhooks' ARNs, so what writes a secret is its webhook being listed.
func TestListenerContainerDefinitionsWriteEachWebhookOnlyWhenListed(t *testing.T) {
	golden := readGolden(t, "listener-github-slack-no-seed.golden")
	const (
		webhooks      = `{"name": "ENVOY_WEBHOOKS", "value": "github,slack"}`
		gitHubEnv     = `, {"name": "ENVOY_GITHUB_MENTION_TRIGGER", "value": "@legion"}, {"name": "ENVOY_REVIEWER_APP_ID", "value": "1234567"}`
		gitHubSecret  = `{"name": "ENVOY_GITHUB_WEBHOOK_SECRET", "valueFrom": "` + listenerSecretArn + `github-webhook"}, `
		slackSecret   = `{"name": "ENVOY_SLACK_SIGNING_SECRET", "valueFrom": "` + listenerSecretArn + `slack-signing"}, `
		gitHubOnlyVar = `{"name": "ENVOY_WEBHOOKS", "value": "github"}`
		slackOnlyVar  = `{"name": "ENVOY_WEBHOOKS", "value": "slack"}`
	)
	container := func(webhooks []string, arns map[string]string) rendering.ListenerContainer {
		appID, trigger := "1234567", "@legion"
		return rendering.ListenerContainer{
			Image: "example.registry/envoy@sha256:" + strings.Repeat("0", 64),
			Env: rendering.ListenerEnv{
				MachineID:      "example-machine",
				NatsUrls:       []string{"nats://example-bus.example-namespace.internal.example:4222"},
				ListenHost:     "0.0.0.0",
				HostBridge:     "127.0.0.1",
				NatsReplicas:   1,
				Webhooks:       webhooks,
				ReviewerAppId:  &appID,
				MentionTrigger: &trigger,
			},
			WebhookSecretArns: arns,
			ApiTokenSecretArn: listenerSecretArn + "api-token",
			LogGroup:          "/ecs/envoy-listener",
			Region:            "example-region-1",
		}
	}
	both := map[string]string{"github": listenerSecretArn + "github-webhook", "slack": listenerSecretArn + "slack-signing"}
	for _, row := range []struct {
		name     string
		webhooks []string
		want     string
	}{
		{"github", []string{"github"}, strings.Replace(without(t, golden, slackSecret), webhooks, gitHubOnlyVar, 1)},
		{"slack", []string{"slack"}, strings.Replace(without(t, golden, gitHubEnv, gitHubSecret), webhooks, slackOnlyVar, 1)},
		{"none", []string{}, without(t, golden, ", "+webhooks, gitHubEnv, gitHubSecret, slackSecret)},
	} {
		t.Run(row.name, func(t *testing.T) {
			definitions, err := rendering.ListenerContainerDefinitions(container(row.webhooks, both))
			require.NoError(t, err)
			require.Equal(t, row.want, definitions)
		})
	}
	t.Run("a listed webhook with no secret ARN", func(t *testing.T) {
		_, err := rendering.ListenerContainerDefinitions(container([]string{"github", "slack"}, map[string]string{"github": both["github"]}))
		require.EqualError(t, err, `webhook "slack" is listed with no secret ARN`)
	})
}

// The ordering inputs come from resources of their own, so a test can tell their dependencies
// from the other Input fields'.
const (
	tokenVersion = resource.URN("urn:pulumi:s::p::aws:secretsmanager/secretVersion:SecretVersion::token-version")
	natsService  = resource.URN("urn:pulumi:s::p::envoy-infra:aws:NatsService$aws:ecs/service:Service::nats-service")
)

// listenerInputs are exampleListenerArgs as a consumer sends them to Construct (listenerInputsOf).
func listenerInputs(t *testing.T, input func(string) property.Value) map[string]property.Value {
	t.Helper()
	return listenerInputsOf(t, exampleListenerArgs(), input)
}

// listenerInputsOf are args as a consumer sends them to Construct, each Input field made a value by
// input, the two ordering inputs carrying their own resources' dependencies.
func listenerInputsOf(t *testing.T, args *envoyaws.WebhookListenerArgs, input func(string) property.Value) map[string]property.Value {
	t.Helper()
	in := inputs(t, args, input)
	in["apiTokenSecretVersionId"] = property.New(literal(t, args.ApiTokenSecretVersionId)).WithDependencies([]resource.URN{tokenVersion})
	in["natsServiceArn"] = property.New(literal(t, args.NatsServiceArn)).WithDependencies([]resource.URN{natsService})
	return in
}

func TestWebhookListenerConstructTakesEveryInputFieldFromAnotherResource(t *testing.T) {
	m, _, err := construct(t, webhookListenerType, "example-listener", listenerInputs(t, fromUpstream), false)
	require.NoError(t, err)

	children := childrenOf(t, m)
	requireChildren(t, exampleListenerChildren(t, true), children)
	for name, c := range children {
		require.Equal(t, constructedListenerURN, c.RPC.GetParent(), "%s's parent is the component", name)
		require.Contains(t, c.RPC.GetProvider(), providerURN+"::", "%s uses the aws provider the component was given", name)
	}
	for _, dep := range []struct{ child, input string }{
		{"example-listener-cloudmap", "namespaceId"},
		{"example-listener-execution-api-token-read", "role"},
		{"example-listener-execution-api-token-read", "policy"},
		{"example-listener-execution-nats-nkey-seed-read", "policy"},
		{"example-listener-task", "executionRoleArn"},
		{"example-listener-task", "taskRoleArn"},
		{"example-listener-task", "containerDefinitions"},
		{"example-listener-service", "cluster"},
		{"example-listener-service", "networkConfiguration"},
		{"example-listener-service", "loadBalancers"},
		{"example-listener-publish-failure", "alarmActions"},
	} {
		require.Contains(t, children[dep.child].RPC.GetPropertyDependencies()[dep.input].GetUrns(), string(upstream),
			"%s's %s depends on the resource its input comes from", dep.child, dep.input)
	}

	// The ordering inputs order a child after their resources and reach no input.
	policies := []string{
		"urn:pulumi:stack::project::" + webhookListenerType + "$" + rolePolicyType + "::example-listener-execution-api-token-read",
		"urn:pulumi:stack::project::" + webhookListenerType + "$" + rolePolicyType + "::example-listener-execution-nats-nkey-seed-read",
	}
	require.Subset(t, children["example-listener-task"].RPC.GetDependencies(), append(policies, string(tokenVersion)))
	require.Contains(t, children["example-listener-service"].RPC.GetDependencies(), string(natsService))
	for name, c := range children {
		for input, deps := range c.RPC.GetPropertyDependencies() {
			require.NotContains(t, deps.GetUrns(), string(tokenVersion), "%s's %s", name, input)
			require.NotContains(t, deps.GetUrns(), string(natsService), "%s's %s", name, input)
		}
	}
}

func TestWebhookListenerConstructTakesUnknownInputFieldsUnderPreview(t *testing.T) {
	m, _, err := construct(t, webhookListenerType, "example-listener", inputs(t, exampleListenerArgs(), unknownFromUpstream), true)
	require.NoError(t, err)

	children := childrenOf(t, m)
	want := exampleListenerChildren(t, true)
	require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(children)), "children's names")
	for name, c := range children {
		require.Equal(t, want[name].typ, c.Type, "%s's type", name)
		require.Equal(t, constructedListenerURN, c.RPC.GetParent(), "%s's parent is the component", name)
	}
	for _, unknown := range []struct{ child, input string }{
		{"example-listener-cloudmap", "namespaceId"},
		{"example-listener-execution-api-token-read", "policy"},
		{"example-listener-execution-nats-nkey-seed-read", "policy"},
		{"example-listener-task", "containerDefinitions"},
		{"example-listener-service", "cluster"},
		{"example-listener-publish-failure", "alarmActions"},
	} {
		v := children[unknown.child].Inputs[resource.PropertyKey(unknown.input)]
		require.True(t, v.ContainsUnknowns(), "%s's %s from an unknown input is unknown, not %s",
			unknown.child, unknown.input, v.TypeString())
	}
	// What depends on no Input field is as in a run with known inputs.
	for _, name := range []string{"example-listener-log-group", "example-listener-publish-failure-filter"} {
		require.Equal(t, want[name].inputs, children[name].Inputs.Mappable(), "%s's inputs", name)
	}
}

// A consumer may pass an ARN with no dependency, such as one an invoke returned: Construct then
// copies the plain string into the optional Input field.
func TestWebhookListenerConstructTakesSecretArnsWithNoDependency(t *testing.T) {
	args := exampleListenerArgs()
	in := listenerInputs(t, fromUpstream)
	for name, arn := range map[string]pulumi.StringPtrInput{
		"seedSecretArn":          args.SeedSecretArn,
		"githubWebhookSecretArn": args.GitHubWebhookSecretArn,
		"slackSigningSecretArn":  args.SlackSigningSecretArn,
	} {
		in[name] = property.New(literal(t, arn.(pulumi.StringInput)))
	}
	m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
	require.NoError(t, err)
	requireChildren(t, exampleListenerChildren(t, true), childrenOf(t, m))
}

// A consumer's `webhooks: []` reaches the component through Construct as an empty list, not as a
// missing input, and with none of a webhook's inputs the listener registers the empty list's
// container, every other child as with both webhooks. A consumer that leaves webhooks out is
// refused before anything registers: an omitted list is not read as [].
func TestWebhookListenerConstructTakesAnEmptyWebhooksListButNotAnOmittedOne(t *testing.T) {
	withoutWebhookInputs := func() map[string]property.Value {
		in := listenerInputs(t, fromUpstream)
		for _, name := range []string{"githubWebhookSecretArn", "slackSigningSecretArn", "reviewerAppId", "mentionTrigger"} {
			delete(in, name)
		}
		return in
	}
	t.Run("webhooks []", func(t *testing.T) {
		in := withoutWebhookInputs()
		in["webhooks"] = property.New(property.NewArray(nil))
		require.True(t, in["webhooks"].IsArray())
		m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
		require.NoError(t, err)
		requireChildren(t, listenerChildren(t, "listener-no-webhooks-seed.golden", true), childrenOf(t, m))
	})
	t.Run("webhooks omitted", func(t *testing.T) {
		in := withoutWebhookInputs()
		delete(in, "webhooks")
		m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
		require.ErrorContains(t, err, "envoy-infra:aws:WebhookListener example-listener: webhooks is required: give [] for a listener with no webhook")
		require.Empty(t, m.Types())
	})
}

// A stack output a consumer builds from a component output would preview as changed if the output
// were unknown, so in a preview whose inputs are known every output is known.
func TestWebhookListenerConstructOutputsAreKnownInAPreviewWithKnownInputs(t *testing.T) {
	_, resp, err := construct(t, webhookListenerType, "example-listener", listenerInputs(t, fromUpstream), true)
	require.NoError(t, err)

	got := map[string]string{}
	for k, v := range resp.State.All {
		require.False(t, v.HasComputed(), "output %s is unknown in a preview with known inputs", k)
		got[k] = v.AsString()
	}
	require.Equal(t, exampleListenerOutputs, got)
}

// An optional field a consumer leaves out is nil at construct, so a listed webhook's missing
// secret is refused there, before the component registers anything.
func TestWebhookListenerConstructRefusesAListedWebhookWithoutItsSecret(t *testing.T) {
	in := listenerInputs(t, fromUpstream)
	delete(in, "slackSigningSecretArn")
	m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
	require.ErrorContains(t, err, `slackSigningSecretArn is required with webhook "slack"`)
	require.Empty(t, m.Types())
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the component registers anything.
func TestWebhookListenerConstructRefusesAPlainFieldFromAnotherResource(t *testing.T) {
	in := listenerInputs(t, fromUpstream)
	in["image"] = fromUpstream(exampleListenerArgs().Image)
	m, _, err := construct(t, webhookListenerType, "example-listener", in, false)
	require.ErrorContains(t, err, "image")
	require.Empty(t, m.Types())
}
