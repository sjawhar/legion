// Package aws holds the components that run Envoy on AWS.
package aws

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/efs"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/servicediscovery"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/infra/nats"
)

// natsServiceType is the component's token. pulumi-go-provider derives the same one from the
// NatsService type's package and name; NewNatsService registers with the constant so it also runs
// outside the plugin, where no call carries a token.
const natsServiceType = "envoy-infra:aws:NatsService"

// nfsPort is the port an EFS mount target serves NFS on.
const nfsPort = 2049

// NatsServiceArgs are the inputs of an envoy-infra:aws:NatsService. Every field that holds a value
// of the deployment is required and has no default.
type NatsServiceArgs struct {
	// The authorization, as core.Authorization takes it; nil runs NATS with no authorization block.
	// configHeader and configPath belong to it: required with it, and refused without it, since only
	// its start script writes a configuration.
	Authorization *AuthorizationArgs `pulumi:"authorization,optional"`
	ConfigHeader  []string           `pulumi:"configHeader,optional"` // the rendered config's header lines
	ServerArgs    []string           `pulumi:"serverArgs"`
	ConfigPath    *string            `pulumi:"configPath,optional"` // where the task's shell writes the config

	NatsImage              string             `pulumi:"natsImage"`
	TailscaleImage         string             `pulumi:"tailscaleImage"`
	TailnetHostname        string             `pulumi:"tailnetHostname"`
	TailnetTags            []string           `pulumi:"tailnetTags"`               // joined with "," into --advertise-tags
	TailnetClientSecretArn pulumi.StringInput `pulumi:"tailnetClientSecretArn"`    // the sidecar's OAuth client secret
	TailnetEphemeral       bool               `pulumi:"tailnetEphemeral,optional"` // the auth key's ?ephemeral= value; false unless set

	// Values a consumer feeds from other resources are Inputs: a remote component's construct
	// refuses a plain field whose input carries a dependency (Pulumi SDK v3.265.0
	// go/pulumi/provider.go:765-766), and infer marks a plain field `plain` in the schema.
	// subnetIds is a slice, so the component knows how many mount targets to register: subnet i's
	// are <N>-efs-mt-<i> and <N>-tsnet-efs-mt-<i>, so a consumer adopting an existing deployment
	// passes its subnets in their original order.
	Region           string               `pulumi:"region"` // the containers' awslogs-region
	VpcId            pulumi.StringInput   `pulumi:"vpcId"`
	SubnetIds        []pulumi.StringInput `pulumi:"subnetIds"`
	SecurityGroupId  pulumi.StringInput   `pulumi:"securityGroupId"` // the NATS task's group, built by the consumer
	ClusterArn       pulumi.StringInput   `pulumi:"clusterArn"`
	ExecutionRoleArn pulumi.StringInput   `pulumi:"executionRoleArn"`
	TaskRoleArn      pulumi.StringInput   `pulumi:"taskRoleArn"`

	CloudMapNamespaceId        pulumi.StringInput `pulumi:"cloudMapNamespaceId"`
	CloudMapNamespaceName      pulumi.StringInput `pulumi:"cloudMapNamespaceName"`
	CloudMapServiceName        string             `pulumi:"cloudMapServiceName"`        // the DNS name
	CloudMapServiceLogicalName string             `pulumi:"cloudMapServiceLogicalName"` // the child's Pulumi name

	Family           string `pulumi:"family"`
	LogGroupName     string `pulumi:"logGroupName"`
	LogRetentionDays int    `pulumi:"logRetentionDays"`
	Cpu              string `pulumi:"cpu"`
	Memory           string `pulumi:"memory"`
	PlatformVersion  string `pulumi:"platformVersion"`

	BaseTags        map[string]string `pulumi:"baseTags"` // the component adds `service` per child
	MetricNamespace string            `pulumi:"metricNamespace"`
	// The four log metric filters are named <prefix>-route-disconnect-filter,
	// <prefix>-slow-consumer-filter, <prefix>-jetstream-error-filter and <prefix>-auth-error-filter.
	MetricFilterLogicalNamePrefix string `pulumi:"metricFilterLogicalNamePrefix"`
	WaitForSteadyState            *bool  `pulumi:"waitForSteadyState,optional"` // unset leaves the service's input unset
	// Each child takes over the root-level resource of its own name and type:
	// pulumi.Alias{NoParent: true}.
	Adopt bool `pulumi:"adopt,optional"`
}

// AuthorizationArgs mirror core.Authorization field for field.
type AuthorizationArgs struct {
	Users      []UserArgs `pulumi:"users"`
	NoAuthUser string     `pulumi:"noAuthUser,optional"` // the name of a keyed user without a seed
}

// Annotate names the type aws:Authorization in the schema: generated SDKs append Args to an object
// type's name for its input type, so the Go name would reach consumers as AuthorizationArgsArgs.
func (a *AuthorizationArgs) Annotate(an infer.Annotator) { an.SetToken("aws", "Authorization") }

// UserArgs mirror core.User field for field. Its rules are nats:Rules, the type every NATS
// component shares.
type UserArgs struct {
	Name      string          `pulumi:"name"`
	NKey      string          `pulumi:"nkey,optional"` // the user public key; unset for a pending user, which is never rendered
	Publish   *nats.RulesArgs `pulumi:"publish,optional"`
	Subscribe *nats.RulesArgs `pulumi:"subscribe,optional"`
	HasSeed   bool            `pulumi:"hasSeed,optional"` // the consumer stores a seed for this user
}

// Annotate names the type aws:User in the schema, for the reason AuthorizationArgs's Annotate gives.
func (u *UserArgs) Annotate(an infer.Annotator) { an.SetToken("aws", "User") }

// core returns the authorization as core.Authorization takes it.
func (a *AuthorizationArgs) core() *core.Authorization {
	users := make([]core.User, len(a.Users))
	for i, u := range a.Users {
		users[i] = core.User{Name: u.Name, NKey: u.NKey, Publish: u.Publish.Core(), Subscribe: u.Subscribe.Core(), HasSeed: u.HasSeed}
	}
	return &core.Authorization{Users: users, NoAuthUser: a.NoAuthUser}
}

// NatsService is Envoy's NATS server on ECS Fargate: JetStream on one EFS file system, a Tailscale
// sidecar serving the client port on the tailnet with its state on another, registered in Cloud Map.
type NatsService struct {
	pulumi.ResourceState
	ServiceArn        pulumi.StringOutput `pulumi:"serviceArn"`
	ServiceName       pulumi.StringOutput `pulumi:"serviceName"`
	EfsId             pulumi.StringOutput `pulumi:"efsId"` // JetStream's file system
	LogGroupName      pulumi.StringOutput `pulumi:"logGroupName"`
	TaskDefinitionArn pulumi.StringOutput `pulumi:"taskDefinitionArn"`
	InternalUrl       pulumi.StringOutput `pulumi:"internalUrl"` // nats://<cloud map name>.<namespace>:4222
	MetricNamespace   pulumi.StringOutput `pulumi:"metricNamespace"`
}

// NewNatsService registers the NATS service. Each child is named from name as the reference names
// its root-level resource from envoy-nats, the Cloud Map service by cloudMapServiceLogicalName and
// the metric filters by metricFilterLogicalNamePrefix. It resolves its inputs (validate), then
// requires an aws provider, and only then registers anything. Each Input it registers from fails
// once it resolves to a value holding a seed (refuseSeedInputs).
func NewNatsService(ctx *pulumi.Context, name string, args *NatsServiceArgs, opts ...pulumi.ResourceOption) (*NatsService, error) {
	if err := refuseSeededName(natsServiceType, name); err != nil {
		return nil, err
	}
	launch, err := args.validate()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", natsServiceType, name, err)
	}
	if err := providers.Require(ctx.Context(), natsServiceType, "aws", opts...); err != nil {
		return nil, err
	}
	checked := *args
	refuseSeedInputs(&checked, natsServiceType+" "+name)
	args = &checked

	comp := &NatsService{}
	if err := ctx.RegisterComponentResource(natsServiceType, name, comp, opts...); err != nil {
		return nil, err
	}
	child := childOptions(comp, args.Adopt)
	tags := func(service string) pulumi.StringMap { return serviceTags(args.BaseTags, service) }

	// JetStream's store, mounted at /data, owned by the nats image's user (uid and gid 1000).
	data, err := newFileSystem(ctx, name, "efs", tags(name), args, child, fileSystemShape{
		groupDescription: "EFS mount targets for NATS JetStream storage",
		groupTags:        tags(name + "-efs"),
		ruleNames:        [2]string{name + "-efs-ingress", name + "-nfs-egress"},
		owner:            1000,
		root:             "/nats-data",
		permissions:      "755",
		accessPointTags:  tags(name),
	})
	if err != nil {
		return nil, err
	}

	logGroup, err := newLogGroup(ctx, name, args.LogGroupName, args.LogRetentionDays, tags(name), child)
	if err != nil {
		return nil, err
	}

	// The sidecar's tailnet state, so its node keeps its identity across tasks. containerboot runs
	// as root.
	tsnet, err := newFileSystem(ctx, name, "tsnet-efs", tags(name+"-tsnet"), args, child, fileSystemShape{
		groupDescription: "EFS mount targets for " + name + " tsnet state",
		groupTags:        tags(name + "-tsnet-efs"),
		ruleNames:        [2]string{name + "-tsnet-efs-ingress", name + "-tsnet-nfs-egress"},
		owner:            0,
		root:             "/tsnet-state",
		permissions:      "700",
		accessPointTags:  tags(name + "-tsnet-ap"),
	})
	if err != nil {
		return nil, err
	}

	definitions := pulumi.All(logGroup.Name, args.TailnetClientSecretArn).ApplyT(func(v []any) string {
		logGroupName, tailnetSecretArn := v[0].(string), v[1].(string)
		return rendering.NatsContainerDefinitions(rendering.NatsContainers{
			NatsImage:        args.NatsImage,
			Launch:           launch,
			TailscaleImage:   args.TailscaleImage,
			TailnetHostname:  args.TailnetHostname,
			TailnetTags:      args.TailnetTags,
			TailnetEphemeral: args.TailnetEphemeral,
			TailnetSecretArn: tailnetSecretArn,
			LogGroup:         logGroupName,
			Region:           args.Region,
		})
	}).(pulumi.StringOutput)
	taskArgs := fargateTask{
		family: args.Family, cpu: args.Cpu, memory: args.Memory,
		executionRoleArn: args.ExecutionRoleArn, taskRoleArn: args.TaskRoleArn,
		containerDefinitions: definitions, tags: tags(name),
	}.args()
	taskArgs.Volumes = ecs.TaskDefinitionVolumeArray{
		data.volume("nats-data"),
		tsnet.volume("tsnet-state"),
	}
	task, err := ecs.NewTaskDefinition(ctx, name+"-task", taskArgs, child...)
	if err != nil {
		return nil, err
	}

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
		HealthCheckCustomConfig: servicediscovery.ServiceHealthCheckCustomConfigArgs{
			FailureThreshold: pulumi.Int(1),
		},
		Tags: tags(name),
	}, child...)
	if err != nil {
		return nil, err
	}

	// The old task stops before the new one starts: both mount the same JetStream store and
	// tailnet state, and two at once would corrupt the store and duplicate the tailnet node.
	serviceArgs := &ecs.ServiceArgs{
		Cluster:                         args.ClusterArn,
		TaskDefinition:                  task.Arn,
		DesiredCount:                    pulumi.Int(1),
		LaunchType:                      pulumi.String("FARGATE"),
		PlatformVersion:                 pulumi.String(args.PlatformVersion),
		AvailabilityZoneRebalancing:     pulumi.String("DISABLED"),
		DeploymentMaximumPercent:        pulumi.Int(100),
		DeploymentMinimumHealthyPercent: pulumi.Int(0),
		EnableExecuteCommand:            pulumi.Bool(true),
		NetworkConfiguration: ecs.ServiceNetworkConfigurationArgs{
			Subnets:        pulumi.StringArray(args.SubnetIds),
			SecurityGroups: pulumi.StringArray{args.SecurityGroupId},
			AssignPublicIp: pulumi.Bool(false),
		},
		ServiceRegistries: ecs.ServiceServiceRegistriesArgs{RegistryArn: cloudMap.Arn},
		Tags:              tags(name),
	}
	if args.WaitForSteadyState != nil {
		serviceArgs.WaitForSteadyState = pulumi.Bool(*args.WaitForSteadyState)
	}
	mountTargets := append(append([]pulumi.Resource{}, data.mountTargets...), tsnet.mountTargets...)
	service, err := ecs.NewService(ctx, name+"-service", serviceArgs,
		append(slices.Clip(child), pulumi.DependsOn(mountTargets))...)
	if err != nil {
		return nil, err
	}

	for _, filter := range rendering.NatsMetricFilters(args.MetricFilterLogicalNamePrefix, args.MetricNamespace) {
		if err := newMetricFilter(ctx, filter, logGroup.Name, child); err != nil {
			return nil, err
		}
	}

	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it. An ECS service's id is its ARN.
	comp.ServiceArn = service.ID().ToStringOutput()
	comp.ServiceName = service.Name
	comp.EfsId = data.fileSystem.ID().ToStringOutput()
	comp.LogGroupName = logGroup.Name
	comp.TaskDefinitionArn = task.Arn
	comp.InternalUrl = pulumi.Sprintf("nats://%s.%s:%d", cloudMap.Name, args.CloudMapNamespaceName, rendering.NatsClientPort)
	comp.MetricNamespace = pulumi.String(args.MetricNamespace).ToStringOutput()
	return comp, nil
}

// validate refuses the inputs NewNatsService cannot build a working service from, each error
// naming its input: a plain input holding a seed (seededPlainInput), configHeader and configPath
// against authorization, then the authorization, configHeader and serverArgs (through
// rendering.NatsLaunch), then the service's. It returns the NATS container's launch.
func (args *NatsServiceArgs) validate() ([]core.Field, error) {
	if err := seededPlainInput(*args); err != nil {
		return nil, err
	}
	owner := rendering.Owner{Name: "authorization", Present: args.Authorization != nil, Absent: "which is not given"}
	if err := rendering.CheckOwnedInputs([]rendering.OwnedInput{
		{Owner: owner, Field: "configHeader", Given: args.ConfigHeader != nil, Set: args.ConfigHeader != nil},
		{Owner: owner, Field: "configPath", Given: args.ConfigPath != nil, Set: args.ConfigPath != nil && *args.ConfigPath != ""},
	}); err != nil {
		return nil, err
	}
	var authorization *core.Authorization
	configPath := ""
	if args.Authorization != nil {
		authorization, configPath = args.Authorization.core(), *args.ConfigPath
	}
	launch, err := rendering.NatsLaunch(authorization, args.ConfigHeader, args.ServerArgs, configPath)
	if err != nil {
		return nil, err
	}
	if len(args.SubnetIds) == 0 {
		return nil, errors.New("subnetIds names no subnet: each file system needs a mount target where the task runs")
	}
	if len(args.TailnetTags) == 0 {
		return nil, errors.New("tailnetTags names no tag: an auth key made from an OAuth client must advertise one")
	}
	if err := refuseServiceTag(args.BaseTags); err != nil {
		return nil, err
	}
	return launch, nil
}

// fileSystemShape is what differs between the service's two file systems.
type fileSystemShape struct {
	groupDescription string
	groupTags        pulumi.StringMap
	ruleNames        [2]string // the NFS ingress to the mount targets' group, the NFS egress from the task's
	owner            int       // the access point's POSIX user and group, and its root's owner
	root             string
	permissions      string
	accessPointTags  pulumi.StringMap
}

// fileSystem is one of the service's encrypted EFS file systems with its access point.
type fileSystem struct {
	fileSystem   *efs.FileSystem
	accessPoint  *efs.AccessPoint
	mountTargets []pulumi.Resource
}

// newFileSystem registers <name>-<infix>, its mount targets' security group <name>-<infix>-sg, the
// NFS rules between that group and the task's, one mount target <name>-<infix>-mt-<i> per subnet,
// and the access point <name>-<infix>-ap.
func newFileSystem(ctx *pulumi.Context, name, infix string, tags pulumi.StringMap, args *NatsServiceArgs,
	child []pulumi.ResourceOption, shape fileSystemShape,
) (*fileSystem, error) {
	prefix := name + "-" + infix
	fs, err := efs.NewFileSystem(ctx, prefix, &efs.FileSystemArgs{
		Encrypted:       pulumi.Bool(true),
		PerformanceMode: pulumi.String("generalPurpose"),
		ThroughputMode:  pulumi.String("elastic"),
		Tags:            tags,
	}, child...)
	if err != nil {
		return nil, err
	}
	group, err := ec2.NewSecurityGroup(ctx, prefix+"-sg", &ec2.SecurityGroupArgs{
		VpcId:       args.VpcId,
		Description: pulumi.String(shape.groupDescription),
		Tags:        shape.groupTags,
	}, child...)
	if err != nil {
		return nil, err
	}
	for _, rule := range []struct {
		name, direction string
		group, source   pulumi.StringInput
	}{
		{shape.ruleNames[0], "ingress", group.ID(), args.SecurityGroupId},
		{shape.ruleNames[1], "egress", args.SecurityGroupId, group.ID()},
	} {
		if _, err := ec2.NewSecurityGroupRule(ctx, rule.name, &ec2.SecurityGroupRuleArgs{
			Type:                  pulumi.String(rule.direction),
			SecurityGroupId:       rule.group,
			FromPort:              pulumi.Int(nfsPort),
			ToPort:                pulumi.Int(nfsPort),
			Protocol:              pulumi.String("tcp"),
			SourceSecurityGroupId: rule.source,
		}, child...); err != nil {
			return nil, err
		}
	}
	result := &fileSystem{fileSystem: fs}
	for i, subnet := range args.SubnetIds {
		target, err := efs.NewMountTarget(ctx, prefix+"-mt-"+strconv.Itoa(i), &efs.MountTargetArgs{
			FileSystemId:   fs.ID(),
			SubnetId:       subnet,
			SecurityGroups: pulumi.StringArray{group.ID()},
		}, child...)
		if err != nil {
			return nil, err
		}
		result.mountTargets = append(result.mountTargets, target)
	}
	result.accessPoint, err = efs.NewAccessPoint(ctx, prefix+"-ap", &efs.AccessPointArgs{
		FileSystemId: fs.ID(),
		PosixUser:    efs.AccessPointPosixUserArgs{Uid: pulumi.Int(shape.owner), Gid: pulumi.Int(shape.owner)},
		RootDirectory: efs.AccessPointRootDirectoryArgs{
			Path: pulumi.String(shape.root),
			CreationInfo: efs.AccessPointRootDirectoryCreationInfoArgs{
				OwnerUid:    pulumi.Int(shape.owner),
				OwnerGid:    pulumi.Int(shape.owner),
				Permissions: pulumi.String(shape.permissions),
			},
		},
		Tags: shape.accessPointTags,
	}, child...)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// volume is the task definition's volume on this file system through its access point, with
// transit encryption and no IAM authorization.
func (f *fileSystem) volume(name string) ecs.TaskDefinitionVolumeArgs {
	return ecs.TaskDefinitionVolumeArgs{
		Name: pulumi.String(name),
		EfsVolumeConfiguration: ecs.TaskDefinitionVolumeEfsVolumeConfigurationArgs{
			FileSystemId:      f.fileSystem.ID(),
			TransitEncryption: pulumi.String("ENABLED"),
			AuthorizationConfig: ecs.TaskDefinitionVolumeEfsVolumeConfigurationAuthorizationConfigArgs{
				AccessPointId: f.accessPoint.ID(),
				Iam:           pulumi.String("DISABLED"),
			},
		},
	}
}
