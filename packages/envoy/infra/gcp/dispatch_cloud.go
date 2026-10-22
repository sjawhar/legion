package gcp

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/container"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/servicenetworking"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/sql"
	random "github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

type dispatchCloud struct {
	cluster       *container.Cluster
	instance      *sql.DatabaseInstance
	password      *random.RandomPassword
	signing       *random.RandomPassword
	agentToken    *random.RandomPassword
	listenerToken *random.RandomPassword
	principal     pulumi.StringOutput
}

func providerFor(ctx context.Context, pkg string, opts ...pulumi.ResourceOption) (pulumi.ProviderResource, error) {
	resources, err := pulumi.NewResourceOptions(opts...)
	if err != nil {
		return nil, err
	}
	for _, provider := range resources.Providers {
		out, err := internals.UnsafeAwaitOutput(ctx, provider.URN())
		if err != nil {
			return nil, err
		}
		if urn, ok := out.Value.(pulumi.URN); ok && resource.URN(urn).Type().Name() == tokens.TypeName(pkg) {
			return provider, nil
		}
	}
	return nil, fmt.Errorf("missing %s provider", pkg)
}

func newDispatchCloud(ctx *pulumi.Context, comp *Dispatch, name string, args *DispatchArgs, s dispatchSettings, opts ...pulumi.ResourceOption) (*dispatchCloud, error) {
	gcpProvider, err := providerFor(ctx.Context(), "gcp", opts...)
	if err != nil {
		return nil, err
	}
	child := pulumi.Parent(comp)
	project := organizations.LookupProjectOutput(ctx, organizations.LookupProjectOutputArgs{}, pulumi.Provider(gcpProvider))
	// The cluster's Workload Identity pool; GKE accepts only the project's own, <project id>.svc.id.goog.
	pool := project.ProjectId().Elem().ApplyT(func(id string) string { return id + ".svc.id.goog" }).(pulumi.StringOutput)

	network, err := compute.NewNetwork(ctx, s.hostname+"-network", &compute.NetworkArgs{AutoCreateSubnetworks: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	subnetwork, err := compute.NewSubnetwork(ctx, s.hostname, &compute.SubnetworkArgs{
		Network: network.ID(), Region: pulumi.String(s.region), IpCidrRange: pulumi.String("10.64.0.0/20"), PrivateIpGoogleAccess: pulumi.Bool(true),
	}, child)
	if err != nil {
		return nil, err
	}
	router, err := compute.NewRouter(ctx, s.hostname, &compute.RouterArgs{Network: network.ID(), Region: pulumi.String(s.region)}, child)
	if err != nil {
		return nil, err
	}
	_, err = compute.NewRouterNat(ctx, s.hostname, &compute.RouterNatArgs{
		Router: router.Name, Region: pulumi.String(s.region), NatIpAllocateOption: pulumi.String("AUTO_ONLY"), SourceSubnetworkIpRangesToNat: pulumi.String("ALL_SUBNETWORKS_ALL_IP_RANGES"),
	}, child)
	if err != nil {
		return nil, err
	}
	address, err := compute.NewGlobalAddress(ctx, s.hostname+"-services", &compute.GlobalAddressArgs{
		Purpose: pulumi.String("VPC_PEERING"), AddressType: pulumi.String("INTERNAL"), PrefixLength: pulumi.Int(16), Network: network.ID(),
	}, child)
	if err != nil {
		return nil, err
	}
	connection, err := servicenetworking.NewConnection(ctx, s.hostname+"-services", &servicenetworking.ConnectionArgs{
		Network: network.ID(), Service: pulumi.String("servicenetworking.googleapis.com"), ReservedPeeringRanges: pulumi.StringArray{address.Name},
	}, child)
	if err != nil {
		return nil, err
	}
	cluster, err := container.NewCluster(ctx, s.hostname, &container.ClusterArgs{
		Name: pulumi.String(s.hostname), Location: pulumi.String(s.region), EnableAutopilot: pulumi.Bool(true), Network: network.ID(), Subnetwork: subnetwork.ID(),
		IpAllocationPolicy:   &container.ClusterIpAllocationPolicyArgs{},
		PrivateClusterConfig: &container.ClusterPrivateClusterConfigArgs{EnablePrivateNodes: pulumi.Bool(true), EnablePrivateEndpoint: pulumi.Bool(false)},
		ReleaseChannel:       &container.ClusterReleaseChannelArgs{Channel: pulumi.String("REGULAR")},
		SecretManagerConfig:  &container.ClusterSecretManagerConfigArgs{Enabled: pulumi.Bool(true)},
		SecretSyncConfig: &container.ClusterSecretSyncConfigArgs{
			Enabled:        pulumi.Bool(true),
			RotationConfig: &container.ClusterSecretSyncConfigRotationConfigArgs{Enabled: pulumi.Bool(true)},
		},
		WorkloadIdentityConfig: &container.ClusterWorkloadIdentityConfigArgs{WorkloadPool: pool},
		DeletionProtection:     pulumi.Bool(s.deletionProtection),
	}, child)
	if err != nil {
		return nil, err
	}
	instance, err := sql.NewDatabaseInstance(ctx, s.hostname, &sql.DatabaseInstanceArgs{
		DatabaseVersion: pulumi.String(s.databaseVersion), Region: pulumi.String(s.region), DeletionProtection: pulumi.Bool(s.deletionProtection),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			Tier: pulumi.String(s.databaseTier), Edition: pulumi.String("ENTERPRISE"), AvailabilityType: pulumi.String("ZONAL"), DiskType: pulumi.String("PD_SSD"), DiskSize: pulumi.Int(10), DiskAutoresize: pulumi.Bool(true),
			IpConfiguration:     &sql.DatabaseInstanceSettingsIpConfigurationArgs{Ipv4Enabled: pulumi.Bool(false), PrivateNetwork: network.ID(), SslMode: pulumi.String("ENCRYPTED_ONLY")},
			BackupConfiguration: &sql.DatabaseInstanceSettingsBackupConfigurationArgs{Enabled: pulumi.Bool(true), PointInTimeRecoveryEnabled: pulumi.Bool(true), StartTime: pulumi.String("08:00"), TransactionLogRetentionDays: pulumi.Int(7), BackupRetentionSettings: &sql.DatabaseInstanceSettingsBackupConfigurationBackupRetentionSettingsArgs{RetainedBackups: pulumi.Int(14)}},
		},
	}, child, pulumi.DependsOn([]pulumi.Resource{connection}))
	if err != nil {
		return nil, err
	}
	password, err := random.NewRandomPassword(ctx, s.hostname+"-password", &random.RandomPasswordArgs{Length: pulumi.Int(32), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	signing, err := random.NewRandomPassword(ctx, s.hostname+"-signing-key", &random.RandomPasswordArgs{Length: pulumi.Int(64), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	agentToken, err := random.NewRandomPassword(ctx, s.hostname+"-agent-token", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	// The listener binds 0.0.0.0 in a pod of its own, where its API refuses to start without a
	// bearer, and its credential is its own, never a key of the Secret holding the database URL
	// and the signing key.
	listenerToken, err := random.NewRandomPassword(ctx, s.hostname+"-listener-token", &random.RandomPasswordArgs{Length: pulumi.Int(48), Special: pulumi.Bool(false)}, child)
	if err != nil {
		return nil, err
	}
	_, err = sql.NewDatabase(ctx, s.hostname, &sql.DatabaseArgs{Instance: instance.Name, Name: pulumi.String("dispatch")}, child)
	if err != nil {
		return nil, err
	}
	_, err = sql.NewUser(ctx, s.hostname, &sql.UserArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), Password: password.Result}, child)
	if err != nil {
		return nil, err
	}
	// The principal lives in the cluster's Workload Identity pool, which Google creates with the
	// cluster; IAM refuses a member of a pool that does not exist yet.
	principal := pulumi.All(project.Number(), pool).ApplyT(func(values []interface{}) string {
		return fmt.Sprintf("principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/ns/%s/sa/%s", values[0].(string), values[1].(string), s.namespace, s.hostname)
	}).(pulumi.StringOutput)
	_, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-nats-seed-accessor", &secretmanager.SecretIamMemberArgs{SecretId: args.NatsSeedSecret, Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn([]pulumi.Resource{cluster}))
	if err != nil {
		return nil, err
	}
	return &dispatchCloud{cluster: cluster, instance: instance, password: password, signing: signing, agentToken: agentToken, listenerToken: listenerToken, principal: principal}, nil
}
