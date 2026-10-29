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
	seedAccessor  *secretmanager.SecretIamMember
	// azureKeyAccessor is set only with azureEmbedKeySecret.
	azureKeyAccessor *secretmanager.SecretIamMember
	// issuer is the cluster's service-account token issuer.
	issuer pulumi.StringOutput
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
	dispatch := dispatchType + " " + name
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
	// AvailabilityType is databaseAvailabilityType. The bridged Terraform provider declares
	// settings.availability_type without ForceNew (terraform-provider-google-beta v8.6.0, commit
	// e6f3d61a347c, the upstream pulumi-gcp v10.1.0 bridges:
	// google-beta/services/sql/resource_sql_database_instance.go:449-462), so switching between
	// ZONAL and REGIONAL updates the instance in place: Cloud SQL restarts it once, and the change
	// cannot be stopped once started.
	instance, err := sql.NewDatabaseInstance(ctx, s.hostname, &sql.DatabaseInstanceArgs{
		DatabaseVersion: pulumi.String(s.databaseVersion), Region: pulumi.String(s.region), DeletionProtection: pulumi.Bool(s.deletionProtection),
		Settings: &sql.DatabaseInstanceSettingsArgs{
			DeletionProtectionEnabled: pulumi.Bool(s.deletionProtection),
			Tier:                      pulumi.String(s.databaseTier), Edition: pulumi.String("ENTERPRISE"), AvailabilityType: pulumi.String(s.databaseAvailabilityType), DiskType: pulumi.String("PD_SSD"), DiskSize: pulumi.Int(10), DiskAutoresize: pulumi.Bool(true),
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
	// deletionProtection also keeps the database itself: PREVENT refuses its delete, which an
	// instance replacement would otherwise make first.
	databasePolicy := "DELETE"
	if s.deletionProtection {
		databasePolicy = "PREVENT"
	}
	_, err = sql.NewDatabase(ctx, s.hostname, &sql.DatabaseArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), DeletionPolicy: pulumi.String(databasePolicy)}, child)
	if err != nil {
		return nil, err
	}
	_, err = sql.NewUser(ctx, s.hostname, &sql.UserArgs{Instance: instance.Name, Name: pulumi.String("dispatch"), Password: password.Result}, child)
	if err != nil {
		return nil, err
	}
	// The principal lives in the cluster's Workload Identity pool, which Google creates with the
	// cluster. It is built from the cluster's own WorkloadIdentityConfig, so it resolves only once
	// the pool exists, and the component's own seed grant below, built from it, waits for the
	// cluster: IAM may refuse a member of a pool that does not exist yet (unproven: the plan's
	// hardening ledger). A consumer's grant taken from the `principal` output would resolve later
	// still, once every child has registered, the app Deployment's readiness wait included, and
	// that wait needs roles/cloudsql.client already granted, so the consumer grants that role from
	// the principal's known form, not from the output; whether IAM accepts it before the pool
	// exists is the same unproven question.
	principal := pulumi.All(project.Number(), cluster.WorkloadIdentityConfig.WorkloadPool()).ApplyT(func(values []interface{}) (string, error) {
		number, err := outputString(values[0])
		if err != nil {
			return "", fmt.Errorf("%s: the project's number: %w", dispatch, err)
		}
		pool, err := outputString(values[1])
		if err != nil {
			return "", fmt.Errorf("%s: the cluster's Workload Identity pool: %w", dispatch, err)
		}
		return fmt.Sprintf("principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/subject/ns/%s/sa/%s", number, pool, s.namespace, s.hostname), nil
	}).(pulumi.StringOutput)
	seedAccessor, err := secretmanager.NewSecretIamMember(ctx, s.hostname+"-nats-seed-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.NatsSeedSecret, dispatch, "natsSeedSecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn([]pulumi.Resource{cluster}))
	if err != nil {
		return nil, err
	}
	var azureKeyAccessor *secretmanager.SecretIamMember
	if s.azure.key {
		// The Azure key is read by the app at run time through the CSI driver, so the pods'
		// principal needs the same per-secret accessor grant the NATS seed has.
		azureKeyAccessor, err = secretmanager.NewSecretIamMember(ctx, s.hostname+"-azure-embed-key-accessor", &secretmanager.SecretIamMemberArgs{SecretId: refuseDispatchSeed(args.AzureEmbedKeySecret, dispatch, "azureEmbedKeySecret"), Role: pulumi.String("roles/secretmanager.secretAccessor"), Member: principal}, child, pulumi.DependsOn([]pulumi.Resource{cluster}))
		if err != nil {
			return nil, err
		}
	}
	// GKE signs a cluster's service-account tokens as the issuer
	// https://container.googleapis.com/v1/projects/<project id>/locations/<location>/clusters/<name>,
	// the URL Google's Workload Identity documentation names the cluster by (its kubernetes.cluster
	// principal set). It is built from the project, the location and the name rather than taken from
	// the cluster's selfLink, which the provider documents only as "the server-defined URL for the
	// resource", a form it does not promise.
	issuer := pulumi.All(project.ProjectId(), cluster.Location, cluster.Name).ApplyT(func(values []interface{}) (string, error) {
		parts := make([]string, len(values))
		for i, what := range []string{"the project's id", "the cluster's location", "the cluster's name"} {
			value, err := outputString(values[i])
			if err != nil {
				return "", fmt.Errorf("%s: %s: %w", dispatch, what, err)
			}
			parts[i] = value
		}
		return fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s", parts[0], parts[1], parts[2]), nil
	}).(pulumi.StringOutput)
	return &dispatchCloud{cluster: cluster, instance: instance, password: password, signing: signing, agentToken: agentToken, listenerToken: listenerToken, principal: principal, seedAccessor: seedAccessor, azureKeyAccessor: azureKeyAccessor, issuer: issuer}, nil
}
