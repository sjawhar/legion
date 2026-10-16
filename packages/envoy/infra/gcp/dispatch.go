package gcp

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/internal/bus"
)

// dispatchType is the component's token. pulumi-go-provider derives the same one from the Dispatch
// type's package and name, and serves it under that token; NewDispatch registers with the constant
// so it also runs outside the plugin, where no call carries a token.
const dispatchType = "envoy-infra:gcp:Dispatch"

const (
	defaultDispatchDatabaseVersion = "POSTGRES_16"
	defaultDispatchDatabaseTier    = "db-custom-1-3840"
	defaultDispatchNamespace       = "dispatch"
)

var (
	tailnetDNSNameRule = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$`)
	signInDomainRule   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	namespaceRule      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// DispatchArgs are the inputs of an envoy-infra:gcp:Dispatch. Every field of a plain Go type is a
// value the consumer's program knows when it runs; the pulumi.*Input fields may come from another
// resource or stack.
type DispatchArgs struct {
	Region         string             `pulumi:"region"`
	Hostname       string             `pulumi:"hostname"`
	TailnetDnsName string             `pulumi:"tailnetDnsName"`
	Tags           []string           `pulumi:"tags"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`

	EnvoyImage         string `pulumi:"envoyImage"`
	TailscaleImage     string `pulumi:"tailscaleImage"`
	CloudSqlProxyImage string `pulumi:"cloudSqlProxyImage"`

	NatsUrl        pulumi.StringInput `pulumi:"natsUrl"`
	NatsSeedSecret pulumi.StringInput `pulumi:"natsSeedSecret"`

	SignInClientId     string             `pulumi:"signInClientId"`
	SignInClientSecret pulumi.StringInput `pulumi:"signInClientSecret" provider:"secret"`
	SignInDomain       string             `pulumi:"signInDomain"`

	DatabaseVersion    *string `pulumi:"databaseVersion,optional"`
	DatabaseTier       *string `pulumi:"databaseTier,optional"`
	DeletionProtection *bool   `pulumi:"deletionProtection,optional"`
	Namespace          *string `pulumi:"namespace,optional"`
}

// dispatchSeededInput names the first plain string input that holds an nkey seed. No component
// input takes an nkey seed here: each value reaches a cloud resource, a Kubernetes object, or the
// pod configuration, so a seed would be disclosed or used as the wrong credential.
func dispatchSeededInput(args *DispatchArgs) (string, bool) {
	for _, value := range []struct {
		input string
		value string
	}{
		{"region", args.Region},
		{"hostname", args.Hostname},
		{"tailnetDnsName", args.TailnetDnsName},
		{"envoyImage", args.EnvoyImage},
		{"tailscaleImage", args.TailscaleImage},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage},
		{"signInClientId", args.SignInClientId},
		{"signInDomain", args.SignInDomain},
	} {
		if bus.HoldsSeed(value.value) {
			return value.input, true
		}
	}
	for i, tag := range args.Tags {
		if bus.HoldsSeed(tag) {
			return fmt.Sprintf("tags[%d]", i), true
		}
	}
	if args.DatabaseVersion != nil && bus.HoldsSeed(*args.DatabaseVersion) {
		return "databaseVersion", true
	}
	if args.DatabaseTier != nil && bus.HoldsSeed(*args.DatabaseTier) {
		return "databaseTier", true
	}
	if args.Namespace != nil && bus.HoldsSeed(*args.Namespace) {
		return "namespace", true
	}
	return "", false
}

// Dispatch is Dispatch on an Autopilot cluster of its own, with Cloud SQL, reached over a tailnet.
type Dispatch struct {
	pulumi.ResourceState
	Url              pulumi.StringOutput `pulumi:"url"`
	ClusterName      pulumi.StringOutput `pulumi:"clusterName"`
	DatabaseInstance pulumi.StringOutput `pulumi:"databaseInstance"`
	Kubeconfig       pulumi.StringOutput `pulumi:"kubeconfig"`
	Principal        pulumi.StringOutput `pulumi:"principal"`
	AgentToken       pulumi.StringOutput `pulumi:"agentToken" provider:"secret"`
}

// dispatchSettings are the plain inputs, validated, with the defaults applied, and the texts built
// from them alone, so they are known at preview: the Serve configuration and the listener's
// environment.
type dispatchSettings struct {
	hostname, tailnetDNSName, region, namespace string
	tags                                        []string
	images                                      struct{ envoy, tailscale, proxy string }
	signIn                                      struct{ clientID, domain string }
	databaseVersion, databaseTier               string
	deletionProtection                          bool
	serveConfig                                 string
}

// newDispatchSettings validates the component's plain inputs and renders the values known before
// any cloud resource is registered. Every refusal names the input that needs correction.
func newDispatchSettings(args *DispatchArgs) (dispatchSettings, error) {
	if input, ok := dispatchSeededInput(args); ok {
		return dispatchSettings{}, core.SeedRefusal(input)
	}
	if !hostnameRule.MatchString(args.Hostname) {
		return dispatchSettings{}, fmt.Errorf("hostname %q must match %s", args.Hostname, hostnameRule)
	}
	if !tailnetDNSNameRule.MatchString(args.TailnetDnsName) {
		return dispatchSettings{}, fmt.Errorf("tailnetDnsName %q must match %s", args.TailnetDnsName, tailnetDNSNameRule)
	}
	if len(args.Tags) == 0 {
		return dispatchSettings{}, errors.New("tags: at least one tag is required")
	}
	for i, tag := range args.Tags {
		if !tagRule.MatchString(tag) {
			return dispatchSettings{}, fmt.Errorf("tags[%d] %q must match %s", i, tag, tagRule)
		}
	}
	for _, image := range []struct{ field, ref string }{
		{"envoyImage", args.EnvoyImage},
		{"tailscaleImage", args.TailscaleImage},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return dispatchSettings{}, fmt.Errorf("%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", image.field, image.ref)
		}
	}
	if args.Region == "" {
		return dispatchSettings{}, errors.New("region is required")
	}
	if args.SignInClientId == "" {
		return dispatchSettings{}, errors.New("signInClientId is required")
	}
	if !signInDomainRule.MatchString(args.SignInDomain) {
		return dispatchSettings{}, fmt.Errorf("signInDomain %q must be a lowercase DNS name", args.SignInDomain)
	}
	if args.TailnetAuthKey == nil {
		return dispatchSettings{}, errors.New("tailnetAuthKey is required")
	}
	if args.NatsUrl == nil {
		return dispatchSettings{}, errors.New("natsUrl is required")
	}
	if args.NatsSeedSecret == nil {
		return dispatchSettings{}, errors.New("natsSeedSecret is required")
	}
	if args.SignInClientSecret == nil {
		return dispatchSettings{}, errors.New("signInClientSecret is required")
	}

	s := dispatchSettings{
		hostname:           args.Hostname,
		tailnetDNSName:     args.TailnetDnsName,
		region:             args.Region,
		tags:               args.Tags,
		databaseVersion:    defaultDispatchDatabaseVersion,
		databaseTier:       defaultDispatchDatabaseTier,
		deletionProtection: true,
		namespace:          defaultDispatchNamespace,
	}
	s.images.envoy = args.EnvoyImage
	s.images.tailscale = args.TailscaleImage
	s.images.proxy = args.CloudSqlProxyImage
	s.signIn.clientID = args.SignInClientId
	s.signIn.domain = args.SignInDomain
	if args.DatabaseVersion != nil {
		if *args.DatabaseVersion == "" {
			return dispatchSettings{}, errors.New("databaseVersion is empty; leave it unset for " + defaultDispatchDatabaseVersion)
		}
		s.databaseVersion = *args.DatabaseVersion
	}
	if args.DatabaseTier != nil {
		if *args.DatabaseTier == "" {
			return dispatchSettings{}, errors.New("databaseTier is empty; leave it unset for " + defaultDispatchDatabaseTier)
		}
		s.databaseTier = *args.DatabaseTier
	}
	if args.DeletionProtection != nil {
		s.deletionProtection = *args.DeletionProtection
	}
	if args.Namespace != nil {
		if !namespaceRule.MatchString(*args.Namespace) {
			return dispatchSettings{}, fmt.Errorf("namespace %q must be a DNS label", *args.Namespace)
		}
		s.namespace = *args.Namespace
	}
	s.serveConfig = fmt.Sprintf(`{"TCP":{"443":{"HTTPS":true}},"Web":{"%s.%s:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8766"}}}}}`,
		s.hostname, s.tailnetDNSName)
	return s, nil
}

// NewDispatch registers a Dispatch component after resolving all plain inputs and requiring the
// providers its cluster, database, generated secrets, and Kubernetes resources use.
func NewDispatch(ctx *pulumi.Context, name string, args *DispatchArgs, opts ...pulumi.ResourceOption) (*Dispatch, error) {
	s, err := newDispatchSettings(args)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", dispatchType, name, err)
	}
	if err := providers.Require(ctx.Context(), dispatchType, "gcp", opts...); err != nil {
		return nil, err
	}
	if err := providers.Require(ctx.Context(), dispatchType, "random", opts...); err != nil {
		return nil, err
	}

	comp := &Dispatch{}
	if err := ctx.RegisterComponentResource(dispatchType, name, comp, opts...); err != nil {
		return nil, err
	}
	cloud, err := newDispatchCloud(ctx, comp, name, args, s, opts...)
	if err != nil {
		return nil, err
	}
	comp.ClusterName = cloud.cluster.Name
	comp.DatabaseInstance = cloud.instance.ConnectionName
	comp.Principal = cloud.principal
	comp.AgentToken = cloud.agentToken.Result
	comp.Url = pulumi.String("https://" + s.hostname + "." + s.tailnetDNSName).ToStringOutput()
	return comp, nil
}
