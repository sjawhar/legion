package aws

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/servicediscovery"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
)

// webhookListenerType is the component's token, registered as a constant for the reason
// natsServiceType gives.
const webhookListenerType = "envoy-infra:aws:WebhookListener"

const (
	// listenerPort is the port the listener serves in the task: its webhooks, /v1 and /healthz,
	// and the port the target group sends traffic to.
	listenerPort = 9020
	// listenerContainerName names the task's one container, which the service attaches to the
	// target group and whose log streams it prefixes.
	listenerContainerName = "envoy-listener"
)

// supportedWebhooks is the one webhooks list this version takes: the list whose container
// definitions the reference renders, byte for byte, and the only one a consumer passes.
var supportedWebhooks = []string{"github", "slack"}

// WebhookListenerArgs are the inputs of an envoy-infra:aws:WebhookListener. Every field that holds
// a value of the deployment is required and has no default.
type WebhookListenerArgs struct {
	MachineId string                  `pulumi:"machineId"`
	NatsUrls  pulumi.StringArrayInput `pulumi:"natsUrls"`
	Image     string                  `pulumi:"image"`
	Region    string                  `pulumi:"region"` // the container's awslogs-region

	// The secrets the container reads, as ARNs: the execution role reads each one at task start.
	ApiTokenSecretArn       pulumi.StringInput    `pulumi:"apiTokenSecretArn"`
	ApiTokenSecretVersionId pulumi.StringInput    `pulumi:"apiTokenSecretVersionId"` // orders the task definition after the token's version
	SeedSecretArn           pulumi.StringPtrInput `pulumi:"seedSecretArn,optional"`  // the NATS user's seed; unset injects none
	// The ENVOY_WEBHOOKS list; this version takes exactly ["github", "slack"]. Each webhook's
	// inputs are required while it is listed and refused while it is not.
	Webhooks               []string              `pulumi:"webhooks"`
	GitHubWebhookSecretArn pulumi.StringPtrInput `pulumi:"githubWebhookSecretArn,optional"`
	SlackSigningSecretArn  pulumi.StringPtrInput `pulumi:"slackSigningSecretArn,optional"`
	ReviewerAppId          *string               `pulumi:"reviewerAppId,optional"` // ENVOY_REVIEWER_APP_ID, with github

	ClusterArn        pulumi.StringInput   `pulumi:"clusterArn"`
	ExecutionRoleArn  pulumi.StringInput   `pulumi:"executionRoleArn"`
	ExecutionRoleName pulumi.StringInput   `pulumi:"executionRoleName"` // the role the read policies attach to
	TaskRoleArn       pulumi.StringInput   `pulumi:"taskRoleArn"`
	SubnetIds         []pulumi.StringInput `pulumi:"subnetIds"`
	SecurityGroupId   pulumi.StringInput   `pulumi:"securityGroupId"` // the listener task's group, built by the consumer
	TargetGroupArn    pulumi.StringInput   `pulumi:"targetGroupArn"`  // the load balancer's target group, built by the consumer

	CloudMapNamespaceId        pulumi.StringInput `pulumi:"cloudMapNamespaceId"`
	CloudMapNamespaceName      pulumi.StringInput `pulumi:"cloudMapNamespaceName"`
	CloudMapServiceName        string             `pulumi:"cloudMapServiceName"`        // the DNS name
	CloudMapServiceLogicalName string             `pulumi:"cloudMapServiceLogicalName"` // the child's Pulumi name

	Family           string `pulumi:"family"`
	LogGroupName     string `pulumi:"logGroupName"`
	LogRetentionDays int    `pulumi:"logRetentionDays"`
	Cpu              string `pulumi:"cpu"`
	Memory           string `pulumi:"memory"`

	// The listener's remaining environment.
	ListenHost     string  `pulumi:"listenHost"`              // ENVOY_LISTEN_HOST
	HostBridge     string  `pulumi:"hostBridge"`              // ENVOY_HOST_BRIDGE
	NatsReplicas   int     `pulumi:"natsReplicas"`            // ENVOY_NATS_REPLICAS
	MentionTrigger *string `pulumi:"mentionTrigger,optional"` // ENVOY_GITHUB_MENTION_TRIGGER, with github

	AlarmTopicArn   pulumi.StringInput `pulumi:"alarmTopicArn"` // the SNS topic the alarms page and clear on
	BaseTags        map[string]string  `pulumi:"baseTags"`      // the component adds `service`
	MetricNamespace string             `pulumi:"metricNamespace"`
	NatsServiceArn  pulumi.StringInput `pulumi:"natsServiceArn"` // NatsService's serviceArn; orders this service after it
	// Each child takes over the root-level resource of its own name and type:
	// pulumi.Alias{NoParent: true}.
	Adopt bool `pulumi:"adopt,optional"`
}

// WebhookListener is Envoy's webhook listener on ECS Fargate: one task behind the consumer's load
// balancer target group, registered in Cloud Map for in-VPC callers, with an alarm on each of the
// log lines that say a delivery or a start went wrong.
type WebhookListener struct {
	pulumi.ResourceState
	ServiceArn        pulumi.StringOutput `pulumi:"serviceArn"`
	ServiceName       pulumi.StringOutput `pulumi:"serviceName"`
	LogGroupName      pulumi.StringOutput `pulumi:"logGroupName"`
	TaskDefinitionArn pulumi.StringOutput `pulumi:"taskDefinitionArn"`
	InternalUrl       pulumi.StringOutput `pulumi:"internalUrl"` // http://<cloud map name>.<namespace>:9020
}

// NewWebhookListener registers the listener. Each child is named from name as the reference names
// its root-level resource from envoy-listener, the Cloud Map service by cloudMapServiceLogicalName.
// It refuses invalid inputs and a missing aws provider before it registers anything.
func NewWebhookListener(ctx *pulumi.Context, name string, args *WebhookListenerArgs, opts ...pulumi.ResourceOption) (*WebhookListener, error) {
	if err := args.validate(); err != nil {
		return nil, fmt.Errorf("%s %s: %w", webhookListenerType, name, err)
	}
	if err := providers.Require(ctx.Context(), webhookListenerType, "aws", opts...); err != nil {
		return nil, err
	}

	comp := &WebhookListener{}
	if err := ctx.RegisterComponentResource(webhookListenerType, name, comp, opts...); err != nil {
		return nil, err
	}
	child := []pulumi.ResourceOption{pulumi.Parent(comp)}
	if args.Adopt {
		child = append(child, pulumi.Aliases([]pulumi.Alias{{NoParent: pulumi.Bool(true)}}))
	}
	serviceTags := map[string]string{"service": name}
	maps.Copy(serviceTags, args.BaseTags) // which holds no service: refused by validate
	tags := pulumi.ToStringMap(serviceTags)
	// given resolves an optional Input field the consumer set; one that resolves to no value is an
	// error naming the field, never a container that silently lacks it or names nothing.
	given := func(field string, in pulumi.StringPtrInput) pulumi.StringOutput {
		return in.ToStringPtrOutput().ApplyT(func(v *string) (string, error) {
			if v == nil || *v == "" {
				return "", fmt.Errorf("%s %s: %s resolved to no value", webhookListenerType, name, field)
			}
			return *v, nil
		}).(pulumi.StringOutput)
	}

	logGroup, err := cloudwatch.NewLogGroup(ctx, name+"-log-group", &cloudwatch.LogGroupArgs{
		Name:            pulumi.String(args.LogGroupName),
		RetentionInDays: pulumi.Int(args.LogRetentionDays),
		Tags:            tags,
	}, child...)
	if err != nil {
		return nil, err
	}

	// ECS registers and deregisters the task's instance with the task itself, so the service has
	// no custom health check: one would only delay deregistering a task ECS already stopped.
	cloudMap, err := servicediscovery.NewService(ctx, args.CloudMapServiceLogicalName, &servicediscovery.ServiceArgs{
		Name:        pulumi.String(args.CloudMapServiceName),
		NamespaceId: args.CloudMapNamespaceId,
		DnsConfig: servicediscovery.ServiceDnsConfigArgs{
			NamespaceId:   args.CloudMapNamespaceId,
			RoutingPolicy: pulumi.String("MULTIVALUE"),
			DnsRecords: servicediscovery.ServiceDnsConfigDnsRecordArray{
				servicediscovery.ServiceDnsConfigDnsRecordArgs{Type: pulumi.String("A"), Ttl: pulumi.Int(10)},
			},
		},
		Tags: tags,
	}, child...)
	if err != nil {
		return nil, err
	}

	// The execution role reads the token and, when there is one, the seed into the container.
	var policies []pulumi.Resource
	readPolicy := func(suffix string, arn pulumi.StringOutput) error {
		policy, err := iam.NewRolePolicy(ctx, name+suffix, &iam.RolePolicyArgs{
			Role:   args.ExecutionRoleName,
			Policy: arn.ApplyT(secretReadPolicy).(pulumi.StringOutput),
		}, child...)
		if err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	}
	if err := readPolicy("-execution-api-token-read", args.ApiTokenSecretArn.ToStringOutput()); err != nil {
		return nil, err
	}
	if args.SeedSecretArn != nil {
		if err := readPolicy("-execution-nats-nkey-seed-read", given("seedSecretArn", args.SeedSecretArn)); err != nil {
			return nil, err
		}
	}

	// An ARN for an unlisted webhook was refused, so an absent one is never written.
	optional := func(field string, in pulumi.StringPtrInput) pulumi.StringOutput {
		if in == nil {
			return pulumi.String("").ToStringOutput()
		}
		return given(field, in)
	}
	definitions := pulumi.All(
		args.NatsUrls.ToStringArrayOutput(),
		logGroup.Name,
		args.ApiTokenSecretArn.ToStringOutput(),
		optional("githubWebhookSecretArn", args.GitHubWebhookSecretArn),
		optional("slackSigningSecretArn", args.SlackSigningSecretArn),
		optional("seedSecretArn", args.SeedSecretArn),
	).ApplyT(func(v []any) (string, error) {
		c := listenerContainer{
			Image:                  args.Image,
			MachineId:              args.MachineId,
			NatsUrls:               v[0].([]string),
			ListenHost:             args.ListenHost,
			HostBridge:             args.HostBridge,
			NatsReplicas:           args.NatsReplicas,
			Webhooks:               args.Webhooks,
			GitHubWebhookSecretArn: v[3].(string),
			SlackSigningSecretArn:  v[4].(string),
			ApiTokenSecretArn:      v[2].(string),
			LogGroup:               v[1].(string),
			Region:                 args.Region,
		}
		if args.MentionTrigger != nil {
			c.MentionTrigger = *args.MentionTrigger
		}
		if args.ReviewerAppId != nil {
			c.ReviewerAppId = *args.ReviewerAppId
		}
		if args.SeedSecretArn != nil {
			seed := v[5].(string)
			c.SeedSecretArn = &seed
		}
		definitions, err := listenerContainerDefinitions(c)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", webhookListenerType, name, err)
		}
		return definitions, nil
	}).(pulumi.StringOutput)
	// A task must not start before the token's version exists or before its execution role may
	// read what the container names.
	task, err := ecs.NewTaskDefinition(ctx, name+"-task", &ecs.TaskDefinitionArgs{
		Family:                  pulumi.String(args.Family),
		Cpu:                     pulumi.String(args.Cpu),
		Memory:                  pulumi.String(args.Memory),
		NetworkMode:             pulumi.String("awsvpc"),
		RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
		ExecutionRoleArn:        args.ExecutionRoleArn,
		TaskRoleArn:             args.TaskRoleArn,
		ContainerDefinitions:    definitions,
		Tags:                    tags,
	}, append(slices.Clip(child), pulumi.DependsOn(policies), dependsOnInput(args.ApiTokenSecretVersionId))...)
	if err != nil {
		return nil, err
	}

	// A rollout starts the replacement before it drains the old task: maximum 200% and minimum
	// healthy 100% of one task keep a healthy target in the target group throughout. The circuit
	// breaker rolls back a replacement that keeps failing to start, and waiting for steady state
	// turns that rollback into a failed update, as do the timeouts if the breaker has not decided.
	// The grace period keeps ECS from acting on load-balancer health while a new task starts. The
	// service rolls after NATS, so the server already holds the key the listener presents.
	service, err := ecs.NewService(ctx, name+"-service", &ecs.ServiceArgs{
		Cluster:                         args.ClusterArn,
		TaskDefinition:                  task.Arn,
		DesiredCount:                    pulumi.Int(1),
		LaunchType:                      pulumi.String("FARGATE"),
		AvailabilityZoneRebalancing:     pulumi.String("DISABLED"),
		DeploymentMaximumPercent:        pulumi.Int(200),
		DeploymentMinimumHealthyPercent: pulumi.Int(100),
		DeploymentCircuitBreaker: ecs.ServiceDeploymentCircuitBreakerArgs{
			Enable:   pulumi.Bool(true),
			Rollback: pulumi.Bool(true),
		},
		WaitForSteadyState:            pulumi.Bool(true),
		HealthCheckGracePeriodSeconds: pulumi.Int(120),
		EnableExecuteCommand:          pulumi.Bool(true),
		NetworkConfiguration: ecs.ServiceNetworkConfigurationArgs{
			Subnets:        pulumi.StringArray(args.SubnetIds),
			SecurityGroups: pulumi.StringArray{args.SecurityGroupId},
			AssignPublicIp: pulumi.Bool(false),
		},
		LoadBalancers: ecs.ServiceLoadBalancerArray{
			ecs.ServiceLoadBalancerArgs{
				TargetGroupArn: args.TargetGroupArn,
				ContainerName:  pulumi.String(listenerContainerName),
				ContainerPort:  pulumi.Int(listenerPort),
			},
		},
		ServiceRegistries: ecs.ServiceServiceRegistriesArgs{RegistryArn: cloudMap.Arn},
		Tags:              tags,
	}, append(slices.Clip(child),
		dependsOnInput(args.NatsServiceArn),
		pulumi.Timeouts(&pulumi.CustomTimeouts{Create: "15m", Update: "15m"}),
	)...)
	if err != nil {
		return nil, err
	}

	for _, signal := range logSignals {
		for _, filter := range signal.filters {
			if _, err := cloudwatch.NewLogMetricFilter(ctx, name+"-"+filter.name, &cloudwatch.LogMetricFilterArgs{
				LogGroupName: logGroup.Name,
				Pattern:      pulumi.String(filter.pattern),
				MetricTransformation: cloudwatch.LogMetricFilterMetricTransformationArgs{
					Name:         pulumi.String(signal.metric),
					Namespace:    pulumi.String(args.MetricNamespace),
					Value:        pulumi.String("1"),
					DefaultValue: pulumi.String("0"),
				},
			}, child...); err != nil {
				return nil, err
			}
		}
		if _, err := cloudwatch.NewMetricAlarm(ctx, name+"-"+signal.alarm, &cloudwatch.MetricAlarmArgs{
			AlarmDescription:   pulumi.String(fmt.Sprintf(signal.description, args.LogGroupName)),
			Namespace:          pulumi.String(args.MetricNamespace),
			MetricName:         pulumi.String(signal.metric),
			Statistic:          pulumi.String("Sum"),
			Period:             pulumi.Int(300),
			EvaluationPeriods:  pulumi.Int(1),
			Threshold:          pulumi.Float64(1),
			ComparisonOperator: pulumi.String("GreaterThanOrEqualToThreshold"),
			TreatMissingData:   pulumi.String("notBreaching"),
			AlarmActions:       pulumi.Array{args.AlarmTopicArn},
			OkActions:          pulumi.Array{args.AlarmTopicArn},
			Tags:               tags,
		}, child...); err != nil {
			return nil, err
		}
	}

	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it. An ECS service's id is its ARN.
	comp.ServiceArn = service.ID().ToStringOutput()
	comp.ServiceName = service.Name
	comp.LogGroupName = logGroup.Name
	comp.TaskDefinitionArn = task.Arn
	comp.InternalUrl = pulumi.Sprintf("http://%s.%s:%d", cloudMap.Name, args.CloudMapNamespaceName, listenerPort)
	return comp, nil
}

// validate refuses the inputs NewWebhookListener cannot build a working listener from, each error
// naming its input. A webhook input is checked against webhooks before the list itself, so each
// refusal is the one its input earns: a listed webhook's missing input, an unlisted webhook's
// input that would otherwise be dropped silently, then a list this version does not take.
func (args *WebhookListenerArgs) validate() error {
	set := func(v *string) bool { return v != nil && strings.TrimSpace(*v) != "" }
	for _, in := range []struct {
		webhook, field string
		given, set     bool // given at all; given with a value the listener can use
	}{
		{"github", "githubWebhookSecretArn", args.GitHubWebhookSecretArn != nil, args.GitHubWebhookSecretArn != nil},
		// The listener refuses to start without it.
		{"github", "reviewerAppId", args.ReviewerAppId != nil, set(args.ReviewerAppId)},
		// The listener would fall back to a trigger of its own without it.
		{"github", "mentionTrigger", args.MentionTrigger != nil, set(args.MentionTrigger)},
		{"slack", "slackSigningSecretArn", args.SlackSigningSecretArn != nil, args.SlackSigningSecretArn != nil},
	} {
		listed := slices.Contains(args.Webhooks, in.webhook)
		if listed && !in.set {
			return fmt.Errorf("%s is required with webhook %q", in.field, in.webhook)
		}
		if !listed && in.given {
			return fmt.Errorf("%s is given for webhook %q, which webhooks does not list", in.field, in.webhook)
		}
	}
	if !slices.Equal(args.Webhooks, supportedWebhooks) {
		return fmt.Errorf("webhooks %q is not supported: this version takes exactly %q", args.Webhooks, supportedWebhooks)
	}
	// The environment's fields known at construct; natsUrls may be known only later, and the
	// container definitions' apply refuses a bad one.
	if err := (core.ListenerConfig{MachineID: args.MachineId, Port: listenerPort, HostBridge: args.HostBridge}).CheckPlain(); err != nil {
		return err
	}
	// The listener binds 127.0.0.1 when ENVOY_LISTEN_HOST is blank, where the target group cannot
	// reach it.
	if strings.TrimSpace(args.ListenHost) == "" {
		return errors.New("listenHost is required: the listener binds 127.0.0.1 without it")
	}
	if args.NatsReplicas < 1 {
		return fmt.Errorf("natsReplicas %d is below 1", args.NatsReplicas)
	}
	if len(args.SubnetIds) == 0 {
		return errors.New("subnetIds names no subnet: the service has nowhere to place its task")
	}
	if _, ok := args.BaseTags["service"]; ok {
		return errors.New(`baseTags holds "service", which the component sets on each child`)
	}
	return nil
}

// dependsOnInput orders a child after the resources in's value comes from. A value used only for
// ordering reaches no input of the child, so the dependencies a consumer's output carries into the
// component would otherwise be lost; DependsOnInputs takes the dependencies of the output it is
// given, which are in's.
func dependsOnInput(in pulumi.StringInput) pulumi.ResourceOption {
	return pulumi.DependsOnInputs(in.ToStringOutput().ApplyT(func(string) []pulumi.Resource { return nil }).(pulumi.ResourceArrayOutput))
}

// secretReadPolicy is the execution-role policy that reads the secret at arn into a container, as
// the reference's json.dumps writes it.
func secretReadPolicy(arn string) string {
	return core.PyJSON(core.Object{
		{Key: "Version", Value: "2012-10-17"},
		{Key: "Statement", Value: []core.Object{{
			{Key: "Effect", Value: "Allow"},
			{Key: "Action", Value: []string{"secretsmanager:GetSecretValue"}},
			{Key: "Resource", Value: arn},
		}}},
	}, core.PyJSONOptions{EnsureASCII: true})
}

// listenerContainer holds what the listener task's container takes from the component's inputs
// and children.
type listenerContainer struct {
	Image          string
	MachineId      string
	NatsUrls       []string
	ListenHost     string
	HostBridge     string
	NatsReplicas   int
	Webhooks       []string
	MentionTrigger string // written only with github
	ReviewerAppId  string // written only with github

	GitHubWebhookSecretArn string // written only with github
	SlackSigningSecretArn  string // written only with slack
	ApiTokenSecretArn      string
	SeedSecretArn          *string // nil injects no seed

	LogGroup string
	Region   string
}

// listenerContainerDefinitions returns the listener task's container definitions. The AWS
// provider compares every string in them byte for byte, so they are written as json.dumps writes
// the reference's list: its dict order, ensure_ascii on. The environment is core.ListenerEnv's,
// with the listen host after the port where the reference lists it, then the replicas and the
// webhooks: ENVOY_WEBHOOKS only when one is listed, the GitHub variables only with github, and
// each webhook's secret only with its webhook.
func listenerContainerDefinitions(c listenerContainer) (string, error) {
	env, err := core.ListenerEnv(core.ListenerConfig{
		MachineID: c.MachineId, NATSURLs: c.NatsUrls, Port: listenerPort, HostBridge: c.HostBridge,
	})
	if err != nil {
		return "", err
	}
	gitHub, slack := slices.Contains(c.Webhooks, "github"), slices.Contains(c.Webhooks, "slack")

	var variables []core.Object
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		variables = append(variables, environment(name, value))
		if name == "PORT" {
			variables = append(variables, environment("ENVOY_LISTEN_HOST", c.ListenHost))
		}
	}
	variables = append(variables, environment("ENVOY_NATS_REPLICAS", strconv.Itoa(c.NatsReplicas)))
	if len(c.Webhooks) > 0 {
		variables = append(variables, environment("ENVOY_WEBHOOKS", strings.Join(c.Webhooks, ",")))
	}
	if gitHub {
		variables = append(variables,
			environment("ENVOY_GITHUB_MENTION_TRIGGER", c.MentionTrigger),
			environment("ENVOY_REVIEWER_APP_ID", c.ReviewerAppId),
		)
	}

	var secrets []core.Object
	if gitHub {
		secrets = append(secrets, secret("ENVOY_GITHUB_WEBHOOK_SECRET", c.GitHubWebhookSecretArn))
	}
	if slack {
		secrets = append(secrets, secret("ENVOY_SLACK_SIGNING_SECRET", c.SlackSigningSecretArn))
	}
	secrets = append(secrets, secret("ENVOY_API_TOKEN", c.ApiTokenSecretArn))
	if c.SeedSecretArn != nil {
		secrets = append(secrets, secret(core.EnvNATSSeed, *c.SeedSecretArn))
	}

	listener := core.Object{
		{Key: "name", Value: listenerContainerName},
		{Key: "image", Value: c.Image},
		{Key: "essential", Value: true},
		{Key: "command", Value: []string{"/usr/local/bin/envoy-listener"}},
		{Key: "portMappings", Value: []core.Object{portMapping(listenerPort)}},
		{Key: "environment", Value: variables},
		{Key: "secrets", Value: secrets},
		// `hostname -i` is the task's ENI address in awsvpc mode, the interface the target group
		// probes, so a listener bound to loopback fails here too. It prints every address the
		// name resolves to; the first one keeps a dual-stack ENI from breaking the probe.
		{Key: "healthCheck", Value: healthCheck(
			fmt.Sprintf(`curl -sf "http://$(hostname -i | cut -d' ' -f1):%d/healthz" || exit 1`, listenerPort), 30)},
		{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, listenerContainerName)},
	}
	return core.PyJSON([]core.Object{listener}, core.PyJSONOptions{EnsureASCII: true}), nil
}

func secret(name, arn string) core.Object {
	return core.Object{{Key: "name", Value: name}, {Key: "valueFrom", Value: arn}}
}

// logSignal is one kind of listener log line that pages: its filters count the line's shapes into
// one metric, and one alarm pages on that metric.
type logSignal struct {
	alarm, metric string // each named after the component's name: <name>-<alarm>
	filters       []struct{ name, pattern string }
	description   string // the log group's name is written at %s
}

// logSignals are the listener's log lines that page. Each alarm pages on one matching line in
// five minutes and delivers the OK transition too, so the channel sees the signal stop: the next
// period is 0 (lines arrived, none matched, the filters' default value) or missing (no lines,
// not breaching), and either clears it. A stopped listener is not these alarms' signal.
//
// The JSON lines are matched by field selectors and the plain lines are anchored on their date and
// time: a quoted or unanchored term would also match an INFO line whose payload quotes the phrase.
var logSignals = []logSignal{
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
