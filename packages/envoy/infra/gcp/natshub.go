// Package gcp holds the components that run Envoy's infrastructure on Google Cloud.
package gcp

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/secretmanager"
	"github.com/pulumi/pulumi-gcp/sdk/v10/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/bus"
)

// natsHubType is the component's token. pulumi-go-provider derives the same one from the NatsHub
// type's package and name, and serves it under that token; NewNatsHub registers with the constant
// so it also runs outside the plugin, where no call carries a token.
const natsHubType = "envoy-infra:gcp:NatsHub"

const (
	defaultMachineType = "e2-small"
	defaultDiskType    = "pd-balanced"
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

// groupWait is the provider's own create and update timeout for an instance group manager
// (pulumi-gcp v10.1.0's upstream resource_compute_instance_group_manager.go:49-53).
const groupWait = 15 * time.Minute

// The group's initial delay, the time it waits after creating a VM before the health check counts:
// bootAndPullsSec of it is the VM's boot and its two image pulls, and the rest the first boot's
// read and format of the data disk (initialDelaySec). A group takes at most maxInitialDelaySec.
const (
	bootAndPullsSec    = 300
	maxInitialDelaySec = 3600
)

// diskReadRates are the data disk types the hub takes, each with the rate, in MiB/s, the first
// boot's full read of the disk is assumed to run at. pd-balanced's is below its documented 140
// MiB/s baseline; pd-ssd's is its documented floor, the 200 MiB/s read limit of an E2 shared-core
// VM, as e2-small is, which is below pd-ssd's 240 MiB/s baseline
// (https://cloud.google.com/compute/docs/disks/performance). Neither rate is measured.
var diskReadRates = map[string]int{"pd-balanced": 100, "pd-ssd": 200}

// refusedDiskTypes are disk types the hub refuses for the data disk with a reason of their own;
// every other type but those of diskReadRates is refused too.
var refusedDiskTypes = map[string]string{
	"pd-standard": "its documented read is 0.12 MiB/s per GiB with no baseline, so the first boot's " +
		"full read of the disk takes over 8,500 s at any size, and the group waits at most 3600 s",
	"hyperdisk-balanced": "an E2 VM, as the default machineType e2-small is, attaches Hyperdisk " +
		"Balanced only by arrangement with Google's account team",
}

// initialDelaySec is the group's initial delay for a data disk of sizeGb read at rate MiB/s. The
// first boot reads a disk blkid finds no filesystem on to its end before it formats it
// (dataDiskEntry), so the delay is
//
//	300 + ceil(1.25 × (S + S/64 + 1024) / R) seconds
//
// with S = sizeGb × 1024 MiB, R = rate, and a quarter more as the margin: 300 s for the boot and
// the image pulls, then the read of S, and mkfs.ext4's writes, S/64 of inode tables (256 bytes per
// 16 KiB, all written at once) and at most 1024 MiB of journal. sizeGb is taken as GiB, the larger
// reading of Compute Engine's GB. Callers keep sizeGb at or below maxDiskSizeGb(rate).
func initialDelaySec(sizeGb, rate int) int {
	work := sizeGb*1024 + sizeGb*16 + 1024
	return bootAndPullsSec + (5*work+4*rate-1)/(4*rate)
}

// maxDiskSizeGb is the largest sizeGb whose initialDelaySec at rate is at most maxInitialDelaySec:
// ceil(5w/4R) <= 3300 holds while w = 1040 × sizeGb + 1024 <= 2640 × R.
func maxDiskSizeGb(rate int) int {
	return ((maxInitialDelaySec-bootAndPullsSec)*4*rate/5 - 1024) / 1040
}

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
	// secretIDRule is Secret Manager's rule for a secret id (secrets.create), which also keeps it
	// whole in the unit's command.
	secretIDRule = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
)

// healthCheckRanges are the source ranges of Google Cloud's health checks.
var healthCheckRanges = []string{"130.211.0.0/22", "35.191.0.0/16"}

// NatsHubArgs are the inputs of an envoy-infra:gcp:NatsHub.
type NatsHubArgs struct {
	Users          []NATSUserArgs     `pulumi:"users,optional"` // see authorization for what may be empty
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`
	Hostname       string             `pulumi:"hostname"`             // ^[a-z][a-z0-9-]{0,24}[a-z0-9]$
	Tags           []string           `pulumi:"tags"`                 // "tag:<name>", at least one
	Zone           string             `pulumi:"zone"`                 // e.g. us-central1-a
	MachineType    *string            `pulumi:"machineType,optional"` // default "e2-small"
	DiskType       *string            `pulumi:"diskType,optional"`    // default "pd-balanced"
	DiskSizeGb     *int               `pulumi:"diskSizeGb,optional"`  // default 20
	NatsImage      string             `pulumi:"natsImage"`            // pinned: <name>@sha256:<digest>
	TailscaleImage string             `pulumi:"tailscaleImage"`       // pinned: <name>@sha256:<digest>
	// created, with a subnetwork per region, when absent; either may come from the consumer's own
	// resources
	Network    pulumi.StringPtrInput `pulumi:"network,optional"`
	Subnetwork pulumi.StringPtrInput `pulumi:"subnetwork,optional"` // only with network
	// The hub's own Envoy listener; see docs/nats-hub.md, "The hub's listener".
	Listener *HubListenerArgs `pulumi:"listener,optional"`
	// The user a client presenting no credential becomes, granted core.SessionPermissions.
	SessionUser *SessionUserArgs `pulumi:"sessionUser,optional"`
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

// HubListenerArgs are the hub's own Envoy listener: its image, the Secret Manager secret in the
// hub's project that holds its NATS user's seed, and that seed's public key. The hub renders the
// user itself, named after the hub's hostname, the listener's machine id, and granted
// core.ListenerPermissions for that machine.
type HubListenerArgs struct {
	Image      string `pulumi:"image"`      // pinned: <name>@sha256:<digest>
	SeedSecret string `pulumi:"seedSecret"` // a secret id, never the seed
	NKey       string `pulumi:"nkey"`       // the seed's public key; never the seed
}

// Annotate names the type gcp:HubListener in the schema, for the reason NATSUserArgs's gives.
func (l *HubListenerArgs) Annotate(a infer.Annotator) { a.SetToken("gcp", "HubListener") }

// SessionUserArgs is the NATS user every client presenting no credential becomes: an agent session
// on any device that reaches the hub. It is granted core.SessionPermissions and has no seed, so its
// nkey is a public key whose seed nobody keeps.
type SessionUserArgs struct {
	Name string `pulumi:"name"`
	NKey string `pulumi:"nkey"` // a user public key; never its seed
}

// Annotate names the type gcp:SessionUser in the schema, for the reason NATSUserArgs's gives.
func (u *SessionUserArgs) Annotate(a infer.Annotator) { a.SetToken("gcp", "SessionUser") }

// NatsHub is NATS with JetStream on one Container-Optimized OS VM, joined to a tailnet. The VM is
// the one instance of a managed group, which recreates it when NATS's /healthz fails and keeps its
// data disk, which holds the stream and the VM's tailnet identity, across instances.
type NatsHub struct {
	pulumi.ResourceState
	NatsUrl       pulumi.StringOutput `pulumi:"natsUrl"`       // nats://<hostname>:4222
	InstanceGroup pulumi.StringOutput `pulumi:"instanceGroup"` // the group's name
}

// settings are a hub's plain inputs, validated, with the defaults applied, and what they render:
// the VM's cloud-config, from vm, as its user data, and the group's initial delay.
type settings struct {
	vm              vm
	userData        string
	zone            string
	machineType     string
	diskType        string
	diskSizeGb      int
	initialDelaySec int
}

// newSettings validates args and renders the NATS configuration and the VM's user data. Every
// refusal names the input.
func newSettings(args *NatsHubArgs) (settings, error) {
	if input, ok := seededInput(args); ok {
		return settings{}, seedRefused(input)
	}
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
	s := settings{zone: args.Zone, machineType: defaultMachineType, diskType: defaultDiskType, diskSizeGb: defaultDiskSizeGb}
	if args.MachineType != nil {
		if *args.MachineType == "" {
			return settings{}, errors.New("machineType is empty; leave it unset for " + defaultMachineType)
		}
		s.machineType = *args.MachineType
	}
	if args.DiskType != nil {
		if *args.DiskType == "" {
			return settings{}, errors.New("diskType is empty; leave it unset for " + defaultDiskType)
		}
		s.diskType = *args.DiskType
	}
	rate, ok := diskReadRates[s.diskType]
	if !ok {
		reason, own := refusedDiskTypes[s.diskType]
		if !own {
			reason = "the hub takes pd-balanced or pd-ssd, the types whose documented read rates the " +
				"first boot's full read of the disk is timed by"
		}
		return settings{}, fmt.Errorf("diskType %q: %s", s.diskType, reason)
	}
	if args.DiskSizeGb != nil {
		if *args.DiskSizeGb < 1 {
			return settings{}, fmt.Errorf("diskSizeGb %d must be positive", *args.DiskSizeGb)
		}
		s.diskSizeGb = *args.DiskSizeGb
	}
	if largest := maxDiskSizeGb(rate); s.diskSizeGb > largest {
		return settings{}, fmt.Errorf("diskSizeGb %d is too large for a %s data disk: the first boot reads "+
			"the disk whole, at an assumed %d MiB/s, before it formats it, and the group waits at most %d s "+
			"for that; the largest diskSizeGb that fits is %d", s.diskSizeGb, s.diskType, rate, maxInitialDelaySec, largest)
	}
	s.initialDelaySec = initialDelaySec(s.diskSizeGb, rate)
	if args.TailnetAuthKey == nil {
		return settings{}, errors.New("tailnetAuthKey is required")
	}
	if args.Subnetwork != nil && args.Network == nil {
		return settings{}, errors.New("subnetwork is set without network; the firewall needs the network the subnetwork belongs to")
	}
	auth, err := authorization(args.Hostname, args.Users, args.Listener, args.SessionUser)
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
	if s.vm.listener, err = resolveListener(args); err != nil {
		return settings{}, err
	}
	if s.userData, err = s.vm.userData(); err != nil {
		return settings{}, fmt.Errorf("render the cloud-config: %w", err)
	}
	return s, nil
}

// resolveListener checks the hub's listener input and returns the listener it gives, or nil for a
// hub without one. The listener runs as machine hostname, listens on the VM's loopback on the
// listener's default port, and reaches NATS there too.
func resolveListener(args *NatsHubArgs) (*listener, error) {
	l := args.Listener
	if l == nil {
		return nil, nil
	}
	if !imageRule.MatchString(l.Image) {
		return nil, fmt.Errorf("listener.image %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", l.Image)
	}
	if !secretIDRule.MatchString(l.SeedSecret) {
		return nil, fmt.Errorf("listener.seedSecret %q must be a secret id in the hub's project, matching %s", l.SeedSecret, secretIDRule)
	}
	if l.SeedSecret == args.Hostname+tailnetAuthKeySuffix {
		return nil, fmt.Errorf("listener.seedSecret %q is the hub's own tailnet auth key secret", l.SeedSecret)
	}
	env, err := core.ListenerConfig{
		MachineID:  args.Hostname,
		Port:       core.DefaultListenerPort,
		ListenHost: "127.0.0.1",
	}.Env([]string{localNATS})
	if err != nil {
		return nil, err
	}
	return &listener{image: l.Image, seedSecret: l.SeedSecret, env: env}, nil
}

// seededInput names the first plain input of args that the hub itself carries and that holds an
// nkey seed (bus.HoldsSeed: any type, either case, whole, inside a longer value, split by '.', '-',
// '_' or whitespace, or percent-encoded), by its path and never its value: the hostname and the
// tags, quoted by the hub's own refusals and written into the user data; the zone, the machine
// type and the disk type, the group's and the template's inputs, the disk type also quoted by its
// refusal; the images, written into the user data; and each user's machine id, which the hub
// turns into that user's rules before core sees them, so core's refusal would name a rule entry
// the user never wrote. A user's name and rule entries are core's:
// core.Authorization refuses a seed in either before any refusal of its own quotes it, and the
// hub's own user refusals name a user by its index alone (authorization). A user's nkey is core's
// too: it refuses any nkey that is not a user public key, a seed included, without quoting it.
// tailnetAuthKey and network and subnetwork are Inputs, checked once they resolve
// (refuseSeedInSecret, refuseSeed).
func seededInput(args *NatsHubArgs) (string, bool) {
	type field struct{ path, value string }
	fields := []field{{"hostname", args.Hostname}, {"zone", args.Zone}, {"natsImage", args.NatsImage},
		{"tailscaleImage", args.TailscaleImage}}
	for i, tag := range args.Tags {
		fields = append(fields, field{fmt.Sprintf("tags[%d]", i), tag})
	}
	if args.MachineType != nil {
		fields = append(fields, field{"machineType", *args.MachineType})
	}
	if args.DiskType != nil {
		fields = append(fields, field{"diskType", *args.DiskType})
	}
	for i, u := range args.Users {
		if u.MachineID != nil {
			fields = append(fields, field{fmt.Sprintf("users[%d].machineId", i), *u.MachineID})
		}
	}
	if args.Listener != nil {
		fields = append(fields, field{"listener.image", args.Listener.Image}, field{"listener.seedSecret", args.Listener.SeedSecret})
	}
	for _, f := range fields {
		if bus.HoldsSeed(f.value) {
			return f.path, true
		}
	}
	return "", false
}

// seedRefused refuses input, which holds an nkey user seed, naming it and not its value.
func seedRefused(input string) error {
	return fmt.Errorf("%s: an nkey user seed, which is never an input of the hub (not echoed)", input)
}

// refuseSeed is value, an Input of the hub that may come from another resource, with a seed in it
// refused once it resolves: the output fails naming input, so no child is registered with it.
func refuseSeed(value pulumi.StringPtrInput, hub, input string) pulumi.StringPtrOutput {
	return value.ToStringPtrOutput().ApplyT(func(v *string) (*string, error) {
		if v != nil && bus.HoldsSeed(*v) {
			return nil, fmt.Errorf("%s: %w", hub, seedRefused(input))
		}
		return v, nil
	}).(pulumi.StringPtrOutput)
}

// refuseSeedInSecret is value, the secret tailnetAuthKey, as a secret, with a value holding an nkey
// seed refused once it resolves: the output fails naming input and never the value, so no secret
// version is written with it. ApplyT keeps value's secretness, and ToSecret marks the result
// secret whatever value's own marking.
func refuseSeedInSecret(value pulumi.StringInput, hub, input string) pulumi.StringOutput {
	return pulumi.ToSecret(value.ToStringOutput().ApplyT(func(v string) (string, error) {
		if bus.HoldsSeed(v) {
			return "", fmt.Errorf("%s: %w", hub, seedRefused(input))
		}
		return v, nil
	})).(pulumi.StringOutput)
}

// authorization maps the hub's users onto core's. Each of users is a keyed user with a seed, whose
// permissions are its host's set when it names a machine id and its own when it names permissions.
// The listener's user follows them, named hostname, with the listener's set for that machine and a
// seed; the session user comes last, with the session's set and no seed, as the authorization's
// noAuthUser. A hub needs users or a listener: with the session user alone, no client could hold
// more than a session's grant, and no listener could run. core validates the rest when it renders,
// a seed in a name or a rule entry first. The hub's own refusals name a user by its index alone:
// they come before core has read the names, so quoting one could quote a seed.
func authorization(hostname string, users []NATSUserArgs, l *HubListenerArgs, session *SessionUserArgs) (core.Authorization, error) {
	if len(users) == 0 && l == nil {
		return core.Authorization{}, errors.New("users: at least one user is required, or a listener")
	}
	auth := core.Authorization{Users: make([]core.User, 0, len(users)+2)}
	for i, u := range users {
		where := fmt.Sprintf("users[%d]", i)
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
	if l != nil {
		if l.NKey == "" {
			return core.Authorization{}, errors.New("listener.nkey is required")
		}
		perms := core.ListenerPermissions(hostname)
		auth.Users = append(auth.Users, core.User{
			Name: hostname, NKey: l.NKey, Publish: &perms.Publish, Subscribe: &perms.Subscribe, HasSeed: true,
			Label: "listener",
		})
	}
	if session != nil {
		if session.NKey == "" {
			return core.Authorization{}, errors.New("sessionUser.nkey is required")
		}
		perms := core.SessionPermissions()
		auth.Users = append(auth.Users, core.User{
			Name: session.Name, NKey: session.NKey, Publish: &perms.Publish, Subscribe: &perms.Subscribe,
			Label: "sessionUser",
		})
		auth.NoAuthUser = session.Name
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
	if bus.HoldsSeed(name) {
		return nil, fmt.Errorf("%s: %w", natsHubType, core.SeedRefusal("the component's name"))
	}
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
		SecretData: refuseSeedInSecret(args.TailnetAuthKey, natsHubType+" "+name, "tailnetAuthKey"),
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
	// The listener's seed secret is the consumer's, as the seed in it is: the hub only reads it.
	readsSecrets := []pulumi.Resource{version, accessor}
	if l := s.vm.listener; l != nil {
		seedAccessor, err := secretmanager.NewSecretIamMember(ctx, hostname+"-listener-seed-accessor", &secretmanager.SecretIamMemberArgs{
			SecretId: pulumi.String(l.seedSecret),
			Role:     pulumi.String("roles/secretmanager.secretAccessor"),
			Member:   account.Member,
		}, child)
		if err != nil {
			return nil, err
		}
		readsSecrets = append(readsSecrets, seedAccessor)
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
		network = refuseSeed(args.Network, natsHubType+" "+name, "network").Elem()
	}
	var subnetwork pulumi.StringPtrInput
	if args.Subnetwork != nil {
		subnetwork = refuseSeed(args.Subnetwork, natsHubType+" "+name, "subnetwork")
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
				DiskType:   pulumi.String(s.diskType),
			},
		},
		NetworkInterfaces: compute.InstanceTemplateNetworkInterfaceArray{compute.InstanceTemplateNetworkInterfaceArgs{
			Network:    network,
			Subnetwork: subnetwork,
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

	// The VM fetches the key at its first boot, and the listener's seed whenever the listener
	// starts, so its group is created once the key's version and the account's access to both
	// exist. pulumi up waits for the group to be stable, which it is once its VM is healthy, so a
	// create or update may wait for the first boot's read of the data disk: its timeout is the
	// initial delay after the provider's own (groupWait).
	wait := (time.Duration(s.initialDelaySec)*time.Second + groupWait).String()
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
			InitialDelaySec: pulumi.Int(s.initialDelaySec),
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
	}, child, pulumi.DependsOn(readsSecrets),
		pulumi.Timeouts(&pulumi.CustomTimeouts{Create: wait, Update: wait}))
	if err != nil {
		return nil, err
	}

	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it.
	comp.NatsUrl = pulumi.String(fmt.Sprintf("nats://%s:%d", hostname, natsPort)).ToStringOutput()
	comp.InstanceGroup = group.Name
	return comp, nil
}
