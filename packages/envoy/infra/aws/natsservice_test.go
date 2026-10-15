package aws_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	envoynats "github.com/sjawhar/envoy/infra/nats"
)

const (
	natsServiceType     = "envoy-infra:aws:NatsService"
	awsProviderType     = "pulumi:providers:aws"
	fileSystemType      = "aws:efs/fileSystem:FileSystem"
	securityGroupType   = "aws:ec2/securityGroup:SecurityGroup"
	ruleType            = "aws:ec2/securityGroupRule:SecurityGroupRule"
	mountTargetType     = "aws:efs/mountTarget:MountTarget"
	accessPointType     = "aws:efs/accessPoint:AccessPoint"
	logGroupType        = "aws:cloudwatch/logGroup:LogGroup"
	taskDefinitionType  = "aws:ecs/taskDefinition:TaskDefinition"
	cloudMapServiceType = "aws:servicediscovery/service:Service"
	ecsServiceType      = "aws:ecs/service:Service"
	metricFilterType    = "aws:cloudwatch/logMetricFilter:LogMetricFilter"

	// componentURN is the URN of the component every test constructs, named example-nats.
	componentURN = "urn:pulumi:s::p::" + natsServiceType + "::example-nats"
	// providerURN is the aws provider every test passes it.
	providerURN = "urn:pulumi:s::p::" + awsProviderType + "::example-aws"
	secretArn   = "arn:aws:secretsmanager:example-region-1:000000000000:secret:example/tailnet-oauth"
)

// newMocks returns the mock engine the AWS components' tests run on. It answers as the AWS provider
// would for what a component reads back: an arn for a task definition and a Cloud Map service, and
// for an ECS service, which the component does not name, an autoname and its ARN as its id.
func newMocks() *pulumitest.Mocks {
	return &pulumitest.Mocks{Computed: func(args pulumi.MockResourceArgs) (string, resource.PropertyMap) {
		switch args.TypeToken {
		case taskDefinitionType, cloudMapServiceType:
			return "", resource.PropertyMap{"arn": resource.NewProperty("arn:example:" + args.Name)}
		case ecsServiceType:
			return "arn:example:" + args.Name, resource.PropertyMap{"name": resource.NewProperty(args.Name + "-autonamed")}
		}
		return "", nil
	}}
}

// childrenOf returns the component's children m registered, by name, failing on a name registered
// twice.
func childrenOf(t *testing.T, m *pulumitest.Mocks) map[string]pulumitest.Registration {
	t.Helper()
	children := map[string]pulumitest.Registration{}
	for _, r := range m.Registrations() {
		if r.Type == awsProviderType || strings.HasPrefix(r.Type, "envoy-infra:") {
			continue
		}
		_, dup := children[r.Name]
		require.False(t, dup, "%s is registered twice", r.Name)
		children[r.Name] = r
	}
	return children
}

// awaitOutputs awaits each of outputs inside the program and returns its value under its schema
// name: an output made from a plain value joins no work group, so RunErr does not wait for an
// apply on it.
func awaitOutputs(ctx *pulumi.Context, outputs map[string]pulumi.StringOutput) (map[string]string, error) {
	values := map[string]string{}
	for name, output := range outputs {
		result, err := internals.UnsafeAwaitOutput(ctx.Context(), output)
		if err != nil {
			return nil, err
		}
		values[name] = result.Value.(string)
	}
	return values, nil
}

// exampleOutputs are the outputs exampleArgs give, under their schema names: in a run under mocks,
// and in a Construct preview whose inputs are known.
var exampleOutputs = map[string]string{
	"serviceArn":        "arn:example:example-nats-service",
	"serviceName":       "example-nats-service-autonamed",
	"efsId":             "example-nats-efs-id",
	"logGroupName":      "/ecs/envoy-nats",
	"taskDefinitionArn": "arn:example:example-nats-task",
	"internalUrl":       "nats://example-bus.example-namespace.internal.example:4222",
	"metricNamespace":   "example/NATS",
}

// run constructs a component named example-nats with args, passing it an aws provider when
// withProvider is set, and returns what the mocks saw, the component's outputs and the run's error.
func run(t *testing.T, args *envoyaws.NatsServiceArgs, withProvider bool) (*pulumitest.Mocks, map[string]string, error) {
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
		n, err := envoyaws.NewNatsService(ctx, "example-nats", args, opts...)
		if err != nil {
			return err
		}
		out, err = awaitOutputs(ctx, map[string]pulumi.StringOutput{
			"serviceArn": n.ServiceArn, "serviceName": n.ServiceName, "efsId": n.EfsId,
			"logGroupName": n.LogGroupName, "taskDefinitionArn": n.TaskDefinitionArn,
			"internalUrl": n.InternalUrl, "metricNamespace": n.MetricNamespace,
		})
		return err
	})
	return m, out, err
}

// exampleAuthorization is Task 2.1's placeholder authorization: public keys only.
func exampleAuthorization() *envoyaws.AuthorizationArgs {
	return &envoyaws.AuthorizationArgs{
		Users: []envoyaws.UserArgs{
			{
				Name:      "example-listener",
				NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
				Publish:   &envoynats.RulesArgs{Allow: []string{"envoy.example.>"}, Deny: []string{"envoy.example.private"}},
				Subscribe: &envoynats.RulesArgs{Allow: []string{"_INBOX.>", "notifications.café.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-daemon",
				NKey:      "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG",
				Publish:   &envoynats.RulesArgs{Allow: []string{"$JS.API.>"}},
				Subscribe: &envoynats.RulesArgs{Allow: []string{"_INBOX.>", "legion.work workers"}},
				HasSeed:   true,
			},
			{
				Name:      "example-fallback",
				NKey:      "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD",
				Publish:   &envoynats.RulesArgs{Allow: []string{">"}},
				Subscribe: &envoynats.RulesArgs{Allow: []string{">"}},
			},
			{Name: "example-pending", HasSeed: true},
		},
		NoAuthUser: "example-fallback",
	}
}

// `envoy-infra render nats-start-script` reads the authorization input into core's types by their
// JSON keys, so each core field's key is the schema's name of the input field it mirrors.
func TestCoreAuthorizationKeysAreTheSchemasNames(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[envoyaws.AuthorizationArgs](), reflect.TypeFor[core.Authorization]()},
		{reflect.TypeFor[envoyaws.UserArgs](), reflect.TypeFor[core.User]()},
		{reflect.TypeFor[envoynats.RulesArgs](), reflect.TypeFor[core.Rules]()},
	} {
		args, mirror := pair[0], pair[1]
		require.Equal(t, args.NumField(), mirror.NumField(), "%s mirrors %s field for field", args, mirror)
		for i := range args.NumField() {
			field := args.Field(i)
			coreField, ok := mirror.FieldByName(field.Name)
			require.True(t, ok, "%s has %s", mirror, field.Name)
			schemaName, _, _ := strings.Cut(field.Tag.Get("pulumi"), ",")
			require.Equal(t, schemaName, coreField.Tag.Get("json"), "%s.%s's JSON key", mirror, field.Name)
		}
	}
}

// exampleArgs are placeholder inputs. The container definitions' inputs are the goldens'.
func exampleArgs() *envoyaws.NatsServiceArgs {
	return &envoyaws.NatsServiceArgs{
		Authorization: exampleAuthorization(),
		ConfigHeader:  []string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."},
		ServerArgs:    []string{"--jetstream", "--store_dir", "/data", "--port", "4222", "--http_port", "8222"},
		ConfigPath:    pulumi.StringRef("/tmp/nats-server.conf"),

		NatsImage:              "example.registry/nats:placeholder",
		TailscaleImage:         "example.registry/tailscale@sha256:" + strings.Repeat("0", 64),
		TailnetHostname:        "envoy-nats",
		TailnetTags:            []string{"tag:example-a", "tag:example-b"},
		TailnetClientSecretArn: pulumi.String(secretArn),

		Region:           "example-region-1",
		VpcId:            pulumi.String("example-vpc"),
		SubnetIds:        []pulumi.StringInput{pulumi.String("example-subnet-a"), pulumi.String("example-subnet-b")},
		SecurityGroupId:  pulumi.String("example-nats-sg"),
		ClusterArn:       pulumi.String("example-cluster-arn"),
		ExecutionRoleArn: pulumi.String("example-execution-role-arn"),
		TaskRoleArn:      pulumi.String("example-task-role-arn"),

		CloudMapNamespaceId:        pulumi.String("example-namespace-id"),
		CloudMapNamespaceName:      pulumi.String("example-namespace.internal.example"),
		CloudMapServiceName:        "example-bus",
		CloudMapServiceLogicalName: "example-nats-cloudmap",

		Family:           "example-nats-family",
		LogGroupName:     "/ecs/envoy-nats",
		LogRetentionDays: 7,
		Cpu:              "256",
		Memory:           "1024",
		PlatformVersion:  "LATEST",

		BaseTags:                      map[string]string{"project": "example-project", "env": "example-env"},
		MetricNamespace:               "example/NATS",
		MetricFilterLogicalNamePrefix: "example-bus",
	}
}

// noAuthorization makes args a service with no authorization: none of authorization, configHeader
// and configPath given. Its container definitions are nats-unauthorized.golden.
func noAuthorization(a *envoyaws.NatsServiceArgs) {
	a.Authorization, a.ConfigHeader, a.ConfigPath = nil, nil, nil
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(text)
}

// tags are the base tags with the service a child is tagged with.
func tags(service string) map[string]any {
	return map[string]any{"project": "example-project", "env": "example-env", "service": service}
}

// child is a registration a test expects: its type and its inputs as plain Go values.
type child struct {
	typ    string
	inputs map[string]any
}

func fileSystem(service string) child {
	return child{fileSystemType, map[string]any{
		"encrypted": true, "performanceMode": "generalPurpose", "throughputMode": "elastic", "tags": tags(service),
	}}
}

func securityGroup(description, service string) child {
	return child{securityGroupType, map[string]any{"vpcId": "example-vpc", "description": description, "tags": tags(service)}}
}

func nfsRule(direction, group, source string) child {
	return child{ruleType, map[string]any{
		"type": direction, "securityGroupId": group, "fromPort": float64(2049), "toPort": float64(2049),
		"protocol": "tcp", "sourceSecurityGroupId": source,
	}}
}

func mountTarget(fileSystem, subnet, group string) child {
	return child{mountTargetType, map[string]any{
		"fileSystemId": fileSystem, "subnetId": subnet, "securityGroups": []any{group},
	}}
}

func accessPoint(fileSystem string, id float64, path, permissions, service string) child {
	return child{accessPointType, map[string]any{
		"fileSystemId": fileSystem,
		"posixUser":    map[string]any{"uid": id, "gid": id},
		"rootDirectory": map[string]any{
			"path":         path,
			"creationInfo": map[string]any{"ownerUid": id, "ownerGid": id, "permissions": permissions},
		},
		"tags": tags(service),
	}}
}

func efsVolume(name, fileSystem, accessPoint string) map[string]any {
	return map[string]any{"name": name, "efsVolumeConfiguration": map[string]any{
		"fileSystemId": fileSystem, "transitEncryption": "ENABLED",
		"authorizationConfig": map[string]any{"accessPointId": accessPoint, "iam": "DISABLED"},
	}}
}

func metricFilter(pattern, metric string) child {
	return child{metricFilterType, map[string]any{
		"logGroupName": "/ecs/envoy-nats",
		"pattern":      pattern,
		"metricTransformation": map[string]any{
			"name": metric, "namespace": "example/NATS", "value": "1", "defaultValue": "0",
		},
	}}
}

// exampleChildren are the children exampleArgs register: the reference's resources with their
// inputs, each named from the component's name as the reference names it from envoy-nats, the
// task definition's container definitions those of the golden named golden.
func exampleChildren(t *testing.T, golden string) map[string]child {
	return map[string]child{
		"example-nats-efs":         fileSystem("example-nats"),
		"example-nats-efs-sg":      securityGroup("EFS mount targets for NATS JetStream storage", "example-nats-efs"),
		"example-nats-efs-ingress": nfsRule("ingress", "example-nats-efs-sg-id", "example-nats-sg"),
		"example-nats-nfs-egress":  nfsRule("egress", "example-nats-sg", "example-nats-efs-sg-id"),
		"example-nats-efs-mt-0":    mountTarget("example-nats-efs-id", "example-subnet-a", "example-nats-efs-sg-id"),
		"example-nats-efs-mt-1":    mountTarget("example-nats-efs-id", "example-subnet-b", "example-nats-efs-sg-id"),
		"example-nats-efs-ap":      accessPoint("example-nats-efs-id", 1000, "/nats-data", "755", "example-nats"),
		"example-nats-log-group": {logGroupType, map[string]any{
			"name": "/ecs/envoy-nats", "retentionInDays": float64(7), "tags": tags("example-nats"),
		}},
		"example-nats-tsnet-efs":         fileSystem("example-nats-tsnet"),
		"example-nats-tsnet-efs-sg":      securityGroup("EFS mount targets for example-nats tsnet state", "example-nats-tsnet-efs"),
		"example-nats-tsnet-efs-ingress": nfsRule("ingress", "example-nats-tsnet-efs-sg-id", "example-nats-sg"),
		"example-nats-tsnet-nfs-egress":  nfsRule("egress", "example-nats-sg", "example-nats-tsnet-efs-sg-id"),
		"example-nats-tsnet-efs-mt-0":    mountTarget("example-nats-tsnet-efs-id", "example-subnet-a", "example-nats-tsnet-efs-sg-id"),
		"example-nats-tsnet-efs-mt-1":    mountTarget("example-nats-tsnet-efs-id", "example-subnet-b", "example-nats-tsnet-efs-sg-id"),
		"example-nats-tsnet-efs-ap":      accessPoint("example-nats-tsnet-efs-id", 0, "/tsnet-state", "700", "example-nats-tsnet-ap"),
		"example-nats-task": {taskDefinitionType, map[string]any{
			"family":                  "example-nats-family",
			"cpu":                     "256",
			"memory":                  "1024",
			"networkMode":             "awsvpc",
			"requiresCompatibilities": []any{"FARGATE"},
			"executionRoleArn":        "example-execution-role-arn",
			"taskRoleArn":             "example-task-role-arn",
			"containerDefinitions":    readGolden(t, golden),
			"volumes": []any{
				efsVolume("nats-data", "example-nats-efs-id", "example-nats-efs-ap-id"),
				efsVolume("tsnet-state", "example-nats-tsnet-efs-id", "example-nats-tsnet-efs-ap-id"),
			},
			"tags": tags("example-nats"),
		}},
		"example-nats-cloudmap": {cloudMapServiceType, map[string]any{
			"name":        "example-bus",
			"namespaceId": "example-namespace-id",
			"dnsConfig": map[string]any{
				"namespaceId":   "example-namespace-id",
				"routingPolicy": "MULTIVALUE",
				"dnsRecords":    []any{map[string]any{"type": "A", "ttl": float64(10)}},
			},
			"healthCheckCustomConfig": map[string]any{"failureThreshold": float64(1)},
			"tags":                    tags("example-nats"),
		}},
		"example-nats-service": {ecsServiceType, map[string]any{
			"cluster":                         "example-cluster-arn",
			"taskDefinition":                  "arn:example:example-nats-task",
			"desiredCount":                    float64(1),
			"launchType":                      "FARGATE",
			"platformVersion":                 "LATEST",
			"availabilityZoneRebalancing":     "DISABLED",
			"deploymentMaximumPercent":        float64(100),
			"deploymentMinimumHealthyPercent": float64(0),
			"enableExecuteCommand":            true,
			"networkConfiguration": map[string]any{
				"subnets":        []any{"example-subnet-a", "example-subnet-b"},
				"securityGroups": []any{"example-nats-sg"},
				"assignPublicIp": false,
			},
			"serviceRegistries": map[string]any{"registryArn": "arn:example:example-nats-cloudmap"},
			"tags":              tags("example-nats"),
		}},
		"example-bus-route-disconnect-filter": metricFilter(`"Router connection closed"`, "RouteDisconnections"),
		"example-bus-slow-consumer-filter":    metricFilter(`"Slow Consumer Detected"`, "SlowConsumers"),
		"example-bus-jetstream-error-filter": metricFilter(
			`?"JetStream out of" ?"JetStream failed to store" ?"JetStream being DISABLED" ?"NO quorum, stalled"`,
			"JetStreamErrors"),
		"example-bus-auth-error-filter": metricFilter(
			`?"authentication error" ?"Subscription Violation" ?"Publish Violation"`, "AuthErrors"),
	}
}

// requireChildren requires the registered children to be exactly want: type, name and inputs.
func requireChildren(t *testing.T, want map[string]child, got map[string]pulumitest.Registration) {
	t.Helper()
	require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got)), "children's names")
	for name, w := range want {
		require.Equal(t, w.typ, got[name].Type, "%s's type", name)
		require.Equal(t, w.inputs, got[name].Inputs.Mappable(), "%s's inputs", name)
	}
}

// The children, their inputs and the outputs, with and without authorization: only the task
// definition's container definitions differ, each the reference's golden.
func TestNatsServiceRegistersTheReferenceChildren(t *testing.T) {
	for _, row := range []struct {
		golden string
		mutate func(*envoyaws.NatsServiceArgs)
	}{
		{"nats-authorized.golden", func(*envoyaws.NatsServiceArgs) {}},
		{"nats-unauthorized.golden", noAuthorization},
	} {
		t.Run(row.golden, func(t *testing.T) {
			args := exampleArgs()
			row.mutate(args)
			m, out, err := run(t, args, true)
			require.NoError(t, err)

			require.Equal(t, "example-nats", m.One(t, natsServiceType).Name)
			children := childrenOf(t, m)
			requireChildren(t, exampleChildren(t, row.golden), children)
			for name, c := range children {
				require.Equal(t, componentURN, c.RPC.GetParent(), "%s's parent is the component", name)
				require.Contains(t, c.RPC.GetProvider(), providerURN+"::", "%s uses the aws provider the component was given", name)
				require.Empty(t, c.RPC.GetAliases(), "%s is adopted from nothing", name)
			}
			require.Equal(t, exampleOutputs, out)
		})
	}
}

// The service tag is derived from the component's name for each child, as the reference derives
// it from envoy-nats; the rules, mount targets and metric filters carry no tags.
func TestNatsServiceTagsEachChildWithItsService(t *testing.T) {
	m, _, err := run(t, exampleArgs(), true)
	require.NoError(t, err)
	children := childrenOf(t, m)

	for _, row := range []struct{ child, service string }{
		{"example-nats-efs", "example-nats"},
		{"example-nats-efs-sg", "example-nats-efs"},
		{"example-nats-efs-ap", "example-nats"},
		{"example-nats-log-group", "example-nats"},
		{"example-nats-tsnet-efs", "example-nats-tsnet"},
		{"example-nats-tsnet-efs-sg", "example-nats-tsnet-efs"},
		{"example-nats-tsnet-efs-ap", "example-nats-tsnet-ap"},
		{"example-nats-task", "example-nats"},
		{"example-nats-cloudmap", "example-nats"},
		{"example-nats-service", "example-nats"},
	} {
		t.Run(row.child, func(t *testing.T) {
			require.Equal(t, tags(row.service), children[row.child].Inputs["tags"].Mappable())
		})
	}
	for name, c := range children {
		switch c.Type {
		case ruleType, mountTargetType, metricFilterType:
			require.NotContains(t, c.Inputs, resource.PropertyKey("tags"), "%s carries no tags", name)
		}
	}
}

func TestNatsServiceECSServiceDependsOnEveryMountTarget(t *testing.T) {
	m, _, err := run(t, exampleArgs(), true)
	require.NoError(t, err)

	var mountTargets []string
	for name := range childrenOf(t, m) {
		if strings.Contains(name, "-mt-") {
			mountTargets = append(mountTargets, "urn:pulumi:s::p::"+natsServiceType+"$"+mountTargetType+"::"+name)
		}
	}
	require.Len(t, mountTargets, 4)
	require.Subset(t, m.One(t, ecsServiceType).RPC.GetDependencies(), mountTargets)
}

func TestNatsServiceWaitForSteadyState(t *testing.T) {
	t.Run("unset leaves the service's input unset", func(t *testing.T) {
		m, _, err := run(t, exampleArgs(), true)
		require.NoError(t, err)
		require.NotContains(t, m.One(t, ecsServiceType).Inputs, resource.PropertyKey("waitForSteadyState"))
	})
	for _, v := range []bool{true, false} {
		t.Run(fmt.Sprintf("%v reaches the service", v), func(t *testing.T) {
			args := exampleArgs()
			args.WaitForSteadyState = pulumi.BoolRef(v)
			m, _, err := run(t, args, true)
			require.NoError(t, err)
			require.Equal(t, resource.NewProperty(v), m.One(t, ecsServiceType).Inputs["waitForSteadyState"])
		})
	}
}

// With adopt, each child takes over the root-level resource of its own name.
func TestNatsServiceAdoptAliasesEachChildToItsRootLevelName(t *testing.T) {
	args := exampleArgs()
	args.Adopt = true
	m, _, err := run(t, args, true)
	require.NoError(t, err)
	requireAdopted(t, exampleChildren(t, "nats-authorized.golden"), childrenOf(t, m))
}

// requireAdopted requires the children to be exactly want's names, each with one alias that has
// no parent and no other name or type.
func requireAdopted(t *testing.T, want map[string]child, children map[string]pulumitest.Registration) {
	t.Helper()
	require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(children)), "children's names")
	for name, c := range children {
		aliases := c.RPC.GetAliases()
		require.Len(t, aliases, 1, "aliases of %s", name)
		spec := aliases[0].GetSpec()
		require.NotNil(t, spec, "%s's alias is a spec", name)
		require.Empty(t, spec.GetName(), "%s's alias keeps its own name", name)
		require.Empty(t, spec.GetType(), "%s's alias keeps its own type", name)
		require.True(t, spec.GetNoParent(), "%s's alias has no parent", name)
	}
}

// The reference's auth key is never ephemeral; tailnetEphemeral changes that one value of the
// sidecar's script and nothing else.
func TestNatsServiceEphemeralTailnetKey(t *testing.T) {
	args := exampleArgs()
	noAuthorization(args)
	args.TailnetEphemeral = true
	m, _, err := run(t, args, true)
	require.NoError(t, err)
	golden := readGolden(t, "nats-unauthorized.golden")
	require.Equal(t, 1, strings.Count(golden, "?ephemeral=false&"))
	require.Equal(t, resource.NewProperty(strings.Replace(golden, "?ephemeral=false&", "?ephemeral=true&", 1)),
		m.One(t, taskDefinitionType).Inputs["containerDefinitions"])
}

func TestNatsServiceRefusesWithoutAnAWSProviderBeforeRegistering(t *testing.T) {
	m, _, err := run(t, exampleArgs(), false)
	require.ErrorContains(t, err, `envoy-infra:aws:NatsService needs a "aws" provider in its providers option`)
	require.Empty(t, m.Types())
}

// validate refuses each input NewNatsService cannot build from. The table runs against validate
// directly; TestNatsServiceRefusesAnInvalidInputBeforeRegistering shows that the constructor runs
// it first and prefixes the component's name.
func TestNatsServiceValidate(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*envoyaws.NatsServiceArgs)
		err    string
	}{
		{
			name:   "an authorization core refuses",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.Authorization.Users[0].NKey = "UNOTAKEY" },
			err:    `authorization: users[0] "example-listener"`,
		},
		// configHeader and configPath belong to authorization: required with it, and refused
		// without it, where the launch would drop them.
		{
			name:   "an authorization with no config header",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.ConfigHeader = nil },
			err:    "configHeader is required with authorization",
		},
		{
			name:   "an authorization with no config path",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.ConfigPath = nil },
			err:    "configPath is required with authorization",
		},
		{
			name:   "an authorization with a blank config path",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.ConfigPath = pulumi.StringRef("") },
			err:    "configPath is required with authorization",
		},
		{
			name:   "a config header with no authorization",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.Authorization = nil },
			err:    "configHeader is given for authorization, which is not given",
		},
		{
			name:   "a config path with no authorization",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.Authorization, a.ConfigHeader = nil, nil },
			err:    "configPath is given for authorization, which is not given",
		},
		{
			name:   "a config header line holding a line break",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.ConfigHeader = []string{"one\ntwo"} },
			err:    "configHeader: header[0] holds a line break",
		},
		{
			name:   "a server argument that loads another configuration, with authorization",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.ServerArgs = append(a.ServerArgs, "-c", "/tmp/other.conf") },
			err:    "serverArgs[7] sets nats-server's -c, which loads another configuration file in place of the rendered one",
		},
		{
			name:   "no subnet",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.SubnetIds = nil },
			err:    "subnetIds names no subnet",
		},
		{
			name:   "no tailnet tag",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.TailnetTags = nil },
			err:    "tailnetTags names no tag",
		},
		{
			name:   "base tags holding service",
			mutate: func(a *envoyaws.NatsServiceArgs) { a.BaseTags["service"] = "example" },
			err:    `baseTags holds "service"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := exampleArgs()
			row.mutate(args)
			require.ErrorContains(t, args.Validate(), row.err)
		})
	}
	require.NoError(t, exampleArgs().Validate())
	unauthorized := exampleArgs()
	noAuthorization(unauthorized)
	require.NoError(t, unauthorized.Validate())
}

func TestNatsServiceRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := exampleArgs()
	args.SubnetIds = nil
	m, _, err := run(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:aws:NatsService example-nats: subnetIds names no subnet")
	require.Equal(t, []string{awsProviderType}, m.Types(), "nothing but the provider is registered")
}

// The plugin's Construct copies a consumer's inputs into NatsServiceArgs, and refuses a
// plain-typed field whose input carries a dependency (Pulumi SDK v3.265.0
// go/pulumi/provider.go:765-766), a path pulumi.WithMocks never takes. These tests send every
// Input field as an output of another resource, then unknown as under preview.

// upstream is the resource the Input fields come from.
var upstream = resource.URN("urn:pulumi:s::p::example:index:Network::network")

// constructedURN is the component's URN under Construct: the integration server's mock engine
// names every resource in its own stack and project.
const constructedURN = "urn:pulumi:stack::project::" + natsServiceType + "::example-nats"

var (
	stringInputType    = reflect.TypeFor[pulumi.StringInput]()
	stringPtrInputType = reflect.TypeFor[pulumi.StringPtrInput]()
)

// literal returns the string an Input field of an example holds.
func literal(t *testing.T, in pulumi.StringInput) string {
	t.Helper()
	s, ok := in.(pulumi.String)
	require.True(t, ok, "an example Input field holds a pulumi.String, not %T", in)
	return string(s)
}

// inputs returns args, a component's args struct, as a consumer sends them to Construct: each
// field under its pulumi name, a field at its zero value left out as unset, and each Input field's
// string made a value by input.
func inputs(t *testing.T, args any, input func(string) property.Value) map[string]property.Value {
	t.Helper()
	return propertyValue(t, reflect.ValueOf(args), input).AsMap().AsMap()
}

func propertyValue(t *testing.T, v reflect.Value, input func(string) property.Value) property.Value {
	t.Helper()
	if v.Type() == stringInputType || v.Type() == stringPtrInputType {
		return input(literal(t, v.Interface().(pulumi.StringInput)))
	}
	switch v.Kind() {
	case reflect.Interface: // an Input of a collection, such as a pulumi.StringArray
		return propertyValue(t, v.Elem(), input)
	case reflect.Pointer:
		return propertyValue(t, v.Elem(), input)
	case reflect.String:
		return property.New(v.String())
	case reflect.Bool:
		return property.New(v.Bool())
	case reflect.Int:
		return property.New(float64(v.Int()))
	case reflect.Slice:
		items := make([]property.Value, v.Len())
		for i := range items {
			items[i] = propertyValue(t, v.Index(i), input)
		}
		return property.New(property.NewArray(items))
	case reflect.Map:
		m := make(map[string]property.Value, v.Len())
		for iter := v.MapRange(); iter.Next(); {
			m[iter.Key().String()] = propertyValue(t, iter.Value(), input)
		}
		return property.New(property.NewMap(m))
	case reflect.Struct:
		m := map[string]property.Value{}
		for i := range v.NumField() {
			if v.Field(i).IsZero() {
				continue
			}
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("pulumi"), ",")
			m[name] = propertyValue(t, v.Field(i), input)
		}
		return property.New(property.NewMap(m))
	}
	t.Fatalf("no property value for a %s", v.Type())
	return property.Value{}
}

// fromUpstream is a known value carrying a dependency on upstream.
func fromUpstream(v string) property.Value {
	return property.New(v).WithDependencies([]resource.URN{upstream})
}

// unknownFromUpstream is an unknown value carrying a dependency on upstream, as under preview.
func unknownFromUpstream(string) property.Value {
	return property.New(property.Computed).WithDependencies([]resource.URN{upstream})
}

// construct constructs a component of type typ named name from inputs with an aws provider,
// through the plugin's Construct on the AWS mock engine, and returns what the engine saw, the
// response and Construct's error.
func construct(t *testing.T, typ, name string, inputs map[string]property.Value, dryRun bool) (*pulumitest.Mocks, p.ConstructResponse, error) {
	t.Helper()
	m := newMocks()
	resp, err := pulumitest.Construct(t, m, typ, name, inputs, dryRun, map[tokens.Package]p.ProviderReference{
		"aws": {Urn: resource.URN(providerURN), ID: "example-aws-id"},
	})
	return m, resp, err
}

func TestConstructTakesEveryInputFieldFromAnotherResource(t *testing.T) {
	m, _, err := construct(t, natsServiceType, "example-nats", inputs(t, exampleArgs(), fromUpstream), false)
	require.NoError(t, err)

	children := childrenOf(t, m)
	requireChildren(t, exampleChildren(t, "nats-authorized.golden"), children)
	for name, c := range children {
		require.Equal(t, constructedURN, c.RPC.GetParent(), "%s's parent is the component", name)
		require.Contains(t, c.RPC.GetProvider(), providerURN+"::", "%s uses the aws provider the component was given", name)
	}
	for _, dep := range []struct{ child, input string }{
		{"example-nats-efs-sg", "vpcId"},
		{"example-nats-tsnet-efs-sg", "vpcId"},
		{"example-nats-efs-ingress", "sourceSecurityGroupId"},
		{"example-nats-tsnet-nfs-egress", "securityGroupId"},
		{"example-nats-efs-mt-0", "subnetId"},
		{"example-nats-tsnet-efs-mt-1", "subnetId"},
		{"example-nats-task", "executionRoleArn"},
		{"example-nats-task", "taskRoleArn"},
		{"example-nats-task", "containerDefinitions"},
		{"example-nats-cloudmap", "namespaceId"},
		{"example-nats-service", "cluster"},
		{"example-nats-service", "networkConfiguration"},
	} {
		require.Contains(t, children[dep.child].RPC.GetPropertyDependencies()[dep.input].GetUrns(), string(upstream),
			"%s's %s depends on the resource its input comes from", dep.child, dep.input)
	}
}

// A consumer that runs NATS with no authorization leaves configHeader and configPath out too, and
// Construct takes that: the service registers the unauthorized golden's container.
func TestConstructTakesNoAuthorizationWithoutItsInputs(t *testing.T) {
	args := exampleArgs()
	noAuthorization(args)
	in := inputs(t, args, fromUpstream)
	for _, name := range []string{"authorization", "configHeader", "configPath"} {
		require.NotContains(t, in, name)
	}
	m, _, err := construct(t, natsServiceType, "example-nats", in, false)
	require.NoError(t, err)
	requireChildren(t, exampleChildren(t, "nats-unauthorized.golden"), childrenOf(t, m))
}

func TestConstructTakesUnknownInputFieldsUnderPreview(t *testing.T) {
	m, _, err := construct(t, natsServiceType, "example-nats", inputs(t, exampleArgs(), unknownFromUpstream), true)
	require.NoError(t, err)

	children := childrenOf(t, m)
	want := exampleChildren(t, "nats-authorized.golden")
	require.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(children)), "children's names")
	for name, c := range children {
		require.Equal(t, want[name].typ, c.Type, "%s's type", name)
		require.Equal(t, constructedURN, c.RPC.GetParent(), "%s's parent is the component", name)
	}
	for _, unknown := range []struct{ child, input string }{
		{"example-nats-efs-sg", "vpcId"},
		{"example-nats-efs-mt-0", "subnetId"},
		{"example-nats-task", "executionRoleArn"},
		{"example-nats-task", "containerDefinitions"},
		{"example-nats-service", "cluster"},
	} {
		v := children[unknown.child].Inputs[resource.PropertyKey(unknown.input)]
		require.True(t, v.ContainsUnknowns(), "%s's %s from an unknown input is unknown, not %s",
			unknown.child, unknown.input, v.TypeString())
	}
	// What depends on no Input field is as in a run with known inputs.
	require.Equal(t, want["example-nats-log-group"].inputs, children["example-nats-log-group"].Inputs.Mappable())
	require.Equal(t, want["example-nats-efs"].inputs, children["example-nats-efs"].Inputs.Mappable())
}

// A stack output a consumer builds from a component output would preview as changed if the output
// were unknown, so in a preview whose inputs are known every output is known.
func TestConstructOutputsAreKnownInAPreviewWithKnownInputs(t *testing.T) {
	_, resp, err := construct(t, natsServiceType, "example-nats", inputs(t, exampleArgs(), fromUpstream), true)
	require.NoError(t, err)

	got := map[string]string{}
	for k, v := range resp.State.All {
		require.False(t, v.HasComputed(), "output %s is unknown in a preview with known inputs", k)
		got[k] = v.AsString()
	}
	require.Equal(t, exampleOutputs, got)
}

// A plain field is for a literal: Construct refuses one whose input carries a dependency, before
// the component registers anything.
func TestConstructRefusesAPlainFieldFromAnotherResource(t *testing.T) {
	args := exampleArgs()
	in := inputs(t, args, fromUpstream)
	in["region"] = fromUpstream(args.Region)
	m, _, err := construct(t, natsServiceType, "example-nats", in, false)
	require.ErrorContains(t, err, "region")
	require.Empty(t, m.Types())
}
