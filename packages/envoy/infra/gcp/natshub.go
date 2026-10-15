// Package gcp holds the components that run Envoy's infrastructure on Google Cloud.
package gcp

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/infra/nats"
)

// natsHubType is the component's token. pulumi-go-provider derives the same one from the NatsHub
// type's package and name, and serves it under that token; NewNatsHub registers with the constant
// so it also runs outside the plugin, where no call carries a token.
const natsHubType = "envoy-infra:gcp:NatsHub"

const (
	defaultMachineType = "e2-small"
	defaultDiskSizeGb  = 20
	natsPort           = 4222
	// monitorPort is nats-server's monitoring port, whose /healthz the group's health check reads.
	monitorPort = 8222
	// dataDeviceName is the data disk's device name: the template attaches it under that name, the
	// group keeps it across instances by that name, and the VM finds it at /dev/disk/by-id/google-<name>.
	dataDeviceName       = "data"
	tailnetAuthKeySuffix = "-tailnet-auth-key"
	cosImage             = "projects/cos-cloud/global/images/family/cos-stable"
	cloudPlatformScope   = "https://www.googleapis.com/auth/cloud-platform"
	// confHeader heads the rendered configuration file, which the VM's metadata holds in the clear.
	confHeader = "Rendered by envoy-infra. Public keys only."
)

var (
	// hostnameRule makes the hostname a DNS label (RFC 1035 §2.3.1: a letter first, a letter or
	// digit last), since it is the VM's tailnet name and natsUrl's host, and short enough that
	// <hostname>-hub is a service account id, which is 6-30 characters.
	hostnameRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,24}[a-z0-9]$`)
	// tagRule is the tailnet's rule for a tag (tailscale v1.104.1 tailcfg/tailcfg.go:766-790).
	tagRule = regexp.MustCompile(`^tag:[A-Za-z][A-Za-z0-9-]*$`)
	// imageRule takes an image reference pinned by digest, with none of the characters that would
	// split the unit's docker command or that systemd expands in it (whitespace, $, %).
	imageRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
)

// healthCheckRanges are the source ranges of Google Cloud's health checks.
var healthCheckRanges = []string{"130.211.0.0/22", "35.191.0.0/16"}

// NatsHubArgs are the inputs of an envoy-infra:gcp:NatsHub.
type NatsHubArgs struct {
	Users          []NATSUserArgs     `pulumi:"users"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`
	Hostname       string             `pulumi:"hostname"`             // ^[a-z][a-z0-9-]{0,24}[a-z0-9]$
	Tags           []string           `pulumi:"tags"`                 // "tag:<name>", at least one
	Zone           string             `pulumi:"zone"`                 // e.g. us-central1-a
	MachineType    *string            `pulumi:"machineType,optional"` // default "e2-small"
	DiskSizeGb     *int               `pulumi:"diskSizeGb,optional"`  // default 20
	NatsImage      string             `pulumi:"natsImage"`            // pinned: <name>@sha256:<digest>
	TailscaleImage string             `pulumi:"tailscaleImage"`       // pinned: <name>@sha256:<digest>
	// created, with a subnetwork per region, when absent; either may come from the consumer's own
	// resources
	Network    pulumi.StringPtrInput `pulumi:"network,optional"`
	Subnetwork pulumi.StringPtrInput `pulumi:"subnetwork,optional"` // only with network
}

// NATSUserArgs are one nkey user of the hub: a host's, as a rule, which its listener and its
// sessions share. Exactly one of machineId and permissions is set. nkey is named as aws:User names
// it, so a consumer running both clouds spells a user alike.
type NATSUserArgs struct {
	Name        string           `pulumi:"name"`
	NKey        string           `pulumi:"nkey"`                 // the user's public key; never its seed
	MachineID   *string          `pulumi:"machineId,optional"`   // permissions = core.HostPermissions(machineId)
	Permissions *PermissionsArgs `pulumi:"permissions,optional"` // explicit
}

// Annotate names the type gcp:NatsUser in the schema: generated SDKs append Args to an object
// type's name for its input type, so the Go name would reach consumers as NATSUserArgsArgs.
func (u *NATSUserArgs) Annotate(a infer.Annotator) { a.SetToken("gcp", "NatsUser") }

// PermissionsArgs are a user's explicit publish and subscribe rules, each nats:Rules, the one rules
// type every NATS component takes, whatever its cloud.
type PermissionsArgs struct {
	Publish   nats.RulesArgs `pulumi:"publish"`
	Subscribe nats.RulesArgs `pulumi:"subscribe"`
}

// Annotate names the type gcp:Permissions in the schema, for the reason NATSUserArgs's gives.
func (p *PermissionsArgs) Annotate(a infer.Annotator) { a.SetToken("gcp", "Permissions") }

// NatsHub is NATS with JetStream on one Container-Optimized OS VM, joined to a tailnet. The VM is
// the one instance of a managed group, which recreates it when NATS's /healthz fails and keeps its
// data disk, which holds the stream and the VM's tailnet identity, across instances.
type NatsHub struct {
	pulumi.ResourceState
	NatsUrl       pulumi.StringOutput `pulumi:"natsUrl"`       // nats://<hostname>:4222
	InstanceGroup pulumi.StringOutput `pulumi:"instanceGroup"` // the group's name
}

// settings are a hub's plain inputs, validated, with the defaults applied, and what they render:
// the VM's cloud-config, from vm, as its user data.
type settings struct {
	vm          vm
	userData    string
	zone        string
	machineType string
	diskSizeGb  int
}

// newSettings validates args and renders the NATS configuration and the VM's user data. Every
// refusal names the input.
func newSettings(args *NatsHubArgs) (settings, error) {
	if !hostnameRule.MatchString(args.Hostname) {
		return settings{}, fmt.Errorf("hostname %q must match %s", args.Hostname, hostnameRule)
	}
	if len(args.Tags) == 0 {
		return settings{}, errors.New("tags: at least one tag is required; the hub advertises its tags as its tailnet identity")
	}
	for i, tag := range args.Tags {
		if !tagRule.MatchString(tag) {
			return settings{}, fmt.Errorf("tags[%d] %q must match %s", i, tag, tagRule)
		}
	}
	if args.Zone == "" {
		return settings{}, errors.New("zone is required")
	}
	for _, image := range []struct{ field, ref string }{
		{"natsImage", args.NatsImage}, {"tailscaleImage", args.TailscaleImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return settings{}, fmt.Errorf("%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>",
				image.field, image.ref)
		}
	}
	s := settings{zone: args.Zone, machineType: defaultMachineType, diskSizeGb: defaultDiskSizeGb}
	if args.MachineType != nil {
		if *args.MachineType == "" {
			return settings{}, errors.New("machineType is empty; leave it unset for " + defaultMachineType)
		}
		s.machineType = *args.MachineType
	}
	if args.DiskSizeGb != nil {
		if *args.DiskSizeGb < 1 {
			return settings{}, fmt.Errorf("diskSizeGb %d must be positive", *args.DiskSizeGb)
		}
		s.diskSizeGb = *args.DiskSizeGb
	}
	if args.TailnetAuthKey == nil {
		return settings{}, errors.New("tailnetAuthKey is required")
	}
	if args.Subnetwork != nil && args.Network == nil {
		return settings{}, errors.New("subnetwork is set without network; the firewall needs the network the subnetwork belongs to")
	}
	auth, err := authorization(args.Users)
	if err != nil {
		return settings{}, err
	}
	conf, err := auth.Render([]string{confHeader})
	if err != nil {
		return settings{}, err
	}
	s.vm = vm{
		hostname:       args.Hostname,
		keySecret:      args.Hostname + tailnetAuthKeySuffix,
		tags:           args.Tags,
		natsImage:      args.NatsImage,
		tailscaleImage: args.TailscaleImage,
		natsConf:       conf,
	}
	if s.userData, err = s.vm.userData(); err != nil {
		return settings{}, fmt.Errorf("render the cloud-config: %w", err)
	}
	return s, nil
}

// authorization maps the users onto core's: each a keyed user with a seed, whose permissions are
// its host's set when it names a machine id and its own when it names permissions. core validates
// the rest when it renders.
func authorization(users []NATSUserArgs) (core.Authorization, error) {
	auth := core.Authorization{Users: make([]core.User, 0, len(users))}
	for i, u := range users {
		where := fmt.Sprintf("users[%d] %q", i, u.Name)
		if u.NKey == "" {
			return core.Authorization{}, fmt.Errorf("%s: nkey is required", where)
		}
		var perms core.Permissions
		switch {
		case u.MachineID != nil && u.Permissions != nil:
			return core.Authorization{}, fmt.Errorf("%s: machineId and permissions are both set; name one", where)
		case u.MachineID != nil:
			if *u.MachineID == "" {
				return core.Authorization{}, fmt.Errorf("%s: machineId is empty", where)
			}
			perms = core.HostPermissions(*u.MachineID)
		case u.Permissions != nil:
			perms = core.Permissions{
				Publish:   core.Rules{Allow: u.Permissions.Publish.Allow, Deny: u.Permissions.Publish.Deny},
				Subscribe: core.Rules{Allow: u.Permissions.Subscribe.Allow, Deny: u.Permissions.Subscribe.Deny},
			}
		default:
			return core.Authorization{}, fmt.Errorf("%s: name machineId, for its host's permission set, or permissions", where)
		}
		auth.Users = append(auth.Users, core.User{
			Name: u.Name, NKey: u.NKey, Publish: &perms.Publish, Subscribe: &perms.Subscribe, HasSeed: true,
		})
	}
	return auth, nil
}

// NewNatsHub registers a hub: its service account, the secret holding the tailnet auth key and
// the account's access to it, a network unless the consumer names one, the firewall rule letting
// Google's health checks reach NATS's monitoring port, the health check, the VM's template and its
// group of one. It refuses invalid inputs and a missing gcp provider before it registers anything.
//
// Each child's logical name starts with the hostname rather than the component's name: GCP names
// the network, firewall rule, health check and group after it, with a random suffix, and the
// hostname rule keeps those names valid there.
func NewNatsHub(ctx *pulumi.Context, name string, args *NatsHubArgs, opts ...pulumi.ResourceOption) (*NatsHub, error) {
	s, err := newSettings(args)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", natsHubType, name, err)
	}
	if err := providers.Require(ctx.Context(), natsHubType, "gcp", opts...); err != nil {
		return nil, err
	}

	comp := &NatsHub{}
	if err := ctx.RegisterComponentResource(natsHubType, name, comp, opts...); err != nil {
		return nil, err
	}
	child := pulumi.Parent(comp)
	hostname, keySecret := s.vm.hostname, s.vm.keySecret
	// The account's id and the VM's network tag.
	hubName := hostname + "-hub"

	account, err := serviceaccount.NewAccount(ctx, hubName, &serviceaccount.AccountArgs{
		AccountId: pulumi.String(hubName),
	}, child)
	if err != nil {
		return nil, err
	}

	secret, err := secretmanager.NewSecret(ctx, keySecret, &secretmanager.SecretArgs{
		SecretId:    pulumi.String(keySecret),
		Replication: secretmanager.SecretReplicationArgs{Auto: &secretmanager.SecretReplicationAutoArgs{}},
	}, child)
	if err != nil {
		return nil, err
	}
	version, err := secretmanager.NewSecretVersion(ctx, keySecret, &secretmanager.SecretVersionArgs{
		Secret:     secret.ID(),
		SecretData: pulumi.ToSecret(args.TailnetAuthKey.ToStringOutput()).(pulumi.StringOutput),
	}, child)
	if err != nil {
		return nil, err
	}
	accessor, err := secretmanager.NewSecretIamMember(ctx, keySecret+"-accessor", &secretmanager.SecretIamMemberArgs{
		SecretId: secret.ID(),
		Role:     pulumi.String("roles/secretmanager.secretAccessor"),
		Member:   account.Member,
	}, child)
	if err != nil {
		return nil, err
	}

	var network pulumi.StringOutput
	if args.Network == nil {
		created, err := compute.NewNetwork(ctx, hostname+"-network", &compute.NetworkArgs{
			AutoCreateSubnetworks: pulumi.Bool(true),
		}, child)
		if err != nil {
			return nil, err
		}
		network = created.SelfLink
	} else {
		network = args.Network.ToStringPtrOutput().Elem()
	}

	if _, err := compute.NewFirewall(ctx, hostname+"-health-checks", &compute.FirewallArgs{
		Network: network,
		Allows: compute.FirewallAllowArray{compute.FirewallAllowArgs{
			Protocol: pulumi.String("tcp"),
			Ports:    pulumi.StringArray{pulumi.String(strconv.Itoa(monitorPort))},
		}},
		SourceRanges: pulumi.ToStringArray(healthCheckRanges),
		TargetTags:   pulumi.StringArray{pulumi.String(hubName)},
	}, child); err != nil {
		return nil, err
	}

	health, err := compute.NewHealthCheck(ctx, hostname+"-healthz", &compute.HealthCheckArgs{
		HttpHealthCheck: compute.HealthCheckHttpHealthCheckArgs{
			Port:        pulumi.Int(monitorPort),
			RequestPath: pulumi.String("/healthz"),
		},
		CheckIntervalSec:   pulumi.Int(10),
		UnhealthyThreshold: pulumi.Int(6),
	}, child)
	if err != nil {
		return nil, err
	}

	template, err := compute.NewInstanceTemplate(ctx, hostname, &compute.InstanceTemplateArgs{
		NamePrefix:  pulumi.String(hostname + "-"),
		MachineType: pulumi.String(s.machineType),
		Disks: compute.InstanceTemplateDiskArray{
			compute.InstanceTemplateDiskArgs{SourceImage: pulumi.String(cosImage), Boot: pulumi.Bool(true)},
			compute.InstanceTemplateDiskArgs{
				DeviceName: pulumi.String(dataDeviceName),
				Boot:       pulumi.Bool(false),
				AutoDelete: pulumi.Bool(false),
				DiskSizeGb: pulumi.Int(s.diskSizeGb),
			},
		},
		NetworkInterfaces: compute.InstanceTemplateNetworkInterfaceArray{compute.InstanceTemplateNetworkInterfaceArgs{
			Network:    network,
			Subnetwork: args.Subnetwork,
			// An external address, the VM's way out to the image registries and the tailnet.
			AccessConfigs: compute.InstanceTemplateNetworkInterfaceAccessConfigArray{
				compute.InstanceTemplateNetworkInterfaceAccessConfigArgs{},
			},
		}},
		ServiceAccount: compute.InstanceTemplateServiceAccountArgs{
			Email:  account.Email,
			Scopes: pulumi.StringArray{pulumi.String(cloudPlatformScope)},
		},
		Tags:     pulumi.StringArray{pulumi.String(hubName)},
		Metadata: pulumi.StringMap{"user-data": pulumi.String(s.userData)},
	}, child)
	if err != nil {
		return nil, err
	}

	// The VM fetches the key at its first boot, so its group is created once the key's version
	// and the account's access to it exist.
	group, err := compute.NewInstanceGroupManager(ctx, hostname, &compute.InstanceGroupManagerArgs{
		BaseInstanceName: pulumi.String(hostname),
		Zone:             pulumi.String(s.zone),
		TargetSize:       pulumi.Int(1),
		Versions: compute.InstanceGroupManagerVersionArray{compute.InstanceGroupManagerVersionArgs{
			InstanceTemplate: template.SelfLinkUnique,
		}},
		StatefulDisks: compute.InstanceGroupManagerStatefulDiskArray{compute.InstanceGroupManagerStatefulDiskArgs{
			DeviceName: pulumi.String(dataDeviceName),
			DeleteRule: pulumi.String("NEVER"),
		}},
		AutoHealingPolicies: compute.InstanceGroupManagerAutoHealingPoliciesArgs{
			HealthCheck:     health.SelfLink,
			InitialDelaySec: pulumi.Int(300),
		},
		UpdatePolicy: compute.InstanceGroupManagerUpdatePolicyArgs{
			Type:                pulumi.String("PROACTIVE"),
			MinimalAction:       pulumi.String("REPLACE"),
			ReplacementMethod:   pulumi.String("RECREATE"),
			MaxSurgeFixed:       pulumi.Int(0),
			MaxUnavailableFixed: pulumi.Int(1),
		},
		WaitForInstances:       pulumi.Bool(true),
		WaitForInstancesStatus: pulumi.String("STABLE"),
	}, child, pulumi.DependsOn([]pulumi.Resource{version, accessor}))
	if err != nil {
		return nil, err
	}

	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it.
	comp.NatsUrl = pulumi.String(fmt.Sprintf("nats://%s:%d", hostname, natsPort)).ToStringOutput()
	comp.InstanceGroup = group.Name
	return comp, nil
}
