package aws

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/servicediscovery"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/internal/providers"
)

// webhookListenerType is the component's token, registered as a constant for the reason
// natsServiceType gives.
const webhookListenerType = "envoy-infra:aws:WebhookListener"

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
	// The ENVOY_WEBHOOKS list; this version takes [] or exactly ["github", "slack"], and refuses it
	// omitted. Each webhook's inputs are required while it is listed and refused while it is not.
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
			Policy: arn.ApplyT(rendering.SecretReadPolicy).(pulumi.StringOutput),
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
		c := rendering.ListenerContainer{
			Image:                  args.Image,
			Env:                    args.environment(v[0].([]string)),
			GitHubWebhookSecretArn: v[3].(string),
			SlackSigningSecretArn:  v[4].(string),
			ApiTokenSecretArn:      v[2].(string),
			LogGroup:               v[1].(string),
			Region:                 args.Region,
		}
		if args.SeedSecretArn != nil {
			seed := v[5].(string)
			c.SeedSecretArn = &seed
		}
		definitions, err := rendering.ListenerContainerDefinitions(c)
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
				ContainerName:  pulumi.String(rendering.ListenerContainerName),
				ContainerPort:  pulumi.Int(rendering.ListenerPort),
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

	for _, signal := range rendering.ListenerLogSignals {
		for _, filter := range signal.MetricFilters(name, args.MetricNamespace) {
			if err := newMetricFilter(ctx, filter, logGroup.Name, child); err != nil {
				return nil, err
			}
		}
		if _, err := cloudwatch.NewMetricAlarm(ctx, signal.AlarmName(name), &cloudwatch.MetricAlarmArgs{
			AlarmDescription:   pulumi.String(signal.Description(args.LogGroupName)),
			Namespace:          pulumi.String(args.MetricNamespace),
			MetricName:         pulumi.String(signal.MetricName()),
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
	comp.InternalUrl = pulumi.Sprintf("http://%s.%s:%d", cloudMap.Name, args.CloudMapNamespaceName, rendering.ListenerPort)
	return comp, nil
}

// validate refuses the inputs NewWebhookListener cannot build a working listener from, each error
// naming its input: an omitted webhooks, then each webhook's secret ARN against webhooks, then the
// environment's plain inputs (rendering.ListenerEnv.Check), then the service's.
func (args *WebhookListenerArgs) validate() error {
	// ListenerEnv.Check reads a nil list as [], which `render webhook-listener-env` relies on for no
	// --webhook; webhooks is a required input, so the component refuses one never given.
	if args.Webhooks == nil {
		return errors.New("webhooks is required: give [] for a listener with no webhook")
	}
	if err := rendering.CheckWebhookInputs(args.Webhooks, []rendering.WebhookInput{
		{Webhook: "github", Field: "githubWebhookSecretArn", Given: args.GitHubWebhookSecretArn != nil, Set: args.GitHubWebhookSecretArn != nil},
		{Webhook: "slack", Field: "slackSigningSecretArn", Given: args.SlackSigningSecretArn != nil, Set: args.SlackSigningSecretArn != nil},
	}); err != nil {
		return err
	}
	// natsUrls may be known only later; the container definitions' apply refuses a bad one.
	if err := args.environment(nil).Check(); err != nil {
		return err
	}
	if len(args.SubnetIds) == 0 {
		return errors.New("subnetIds names no subnet: the service has nowhere to place its task")
	}
	if _, ok := args.BaseTags["service"]; ok {
		return errors.New(`baseTags holds "service", which the component sets on each child`)
	}
	return nil
}

// environment returns the inputs the container's environment is made of, with natsUrls.
func (args *WebhookListenerArgs) environment(natsUrls []string) rendering.ListenerEnv {
	return rendering.ListenerEnv{
		MachineId:      args.MachineId,
		NatsUrls:       natsUrls,
		ListenHost:     args.ListenHost,
		HostBridge:     args.HostBridge,
		NatsReplicas:   args.NatsReplicas,
		Webhooks:       args.Webhooks,
		ReviewerAppId:  args.ReviewerAppId,
		MentionTrigger: args.MentionTrigger,
	}
}

// dependsOnInput orders a child after the resources in's value comes from. A value used only for
// ordering reaches no input of the child, so the dependencies a consumer's output carries into the
// component would otherwise be lost; DependsOnInputs takes the dependencies of the output it is
// given, which are in's.
func dependsOnInput(in pulumi.StringInput) pulumi.ResourceOption {
	return pulumi.DependsOnInputs(in.ToStringOutput().ApplyT(func(string) []pulumi.Resource { return nil }).(pulumi.ResourceArrayOutput))
}
