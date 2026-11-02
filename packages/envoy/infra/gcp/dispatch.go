package gcp

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"slices"
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
	defaultDispatchDatabaseVersion = "POSTGRES_18"
	// defaultDispatchDatabaseTier is the Cloud SQL tier when databaseTier is unset: 4 vCPU and
	// 15 GB. During a roll, every app pod that holds connections (the replicas, the surge pod and
	// pods still terminating, for up to their grace period) times dispatchConnectionsPerPod must
	// fit in the instance's max_connections less PostgreSQL's 3 superuser-reserved slots, so a
	// consumer sets databaseTier for its replica count.
	defaultDispatchDatabaseTier = "db-custom-4-15360"
	defaultDispatchNamespace    = "dispatch"
	defaultDispatchReplicas     = 1

	// defaultDispatchFrontDoorReplicas and defaultDispatchListenerReplicas are the front door's
	// and the listener's pod counts when frontDoorReplicas and listenerReplicas are unset. The
	// front door runs two by default, since with one pod every node drain, upgrade or scale-down
	// that moves it takes Dispatch off the tailnet until its replacement is ready.
	defaultDispatchFrontDoorReplicas = 2
	defaultDispatchListenerReplicas  = 1

	// minDispatchReplicas is the fewest pods a workload of this component may run, and the count
	// at which its Deployment is recreated rather than rolled: a roll runs the outgoing build and
	// the incoming one together, which is safe only once the outgoing build follows the rules for
	// two builds running side by side and keeps its assets in the shared store. An operator who
	// runs a workload above this count runs such builds. It is what an unset replicas happens to
	// default to, not the same rule.
	minDispatchReplicas = 1

	// dispatchStrategyRecreate stops every running pod before starting the new one;
	// dispatchStrategyRollingUpdate starts new pods beside the old ones. dispatchStrategyFor picks
	// between them.
	dispatchStrategyRecreate      = "Recreate"
	dispatchStrategyRollingUpdate = "RollingUpdate"

	// dispatchAvailabilityZonal and dispatchAvailabilityRegional are the two Cloud SQL
	// availability types databaseAvailabilityType takes: one zone, the default, or a standby in a
	// second zone that Cloud SQL fails over to.
	dispatchAvailabilityZonal    = "ZONAL"
	dispatchAvailabilityRegional = "REGIONAL"

	// defaultAzureFederationAudience is the audience of the keyless form's projected token when
	// azureFederationAudience is unset: the one Entra's federated credentials expect by default.
	defaultAzureFederationAudience = "api://AzureADTokenExchange"

	// defaultDispatchAppCPU and defaultDispatchAppMemory size the app's envoy-dispatch container
	// when appCpu and appMemory are unset.
	defaultDispatchAppCPU    = "1"
	defaultDispatchAppMemory = "4Gi"

	// defaultDispatchProxyCPU and defaultDispatchProxyMemory size the app pod's cloud-sql-proxy
	// sidecar when proxyCpu and proxyMemory are unset; Autopilot counts it in the pod's request
	// beside envoy-dispatch. Every database call of the pod passes through the proxy's TLS: up
	// to dispatchConnectionsPerPod connections at once. The proxy's own
	// Kubernetes sidecar example requests 1 CPU, since its CPU use scales linearly with the IO
	// between the database and the application, and 2Gi, since its memory scales linearly with
	// the active connections
	// (https://github.com/GoogleCloudPlatform/cloud-sql-proxy/blob/v2.26.3/examples/k8s-sidecar/README.md,
	// "Run the Cloud SQL Auth Proxy as a sidecar"). One server pod's pools are that example's
	// load, so these are its values. With the app's defaults the pod requests 2 CPU and 6Gi,
	// inside Autopilot's bounds below.
	defaultDispatchProxyCPU    = "1"
	defaultDispatchProxyMemory = "2Gi"

	// autopilotDefaultEphemeralStorage is the ephemeral storage Autopilot gives a container that
	// names none, which each of the app pod's two containers does while appEphemeralStorage or
	// proxyEphemeralStorage is unset
	// (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests).
	autopilotDefaultEphemeralStorage = "1Gi"
)

// dispatchConnectionsPerPod is the most connections one app pod opens to Postgres: envoy-dispatch's
// store.ConnectionsPerProcess (packages/envoy/internal/dispatch/store/pool.go), the sum of its
// counted pools and its permanent listeners. The listener's and the front door's pods open none.
// TestDispatchConnectionsPerPodIsTheServers fails when a counted pool's size changes and this
// does not.
const dispatchConnectionsPerPod = 93

// Autopilot's bounds for the app pod's total request, the general-purpose compute class's for a pod
// with pod anti-affinity, which every Dispatch pod carries, or with the safe-to-evict annotation,
// which a pod no budget covers carries (https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests):
// below the floor or outside the memory-per-CPU ratio Autopilot raises the pod's request, so the
// container would not run at the size the component renders; above the ceiling it rejects the pod.
var (
	autopilotPodMinCPU    = big.NewRat(1, 2)
	autopilotPodMaxCPU    = big.NewRat(30, 1)
	autopilotPodMinMemory = new(big.Rat).SetInt64(512 << 20)
	autopilotPodMaxMemory = new(big.Rat).SetInt64(110 << 30)
	autopilotMinGiBPerCPU = big.NewRat(1, 1)
	autopilotMaxGiBPerCPU = big.NewRat(13, 2)
	// The pod's ephemeral storage, the sum over its containers, must be from 10Mi to 10Gi in the
	// general-purpose class: Autopilot raises a pod below the floor and rejects one above the
	// ceiling.
	autopilotPodMinEphemeralStorage = new(big.Rat).SetInt64(10 << 20)
	autopilotPodMaxEphemeralStorage = new(big.Rat).SetInt64(10 << 30)
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
	Region   string `pulumi:"region"`
	Hostname string `pulumi:"hostname"`
	// Every input from tailnetDnsName through signInDomain configures Dispatch itself, and is
	// optional in the schema while the resolver is what requires it: a generated Go SDK emits a
	// nil check in its constructor for every required input whose type can be nil, so a
	// cluster-only consumer, which sets none of these, could not call the component at all while
	// they stayed required. With clusterOnly off, four of the twelve — the digest-pinned images
	// envoyImage and cloudSqlProxyImage, natsUrl and natsSeedSecret — every deployment sets, and
	// newDispatchSettings refuses each one missing by name, in the wording it always used. The
	// other eight are two groups, each set whole or left wholly unset: the tailnet group
	// (dispatchTailnetInputs), which builds the front door and serves Dispatch at its tailnet
	// name, and the sign-in group (dispatchSignInInputs), which runs people's browser sign-in.
	// Both unset is the sandbox, which the resolver accepts; the tailnet group set with the
	// sign-in group unset is a tailnet Dispatch that runs no browser sign-in, which it accepts
	// too. A sign-in input sent with the tailnet group unset is refused for being sent at all
	// (refuseDispatchSandbox). Otherwise a group set in part is refused by the per-input refusal
	// it always gave, trailing one clause naming every input of that group still unset
	// (dispatchGroupClause). With clusterOnly on the resolver refuses each of the twelve that
	// is set.
	TailnetDnsName string             `pulumi:"tailnetDnsName,optional"`
	Tags           []string           `pulumi:"tags,optional"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey,optional" provider:"secret"`

	EnvoyImage         string `pulumi:"envoyImage,optional"`
	TailscaleImage     string `pulumi:"tailscaleImage,optional"`
	CloudSqlProxyImage string `pulumi:"cloudSqlProxyImage,optional"`
	// FrontDoorToolsImage is the image the front door's cert-publish container runs on, pinned by
	// digest: it needs /bin/sh, curl, openssl 3, base64, GNU date, sed, grep and tr, which the envoy
	// image carries. An input of its own, so a release of the app's image never rolls the front door.
	FrontDoorToolsImage string `pulumi:"frontDoorToolsImage,optional"`

	NatsUrl        pulumi.StringInput `pulumi:"natsUrl,optional"`
	NatsSeedSecret pulumi.StringInput `pulumi:"natsSeedSecret,optional"`

	SignInClientId     string             `pulumi:"signInClientId,optional"`
	SignInClientSecret pulumi.StringInput `pulumi:"signInClientSecret,optional" provider:"secret"`
	SignInDomain       string             `pulumi:"signInDomain,optional"`

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
	// DatabaseAvailabilityType is the Cloud SQL instance's availability type: ZONAL, one zone and
	// the default, or REGIONAL, a standby in a second zone that Cloud SQL fails over to. Switching
	// changes the instance in place, restarting it once.
	DatabaseAvailabilityType *string `pulumi:"databaseAvailabilityType,optional"`
	// DatabaseWaitsFor is a value of the resource whose update the Cloud SQL instance's must
	// follow, so that a step of the consumer's own — deleting the old tailnet device before the
	// front door comes back, for instance — runs before the instance restarts. The component never
	// reads the value, and it reaches no resource: only the resources the value came from are
	// used, which is what pulumi.DependsOnInputs reads off it. Unset, the instance's ordering is
	// what it is today.
	DatabaseWaitsFor pulumi.StringInput `pulumi:"databaseWaitsFor,optional"`
	Namespace        *string            `pulumi:"namespace,optional"`
	// ExistingNamespace: use the namespace as it exists in the cluster rather than creating it
	// (another program owns the namespace).
	ExistingNamespace *bool `pulumi:"existingNamespace,optional"`

	// ClusterOnly builds the cluster alone: the VPC, its subnetwork, the router, Cloud NAT and the
	// Autopilot cluster, and nothing else. No Cloud SQL instance, database or user, no generated
	// password, no Secret Manager grant, no Kubernetes provider and no Kubernetes object is
	// registered, and the component requires no random provider. Every input that configures
	// Dispatch rather than the cluster is refused by name, so only region, hostname and
	// deletionProtection come with it; clusterName, kubeconfig and serviceAccountIssuer carry the
	// cluster's own values and url, databaseInstance, principal, agentToken and
	// serviceAccountSubject are "". A stack already running Dispatch cannot turn it on: such a
	// stack sets the inputs this mode refuses, so the update is refused before anything is deleted.
	ClusterOnly *bool `pulumi:"clusterOnly,optional"`

	// Replicas is the app Deployment's replica count, 1 by default. Above 1 the Deployment rolls
	// its pods instead of being recreated.
	Replicas *int `pulumi:"replicas,optional"`
	// FrontDoorReplicas is the front door's (<hostname>-tailnet) replica count, 2 by default, and
	// ListenerReplicas the listener's (<hostname>-listener), 1 by default; each at least 1. Each
	// Deployment is recreated at one replica and rolls above one, by its own count.
	FrontDoorReplicas *int `pulumi:"frontDoorReplicas,optional"`
	ListenerReplicas  *int `pulumi:"listenerReplicas,optional"`

	// NatsInCluster: the app and the listener dial natsUrl directly, at an address their own
	// cluster resolves, rather than through the front door's SOCKS5 proxy. Neither NATS client
	// carries ENVOY_NATS_SOCKS5_PROXY then, and the component renders no SOCKS5 proxy on the front
	// door's pods, no port-1055 Service and no ingress rule admitting the two workloads to it.
	// Unset, the SOCKS5 path renders as it does today, for a deployment whose NATS hub is outside
	// this cluster.
	NatsInCluster *bool `pulumi:"natsInCluster,optional"`
	// NatsReplicas is the number of copies JetStream keeps of every stream and bucket, from 1 to
	// bus.MaxReplicas: ENVOY_NATS_REPLICAS on the app's and the listener's containers, which every
	// Envoy program of a deployment carries alike. It is not replicas, the app Deployment's pod
	// count above: more than one copy needs a clustered NATS, which no pod count gives. Unset
	// renders nothing and both binaries keep one copy (bus.ParseNATSReplicas).
	NatsReplicas *int `pulumi:"natsReplicas,optional"`
	// StreamMaxBytes is the stream's byte limit, at least 1: ENVOY_STREAM_MAX_BYTES on the same two
	// containers. A deployed limit is never lowered, so programs carrying different limits leave
	// the largest. Unset renders nothing and both binaries set no limit, so a deployed stream keeps
	// the one it has (bus.ParseStreamMaxBytes).
	StreamMaxBytes *int `pulumi:"streamMaxBytes,optional"`

	// SigningKeyGenerations is the ordered key set the server holds, the shape
	// auth.ParseSigningKeys reads DISPATCH_SIGNING_KEY into: the first entry signs every new
	// cookie and seals every new refresh token, and every entry verifies a cookie and opens a
	// seal. Each entry is a generation, which names one generated key: generation 0 is
	// <hostname>-signing-key, the key a running deployment already holds, and generation n above
	// 0 is <hostname>-signing-key-<n>. Unset is [0], the single key this component has always
	// registered. Only the generations the list names are registered, so one it drops is
	// destroyed and that key cannot come back. A rotation is three applies, nobody signed out:
	// [0] to [0, 1] adds the new key, which every process then verifies; [0, 1] to [1, 0] makes
	// it the key every new cookie and seal is made with; and [1, 0] to [1] drops the old key once
	// nothing made under it is left.
	SigningKeyGenerations []int `pulumi:"signingKeyGenerations,optional"`

	// AppCpu and appMemory size the app Deployment's envoy-dispatch container, as Kubernetes
	// quantities set as both its request and its limit: 1 CPU and 4Gi by default. The pod's
	// cloud-sql-proxy sidecar is sized by proxyCpu and proxyMemory, and the front door and the
	// listener are unchanged.
	AppCpu    *string `pulumi:"appCpu,optional"`
	AppMemory *string `pulumi:"appMemory,optional"`
	// ProxyCpu and proxyMemory size the app pod's cloud-sql-proxy sidecar, through whose TLS every
	// database call of the pod passes, as Kubernetes quantities set as both its request and its
	// limit: 1 CPU and 2Gi by default, sized for one server pod's connection pools. Autopilot's
	// bounds apply to the pod's total, envoy-dispatch's and the proxy's together.
	ProxyCpu    *string `pulumi:"proxyCpu,optional"`
	ProxyMemory *string `pulumi:"proxyMemory,optional"`
	// AppEphemeralStorage and proxyEphemeralStorage are the ephemeral-storage request and limit,
	// as Kubernetes quantities, of envoy-dispatch and of the cloud-sql-proxy sidecar: the disk a
	// container's writable layer, temporary files and logs may use before the kubelet evicts the
	// pod. Each unset renders nothing, and Autopilot gives that container its default 1Gi.
	// Autopilot's bounds apply to the pod's total, the two containers' together.
	AppEphemeralStorage   *string `pulumi:"appEphemeralStorage,optional"`
	ProxyEphemeralStorage *string `pulumi:"proxyEphemeralStorage,optional"`

	// PprofPort turns on envoy-dispatch's Go profiler: DISPATCH_PPROF_PORT on its container alone,
	// so it serves net/http/pprof on 127.0.0.1 at this port, reachable through kubectl
	// port-forward and from nowhere outside the pod. Unset, nothing is rendered and no profiler
	// listens.
	PprofPort *int `pulumi:"pprofPort,optional"`

	// PriorityClassName is the priorityClassName of every pod of the three Deployments (the app,
	// the front door and the listener), the name of a PriorityClass that already exists in the
	// cluster, which this component reads once but never creates: a missing class fails the
	// preview or update before any Deployment changes, since Kubernetes refuses every pod naming
	// one. Unset, the pods name none and run at the cluster's default priority.
	PriorityClassName *string `pulumi:"priorityClassName,optional"`

	// AppPdbMinAvailable turns on a PodDisruptionBudget, <hostname>, over the app's pods: its
	// minAvailable, from 1 to replicas. FrontDoorPdbMinAvailable turns on <hostname>-tailnet over
	// the front door's pods, from 1 to frontDoorReplicas, and ListenerPdbMinAvailable
	// <hostname>-listener over the listener's, from 1 to listenerReplicas. Unset, the component
	// creates no budget for that Deployment, and its pods carry the cluster autoscaler's
	// safe-to-evict "false" annotation instead.
	AppPdbMinAvailable       *int `pulumi:"appPdbMinAvailable,optional"`
	FrontDoorPdbMinAvailable *int `pulumi:"frontDoorPdbMinAvailable,optional"`
	ListenerPdbMinAvailable  *int `pulumi:"listenerPdbMinAvailable,optional"`

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
		{"frontDoorToolsImage", args.FrontDoorToolsImage},
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
	if args.DatabaseAvailabilityType != nil && bus.HoldsSeed(*args.DatabaseAvailabilityType) {
		return "databaseAvailabilityType", true
	}
	if args.AppCpu != nil && bus.HoldsSeed(*args.AppCpu) {
		return "appCpu", true
	}
	if args.AppMemory != nil && bus.HoldsSeed(*args.AppMemory) {
		return "appMemory", true
	}
	if args.ProxyCpu != nil && bus.HoldsSeed(*args.ProxyCpu) {
		return "proxyCpu", true
	}
	if args.ProxyMemory != nil && bus.HoldsSeed(*args.ProxyMemory) {
		return "proxyMemory", true
	}
	if args.AppEphemeralStorage != nil && bus.HoldsSeed(*args.AppEphemeralStorage) {
		return "appEphemeralStorage", true
	}
	if args.ProxyEphemeralStorage != nil && bus.HoldsSeed(*args.ProxyEphemeralStorage) {
		return "proxyEphemeralStorage", true
	}
	if args.PriorityClassName != nil && bus.HoldsSeed(*args.PriorityClassName) {
		return "priorityClassName", true
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
	// ServiceAccountIssuer and ServiceAccountSubject are the issuer and subject of the app's and
	// the listener's service-account tokens: the cluster's OIDC issuer, and those pods' Kubernetes
	// service account. A federated credential trusting the pair (an Azure identity's, for the
	// keyless form) admits exactly those pods; the front door's pods run as an account of their own.
	ServiceAccountIssuer  pulumi.StringOutput `pulumi:"serviceAccountIssuer"`
	ServiceAccountSubject pulumi.StringOutput `pulumi:"serviceAccountSubject"`
}

// dispatchSettings are the plain inputs, validated, with the defaults applied, so they are known at
// preview. images.tools is frontDoorToolsImage. With clusterOnly set only hostname, region and
// deletionProtection are resolved: no other field is read by the cluster the mode builds.
// natsReplicas and streamMaxBytes are 0 when their inputs are unset, where the component renders
// neither setting, and signingKeyGenerations is the generations the component registers a signing
// key for, in the order the server holds them, [0] when its input is unset. frontDoor and
// signInOn are the two groups of inputs a consumer sets together or not at all: frontDoor is set
// when any tailnet input is, where the component builds the front door and Dispatch is reached at
// its tailnet name, and signInOn when any sign-in input is, where the app runs browser sign-in.
// With neither the component is the sandbox: Dispatch is reached in the cluster alone, by readers
// the app's NetworkPolicy admits, and tailnetDNSName, tags, the tailscale and tools images and
// signIn are empty, since nothing reads them.
type dispatchSettings struct {
	clusterOnly                                 bool
	frontDoor, signInOn                         bool
	hostname, tailnetDNSName, region, namespace string
	tags                                        []string
	images                                      struct{ envoy, tailscale, proxy, tools string }
	signIn                                      struct{ clientID, domain string }
	databaseVersion, databaseTier               string
	databaseAvailabilityType                    string
	agentSecretsURL, agentSecretsAudience       string
	appClientID, appID                          string
	appKeySecretName, appKeySecretKey           string
	deletionProtection, existingNamespace       bool
	replicas                                    int
	frontDoorReplicas, listenerReplicas         int
	natsInCluster                               bool
	natsReplicas, streamMaxBytes                int
	signingKeyGenerations                       []int
	appResources                                dispatchAppResources
	pprofPort                                   int
	disruption                                  dispatchDisruption
	azure                                       dispatchAzure
}

// socks5Proxy is the address of the front door's SOCKS5 proxy, the one the app and the listener
// dial the NATS hub through, and "" with natsInCluster, where no proxy runs and the two dial
// natsUrl themselves. One derivation, so the mode and the address can never disagree.
func (s dispatchSettings) socks5Proxy() string {
	if s.natsInCluster {
		return ""
	}
	return clusterLocal(s.hostname+"-tailnet", s.namespace) + ":" + strconv.Itoa(dispatchSOCKS5Port)
}

// clusterURL is the app's own Service inside the cluster, the address a caller already in the
// cluster reaches Dispatch at: http because that Service terminates no TLS, on the 8766 it
// publishes. It is what a sandbox tells a reader it is at (serverURL) and what the front door's
// serve configuration forwards a tailnet request to, so both come from here and can never
// disagree.
func (s dispatchSettings) clusterURL() string {
	return "http://" + clusterLocal(s.hostname, s.namespace) + ":8766"
}

// serverURL is the address Dispatch tells a reader it is at, the url output and the app's
// DISPATCH_SERVER_URL: the tailnet name the front door hosts, or, in the sandbox, the app's own
// Service inside the cluster, which is all there is to reach. It carries no trailing slash, it is
// http in the sandbox because that Service terminates no TLS, and its port is the 8766 that
// Service publishes. One derivation, so the output and the variable can never disagree.
func (s dispatchSettings) serverURL() string {
	if s.frontDoor {
		return "https://" + core.TailnetHostName(s.hostname, s.tailnetDNSName)
	}
	return s.clusterURL()
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

// dispatchStrategy is one Deployment's update strategy. rollingUpdate is set only when
// strategyType is dispatchStrategyRollingUpdate; a Recreate Deployment carries no such block.
type dispatchStrategy struct {
	strategyType  string
	rollingUpdate *dispatchRollingUpdate
}

// dispatchStrategyFor turns a workload's replica count into its Deployment's update strategy; each
// of the three Deployments takes it from its own count. At minDispatchReplicas the Deployment is
// recreated, stopping the old pod before starting the new one: a roll would run the outgoing and
// the incoming builds together, which is safe only once the outgoing build follows the rules for
// two builds running side by side and keeps its assets in the shared store, and a one-pod release
// cannot know that of the build it replaces. Above that count the operator runs such builds and
// wants no gap in service, so the Deployment rolls: one new pod comes up before any old one goes
// down.
func dispatchStrategyFor(replicas int) dispatchStrategy {
	if replicas <= minDispatchReplicas {
		return dispatchStrategy{strategyType: dispatchStrategyRecreate}
	}
	return dispatchStrategy{
		strategyType:  dispatchStrategyRollingUpdate,
		rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
	}
}

// dispatchInput is one input of a refusal table: the input's schema name, and whether the
// consumer set it. A plain string is set when it is not empty, a slice when it has an element,
// and a pointer or a pulumi.*Input when it is not nil, as newDispatchAzure judges its pointers
// and Inputs, so a value unknown at preview is set as it will be at update.
type dispatchInput struct {
	input string
	set   bool
}

// dispatchTailnetInputs is the tailnet group: every input that exists only because Dispatch has a
// front door on a tailnet, from the name it is served at to the images its pods run. A consumer
// sets all five, for a Dispatch readers reach over the tailnet, or none, for the sandbox, where no
// front door is built and the app is reached inside its cluster alone. Each row's set-predicate is
// written here and nowhere else, dispatchClusterOnlyRefusals taking the group from here, so the
// resolver's mode, the refusals of a half-set group and clusterOnly's can never read an input two
// ways. The rows are in the order a refusal names them in (dispatchGroupClause).
func dispatchTailnetInputs(args *DispatchArgs) []dispatchInput {
	return []dispatchInput{
		{"tailnetDnsName", args.TailnetDnsName != ""},
		{"tags", len(args.Tags) > 0},
		{"tailnetAuthKey", args.TailnetAuthKey != nil},
		{"tailscaleImage", args.TailscaleImage != ""},
		{"frontDoorToolsImage", args.FrontDoorToolsImage != ""},
	}
}

// dispatchSignInInputs is the sign-in group: the Google client Dispatch hands a browser to, and
// the domain whose accounts it admits. All three or none, as the tailnet group; with none the app
// runs no browser sign-in at all, which is what the sandbox is for. Written here alone, as the
// tailnet group is.
func dispatchSignInInputs(args *DispatchArgs) []dispatchInput {
	return []dispatchInput{
		{"signInClientId", args.SignInClientId != ""},
		{"signInClientSecret", args.SignInClientSecret != nil},
		{"signInDomain", args.SignInDomain != ""},
	}
}

// dispatchClusterOnlyRefusals is every pulumi-tagged input of DispatchArgs that configures
// Dispatch rather than the cluster, which clusterOnly refuses by name. Only the four inputs both
// modes use are absent: clusterOnly itself, region and hostname, which name every cloud resource,
// and deletionProtection, which guards the cluster as well as the database. The eight inputs of
// the two groups come from the groups' own definitions, so an input either group gains is refused
// here without being written down a second time.
func dispatchClusterOnlyRefusals(args *DispatchArgs) []dispatchInput {
	return slices.Concat(dispatchTailnetInputs(args), dispatchSignInInputs(args), []dispatchInput{
		{"envoyImage", args.EnvoyImage != ""},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage != ""},
		{"natsUrl", args.NatsUrl != nil},
		{"natsSeedSecret", args.NatsSeedSecret != nil},
		{"agentSecretsUrl", args.AgentSecretsUrl != ""},
		{"agentSecretsAudience", args.AgentSecretsAudience != ""},
		{"appClientId", args.AppClientId != ""},
		{"appId", args.AppId != ""},
		{"appPrivateKeySecretName", args.AppPrivateKeySecretName != ""},
		{"appPrivateKeySecretKey", args.AppPrivateKeySecretKey != ""},
		{"databaseVersion", args.DatabaseVersion != nil},
		{"databaseTier", args.DatabaseTier != nil},
		{"databaseAvailabilityType", args.DatabaseAvailabilityType != nil},
		{"databaseWaitsFor", args.DatabaseWaitsFor != nil},
		{"namespace", args.Namespace != nil},
		{"existingNamespace", args.ExistingNamespace != nil},
		{"replicas", args.Replicas != nil},
		{"frontDoorReplicas", args.FrontDoorReplicas != nil},
		{"listenerReplicas", args.ListenerReplicas != nil},
		{"natsInCluster", args.NatsInCluster != nil},
		{"natsReplicas", args.NatsReplicas != nil},
		{"streamMaxBytes", args.StreamMaxBytes != nil},
		{"signingKeyGenerations", len(args.SigningKeyGenerations) > 0},
		{"appCpu", args.AppCpu != nil},
		{"appMemory", args.AppMemory != nil},
		{"proxyCpu", args.ProxyCpu != nil},
		{"proxyMemory", args.ProxyMemory != nil},
		{"appEphemeralStorage", args.AppEphemeralStorage != nil},
		{"proxyEphemeralStorage", args.ProxyEphemeralStorage != nil},
		{"pprofPort", args.PprofPort != nil},
		{"priorityClassName", args.PriorityClassName != nil},
		{"appPdbMinAvailable", args.AppPdbMinAvailable != nil},
		{"frontDoorPdbMinAvailable", args.FrontDoorPdbMinAvailable != nil},
		{"listenerPdbMinAvailable", args.ListenerPdbMinAvailable != nil},
		{"allowedNamespaces", len(args.AllowedNamespaces) > 0},
		{"azureEmbedEndpoint", args.AzureEmbedEndpoint != nil},
		{"azureEmbedDeployment", args.AzureEmbedDeployment != nil},
		{"azureEmbedTokensPerMinute", args.AzureEmbedTokensPerMinute != nil},
		{"azureEmbedKeySecret", args.AzureEmbedKeySecret != nil},
		{"azureTenantId", args.AzureTenantId != nil},
		{"azureClientId", args.AzureClientId != nil},
		{"azureFederationAudience", args.AzureFederationAudience != nil},
	})
}

// dispatchGroupSet reports whether the consumer set any input of group, which is what turns the
// feature the group configures on: one input is enough, so a consumer who meant to set the group
// and missed an input is refused by that group's own checks rather than silently given the mode
// it did not ask for.
func dispatchGroupSet(group []dispatchInput) bool {
	return slices.ContainsFunc(group, func(in dispatchInput) bool { return in.set })
}

// dispatchGroupClause is the clause every refusal of one of feature's own inputs trails while the
// group is set in part: the rule the consumer broke, then unset, every input of the group left
// unset, comma-joined and in the group's order. One run then names the whole shortfall whatever
// the refusal was about, the value an input carries or an input missing: the checks stop at the
// first input that is wrong, and a consumer told about that one alone would correct it and be
// refused again for the next. A whole group is missing nothing to name, so its clause is "".
func dispatchGroupClause(feature string, group []dispatchInput) string {
	unset := make([]string, 0, len(group))
	for _, in := range group {
		if !in.set {
			unset = append(unset, in.input)
		}
	}
	if len(unset) == 0 {
		return ""
	}
	return fmt.Sprintf(" (the %s inputs are set together or not at all; unset: %s)", feature, strings.Join(unset, ", "))
}

// refuseDispatchTailnetGroup judges the tailnet group alone: the tailnet name Dispatch is served
// at, the tags its front-door nodes carry, the two images those pods run, held to the digest rule
// the app's own images are held to, and the key the nodes join the tailnet with. It runs only
// while that group is set, so the sandbox, which sets none of it, reaches none of these checks,
// and every refusal trails clause, which names every tailnet input still unset
// (dispatchGroupClause): a group set in part is refused for the first input that is wrong and
// told the whole shortfall at once.
func refuseDispatchTailnetGroup(args *DispatchArgs, clause string) error {
	if !tailnetDNSNameRule.MatchString(args.TailnetDnsName) {
		return fmt.Errorf("tailnetDnsName %q must match %s%s", args.TailnetDnsName, tailnetDNSNameRule, clause)
	}
	if len(args.Tags) == 0 {
		return fmt.Errorf("tags: at least one tag is required%s", clause)
	}
	for i, tag := range args.Tags {
		if !tagRule.MatchString(tag) {
			return fmt.Errorf("tags[%d] %q must match %s%s", i, tag, tagRule, clause)
		}
	}
	for _, image := range []struct{ field, ref string }{
		{"tailscaleImage", args.TailscaleImage},
		{"frontDoorToolsImage", args.FrontDoorToolsImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return fmt.Errorf("%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>%s", image.field, image.ref, clause)
		}
	}
	if args.TailnetAuthKey == nil {
		return fmt.Errorf("tailnetAuthKey is required%s", clause)
	}
	return nil
}

// refuseDispatchSandbox judges what the sandbox cannot make good, and requires the two inputs the
// mode needs. It runs only while the tailnet group is wholly unset, which is what asks for the
// mode: no front door is built, so a sign-in input would ask an OAuth provider to redirect to an
// in-cluster http:// address, frontDoorReplicas and frontDoorPdbMinAvailable size and protect a
// Deployment that does not exist, the pods reach a NATS hub only inside their own cluster, and
// the app's own policy is its only ingress. Each is refused by its own name rather than accepted
// and quietly dropped. The sign-in inputs are refused here, before the sign-in group's own
// checks, so one sent to a sandbox is refused for being sent at all and not for the rest of its
// group being missing.
func refuseDispatchSandbox(args *DispatchArgs, signInGroup []dispatchInput) error {
	for _, in := range signInGroup {
		if in.set {
			return fmt.Errorf("%s configures browser sign-in, which needs the tailnet inputs: without them Dispatch is reached at an in-cluster http:// address, which no OAuth provider takes as a redirect origin, so no browser could sign in; unset the sign-in inputs, or set the tailnet inputs", in.input)
		}
	}
	if args.NatsInCluster == nil || !*args.NatsInCluster {
		return errors.New("natsInCluster is required without the tailnet inputs: the pods reach a tailnet NATS hub only through the front door's SOCKS5 proxy, and no front door is built, so the hub must run in the cluster")
	}
	if len(args.AllowedNamespaces) == 0 {
		return errors.New("allowedNamespaces must name at least one namespace without the tailnet inputs: with no front door its rules are the app's only ingress, and a NetworkPolicy with no rule admits nobody, so nothing could reach Dispatch at all")
	}
	if args.FrontDoorReplicas != nil {
		return errors.New("frontDoorReplicas sizes a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs")
	}
	if args.FrontDoorPdbMinAvailable != nil {
		return errors.New("frontDoorPdbMinAvailable covers a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs")
	}
	return nil
}

// refuseDispatchSignInGroup judges the sign-in group alone: the Google client Dispatch hands a
// browser to, the domain whose accounts it admits and that client's secret. It runs only while
// that group is set, so a deployment that runs no browser sign-in reaches none of these checks,
// and every refusal trails clause, which names every sign-in input still unset, as the tailnet
// group's refusals do.
func refuseDispatchSignInGroup(args *DispatchArgs, clause string) error {
	if args.SignInClientId == "" {
		return fmt.Errorf("signInClientId is required%s", clause)
	}
	if !signInDomainRule.MatchString(args.SignInDomain) {
		return fmt.Errorf("signInDomain %q must be a lowercase DNS name%s", args.SignInDomain, clause)
	}
	if args.SignInClientSecret == nil {
		return fmt.Errorf("signInClientSecret is required%s", clause)
	}
	return nil
}

// refuseDispatchHostname refuses a hostname that is not hostnameRule's shape. Both resolvers run
// it, so a cluster-only component holds its hostname, which names every cloud resource, to the
// rule the full resolver holds it to, with the same refusal.
func refuseDispatchHostname(hostname string) error {
	if !hostnameRule.MatchString(hostname) {
		return fmt.Errorf("hostname %q must match %s", hostname, hostnameRule)
	}
	return nil
}

// refuseDispatchRegion refuses an unset region, which both modes need: it is the location of the
// cluster and of every resource around it.
func refuseDispatchRegion(region string) error {
	if region == "" {
		return errors.New("region is required")
	}
	return nil
}

// dispatchDeletionProtection resolves deletionProtection, true when the consumer sets nothing. It
// guards the cluster as well as the database, so both resolvers default it the same way.
func dispatchDeletionProtection(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// newDispatchClusterOnlySettings resolves the inputs clusterOnly leaves live, hostname, region and
// deletionProtection, each as the full resolver resolves it, and refuses every input that
// configures Dispatch, which this mode does not build. It validates nothing else and applies no
// other default: no other value reaches a resource here.
func newDispatchClusterOnlySettings(args *DispatchArgs) (dispatchSettings, error) {
	if err := refuseDispatchHostname(args.Hostname); err != nil {
		return dispatchSettings{}, err
	}
	if err := refuseDispatchRegion(args.Region); err != nil {
		return dispatchSettings{}, err
	}
	s := dispatchSettings{clusterOnly: true, hostname: args.Hostname, region: args.Region, deletionProtection: dispatchDeletionProtection(args.DeletionProtection)}
	for _, in := range dispatchClusterOnlyRefusals(args) {
		if in.set {
			return dispatchSettings{}, fmt.Errorf("%s configures Dispatch, which clusterOnly does not build: unset it, or unset clusterOnly", in.input)
		}
	}
	return s, nil
}

// newDispatchSettings validates the component's plain inputs and renders the values known before
// any cloud resource is registered. Every refusal names the input that needs correction.
func newDispatchSettings(args *DispatchArgs) (dispatchSettings, error) {
	if input, ok := dispatchSeededInput(args); ok {
		return dispatchSettings{}, core.SeedRefusal(input)
	}
	if args.ClusterOnly != nil && *args.ClusterOnly {
		return newDispatchClusterOnlySettings(args)
	}
	if err := refuseDispatchHostname(args.Hostname); err != nil {
		return dispatchSettings{}, err
	}
	// Each group is judged by its own function while it is set, and what the sandbox cannot make
	// good by a third while the tailnet group is unset.
	tailnetGroup, signInGroup := dispatchTailnetInputs(args), dispatchSignInInputs(args)
	frontDoor, signInOn := dispatchGroupSet(tailnetGroup), dispatchGroupSet(signInGroup)
	if frontDoor {
		if err := refuseDispatchTailnetGroup(args, dispatchGroupClause("tailnet", tailnetGroup)); err != nil {
			return dispatchSettings{}, err
		}
	} else if err := refuseDispatchSandbox(args, signInGroup); err != nil {
		return dispatchSettings{}, err
	}
	if signInOn {
		if err := refuseDispatchSignInGroup(args, dispatchGroupClause("sign-in", signInGroup)); err != nil {
			return dispatchSettings{}, err
		}
	}
	// The app's two images, which the app's pod runs in either mode: they belong to no group, so
	// every consumer sets both and neither refusal trails a clause. The front door's two are the
	// tailnet group's, held to this same digest rule by that group's own function.
	for _, image := range []struct{ field, ref string }{
		{"envoyImage", args.EnvoyImage},
		{"cloudSqlProxyImage", args.CloudSqlProxyImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return dispatchSettings{}, fmt.Errorf("%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", image.field, image.ref)
		}
	}
	if err := refuseDispatchRegion(args.Region); err != nil {
		return dispatchSettings{}, err
	}
	if args.NatsUrl == nil {
		return dispatchSettings{}, errors.New("natsUrl is required")
	}
	if args.NatsSeedSecret == nil {
		return dispatchSettings{}, errors.New("natsSeedSecret is required")
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
		frontDoor:       frontDoor,
		signInOn:        signInOn,
		hostname:        args.Hostname,
		tailnetDNSName:  args.TailnetDnsName,
		region:          args.Region,
		tags:            args.Tags,
		databaseVersion: defaultDispatchDatabaseVersion,
		databaseTier:    defaultDispatchDatabaseTier,
		namespace:       defaultDispatchNamespace,
	}
	s.images.envoy = args.EnvoyImage
	s.images.tailscale = args.TailscaleImage
	s.images.proxy = args.CloudSqlProxyImage
	s.images.tools = args.FrontDoorToolsImage
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
	s.databaseAvailabilityType = dispatchAvailabilityZonal
	if args.DatabaseAvailabilityType != nil {
		switch availability := *args.DatabaseAvailabilityType; availability {
		case dispatchAvailabilityZonal, dispatchAvailabilityRegional:
			s.databaseAvailabilityType = availability
		default:
			return dispatchSettings{}, fmt.Errorf("databaseAvailabilityType %q must be %s or %s; leave it unset for %s",
				availability, dispatchAvailabilityZonal, dispatchAvailabilityRegional, dispatchAvailabilityZonal)
		}
	}
	s.deletionProtection = dispatchDeletionProtection(args.DeletionProtection)
	if s.replicas, err = dispatchReplicas("replicas", args.Replicas, defaultDispatchReplicas); err != nil {
		return dispatchSettings{}, err
	}
	if s.frontDoorReplicas, err = dispatchReplicas("frontDoorReplicas", args.FrontDoorReplicas, defaultDispatchFrontDoorReplicas); err != nil {
		return dispatchSettings{}, err
	}
	if s.listenerReplicas, err = dispatchReplicas("listenerReplicas", args.ListenerReplicas, defaultDispatchListenerReplicas); err != nil {
		return dispatchSettings{}, err
	}
	s.natsInCluster = args.NatsInCluster != nil && *args.NatsInCluster
	// The two deployment settings both NATS clients carry, each unset (0) rendering nothing: the
	// binaries then keep one copy and set no byte limit (bus.ParseNATSReplicas,
	// bus.ParseStreamMaxBytes).
	if args.NatsReplicas != nil {
		replicas := *args.NatsReplicas
		switch {
		case replicas < 1:
			return dispatchSettings{}, core.InputErrorf("natsReplicas", "natsReplicas %d is below 1; leave it unset for one copy", replicas)
		case replicas > bus.MaxReplicas:
			return dispatchSettings{}, core.InputErrorf("natsReplicas", "natsReplicas %d is above %d, the most copies NATS keeps", replicas, bus.MaxReplicas)
		}
		s.natsReplicas = replicas
	}
	if args.StreamMaxBytes != nil {
		if limit := *args.StreamMaxBytes; limit < 1 {
			return dispatchSettings{}, core.InputErrorf("streamMaxBytes", "streamMaxBytes %d is below 1; leave it unset for no limit", limit)
		}
		s.streamMaxBytes = *args.StreamMaxBytes
	}
	if s.signingKeyGenerations, err = newDispatchSigningKeyGenerations(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.appResources, err = newDispatchAppResources(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.pprofPort, err = newDispatchPprofPort(args); err != nil {
		return dispatchSettings{}, err
	}
	if s.disruption, err = newDispatchDisruption(args, s.replicas, s.frontDoorReplicas, s.listenerReplicas); err != nil {
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

// podPriorityClass resolves input, the name of a PriorityClass every pod of a component names, ""
// when unset: gcp:Dispatch's and gcp:ClusteredNatsHub's priorityClassName. It refuses a name that
// is empty or not a DNS subdomain, and one with the system- prefix, which Kubernetes reserves for
// its own system-cluster-critical and system-node-critical classes: those outrank every workload
// so the cluster's own components are scheduled first, and an application pod holding one could
// preempt them. Each refusal is a core.InputError naming input.
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

// newDispatchSigningKeyGenerations resolves signingKeyGenerations, the generations the component
// registers a signing key for and the order the server holds them in: [0] when the input is unset
// or empty, which is the single key this component has always registered, since generation 0
// keeps the name <hostname>-signing-key and is never replaced. A generation above 0 is a key of
// its own, <hostname>-signing-key-<n>, and only the generations the list names are registered, so
// one the list drops is destroyed and that key cannot come back.
//
// Two refusals, both before any resource is registered. A negative generation names no resource.
// A repeated generation would render the same key twice, which parseSettingEntries refuses at the
// server's boot, so it is refused here rather than shipped as a Secret no pod can start on.
//
// The resolver sees one state and never the one before it, so the rest is what an operator keeps
// and the component cannot refuse:
//   - [0] to [1] with no add step. Every process refuses to start beside a live one whose key set
//     does not share its current key both ways (processes.keySetsShared), and at one replica,
//     whose Deployment strategy is Recreate, the running pod is gone before the new one starts,
//     so that apply signs everyone out rather than being refused.
//   - [0, 1] to [1], the same skip from the other end: the old key is dropped in the very apply
//     that promotes the new one.
//   - dropping the old generation sooner than 30 days after the promote, a session cookie's life
//     (auth's sessionMaxAgeSeconds): every cookie and every refresh token still under that key
//     dies with it.
//   - a rollback to a generation the list already dropped, whose key the random provider
//     destroyed and which no later apply brings back.
func newDispatchSigningKeyGenerations(args *DispatchArgs) ([]int, error) {
	if len(args.SigningKeyGenerations) == 0 {
		return []int{0}, nil
	}
	for i, generation := range args.SigningKeyGenerations {
		switch {
		case generation < 0:
			return nil, core.InputErrorf("signingKeyGenerations", "signingKeyGenerations[%d] is generation %d, which names no resource: a generation is 0, the key a running deployment already holds, or a number above it", i, generation)
		case slices.Contains(args.SigningKeyGenerations[:i], generation):
			return nil, core.InputErrorf("signingKeyGenerations", "signingKeyGenerations[%d] repeats generation %d, which would render the same key twice; the server refuses a repeated DISPATCH_SIGNING_KEY entry at boot", i, generation)
		}
	}
	return args.SigningKeyGenerations, nil
}

// dispatchReplicas resolves one Deployment's replica count, input, to fallback when it is unset,
// refusing a count below minDispatchReplicas, which leaves the workload with no pod.
func dispatchReplicas(input string, value *int, fallback int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < minDispatchReplicas {
		return 0, fmt.Errorf("%s %d must be at least %d", input, *value, minDispatchReplicas)
	}
	return *value, nil
}

// dispatchDisruption is the pods' resolved disruption protection: the one priority class every
// pod of the three Deployments names, and each Deployment's PodDisruptionBudget minAvailable. ""
// and 0, rendering nothing, are unset.
type dispatchDisruption struct {
	priorityClass                                string
	appPDBMinAvailable, frontDoorPDBMinAvailable int
	listenerPDBMinAvailable                      int
}

// newDispatchDisruption resolves priorityClassName, by podPriorityClass's rule, and the three
// budgets, appPdbMinAvailable, frontDoorPdbMinAvailable and listenerPdbMinAvailable, each refusal
// naming its own input. It refuses a budget below 1, which protects no pod, and above its
// Deployment's replica count, which no number of ready pods can meet.
func newDispatchDisruption(args *DispatchArgs, replicas, frontDoorReplicas, listenerReplicas int) (dispatchDisruption, error) {
	var d dispatchDisruption
	var err error
	if d.priorityClass, err = podPriorityClass("priorityClassName", args.PriorityClassName); err != nil {
		return dispatchDisruption{}, err
	}
	for _, budget := range []struct {
		input, replicasInput string
		value                *int
		replicas             int
		into                 *int
	}{
		{"appPdbMinAvailable", "replicas", args.AppPdbMinAvailable, replicas, &d.appPDBMinAvailable},
		{"frontDoorPdbMinAvailable", "frontDoorReplicas", args.FrontDoorPdbMinAvailable, frontDoorReplicas, &d.frontDoorPDBMinAvailable},
		{"listenerPdbMinAvailable", "listenerReplicas", args.ListenerPdbMinAvailable, listenerReplicas, &d.listenerPDBMinAvailable},
	} {
		if budget.value == nil {
			continue
		}
		if *budget.value < 1 || *budget.value > budget.replicas {
			return dispatchDisruption{}, fmt.Errorf("%s %d must be from 1 to %s (%d); leave it unset for no PodDisruptionBudget",
				budget.input, *budget.value, budget.replicasInput, budget.replicas)
		}
		*budget.into = *budget.value
	}
	return d, nil
}

// dispatchAppResources is the app pod's resolved container sizes: envoy-dispatch's request and
// limit, appCPU and appMemory, and the cloud-sql-proxy sidecar's, proxyCPU and proxyMemory; and
// each container's ephemeral storage, "" (rendering nothing) when its input is unset.
type dispatchAppResources struct {
	appCPU, appMemory, proxyCPU, proxyMemory   string
	appEphemeralStorage, proxyEphemeralStorage string
}

// newDispatchAppResources resolves appCpu and appMemory, the envoy-dispatch container's request
// and limit, and proxyCpu and proxyMemory, the cloud-sql-proxy sidecar's, and refuses a size at
// which Autopilot would not run the app pod as rendered: the two containers' total below
// autopilotPodMinCPU or autopilotPodMinMemory or outside the memory-per-CPU ratio (Autopilot
// raises it), or above autopilotPodMaxCPU or autopilotPodMaxMemory (Autopilot rejects it). Each
// refusal names the inputs whose sum it judges.
func newDispatchAppResources(args *DispatchArgs) (dispatchAppResources, error) {
	cpu, appCPU, err := appQuantity("appCpu", args.AppCpu, defaultDispatchAppCPU, "1, 1500m or 2")
	if err != nil {
		return dispatchAppResources{}, err
	}
	memory, appMemory, err := appQuantity("appMemory", args.AppMemory, defaultDispatchAppMemory, "4Gi or 6144Mi")
	if err != nil {
		return dispatchAppResources{}, err
	}
	proxyCPU, proxyCPUValue, err := appQuantity("proxyCpu", args.ProxyCpu, defaultDispatchProxyCPU, "1, 1500m or 2")
	if err != nil {
		return dispatchAppResources{}, err
	}
	proxyMemory, proxyMemoryValue, err := appQuantity("proxyMemory", args.ProxyMemory, defaultDispatchProxyMemory, "2Gi or 1536Mi")
	if err != nil {
		return dispatchAppResources{}, err
	}
	podCPU := new(big.Rat).Add(appCPU, proxyCPUValue)
	podMemory := new(big.Rat).Add(appMemory, proxyMemoryValue)
	if podCPU.Cmp(autopilotPodMinCPU) < 0 {
		return dispatchAppResources{}, fmt.Errorf("appCpu %q and proxyCpu %q must make at least %s together: below it Autopilot raises the app pod's request, since the pod carries pod anti-affinity (and safe-to-evict when no budget covers it)",
			cpu, proxyCPU, milliCPU(autopilotPodMinCPU))
	}
	if podCPU.Cmp(autopilotPodMaxCPU) > 0 {
		return dispatchAppResources{}, fmt.Errorf("appCpu %q and proxyCpu %q must make at most %s CPU together: Autopilot rejects an app pod that requests more",
			cpu, proxyCPU, autopilotPodMaxCPU.RatString())
	}
	if podMemory.Cmp(autopilotPodMinMemory) < 0 {
		return dispatchAppResources{}, fmt.Errorf("appMemory %q and proxyMemory %q must make at least %s together: below it Autopilot raises the app pod's request, since the pod carries pod anti-affinity (and safe-to-evict when no budget covers it)",
			memory, proxyMemory, mebibytes(autopilotPodMinMemory))
	}
	if podMemory.Cmp(autopilotPodMaxMemory) > 0 {
		return dispatchAppResources{}, fmt.Errorf("appMemory %q and proxyMemory %q must make at most 110Gi together: Autopilot rejects an app pod that requests more",
			memory, proxyMemory)
	}
	gibPerCPU := new(big.Rat).Quo(podMemory, podCPU)
	gibPerCPU.Quo(gibPerCPU, new(big.Rat).SetInt64(1<<30))
	if gibPerCPU.Cmp(autopilotMinGiBPerCPU) < 0 || gibPerCPU.Cmp(autopilotMaxGiBPerCPU) > 0 {
		return dispatchAppResources{}, fmt.Errorf("appCpu %q, appMemory %q, proxyCpu %q and proxyMemory %q: the app pod must hold between 1 and 6.5 GiB of memory per CPU, or Autopilot raises the smaller of the two",
			cpu, memory, proxyCPU, proxyMemory)
	}
	appEphemeral, proxyEphemeral, err := newDispatchEphemeralStorage(args)
	if err != nil {
		return dispatchAppResources{}, err
	}
	return dispatchAppResources{appCPU: cpu, appMemory: memory, proxyCPU: proxyCPU, proxyMemory: proxyMemory,
		appEphemeralStorage: appEphemeral, proxyEphemeralStorage: proxyEphemeral}, nil
}

// newDispatchEphemeralStorage resolves appEphemeralStorage and proxyEphemeralStorage, each "" when
// unset, and refuses a pair at which Autopilot would not run the app pod as rendered: their sum,
// an unset container counted at the autopilotDefaultEphemeralStorage Autopilot gives it, below
// autopilotPodMinEphemeralStorage (Autopilot raises it) or above autopilotPodMaxEphemeralStorage
// (Autopilot rejects it). The refusal names both inputs, an unset one as Autopilot's default.
func newDispatchEphemeralStorage(args *DispatchArgs) (app, proxy string, err error) {
	resolve := func(input string, value *string) (rendered, named string, quantity *big.Rat, err error) {
		if value == nil {
			quantity, _ = quantityValue(autopilotDefaultEphemeralStorage)
			return "", fmt.Sprintf("%s unset (Autopilot's %s)", input, autopilotDefaultEphemeralStorage), quantity, nil
		}
		rendered, quantity, err = appQuantity(input, value, "Autopilot's default of "+autopilotDefaultEphemeralStorage, "4Gi or 4096Mi")
		return rendered, fmt.Sprintf("%s %q", input, rendered), quantity, err
	}
	app, appNamed, appValue, err := resolve("appEphemeralStorage", args.AppEphemeralStorage)
	if err != nil {
		return "", "", err
	}
	proxy, proxyNamed, proxyValue, err := resolve("proxyEphemeralStorage", args.ProxyEphemeralStorage)
	if err != nil {
		return "", "", err
	}
	pod := new(big.Rat).Add(appValue, proxyValue)
	if pod.Cmp(autopilotPodMinEphemeralStorage) < 0 {
		return "", "", fmt.Errorf("%s and %s must make at least %s together: below it Autopilot raises the app pod's ephemeral storage, and each container's logs count against its own",
			appNamed, proxyNamed, mebibytes(autopilotPodMinEphemeralStorage))
	}
	if pod.Cmp(autopilotPodMaxEphemeralStorage) > 0 {
		return "", "", fmt.Errorf("%s and %s must make at most 10Gi together: Autopilot rejects an app pod with more ephemeral storage; a larger working set belongs in a generic ephemeral volume",
			appNamed, proxyNamed)
	}
	return app, proxy, nil
}

// appQuantity resolves one container size input of the app pod: fallback when it is unset, and
// its exact value, or a refusal naming input. It refuses an empty value, which is no quantity,
// and one of 0 or less, which sizes no container: the pod's floor is judged on the two
// containers' sum, so a negative size beside a large one would otherwise pass it.
func appQuantity(input string, value *string, fallback, examples string) (string, *big.Rat, error) {
	resolved := fallback
	if value != nil {
		if *value == "" {
			return "", nil, errors.New(input + " is empty; leave it unset for " + fallback)
		}
		resolved = *value
	}
	quantity, err := quantityValue(resolved)
	switch {
	case errors.Is(err, errQuantityExponent):
		return "", nil, fmt.Errorf("%s %q has a decimal exponent outside e%d to e%d, the range of the suffixes n to E; write it with a suffix, such as %s", input, resolved, minQuantityExponent, maxQuantityExponent, examples)
	case err != nil:
		return "", nil, fmt.Errorf("%s %q is not a Kubernetes quantity, such as %s", input, resolved, examples)
	case quantity.Sign() <= 0:
		return "", nil, fmt.Errorf("%s %q must be more than 0, such as %s", input, resolved, examples)
	}
	return resolved, quantity, nil
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
	// clusterOnly registers no RandomPassword, so it requires no random provider: a cluster-only
	// program would otherwise have to declare one it never uses.
	if !s.clusterOnly {
		if err := providers.Require(ctx.Context(), dispatchType, "random", opts...); err != nil {
			return nil, err
		}
	}
	// Only the front door declares Dispatch's Tailscale Service, so a consumer that builds none —
	// the sandbox, and clusterOnly, which leave the mode off — declares no tailscale provider.
	if s.frontDoor {
		if err := providers.Require(ctx.Context(), dispatchType, "tailscale", opts...); err != nil {
			return nil, err
		}
	}

	comp := &Dispatch{}
	if err := ctx.RegisterComponentResource(dispatchType, name, comp, opts...); err != nil {
		return nil, err
	}
	cloud, err := newDispatchCluster(ctx, comp, name, s, opts...)
	if err != nil {
		return nil, err
	}
	// The three outputs the cluster alone decides, which both modes carry.
	comp.ClusterName = cloud.cluster.Name
	comp.Kubeconfig = cloud.kubeconfig
	comp.ServiceAccountIssuer = cloud.issuer
	if s.clusterOnly {
		// The five outputs Dispatch would decide are an explicit empty string: an output field
		// left unset awaits as unknown, which is one value in preview and another in update,
		// while "" is the same in both and is what a consumer already reads as "not applied".
		empty := pulumi.String("").ToStringOutput()
		comp.Url, comp.DatabaseInstance, comp.Principal = empty, empty, empty
		comp.AgentToken, comp.ServiceAccountSubject = empty, empty
		return comp, nil
	}
	database, err := newDispatchDatabase(ctx, comp, name, args, s, cloud)
	if err != nil {
		return nil, err
	}
	if err := newDispatchKubernetes(ctx, comp, name, args, s, cloud, database); err != nil {
		return nil, err
	}
	comp.DatabaseInstance = database.instance.ConnectionName
	comp.Principal = database.principal
	comp.AgentToken = database.agentToken.Result
	comp.Url = pulumi.String(s.serverURL()).ToStringOutput()
	// The app's and the listener's pods run as ServiceAccount <hostname> in the component's
	// namespace, the account newDispatchKubernetes registers, and Kubernetes names a token's subject
	// after it. The front door's pods run as <hostname>-tailnet, which this subject never names.
	comp.ServiceAccountSubject = pulumi.String("system:serviceaccount:" + s.namespace + ":" + s.hostname).ToStringOutput()
	return comp, nil
}
