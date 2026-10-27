package gcp

import (
	"errors"
	"fmt"
	"math/big"
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

	// defaultAzureFederationAudience is the audience of the keyless form's projected token when
	// azureFederationAudience is unset: the one Entra's federated credentials expect by default.
	defaultAzureFederationAudience = "api://AzureADTokenExchange"

	// defaultDispatchAppCPU and defaultDispatchAppMemory size the app's envoy-dispatch container
	// when appCpu and appMemory are unset.
	defaultDispatchAppCPU    = "1"
	defaultDispatchAppMemory = "4Gi"

	// dispatchProxyCPU and dispatchProxyMemory size the app pod's cloud-sql-proxy sidecar, which
	// Autopilot counts in the pod's request beside envoy-dispatch.
	dispatchProxyCPU    = "200m"
	dispatchProxyMemory = "256Mi"
)

// Autopilot's bounds for a pod's total request, the general-purpose compute class's under the
// safe-to-evict annotation (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests):
// below the floor or outside the memory-per-CPU ratio Autopilot raises the pod's request, so the
// container would not run at the size the component renders; above the ceiling it rejects the pod.
// autopilotPodSize checks a pod against them.
var (
	autopilotPodMinCPU    = big.NewRat(1, 2)
	autopilotPodMaxCPU    = big.NewRat(30, 1)
	autopilotPodMinMemory = new(big.Rat).SetInt64(512 << 20)
	autopilotPodMaxMemory = new(big.Rat).SetInt64(110 << 30)
	autopilotMinGiBPerCPU = big.NewRat(1, 1)
	autopilotMaxGiBPerCPU = big.NewRat(13, 2)
)

var (
	tailnetDNSNameRule = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ts\.net$`)
	signInDomainRule   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	namespaceRule      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretKeyRule      = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	// quantityRule is a Kubernetes resource quantity (k8s.io/apimachinery's resource.Quantity):
	// a signed decimal, then a binary suffix, a decimal suffix or a decimal exponent.
	quantityRule = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(Ki|Mi|Gi|Ti|Pi|Ei|n|u|m|k|M|G|T|P|E|[eE][+-]?[0-9]+)?$`)
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

	// AppCpu and appMemory size the app Deployment's envoy-dispatch container, as Kubernetes
	// quantities set as both its request and its limit: 1 CPU and 4Gi by default. The pod's
	// cloud-sql-proxy sidecar keeps its own 200m and 256Mi, and the front door and the listener
	// are unchanged.
	AppCpu    *string `pulumi:"appCpu,optional"`
	AppMemory *string `pulumi:"appMemory,optional"`

	// PprofPort turns on envoy-dispatch's Go profiler: DISPATCH_PPROF_PORT on its container alone,
	// so it serves net/http/pprof on 127.0.0.1 at this port, reachable through kubectl
	// port-forward and from nowhere outside the pod. Unset, nothing is rendered and no profiler
	// listens.
	PprofPort *int `pulumi:"pprofPort,optional"`

	// AppPriorityClassName is the app pod's priorityClassName, the name of a PriorityClass that
	// already exists in the cluster, which this component reads but never creates: a missing class
	// fails the preview or update before the app Deployment changes, since Kubernetes refuses every
	// pod naming one. Unset, the pod names none and runs at the cluster's default priority. The
	// front door and the listener are unchanged.
	AppPriorityClassName *string `pulumi:"appPriorityClassName,optional"`

	// AppPdbMinAvailable turns on a PodDisruptionBudget, <hostname>, over the app's pods: its
	// minAvailable, from 1 to replicas. Unset, the component creates no budget.
	AppPdbMinAvailable *int `pulumi:"appPdbMinAvailable,optional"`

	// AllowedNamespaces names the Kubernetes namespaces, in the Dispatch pod's own cluster, whose
	// pods the app's NetworkPolicy admits on port 8766. Unset or empty admits no in-cluster
	// namespace: only the component's own tailnet front door, which Tailscale Serve on 443 proxies
	// through, reaches the app.
	AllowedNamespaces []string `pulumi:"allowedNamespaces,optional"`

	// AzureEmbedEndpoint turns on meaning search through Azure AI Foundry: DISPATCH_EMBEDDER=azure
	// and DISPATCH_AZURE_EMBED_ENDPOINT, the base URL the embeddings route is appended to (such as
	// https://<resource>.services.ai.azure.com/models), which may be an Azure resource's output.
	// AzureEmbedTokensPerMinute, the deployment's quota, comes with it, and AzureEmbedDeployment
	// (DISPATCH_AZURE_EMBED_DEPLOYMENT, Dispatch's default when unset) may. Every other Azure input
	// needs the endpoint.
	AzureEmbedEndpoint        pulumi.StringInput `pulumi:"azureEmbedEndpoint,optional"`
	AzureEmbedDeployment      pulumi.StringInput `pulumi:"azureEmbedDeployment,optional"`
	AzureEmbedTokensPerMinute *int               `pulumi:"azureEmbedTokensPerMinute,optional"`

	// The endpoint takes exactly one credential. AzureEmbedKeySecret is the key form: the Secret
	// Manager secret's resource path (projects/<project>/secrets/<id>) holding the deployment's API
	// key, mounted through the Secret Manager CSI driver as the file DISPATCH_AZURE_EMBED_KEY_FILE
	// names, so the key never enters the component or its state. AzureTenantId and AzureClientId
	// are the keyless form, set together (a managed identity's outputs): the pod's projected
	// service-account token, for audience AzureFederationAudience (api://AzureADTokenExchange when
	// unset), is the assertion Dispatch exchanges with Entra, through the federated credential the
	// identity holds for this component's serviceAccountIssuer and serviceAccountSubject.
	AzureEmbedKeySecret     pulumi.StringInput `pulumi:"azureEmbedKeySecret,optional"`
	AzureTenantId           pulumi.StringInput `pulumi:"azureTenantId,optional"`
	AzureClientId           pulumi.StringInput `pulumi:"azureClientId,optional"`
	AzureFederationAudience *string            `pulumi:"azureFederationAudience,optional"`
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
	if args.AppCpu != nil && bus.HoldsSeed(*args.AppCpu) {
		return "appCpu", true
	}
	if args.AppMemory != nil && bus.HoldsSeed(*args.AppMemory) {
		return "appMemory", true
	}
	if args.AppPriorityClassName != nil && bus.HoldsSeed(*args.AppPriorityClassName) {
		return "appPriorityClassName", true
	}
	if args.Namespace != nil && bus.HoldsSeed(*args.Namespace) {
		return "namespace", true
	}
	for i, namespace := range args.AllowedNamespaces {
		if bus.HoldsSeed(namespace) {
			return fmt.Sprintf("allowedNamespaces[%d]", i), true
		}
	}
	if args.AzureFederationAudience != nil && bus.HoldsSeed(*args.AzureFederationAudience) {
		return "azureFederationAudience", true
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
	// ServiceAccountIssuer and ServiceAccountSubject are the issuer and subject of the Dispatch
	// pods' service-account tokens: the cluster's OIDC issuer, and the pods' Kubernetes service
	// account. A federated credential trusting the pair (an Azure identity's, for the keyless
	// form) admits exactly those pods.
	ServiceAccountIssuer  pulumi.StringOutput `pulumi:"serviceAccountIssuer"`
	ServiceAccountSubject pulumi.StringOutput `pulumi:"serviceAccountSubject"`
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
	appCPU, appMemory                           string
	pprofPort                                   int
	appPriorityClassName                        string
	appPDBMinAvailable                          int
	strategy                                    dispatchStrategy
	serveConfig                                 string
	azure                                       dispatchAzure
}

// dispatchAzure is the Azure embedder's plain configuration, the zero value when
// azureEmbedEndpoint is unset. When enabled, exactly one of key (azureEmbedKeySecret) and keyless
// (azureTenantId with azureClientId) is set, and audience is the keyless form's token audience.
type dispatchAzure struct {
	key, keyless    bool
	tokensPerMinute int
	audience        string
}

// enabled reports whether azureEmbedEndpoint turned the Azure embedder on: one credential form is
// then set.
func (a dispatchAzure) enabled() bool { return a.key || a.keyless }

// newDispatchAzure validates the Azure embedder's inputs: the endpoint turns it on and needs the
// quota and exactly one credential form; no other Azure input is taken without the endpoint. The
// Inputs are judged by being set alone, so a value unknown at preview passes as it will at update.
func newDispatchAzure(args *DispatchArgs) (dispatchAzure, error) {
	keyless := args.AzureTenantId != nil || args.AzureClientId != nil
	if args.AzureEmbedEndpoint == nil {
		for _, in := range []struct {
			input string
			set   bool
		}{
			{"azureEmbedDeployment", args.AzureEmbedDeployment != nil},
			{"azureEmbedTokensPerMinute", args.AzureEmbedTokensPerMinute != nil},
			{"azureEmbedKeySecret", args.AzureEmbedKeySecret != nil},
			{"azureTenantId", args.AzureTenantId != nil},
			{"azureClientId", args.AzureClientId != nil},
			{"azureFederationAudience", args.AzureFederationAudience != nil},
		} {
			if in.set {
				return dispatchAzure{}, fmt.Errorf("azureEmbedEndpoint is required when %s is set: every Azure input configures the Azure embedder, which the endpoint turns on", in.input)
			}
		}
		return dispatchAzure{}, nil
	}
	if args.AzureEmbedTokensPerMinute == nil {
		return dispatchAzure{}, errors.New("azureEmbedTokensPerMinute is required when azureEmbedEndpoint is set: the deployment's tokens-per-minute quota, which Dispatch paces its embedding by")
	}
	if *args.AzureEmbedTokensPerMinute <= 0 {
		return dispatchAzure{}, fmt.Errorf("azureEmbedTokensPerMinute %d must be a positive integer", *args.AzureEmbedTokensPerMinute)
	}
	if args.AzureEmbedKeySecret != nil && keyless {
		return dispatchAzure{}, errors.New("azureEmbedKeySecret is set with azureTenantId or azureClientId: the key and keyless credential forms are exclusive, so set exactly one")
	}
	if args.AzureTenantId != nil && args.AzureClientId == nil {
		return dispatchAzure{}, errors.New("azureClientId is required when azureTenantId is set: the keyless form takes both")
	}
	if args.AzureClientId != nil && args.AzureTenantId == nil {
		return dispatchAzure{}, errors.New("azureTenantId is required when azureClientId is set: the keyless form takes both")
	}
	if args.AzureFederationAudience != nil && !keyless {
		return dispatchAzure{}, errors.New("azureFederationAudience is set without azureTenantId and azureClientId: it is the keyless form's token audience")
	}
	if args.AzureEmbedKeySecret == nil && !keyless {
		return dispatchAzure{}, errors.New("azureEmbedEndpoint needs one credential: azureEmbedKeySecret, or azureTenantId with azureClientId")
	}
	azure := dispatchAzure{key: !keyless, keyless: keyless, tokensPerMinute: *args.AzureEmbedTokensPerMinute}
	if keyless {
		azure.audience = defaultAzureFederationAudience
		if args.AzureFederationAudience != nil {
			if *args.AzureFederationAudience == "" {
				return dispatchAzure{}, errors.New("azureFederationAudience is empty; leave it unset for " + defaultAzureFederationAudience)
			}
			azure.audience = *args.AzureFederationAudience
		}
	}
	return azure, nil
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
	azure, err := newDispatchAzure(args)
	if err != nil {
		return dispatchSettings{}, err
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
	s.azure = azure
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
	if s.appCPU, s.appMemory, err = newDispatchAppResources(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.pprofPort, err = newDispatchPprofPort(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.appPriorityClassName, s.appPDBMinAvailable, err = newDispatchAppDisruption(args, s.replicas); err != nil {
		return dispatchSettings{}, err
	}
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

// newDispatchPprofPort resolves pprofPort, the port envoy-dispatch's profiler binds on the pod's
// loopback: 0, rendering nothing, when it is unset. It refuses a value outside 1-65535, which
// Dispatch would refuse at startup, and the two ports the app pod already binds on its loopback,
// envoy-dispatch's own 8766 and the cloud-sql-proxy sidecar's 5432, where the bind would fail and
// the pod would never start.
func newDispatchPprofPort(args *DispatchArgs) (int, error) {
	if args.PprofPort == nil {
		return 0, nil
	}
	port := *args.PprofPort
	switch {
	case port < 1 || port > 65535:
		return 0, fmt.Errorf("pprofPort %d must be a port number 1-65535; leave it unset for no profiler", port)
	case port == 8766:
		return 0, fmt.Errorf("pprofPort %d is envoy-dispatch's own port; choose another, such as 6060", port)
	case port == 5432:
		return 0, fmt.Errorf("pprofPort %d is the cloud-sql-proxy sidecar's port in the same pod; choose another, such as 6060", port)
	}
	return port, nil
}

// newDispatchAppDisruption resolves appPriorityClassName and appPdbMinAvailable, the app pod's
// priority class and its PodDisruptionBudget's minAvailable: "" and 0, rendering nothing, when
// unset. It refuses a class name as podPriorityClass does, and a budget below 1, which protects
// no pod, or above replicas, which no number of ready pods can meet.
func newDispatchAppDisruption(args *DispatchArgs, replicas int) (priorityClass string, pdbMinAvailable int, err error) {
	if priorityClass, err = podPriorityClass("appPriorityClassName", args.AppPriorityClassName); err != nil {
		return "", 0, err
	}
	if args.AppPdbMinAvailable != nil {
		pdbMinAvailable = *args.AppPdbMinAvailable
		if pdbMinAvailable < 1 || pdbMinAvailable > replicas {
			return "", 0, fmt.Errorf("appPdbMinAvailable %d must be from 1 to replicas (%d); leave it unset for no PodDisruptionBudget", pdbMinAvailable, replicas)
		}
	}
	return priorityClass, pdbMinAvailable, nil
}

// newDispatchAppResources resolves appCpu and appMemory, the envoy-dispatch container's request
// and limit, and refuses a size at which Autopilot would not run the app pod as rendered, with the
// cloud-sql-proxy sidecar beside it (autopilotPodSize).
func newDispatchAppResources(args *DispatchArgs) (cpu, memory string, err error) {
	return autopilotPodSize(autopilotPod{
		name:     "the app pod",
		cpuInput: "appCpu", memoryInput: "appMemory",
		cpu: args.AppCpu, memory: args.AppMemory,
		defaultCPU: defaultDispatchAppCPU, defaultMemory: defaultDispatchAppMemory,
		memoryExamples: "4Gi or 6144Mi",
		sidecars:       []autopilotSidecar{{name: "cloud-sql-proxy", cpu: dispatchProxyCPU, memory: dispatchProxyMemory}},
	})
}

// podPriorityClass resolves input, a pod's priority class (name, nil when unset): "" when unset.
// It refuses a name that is empty or not a DNS subdomain, and one with the system- prefix, which
// Kubernetes reserves for its own system-cluster-critical and system-node-critical classes: those
// outrank every workload so the cluster's own components are scheduled first, and an application
// pod holding one could preempt them. Each refusal is a core.InputError naming input.
func podPriorityClass(input string, name *string) (string, error) {
	if name == nil {
		return "", nil
	}
	switch {
	case *name == "":
		return "", core.InputErrorf(input, "%s is empty; leave it unset for no priority class", input)
	case !dnsSubdomain(*name):
		return "", core.InputErrorf(input, "%s %q must be a DNS subdomain, the shape of a PriorityClass's name", input, *name)
	case strings.HasPrefix(*name, "system-"):
		return "", core.InputErrorf(input, "%s %q is a class Kubernetes reserves for its own system components (the system- prefix); name a PriorityClass of your own", input, *name)
	}
	return *name, nil
}

// autopilotPod is a pod Autopilot sizes: its main container, whose request and limit the inputs
// cpuInput and memoryInput set (cpu and memory, nil when unset for defaultCPU and defaultMemory),
// and the sidecars beside it at fixed sizes. name is the pod as a refusal names it, such as "the
// app pod", and memoryExamples the quantities a refusal of memoryInput suggests.
type autopilotPod struct {
	name                      string
	cpuInput, memoryInput     string
	cpu, memory               *string
	defaultCPU, defaultMemory string
	memoryExamples            string
	sidecars                  []autopilotSidecar
}

// autopilotSidecar is a sidecar's fixed size, request and limit, and its name as a refusal writes
// it before 's, such as "cloud-sql-proxy" or "the reloader".
type autopilotSidecar struct{ name, cpu, memory string }

// autopilotPodSize resolves pod's main container's CPU and memory and refuses a size at which
// Autopilot would not run the pod as rendered: the pod's total, with its sidecars', below
// autopilotPodMinCPU or autopilotPodMinMemory or outside the memory-per-CPU ratio (Autopilot
// raises the smaller of the two, so the refusal names that one's input), or above
// autopilotPodMaxCPU or autopilotPodMaxMemory (Autopilot rejects the pod). Each refusal is a
// core.InputError naming cpuInput or memoryInput, and quotes the sidecars' sizes it counted.
func autopilotPodSize(pod autopilotPod) (cpu, memory string, err error) {
	cpu, memory = pod.defaultCPU, pod.defaultMemory
	if pod.cpu != nil {
		if *pod.cpu == "" {
			return "", "", core.InputErrorf(pod.cpuInput, "%s is empty; leave it unset for %s", pod.cpuInput, pod.defaultCPU)
		}
		cpu = *pod.cpu
	}
	if pod.memory != nil {
		if *pod.memory == "" {
			return "", "", core.InputErrorf(pod.memoryInput, "%s is empty; leave it unset for %s", pod.memoryInput, pod.defaultMemory)
		}
		memory = *pod.memory
	}
	podCPU, err := appQuantity(pod.cpuInput, cpu, "1, 1500m or 2")
	if err != nil {
		return "", "", &core.InputError{Input: pod.cpuInput, Err: err}
	}
	podMemory, err := appQuantity(pod.memoryInput, memory, pod.memoryExamples)
	if err != nil {
		return "", "", &core.InputError{Input: pod.memoryInput, Err: err}
	}
	sidecarCPU, sidecarMemory := new(big.Rat), new(big.Rat)
	for _, sidecar := range pod.sidecars {
		value, _ := quantityValue(sidecar.cpu)
		sidecarCPU.Add(sidecarCPU, value)
		value, _ = quantityValue(sidecar.memory)
		sidecarMemory.Add(sidecarMemory, value)
	}
	podCPU.Add(podCPU, sidecarCPU)
	podMemory.Add(podMemory, sidecarMemory)
	// with names each sidecar's size, as size writes it, for a refusal.
	with := func(size func(autopilotSidecar) string) string {
		sizes := make([]string, len(pod.sidecars))
		for i, sidecar := range pod.sidecars {
			sizes[i] = sidecar.name + "'s " + size(sidecar)
		}
		return "with " + strings.Join(sizes, " and ")
	}
	cpuOf := func(s autopilotSidecar) string { return s.cpu }
	memoryOf := func(s autopilotSidecar) string { return s.memory }
	switch {
	case podCPU.Cmp(autopilotPodMinCPU) < 0:
		return "", "", core.InputErrorf(pod.cpuInput, "%s %q must be at least %s: %s %s would request less than %s, below which Autopilot raises it",
			pod.cpuInput, cpu, milliCPU(new(big.Rat).Sub(autopilotPodMinCPU, sidecarCPU)), with(cpuOf), pod.name, milliCPU(autopilotPodMinCPU))
	case podCPU.Cmp(autopilotPodMaxCPU) > 0:
		return "", "", core.InputErrorf(pod.cpuInput, "%s %q must be at most %s: %s %s would request above %s CPU, and Autopilot rejects such a pod",
			pod.cpuInput, cpu, milliCPU(new(big.Rat).Sub(autopilotPodMaxCPU, sidecarCPU)), with(cpuOf), pod.name, autopilotPodMaxCPU.RatString())
	case podMemory.Cmp(autopilotPodMinMemory) < 0:
		return "", "", core.InputErrorf(pod.memoryInput, "%s %q must be at least %s: %s %s would request less than %s, below which Autopilot raises it",
			pod.memoryInput, memory, mebibytes(new(big.Rat).Sub(autopilotPodMinMemory, sidecarMemory)), with(memoryOf), pod.name, mebibytes(autopilotPodMinMemory))
	case podMemory.Cmp(autopilotPodMaxMemory) > 0:
		return "", "", core.InputErrorf(pod.memoryInput, "%s %q must be at most %s: %s %s would request above 110Gi, and Autopilot rejects such a pod",
			pod.memoryInput, memory, mebibytes(new(big.Rat).Sub(autopilotPodMaxMemory, sidecarMemory)), with(memoryOf), pod.name)
	}
	gibPerCPU := new(big.Rat).Quo(podMemory, podCPU)
	gibPerCPU.Quo(gibPerCPU, gibibyte)
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) >= 0 && gibPerCPU.Cmp(autopilotMaxGiBPerCPU) <= 0 {
		return cpu, memory, nil
	}
	smaller := pod.cpuInput // too much memory per CPU: Autopilot raises the CPU
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) < 0 {
		smaller = pod.memoryInput
	}
	return "", "", core.InputErrorf(smaller, "%s %q and %s %q: %s %s must hold between 1 and 6.5 GiB of memory per CPU, or Autopilot raises the smaller of the two",
		pod.cpuInput, cpu, pod.memoryInput, memory,
		with(func(s autopilotSidecar) string { return s.cpu + " and " + s.memory }), pod.name)
}

// appQuantity is the exact value of input's quantity, or a refusal naming input.
func appQuantity(input, value, examples string) (*big.Rat, error) {
	quantity, err := quantityValue(value)
	switch {
	case errors.Is(err, errQuantityExponent):
		return nil, fmt.Errorf("%s %q has a decimal exponent outside e%d to e%d, the range of the suffixes n to E; write it with a suffix, such as %s", input, value, minQuantityExponent, maxQuantityExponent, examples)
	case err != nil:
		return nil, fmt.Errorf("%s %q is not a Kubernetes quantity, such as %s", input, value, examples)
	}
	return quantity, nil
}

var (
	quantityBinarySuffixes  = map[string]uint{"Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40, "Pi": 50, "Ei": 60}
	quantityDecimalSuffixes = map[string]int{"n": -9, "u": -6, "m": -3, "": 0, "k": 3, "M": 6, "G": 9, "T": 12, "P": 15, "E": 18}

	errNotQuantity      = errors.New("not a Kubernetes quantity")
	errQuantityExponent = errors.New("decimal exponent out of range")
)

// minQuantityExponent and maxQuantityExponent bound a quantity's decimal exponent (the e<n> form)
// to the range its suffixes cover, n to E. Every value inside it is computed exactly, whatever
// its mantissa; one outside it is refused rather than computed, since 10^n costs time without
// bound, and no size this component takes needs one.
const (
	minQuantityExponent = -9
	maxQuantityExponent = 18
)

// quantityValue is the exact value of a Kubernetes quantity (CPU in cores, memory in bytes):
// errNotQuantity when value is not one, errQuantityExponent when its decimal exponent is outside
// minQuantityExponent to maxQuantityExponent.
func quantityValue(value string) (*big.Rat, error) {
	match := quantityRule.FindStringSubmatch(value)
	if match == nil {
		return nil, errNotQuantity
	}
	number, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return nil, errNotQuantity
	}
	suffix := match[2]
	if shift, ok := quantityBinarySuffixes[suffix]; ok {
		return number.Mul(number, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), shift))), nil
	}
	exponent, ok := quantityDecimalSuffixes[suffix]
	if !ok {
		// A decimal exponent, whose digits quantityRule guarantees; Atoi fails only past int's
		// range, which is outside the bounds too.
		var err error
		exponent, err = strconv.Atoi(suffix[1:])
		if err != nil || exponent < minQuantityExponent || exponent > maxQuantityExponent {
			return nil, errQuantityExponent
		}
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
	if exponent < 0 {
		return number.Quo(number, new(big.Rat).SetInt(power)), nil
	}
	return number.Mul(number, new(big.Rat).SetInt(power)), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// milliCPU writes a whole number of millicores as a quantity, such as 300m.
func milliCPU(cores *big.Rat) string {
	return new(big.Rat).Mul(cores, big.NewRat(1000, 1)).FloatString(0) + "m"
}

// mebibytes writes a whole number of mebibytes as a quantity, such as 256Mi.
func mebibytes(bytes *big.Rat) string {
	return new(big.Rat).Quo(bytes, new(big.Rat).SetInt64(1<<20)).FloatString(0) + "Mi"
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
	comp.ServiceAccountIssuer = cloud.issuer
	// Every Dispatch pod runs as ServiceAccount <hostname> in the component's namespace, the
	// account newDispatchKubernetes registers, and Kubernetes names a token's subject after it.
	comp.ServiceAccountSubject = pulumi.String("system:serviceaccount:" + s.namespace + ":" + s.hostname).ToStringOutput()
	return comp, nil
}
