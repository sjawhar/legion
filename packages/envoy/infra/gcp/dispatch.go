package gcp

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

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
	defaultDispatchReplicas        = 1

	// minDispatchReplicas is the fewest pods a workload of this component may run, and the count
	// at which a Deployment must be recreated rather than rolled: two processes of one pod's kind
	// must not overlap. It is what an unset replicas happens to default to, not the same rule.
	minDispatchReplicas = 1

	// dispatchStrategyRecreate stops every running pod before starting the new one;
	// dispatchStrategyRollingUpdate starts new pods beside the old ones. dispatchStrategyFor picks
	// between them.
	dispatchStrategyRecreate      = "Recreate"
	dispatchStrategyRollingUpdate = "RollingUpdate"
)

var (
	tailnetDNSNameRule = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$`)
	signInDomainRule   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	namespaceRule      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretKeyRule      = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
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

	// AgentSecretsUrl and agentSecretsAudience turn on Dispatch's secrets-broker relay together:
	// DISPATCH_AGENT_SECRETS_URL (an absolute http(s) URL with no path; a trailing slash is
	// trimmed, as Dispatch trims it) and the audience of the projected ServiceAccount token the
	// app presents to the broker on every call, read at call time from the file
	// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE names. AgentSecretsAudience is the broker's
	// BROKER_K8S_OIDC_AUDIENCE: the kubelet mints the token for it and refreshes it in place, so
	// no token is an input, a stored secret or part of the component's state. Set both or neither.
	AgentSecretsUrl      string `pulumi:"agentSecretsUrl,optional"`
	AgentSecretsAudience string `pulumi:"agentSecretsAudience,optional"`

	// AppClientId, appId, appPrivateKeySecretName and appPrivateKeySecretKey turn on Dispatch's
	// GitHub App together: DISPATCH_APP_CLIENT_ID and DISPATCH_APP_ID as plain values, and
	// DISPATCH_APP_PEM_B64 read from the key AppPrivateKeySecretKey of the Kubernetes Secret
	// AppPrivateKeySecretName in the component's namespace. That Secret is created at run time
	// (GKE SecretSync), never by this component, and the key never enters it or its state. Set
	// all four or none.
	AppClientId             string `pulumi:"appClientId,optional"`
	AppId                   string `pulumi:"appId,optional"`
	AppPrivateKeySecretName string `pulumi:"appPrivateKeySecretName,optional"`
	AppPrivateKeySecretKey  string `pulumi:"appPrivateKeySecretKey,optional"`

	DatabaseVersion    *string `pulumi:"databaseVersion,optional"`
	DatabaseTier       *string `pulumi:"databaseTier,optional"`
	DeletionProtection *bool   `pulumi:"deletionProtection,optional"`
	Namespace          *string `pulumi:"namespace,optional"`
	// ExistingNamespace: use the namespace as it exists in the cluster rather than creating it
	// (another program owns the namespace).
	ExistingNamespace *bool `pulumi:"existingNamespace,optional"`

	// Replicas is the app Deployment's replica count, 1 by default. Above 1 the Deployment rolls
	// its pods instead of being recreated.
	Replicas *int `pulumi:"replicas,optional"`

	// AllowedNamespaces names the Kubernetes namespaces, in the Dispatch pod's own cluster, whose
	// pods the app's NetworkPolicy admits on port 8766. Unset or empty admits no in-cluster
	// namespace: only the component's own tailnet front door, which Tailscale Serve on 443 proxies
	// through, reaches the app.
	AllowedNamespaces []string `pulumi:"allowedNamespaces,optional"`
}

// dispatchSeededInput names the first plain string input that holds an nkey seed. No component
// input takes an nkey seed here: each value reaches a cloud resource, a Kubernetes object, or the
// pod configuration, so a seed would be disclosed or used as the wrong credential. An int input,
// such as replicas, cannot carry an nkey seed, so it takes no row here.
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
		{"agentSecretsUrl", args.AgentSecretsUrl},
		{"agentSecretsAudience", args.AgentSecretsAudience},
		{"appClientId", args.AppClientId},
		{"appId", args.AppId},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey},
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
	for i, namespace := range args.AllowedNamespaces {
		if bus.HoldsSeed(namespace) {
			return fmt.Sprintf("allowedNamespaces[%d]", i), true
		}
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
	agentSecretsURL, agentSecretsAudience       string
	appClientID, appID                          string
	appKeySecretName, appKeySecretKey           string
	deletionProtection, existingNamespace       bool
	replicas                                    int
	strategy                                    dispatchStrategy
	serveConfig                                 string
}

// dispatchRollingUpdate bounds a rolling update: how many of the wanted pods may be missing at
// once, and how many may run above that count.
type dispatchRollingUpdate struct {
	maxUnavailable, maxSurge int
}

// dispatchStrategy is the app Deployment's update strategy. rollingUpdate is set only when
// strategyType is dispatchStrategyRollingUpdate; a Recreate Deployment carries no such block.
type dispatchStrategy struct {
	strategyType  string
	rollingUpdate *dispatchRollingUpdate
}

// dispatchStrategyFor turns a workload's replica count into its Deployment's update strategy. At
// minDispatchReplicas two processes of that workload must never overlap, since a second
// envoy-dispatch cannot bind the exclusive NATS push durable, so Recreate, which stops the old pod
// before starting the new one, is the only safe strategy. Above that count the operator has
// accepted the overlap and wants no gap in service, so the Deployment rolls: one new pod comes up
// before any old one goes down.
func dispatchStrategyFor(replicas int) dispatchStrategy {
	if replicas <= minDispatchReplicas {
		return dispatchStrategy{strategyType: dispatchStrategyRecreate}
	}
	return dispatchStrategy{
		strategyType:  dispatchStrategyRollingUpdate,
		rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
	}
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
	if args.AgentSecretsAudience != "" && args.AgentSecretsUrl == "" {
		return dispatchSettings{}, errors.New("agentSecretsUrl is required when agentSecretsAudience is set")
	}
	agentSecretsURL := strings.TrimSuffix(args.AgentSecretsUrl, "/")
	if args.AgentSecretsUrl != "" {
		if args.AgentSecretsAudience == "" {
			return dispatchSettings{}, errors.New("agentSecretsAudience is required when agentSecretsUrl is set")
		}
		// The shape Dispatch itself requires of DISPATCH_AGENT_SECRETS_URL, checked here so a bad
		// URL fails the resolver rather than the pod's boot. It is rendered as a plain environment
		// variable, so a user, a password, a query or a fragment, any of which could carry a
		// credential, is refused too, and no refusal quotes it.
		parsed, err := url.Parse(agentSecretsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return dispatchSettings{}, errors.New("agentSecretsUrl must be an absolute http(s) URL with no path, as Dispatch requires of DISPATCH_AGENT_SECRETS_URL (not echoed)")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(agentSecretsURL, "#") {
			return dispatchSettings{}, errors.New("agentSecretsUrl must carry no user, password, query or fragment: it is rendered as a plain environment variable (not echoed)")
		}
	}
	var appSet, appMissing []string
	for _, in := range []struct{ input, value string }{
		{"appClientId", args.AppClientId},
		{"appId", args.AppId},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey},
	} {
		if in.value != "" {
			appSet = append(appSet, in.input)
		} else {
			appMissing = append(appMissing, in.input)
		}
	}
	if len(appSet) > 0 && len(appMissing) > 0 {
		return dispatchSettings{}, fmt.Errorf("%s required when %s is set: appClientId, appId, appPrivateKeySecretName and appPrivateKeySecretKey are set together or not at all",
			strings.Join(appMissing, ", "), appSet[0])
	}
	if len(appSet) > 0 {
		if id, err := strconv.ParseUint(args.AppId, 10, 64); err != nil || id == 0 {
			return dispatchSettings{}, errors.New("appId must be the GitHub App's numeric ID: digits only, not 0 (not echoed)")
		}
		if !dnsSubdomain(args.AppPrivateKeySecretName) {
			return dispatchSettings{}, fmt.Errorf("appPrivateKeySecretName %q must be a DNS subdomain", args.AppPrivateKeySecretName)
		}
		if !secretKeyRule.MatchString(args.AppPrivateKeySecretKey) {
			return dispatchSettings{}, fmt.Errorf("appPrivateKeySecretKey %q must match %s", args.AppPrivateKeySecretKey, secretKeyRule)
		}
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
		replicas:           defaultDispatchReplicas,
	}
	s.images.envoy = args.EnvoyImage
	s.images.tailscale = args.TailscaleImage
	s.images.proxy = args.CloudSqlProxyImage
	s.signIn.clientID = args.SignInClientId
	s.signIn.domain = args.SignInDomain
	s.agentSecretsURL = agentSecretsURL
	s.agentSecretsAudience = args.AgentSecretsAudience
	s.appClientID = args.AppClientId
	s.appID = args.AppId
	s.appKeySecretName = args.AppPrivateKeySecretName
	s.appKeySecretKey = args.AppPrivateKeySecretKey
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
	if args.Replicas != nil {
		if *args.Replicas < minDispatchReplicas {
			return dispatchSettings{}, fmt.Errorf("replicas %d must be at least %d", *args.Replicas, minDispatchReplicas)
		}
		s.replicas = *args.Replicas
	}
	s.strategy = dispatchStrategyFor(s.replicas)
	s.existingNamespace = args.ExistingNamespace != nil && *args.ExistingNamespace
	if args.Namespace != nil {
		if !dnsLabel(*args.Namespace) {
			return dispatchSettings{}, fmt.Errorf("namespace %q must be a DNS label", *args.Namespace)
		}
		s.namespace = *args.Namespace
	}
	for i, namespace := range args.AllowedNamespaces {
		if !dnsLabel(namespace) {
			return dispatchSettings{}, fmt.Errorf("allowedNamespaces[%d] %q must be a DNS label", i, namespace)
		}
	}
	// The tailnet node runs in a pod of its own now, so Serve proxies to the app's Service rather
	// than to a container beside it. The Web key keeps the node's name and certificate domain.
	s.serveConfig = fmt.Sprintf(`{"TCP":{"443":{"HTTPS":true}},"Web":{"%s:443":{"Handlers":{"/":{"Proxy":"http://%s:8766"}}}}}`,
		core.TailnetHostName(s.hostname, s.tailnetDNSName), clusterLocal(s.hostname, s.namespace))
	return s, nil
}

// dnsLabel reports whether value is a DNS-1123 label: namespaceRule's shape, at most 63 characters.
func dnsLabel(value string) bool {
	return len(value) <= 63 && namespaceRule.MatchString(value)
}

// dnsSubdomain reports whether value is a DNS-1123 subdomain, the shape of a Kubernetes Secret's
// name: dot-separated DNS labels, at most 253 characters.
func dnsSubdomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !dnsLabel(label) {
			return false
		}
	}
	return true
}

// NewDispatch registers a Dispatch component after resolving all plain inputs and requiring the
// providers its cluster, database, generated secrets, and Kubernetes resources use.
func NewDispatch(ctx *pulumi.Context, name string, args *DispatchArgs, opts ...pulumi.ResourceOption) (*Dispatch, error) {
	// The name is quoted in every refusal below and the component registers under it, so a seed
	// there is refused first, as NewNatsHub refuses one in its own name.
	if bus.HoldsSeed(name) {
		return nil, fmt.Errorf("%s: %w", dispatchType, core.SeedRefusal("the component's name"))
	}
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
	kube, err := newDispatchKubernetes(ctx, comp, name, args, s, cloud)
	if err != nil {
		return nil, err
	}
	comp.ClusterName = cloud.cluster.Name
	comp.DatabaseInstance = cloud.instance.ConnectionName
	comp.Kubeconfig = kube.kubeconfig
	comp.Principal = cloud.principal
	comp.AgentToken = cloud.agentToken.Result
	comp.Url = pulumi.String("https://" + core.TailnetHostName(s.hostname, s.tailnetDNSName)).ToStringOutput()
	return comp, nil
}
