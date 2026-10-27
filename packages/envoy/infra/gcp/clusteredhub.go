package gcp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// clusteredHubType is the component's token. pulumi-go-provider derives the same one from the
// ClusteredNatsHub type's package and name, and serves it under that token; NewClusteredNatsHub
// registers with the constant so it also runs outside the plugin, where no call carries a token.
const clusteredHubType = "envoy-infra:gcp:ClusteredNatsHub"

const (
	// maxClusteredHubNameBytes keeps <name>-tailnet within the 52 bytes a StatefulSet's or a
	// Deployment's name may have, so its controller-revision-hash label stays a valid label value.
	maxClusteredHubNameBytes = 44

	// clusteredHubServers is the number of NATS servers the hub runs, and so the most copies
	// JetStream keeps of a stream there: it refuses a stream with more replicas than servers.
	clusteredHubServers = 3

	defaultClusteredHubDiskSizeGb      = 10
	defaultClusteredHubNatsCPU         = "1"
	defaultClusteredHubNatsMemory      = "2Gi"
	defaultClusteredHubTailnetReplicas = 2
	// minClusteredHubTailnetReplicas is the fewest tailnet hosts the hub runs: with one, every
	// drain of its node takes the hub off the tailnet.
	minClusteredHubTailnetReplicas    = 2
	defaultClusteredHubListenerCPU    = "500m"
	defaultClusteredHubListenerMemory = "1Gi"

	// The fixed sizes, request and limit, of each hub pod's two sidecars, the chart's config
	// reloader and its Prometheus exporter, which Autopilot counts in the pod's request beside
	// nats-server. Autopilot's floors and memory-per-CPU ratio apply to the pod's total
	// (autopilotPodMinCPU and the rest), so the sidecars need only be large enough to run: the
	// reloader watches one directory and signals one process, and the exporter holds one scrape of
	// the server's JetStream report (-jsz=all), which grows with the streams and consumers.
	clusteredHubReloaderCPU    = "50m"
	clusteredHubReloaderMemory = "64Mi"
	clusteredHubExporterCPU    = "50m"
	clusteredHubExporterMemory = "128Mi"
)

// dns1035LabelRule is a DNS-1035 label's shape, the one a Kubernetes Service's name takes: a
// letter first, a letter or digit last, at most 63 characters (checked beside it).
var dns1035LabelRule = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// defaultClusteredHubMetricsNamespaces are the namespaces Google Managed Prometheus's collectors
// run in: gmp-system on a Standard cluster, gke-gmp-system on Autopilot.
func defaultClusteredHubMetricsNamespaces() []string {
	return []string{"gmp-system", "gke-gmp-system"}
}

// clusteredHubSelector is the hub pods' selector labels, as the nats chart (2.15.1,
// templates/_helpers.tpl, nats.selectorLabels) renders them for release name with no
// nameOverride: the chart's name, the release and the component. The component installs the chart
// under release name, so these labels select exactly its NATS pods.
func clusteredHubSelector(release string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "nats",
		"app.kubernetes.io/instance":  release,
		"app.kubernetes.io/component": "nats",
	}
}

// ClusteredNatsHubArgs are the inputs of an envoy-infra:gcp:ClusteredNatsHub. Every field of a
// plain Go type is a value the consumer's program knows when it runs; kubeconfig and
// tailnetAuthKey may come from another resource or stack.
type ClusteredNatsHubArgs struct {
	// The kubeconfig of the cluster the hub runs on, from which the component builds its own
	// Kubernetes provider.
	Kubeconfig pulumi.StringInput `pulumi:"kubeconfig" provider:"secret"`
	// The existing namespace the hub runs in; the component never creates it.
	Namespace string `pulumi:"namespace"`
	// The chart's release name and fullnameOverride, and so the StatefulSet's name: a DNS-1123
	// label of at most 44 bytes.
	Name string `pulumi:"name"`
	// The in-cluster client Service's name, a DNS-1035 label.
	ServiceName   string `pulumi:"serviceName"`
	NatsImage     string `pulumi:"natsImage"`     // pinned: <name>@sha256:<digest>
	ReloaderImage string `pulumi:"reloaderImage"` // pinned: <name>@sha256:<digest>
	ExporterImage string `pulumi:"exporterImage"` // pinned: <name>@sha256:<digest>
	// The copies JetStream keeps of every stream and bucket the session listener creates,
	// ENVOY_NATS_REPLICAS, 1 to 3: refused without tailnet, whose listener alone takes it. Unset,
	// the variable is unset.
	NatsReplicas *int `pulumi:"natsReplicas,optional"`
	// The byte limit of the session listener's notification stream, ENVOY_STREAM_MAX_BYTES, at
	// least 1. Unset, the variable is unset.
	StreamMaxBytes *int `pulumi:"streamMaxBytes,optional"`
	// JetStream's max_file_store in whole GiB, below diskSizeGb: ⌊0.9 × diskSizeGb⌋ (at least 1)
	// by default.
	MaxFileStoreGb *int `pulumi:"maxFileStoreGb,optional"`
	// Each pod's volume size in whole GiB, above maxFileStoreGb: 10 by default.
	DiskSizeGb *int `pulumi:"diskSizeGb,optional"`
	// The volumes' StorageClass; the cluster's default class when unset.
	StorageClassName *string `pulumi:"storageClassName,optional"`
	// nats-server's container size, a Kubernetes quantity each, set as both its request and its
	// limit: 1 CPU and 2Gi by default.
	NatsCpu    *string `pulumi:"natsCpu,optional"`
	NatsMemory *string `pulumi:"natsMemory,optional"`
	// The name of an existing PriorityClass every pod of the component names; none when unset.
	PriorityClassName *string `pulumi:"priorityClassName,optional"`
	// The namespaces admitted to NATS's client port: at least one.
	ClientNamespaces []string `pulumi:"clientNamespaces"`
	// The namespaces admitted to NATS's monitoring port; none when unset.
	MonitorNamespaces []string `pulumi:"monitorNamespaces,optional"`
	// The namespaces admitted to the exporter's metrics port: gmp-system and gke-gmp-system,
	// Google Managed Prometheus's, when unset.
	MetricsNamespaces []string `pulumi:"metricsNamespaces,optional"`
	// The hub's NATS users, as gcp:NatsHub's; see authorization for what may be empty.
	Users []NATSUserArgs `pulumi:"users,optional"`
	// The user a client presenting no credential becomes, granted core.SessionPermissions.
	SessionUser *SessionUserArgs `pulumi:"sessionUser,optional"`
	// The hub's tailnet entry: a Tailscale Service serving NATS and the session listener.
	Tailnet *ClusteredHubTailnetArgs `pulumi:"tailnet,optional"`
	// The tailnet auth key the tailnet hosts join with: required with tailnet, refused without.
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey,optional" provider:"secret"`
}

// ClusteredHubTailnetArgs are the hub's tailnet entry: the Tailscale Service svc:<serviceName>,
// served by replicas hosts, each joined with the tags and running the session listener.
type ClusteredHubTailnetArgs struct {
	ServiceName    string                    `pulumi:"serviceName"`    // the Service's name without svc:
	TailnetDnsName string                    `pulumi:"tailnetDnsName"` // <tailnet>.ts.net
	Tags           []string                  `pulumi:"tags"`           // "tag:<name>", at least one
	Replicas       *int                      `pulumi:"replicas,optional"`
	TailscaleImage string                    `pulumi:"tailscaleImage"` // pinned: <name>@sha256:<digest>
	Listener       *ClusteredHubListenerArgs `pulumi:"listener"`
}

// Annotate names the type gcp:ClusteredHubTailnet in the schema, for the reason NATSUserArgs's
// gives.
func (t *ClusteredHubTailnetArgs) Annotate(a infer.Annotator) {
	a.SetToken("gcp", "ClusteredHubTailnet")
}

// ClusteredHubListenerArgs are the session listener each tailnet host runs: its image, its
// machine id, the public key of its NATS user's seed, and the existing Kubernetes Secret in the
// hub's namespace, and the key in it, holding that seed, which the component mounts by reference
// and never reads. The hub renders the listener's user itself, named after the machine id and
// granted core.ListenerPermissions for it, as gcp:NatsHub renders its own listener's.
type ClusteredHubListenerArgs struct {
	Image          string  `pulumi:"image"`              // pinned: <name>@sha256:<digest>
	MachineID      *string `pulumi:"machineId,optional"` // default tailnet.serviceName
	NKey           string  `pulumi:"nkey"`               // the seed's public key; never the seed
	SeedSecretName string  `pulumi:"seedSecretName"`
	SeedSecretKey  string  `pulumi:"seedSecretKey"`
	// Any further address a client dials the listener by, beyond <serviceName>.<tailnetDnsName>,
	// which it always serves.
	AllowedHosts []string `pulumi:"allowedHosts,optional"`
	// The listener container's size, a Kubernetes quantity each, set as both its request and its
	// limit: 500m and 1Gi by default.
	Cpu    *string `pulumi:"cpu,optional"`
	Memory *string `pulumi:"memory,optional"`
}

// Annotate names the type gcp:ClusteredHubListener in the schema, for the reason NATSUserArgs's
// gives.
func (l *ClusteredHubListenerArgs) Annotate(a infer.Annotator) {
	a.SetToken("gcp", "ClusteredHubListener")
}

// ClusteredNatsHub is NATS with JetStream as a three-server cluster on a Kubernetes cluster, with
// an optional tailnet entry.
type ClusteredNatsHub struct {
	pulumi.ResourceState
	ServiceUrl      pulumi.StringOutput    `pulumi:"serviceUrl"`      // nats://<serviceName>.<namespace>.svc.cluster.local:4222
	PodSelector     pulumi.StringMapOutput `pulumi:"podSelector"`     // the hub pods' selector labels
	StatefulSetName pulumi.StringOutput    `pulumi:"statefulSetName"` // name
	// With tailnet: nats://<tailnet.serviceName>.<tailnetDnsName>:4222 and http://…:9020; null
	// without. They take no optional tag: pulumi-go-provider v1.7.0 (introspect.StructToMap) and
	// the SDK's construct result key a component's outputs by the whole pulumi tag, so
	// "tailnetNatsUrl,optional" would be the output's name.
	TailnetNatsUrl  pulumi.StringPtrOutput `pulumi:"tailnetNatsUrl"`
	TailnetEnvoyUrl pulumi.StringPtrOutput `pulumi:"tailnetEnvoyUrl"`
}

// clusteredHubSettings are the hub's plain inputs, validated, with the defaults applied, and what
// they alone render, so all of it is known at preview: the users' auth.conf and, with tailnet, the
// session listener's environment.
type clusteredHubSettings struct {
	name, namespace, serviceName string
	serviceURL                   string
	images                       struct{ nats, reloader, exporter string }
	natsReplicas, streamMaxBytes int
	size                         clusteredHubSize
	storageClassName             string
	priorityClassName            string
	clientNamespaces             []string
	monitorNamespaces            []string
	metricsNamespaces            []string
	authConf                     string
	tailnet                      *clusteredHubTailnet
}

// clusteredHubSize is each hub pod's volume, JetStream's file store within it, and nats-server's
// container size.
type clusteredHubSize struct {
	diskSizeGb, maxFileStoreGb int
	natsCPU, natsMemory        string
}

// clusteredHubTailnet is the resolved tailnet entry, nil without tailnet.
type clusteredHubTailnet struct {
	serviceName, tailnetDNSName string
	tags                        []string
	replicas                    int
	tailscaleImage              string
	listener                    clusteredHubListener
	natsURL, envoyURL           string
}

// clusteredHubListener is the resolved session listener of each tailnet host: env is its
// environment but the seed (core.ListenerConfig.Env).
type clusteredHubListener struct {
	image, machineID              string
	seedSecretName, seedSecretKey string
	cpu, memory                   string
	env                           []string
}

// clusteredHubSeededInput names the first plain string input of args that holds an nkey seed
// (bus.HoldsSeed), by its path and never its value. Every one of them reaches a Kubernetes object,
// the chart's values or a refusal that quotes it, and each user's machine id becomes that user's
// rules before core sees them, as seededInput says of gcp:NatsHub's. A user's name, nkey and rule
// entries, the session user's, and the listener's nkey are core's to refuse, as they are for
// gcp:NatsHub; kubeconfig and tailnetAuthKey are Inputs, checked once they resolve.
func clusteredHubSeededInput(args *ClusteredNatsHubArgs) (string, bool) {
	type field struct{ path, value string }
	fields := []field{
		{"namespace", args.Namespace}, {"name", args.Name}, {"serviceName", args.ServiceName},
		{"natsImage", args.NatsImage}, {"reloaderImage", args.ReloaderImage}, {"exporterImage", args.ExporterImage},
	}
	for _, f := range []struct {
		path  string
		value *string
	}{
		{"storageClassName", args.StorageClassName}, {"natsCpu", args.NatsCpu}, {"natsMemory", args.NatsMemory},
		{"priorityClassName", args.PriorityClassName},
	} {
		if f.value != nil {
			fields = append(fields, field{f.path, *f.value})
		}
	}
	for _, list := range []struct {
		input  string
		values []string
	}{
		{"clientNamespaces", args.ClientNamespaces}, {"monitorNamespaces", args.MonitorNamespaces},
		{"metricsNamespaces", args.MetricsNamespaces},
	} {
		for i, value := range list.values {
			fields = append(fields, field{fmt.Sprintf("%s[%d]", list.input, i), value})
		}
	}
	for i, u := range args.Users {
		for _, kind := range userKinds(u) {
			if kind.id != nil {
				fields = append(fields, field{fmt.Sprintf("users[%d].%s", i, kind.input), *kind.id})
			}
		}
	}
	if t := args.Tailnet; t != nil {
		fields = append(fields, field{"tailnet.serviceName", t.ServiceName}, field{"tailnet.tailnetDnsName", t.TailnetDnsName},
			field{"tailnet.tailscaleImage", t.TailscaleImage})
		for i, tag := range t.Tags {
			fields = append(fields, field{fmt.Sprintf("tailnet.tags[%d]", i), tag})
		}
		if l := t.Listener; l != nil {
			fields = append(fields, field{"tailnet.listener.image", l.Image},
				field{"tailnet.listener.seedSecretName", l.SeedSecretName}, field{"tailnet.listener.seedSecretKey", l.SeedSecretKey})
			for _, f := range []struct {
				path  string
				value *string
			}{
				{"tailnet.listener.machineId", l.MachineID}, {"tailnet.listener.cpu", l.Cpu}, {"tailnet.listener.memory", l.Memory},
			} {
				if f.value != nil {
					fields = append(fields, field{f.path, *f.value})
				}
			}
			for i, entry := range l.AllowedHosts {
				fields = append(fields, field{fmt.Sprintf("tailnet.listener.allowedHosts[%d]", i), entry})
			}
		}
	}
	for _, f := range fields {
		if bus.HoldsSeed(f.value) {
			return f.path, true
		}
	}
	return "", false
}

// newClusteredHubSettings validates args and renders what the plain inputs alone decide. Every
// refusal is a core.InputError naming the input, a seed in any plain string input first.
func newClusteredHubSettings(args *ClusteredNatsHubArgs) (clusteredHubSettings, error) {
	if input, ok := clusteredHubSeededInput(args); ok {
		return clusteredHubSettings{}, core.InputErrorf(input, "%w", core.SeedRefusal(input))
	}
	if args.Kubeconfig == nil {
		return clusteredHubSettings{}, core.InputErrorf("kubeconfig", "kubeconfig is required: the hub builds its own Kubernetes provider from it")
	}
	if len(args.Name) > maxClusteredHubNameBytes || !namespaceRule.MatchString(args.Name) {
		return clusteredHubSettings{}, core.InputErrorf("name", "name %q must be a DNS-1123 label of at most %d bytes, so <name>-tailnet stays within the 52 bytes a StatefulSet's or Deployment's name may have",
			args.Name, maxClusteredHubNameBytes)
	}
	if !dnsLabel(args.Namespace) {
		return clusteredHubSettings{}, core.InputErrorf("namespace", "namespace %q must be a DNS label", args.Namespace)
	}
	if len(args.ServiceName) > 63 || !dns1035LabelRule.MatchString(args.ServiceName) {
		return clusteredHubSettings{}, core.InputErrorf("serviceName", "serviceName %q must be a DNS-1035 label, a Service's name: a letter first, at most 63 characters", args.ServiceName)
	}
	for _, image := range []struct{ input, ref string }{
		{"natsImage", args.NatsImage}, {"reloaderImage", args.ReloaderImage}, {"exporterImage", args.ExporterImage},
	} {
		if !imageRule.MatchString(image.ref) {
			return clusteredHubSettings{}, core.InputErrorf(image.input, "%s %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", image.input, image.ref)
		}
	}
	s := clusteredHubSettings{
		name:        args.Name,
		namespace:   args.Namespace,
		serviceName: args.ServiceName,
		serviceURL:  fmt.Sprintf("nats://%s:%d", clusterLocal(args.ServiceName, args.Namespace), natsPort),
	}
	s.images.nats, s.images.reloader, s.images.exporter = args.NatsImage, args.ReloaderImage, args.ExporterImage
	if args.NatsReplicas != nil {
		if *args.NatsReplicas < 1 || *args.NatsReplicas > clusteredHubServers {
			return clusteredHubSettings{}, core.InputErrorf("natsReplicas", "natsReplicas %d must be from 1 to %d: the hub runs %d servers, and JetStream keeps no more copies of a stream than it has servers; leave it unset for the listener's default",
				*args.NatsReplicas, clusteredHubServers, clusteredHubServers)
		}
		s.natsReplicas = *args.NatsReplicas
	}
	if args.StreamMaxBytes != nil {
		if *args.StreamMaxBytes < 1 {
			return clusteredHubSettings{}, core.InputErrorf("streamMaxBytes", "streamMaxBytes %d must be at least 1; leave it unset for the listener's default", *args.StreamMaxBytes)
		}
		s.streamMaxBytes = *args.StreamMaxBytes
	}
	var err error
	if s.size, err = hubSizing(args); err != nil {
		return clusteredHubSettings{}, err
	}
	if args.StorageClassName != nil {
		if !dnsSubdomain(*args.StorageClassName) {
			return clusteredHubSettings{}, core.InputErrorf("storageClassName", "storageClassName %q must be a DNS subdomain, the shape of a StorageClass's name; leave it unset for the cluster's default class", *args.StorageClassName)
		}
		s.storageClassName = *args.StorageClassName
	}
	if s.priorityClassName, err = podPriorityClass("priorityClassName", args.PriorityClassName); err != nil {
		return clusteredHubSettings{}, err
	}
	if len(args.ClientNamespaces) == 0 {
		return clusteredHubSettings{}, core.InputErrorf("clientNamespaces", "clientNamespaces: at least one namespace is required, or no client reaches NATS")
	}
	s.metricsNamespaces = defaultClusteredHubMetricsNamespaces()
	if len(args.MetricsNamespaces) > 0 {
		s.metricsNamespaces = args.MetricsNamespaces
	}
	s.clientNamespaces, s.monitorNamespaces = args.ClientNamespaces, args.MonitorNamespaces
	for _, list := range []struct {
		input      string
		namespaces []string
	}{
		{"clientNamespaces", s.clientNamespaces}, {"monitorNamespaces", s.monitorNamespaces}, {"metricsNamespaces", s.metricsNamespaces},
	} {
		if err := checkNamespaces(list.input, list.namespaces); err != nil {
			return clusteredHubSettings{}, err
		}
	}
	var listener *listenerUser
	if s.tailnet, listener, err = newClusteredHubTailnet(args, s); err != nil {
		return clusteredHubSettings{}, err
	}
	auth, err := authorization(args.Users, listener, args.SessionUser)
	if err != nil {
		return clusteredHubSettings{}, authorizationRefusal(err)
	}
	if s.authConf, err = auth.Render([]string{confHeader}); err != nil {
		return clusteredHubSettings{}, authorizationRefusal(err)
	}
	return s, nil
}

// hubSizing resolves each hub pod's volume (diskSizeGb), JetStream's file store within it
// (maxFileStoreGb) and nats-server's container size (natsCpu, natsMemory). The file store stays
// below the volume, which the filesystem's own overhead takes part of; with diskSizeGb alone, it
// is ⌊0.9 × diskSizeGb⌋ and at least 1. A store at or above the volume is refused naming both
// inputs: maxFileStoreGb when it was given, diskSizeGb when the store is its default. The
// container's size is refused where Autopilot would not run the pod as rendered (clusteredHubPod).
func hubSizing(args *ClusteredNatsHubArgs) (clusteredHubSize, error) {
	size := clusteredHubSize{diskSizeGb: defaultClusteredHubDiskSizeGb}
	if args.DiskSizeGb != nil {
		size.diskSizeGb = *args.DiskSizeGb
	}
	if args.MaxFileStoreGb != nil {
		size.maxFileStoreGb = *args.MaxFileStoreGb
		if size.maxFileStoreGb < 1 {
			return clusteredHubSize{}, core.InputErrorf("maxFileStoreGb", "maxFileStoreGb %d must be at least 1", size.maxFileStoreGb)
		}
		if size.maxFileStoreGb >= size.diskSizeGb {
			return clusteredHubSize{}, core.InputErrorf("maxFileStoreGb", "maxFileStoreGb %d must be below diskSizeGb %d: the filesystem takes part of the volume, so a store as large as the volume fills it before JetStream's limit holds",
				size.maxFileStoreGb, size.diskSizeGb)
		}
	} else {
		size.maxFileStoreGb = max(1, ninetyPercent(size.diskSizeGb))
		if size.diskSizeGb <= size.maxFileStoreGb {
			return clusteredHubSize{}, core.InputErrorf("diskSizeGb", "diskSizeGb %d must be above maxFileStoreGb %d, its default for this volume: the filesystem takes part of the volume, so a store as large as the volume fills it before JetStream's limit holds",
				size.diskSizeGb, size.maxFileStoreGb)
		}
	}
	var err error
	if size.natsCPU, size.natsMemory, err = clusteredHubPod(args); err != nil {
		return clusteredHubSize{}, err
	}
	return size, nil
}

// ninetyPercent is ⌊0.9 × n⌋ for n ≥ 0, n − ⌈n / 10⌉, computed without overflow. A negative n gives
// a negative result, which the caller raises to 1.
func ninetyPercent(n int) int {
	tenth := n / 10
	if n%10 > 0 {
		tenth++
	}
	return n - tenth
}

// clusteredHubPod resolves natsCpu and natsMemory, nats-server's request and limit, and refuses a
// size at which Autopilot would not run the hub pod as rendered, with the reloader's and the
// exporter's fixed sizes beside it (autopilotPodSize).
func clusteredHubPod(args *ClusteredNatsHubArgs) (cpu, memory string, err error) {
	return autopilotPodSize(autopilotPod{
		name:     "the hub pod",
		cpuInput: "natsCpu", memoryInput: "natsMemory",
		cpu: args.NatsCpu, memory: args.NatsMemory,
		defaultCPU: defaultClusteredHubNatsCPU, defaultMemory: defaultClusteredHubNatsMemory,
		memoryExamples: "2Gi or 4096Mi",
		sidecars: []autopilotSidecar{
			{name: "the reloader", cpu: clusteredHubReloaderCPU, memory: clusteredHubReloaderMemory},
			{name: "the exporter", cpu: clusteredHubExporterCPU, memory: clusteredHubExporterMemory},
		},
	})
}

// checkNamespaces refuses an entry of input, a list of namespaces, that is not a DNS label or
// repeats an earlier one, naming input and the entry's index.
func checkNamespaces(input string, namespaces []string) error {
	seen := make(map[string]int, len(namespaces))
	for i, namespace := range namespaces {
		if !dnsLabel(namespace) {
			return core.InputErrorf(input, "%s[%d] %q must be a DNS label, a namespace's name", input, i, namespace)
		}
		if first, ok := seen[namespace]; ok {
			return core.InputErrorf(input, "%s[%d] %q repeats %s[%d]", input, i, namespace, input, first)
		}
		seen[namespace] = i
	}
	return nil
}

// newClusteredHubTailnet resolves the tailnet entry and the listener user the hub renders for it:
// both nil without tailnet, where tailnetAuthKey, which only the tailnet hosts use, and
// natsReplicas, which only their session listener takes, are refused. The listener runs as its
// machine id, serves on the pod's loopback on the listener's default port, where the tailnet host
// forwards the Service's port 9020, and reaches NATS through the hub's client Service. It answers
// on the Service's tailnet name and on every further address allowedHosts names; the operator's
// entries are checked as their own list, so a refusal names tailnet.listener.allowedHosts and the
// index the operator wrote.
func newClusteredHubTailnet(args *ClusteredNatsHubArgs, s clusteredHubSettings) (*clusteredHubTailnet, *listenerUser, error) {
	t := args.Tailnet
	if t == nil {
		if args.TailnetAuthKey != nil {
			return nil, nil, core.InputErrorf("tailnetAuthKey", "tailnetAuthKey is given without tailnet, the only part of the hub that joins the tailnet")
		}
		if args.NatsReplicas != nil {
			return nil, nil, core.InputErrorf("natsReplicas", "natsReplicas is given without tailnet, whose session listener alone takes it (ENVOY_NATS_REPLICAS)")
		}
		return nil, nil, nil
	}
	if args.TailnetAuthKey == nil {
		return nil, nil, core.InputErrorf("tailnetAuthKey", "tailnetAuthKey is required with tailnet: the tailnet hosts join the tailnet with it")
	}
	if !hostnameRule.MatchString(t.ServiceName) {
		return nil, nil, core.InputErrorf("tailnet.serviceName", "tailnet.serviceName %q must match %s", t.ServiceName, hostnameRule)
	}
	if !tailnetDNSNameRule.MatchString(t.TailnetDnsName) {
		return nil, nil, core.InputErrorf("tailnet.tailnetDnsName", "tailnet.tailnetDnsName %q must match %s", t.TailnetDnsName, tailnetDNSNameRule)
	}
	if len(t.Tags) == 0 {
		return nil, nil, core.InputErrorf("tailnet.tags", "tailnet.tags: at least one tag is required; the tailnet hosts advertise their tags as their tailnet identity")
	}
	for i, tag := range t.Tags {
		if !tagRule.MatchString(tag) {
			return nil, nil, core.InputErrorf("tailnet.tags", "tailnet.tags[%d] %q must match %s", i, tag, tagRule)
		}
	}
	tailnet := &clusteredHubTailnet{
		serviceName:    t.ServiceName,
		tailnetDNSName: t.TailnetDnsName,
		tags:           t.Tags,
		replicas:       defaultClusteredHubTailnetReplicas,
		tailscaleImage: t.TailscaleImage,
	}
	if t.Replicas != nil {
		if *t.Replicas < minClusteredHubTailnetReplicas {
			return nil, nil, core.InputErrorf("tailnet.replicas", "tailnet.replicas %d must be at least %d, so a drained host leaves another serving the tailnet", *t.Replicas, minClusteredHubTailnetReplicas)
		}
		tailnet.replicas = *t.Replicas
	}
	if !imageRule.MatchString(t.TailscaleImage) {
		return nil, nil, core.InputErrorf("tailnet.tailscaleImage", "tailnet.tailscaleImage %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", t.TailscaleImage)
	}
	l := t.Listener
	if l == nil {
		return nil, nil, core.InputErrorf("tailnet.listener", "tailnet.listener is required with tailnet: each tailnet host serves the session listener on port %d", core.DefaultListenerPort)
	}
	if !imageRule.MatchString(l.Image) {
		return nil, nil, core.InputErrorf("tailnet.listener.image", "tailnet.listener.image %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", l.Image)
	}
	machineID := t.ServiceName
	if l.MachineID != nil {
		machineID = *l.MachineID
	}
	if err := core.CheckMachineID("tailnet.listener.machineId", machineID); err != nil {
		return nil, nil, err
	}
	if l.NKey == "" {
		return nil, nil, core.InputErrorf("tailnet.listener.nkey", "tailnet.listener.nkey is required")
	}
	if !dnsSubdomain(l.SeedSecretName) {
		return nil, nil, core.InputErrorf("tailnet.listener.seedSecretName", "tailnet.listener.seedSecretName %q must be a DNS subdomain, the name of a Secret in namespace %s", l.SeedSecretName, s.namespace)
	}
	if !secretKeyRule.MatchString(l.SeedSecretKey) {
		return nil, nil, core.InputErrorf("tailnet.listener.seedSecretKey", "tailnet.listener.seedSecretKey %q must match %s", l.SeedSecretKey, secretKeyRule)
	}
	if err := core.CheckAllowedHosts("tailnet.listener.allowedHosts", l.AllowedHosts); err != nil {
		return nil, nil, err
	}
	listener := clusteredHubListener{
		image:          l.Image,
		machineID:      machineID,
		seedSecretName: l.SeedSecretName,
		seedSecretKey:  l.SeedSecretKey,
		cpu:            defaultClusteredHubListenerCPU,
		memory:         defaultClusteredHubListenerMemory,
	}
	for _, q := range []struct {
		input, examples string
		value           *string
		into            *string
	}{
		{"tailnet.listener.cpu", "500m or 1", l.Cpu, &listener.cpu},
		{"tailnet.listener.memory", "1Gi or 512Mi", l.Memory, &listener.memory},
	} {
		if q.value == nil {
			continue
		}
		if *q.value == "" {
			return nil, nil, core.InputErrorf(q.input, "%s is empty; leave it unset for %s", q.input, *q.into)
		}
		if _, err := appQuantity(q.input, *q.value, q.examples); err != nil {
			return nil, nil, &core.InputError{Input: q.input, Err: err}
		}
		*q.into = *q.value
	}
	host := core.TailnetHostName(t.ServiceName, t.TailnetDnsName)
	env, err := core.ListenerConfig{
		MachineID:      machineID,
		Port:           core.DefaultListenerPort,
		ListenHost:     "127.0.0.1",
		AllowedHosts:   append([]string{host}, l.AllowedHosts...),
		NATSReplicas:   s.natsReplicas,
		StreamMaxBytes: s.streamMaxBytes,
	}.Env([]string{s.serviceURL})
	if err != nil {
		return nil, nil, err
	}
	listener.env = env
	tailnet.listener = listener
	tailnet.natsURL = fmt.Sprintf("nats://%s:%d", host, natsPort)
	tailnet.envoyURL = fmt.Sprintf("http://%s:%d", host, core.DefaultListenerPort)
	return tailnet, &listenerUser{machineID: machineID, nkey: l.NKey, input: "tailnet.listener.nkey"}, nil
}

// authorizationRefusal is a refusal of the users' rendering as a core.InputError: one that already
// is passes unchanged; any other names the input its message starts with, as authorization and core
// name a user (users[<i>], the listener, the session user or its noAuthUser).
func authorizationRefusal(err error) error {
	var input *core.InputError
	if errors.As(err, &input) {
		return err
	}
	switch message := err.Error(); {
	case strings.HasPrefix(message, "sessionUser"), strings.HasPrefix(message, "noAuthUser"):
		return &core.InputError{Input: "sessionUser", Err: err}
	case strings.HasPrefix(message, "listener"):
		return &core.InputError{Input: "tailnet.listener", Err: err}
	}
	return &core.InputError{Input: "users", Err: err}
}

// NewClusteredNatsHub registers a clustered hub. It refuses a seed in its name, then every invalid
// plain input, before it registers anything, and sets the outputs, which the plain inputs alone
// decide, after the component registers.
func NewClusteredNatsHub(ctx *pulumi.Context, name string, args *ClusteredNatsHubArgs, opts ...pulumi.ResourceOption) (*ClusteredNatsHub, error) {
	// The name is quoted in every refusal below and the component registers under it, so a seed
	// there is refused first, as NewNatsHub refuses one in its own name.
	if bus.HoldsSeed(name) {
		return nil, fmt.Errorf("%s: %w", clusteredHubType, core.SeedRefusal("the component's name"))
	}
	s, err := newClusteredHubSettings(args)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", clusteredHubType, name, err)
	}

	comp := &ClusteredNatsHub{}
	if err := ctx.RegisterComponentResource(clusteredHubType, name, comp, opts...); err != nil {
		return nil, err
	}
	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it.
	comp.ServiceUrl = pulumi.String(s.serviceURL).ToStringOutput()
	comp.PodSelector = pulumi.ToStringMap(clusteredHubSelector(s.name)).ToStringMapOutput()
	comp.StatefulSetName = pulumi.String(s.name).ToStringOutput()
	comp.TailnetNatsUrl = pulumi.ToOutput((*string)(nil)).(pulumi.StringPtrOutput)
	comp.TailnetEnvoyUrl = pulumi.ToOutput((*string)(nil)).(pulumi.StringPtrOutput)
	if t := s.tailnet; t != nil {
		comp.TailnetNatsUrl = pulumi.StringPtr(t.natsURL).ToStringPtrOutput()
		comp.TailnetEnvoyUrl = pulumi.StringPtr(t.envoyURL).ToStringPtrOutput()
	}
	return comp, nil
}
