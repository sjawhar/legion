package gcp

import (
	"context"
	"fmt"

	kubernetes "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes"
	apiextensions "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apiextensions"
	helmv4 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v4"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	schedulingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/scheduling/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// The nats chart the hub installs: the chart, its version and the repository serving it. The
// values (clusteredHubChartValues) are written against this version's values.yaml and templates.
const (
	clusteredHubChart           = "nats"
	clusteredHubChartVersion    = "2.15.1"
	clusteredHubChartRepository = "https://nats-io.github.io/k8s/helm/charts/"
)

const (
	// clusteredHubClusterPort is the chart's route port (config.cluster.port), on which the hub's
	// servers reach one another.
	clusteredHubClusterPort = 6222
	// clusteredHubExporterPort and clusteredHubExporterPortName are the chart's Prometheus exporter
	// port (promExporter.port) and the name its container gives it
	// (files/stateful-set/prom-exporter-container.yaml).
	clusteredHubExporterPort     = 7777
	clusteredHubExporterPortName = "prom-metrics"
	// clusteredHubNoAuthUserAnnotation, on the hub's pod template, holds the session user's public
	// key. nats-server refuses a reload that changes no_auth_user, so a session-user change must
	// restart each server: the annotation changes the template, and the StatefulSet rolls its pods
	// one at a time. Any other users change rewrites only auth.conf, which the reloader applies.
	clusteredHubNoAuthUserAnnotation = "envoy-infra.sjawhar.dev/no-auth-user"
	// clusteredHubAuthConf is the file of the chart's ConfigMap holding the users' authorization,
	// which nats.conf includes and the reloader watches.
	clusteredHubAuthConf = "auth.conf"
	// statefulSetType is the type of the chart's StatefulSet, the one child the hub protects.
	statefulSetType = "kubernetes:apps/v1:StatefulSet"
)

// clusteredHubTailnetSelector is the selector labels of the hub's tailnet hosts, the pods that
// forward the tailnet's NATS port to the hub's client Service: what their workload selects and
// what the hub's NetworkPolicy admits to NATS's client port. They share no value of
// app.kubernetes.io/name or app.kubernetes.io/component with clusteredHubSelector, so neither
// selector matches the other's pods.
func clusteredHubTailnetSelector(release string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "nats-tailnet",
		"app.kubernetes.io/instance":  release,
		"app.kubernetes.io/component": "tailnet",
	}
}

// clusteredHubTailnetName is the name of the hub's tailnet hosts' Deployment, ConfigMap,
// ServiceAccount and PodDisruptionBudget, and the stem of their auth key Secret's.
func clusteredHubTailnetName(release string) string {
	return release + "-tailnet"
}

// refuseClusteredHubSecret is value, gcp:ClusteredNatsHub's secret Input input (kubeconfig or
// tailnetAuthKey), as the hub named name reads it once it resolves: blank and seed refused by the
// rule gcp:Dispatch's credentials run, refuseDispatchSeedInSecret.
func refuseClusteredHubSecret(value pulumi.StringInput, name, input string) pulumi.StringOutput {
	return refuseDispatchSeedInSecret(value, clusteredHubType+" "+name, input)
}

// newClusteredHubKubernetes registers the hub's Kubernetes children, each the component's child
// on the Kubernetes provider it builds from kubeconfig: the priority class read, the nats chart,
// the hub pods' NetworkPolicy and their PodMonitoring and, with tailnet, the tailnet hosts
// (newClusteredHubTailnetHosts).
func newClusteredHubKubernetes(ctx *pulumi.Context, comp *ClusteredNatsHub, name string, args *ClusteredNatsHubArgs, s clusteredHubSettings) error {
	// The provider is the component's own, so it can promise mutable ConfigMaps: pulumi-kubernetes
	// replaces a ConfigMap whose data changes otherwise, deleting it first, and a hub pod
	// scheduled in that window fails to mount its configuration.
	provider, err := kubernetes.NewProvider(ctx, s.name+"-kubernetes", &kubernetes.ProviderArgs{
		Kubeconfig:             refuseClusteredHubSecret(args.Kubeconfig, name, "kubeconfig"),
		EnableServerSideApply:  pulumi.Bool(true),
		EnableConfigMapMutable: pulumi.Bool(true),
	}, pulumi.Parent(comp))
	if err != nil {
		return err
	}
	child := []pulumi.ResourceOption{pulumi.Parent(comp), pulumi.Provider(provider)}
	chartOpts := []pulumi.ResourceOption{
		pulumi.Parent(comp), pulumi.Providers(provider),
		pulumi.Transforms([]pulumi.ResourceTransform{protectClusteredHubStatefulSet}),
	}
	// Kubernetes refuses every pod naming a class that does not exist, so the class is read first
	// and the chart and the tailnet hosts wait on the read: a missing class fails the preview rather
	// than the rollout.
	var classRead []pulumi.Resource
	if s.priorityClassName != "" {
		class, err := schedulingv1.GetPriorityClass(ctx, s.name+"-priority-class", pulumi.ID(s.priorityClassName), nil, child...)
		if err != nil {
			return err
		}
		classRead = []pulumi.Resource{class}
		chartOpts = append(chartOpts, pulumi.DependsOn(classRead))
	}
	if _, err := helmv4.NewChart(ctx, s.name, &helmv4.ChartArgs{
		Chart:          pulumi.String(clusteredHubChart),
		Version:        pulumi.String(clusteredHubChartVersion),
		RepositoryOpts: &helmv4.RepositoryOptsArgs{Repo: pulumi.String(clusteredHubChartRepository)},
		Name:           pulumi.String(s.name),
		Namespace:      pulumi.String(s.namespace),
		Values:         clusteredHubChartValues(s),
	}, chartOpts...); err != nil {
		return err
	}

	hubPods := clusteredHubSelector(s.name)
	if err := networkPolicy(ctx, s.name, s.namespace, hubPods, clusteredHubIngress(s), child...); err != nil {
		return err
	}
	_, err = apiextensions.NewCustomResource(ctx, s.name, &apiextensions.CustomResourceArgs{
		ApiVersion: pulumi.String("monitoring.googleapis.com/v1"), Kind: pulumi.String("PodMonitoring"),
		Metadata: objectMeta(s.name, s.namespace),
		OtherFields: kubernetes.UntypedArgs{"spec": pulumi.Map{
			"selector": pulumi.Map{"matchLabels": pulumi.ToStringMap(hubPods)},
			"endpoints": pulumi.Array{pulumi.Map{
				"port": pulumi.String(clusteredHubExporterPortName), "interval": pulumi.String("30s"),
			}},
		}},
	}, child...)
	if err != nil || s.tailnet == nil {
		return err
	}
	return newClusteredHubTailnetHosts(ctx, name, args.TailnetAuthKey, s, classRead, child...)
}

// clusteredHubIngress is what the hub pods' NetworkPolicy admits, by TCP port: NATS's client port
// from every pod of each client namespace and, with tailnet, from the component's tailnet hosts,
// which forward the tailnet's NATS port to it; the route port from the hub pods alone, which is
// all that keeps anything else from joining the cluster, since routes carry no credential; the
// monitoring port from each monitor namespace, and from none without one, as an empty from would
// admit every pod; and the exporter's port from each metrics namespace.
func clusteredHubIngress(s clusteredHubSettings) networkingv1.NetworkPolicyIngressRuleArray {
	ingress := networkingv1.NetworkPolicyIngressRuleArray{ingressFrom(natsPort, namespacePeers(s.clientNamespaces))}
	if s.tailnet != nil {
		ingress = append(ingress, ingressFrom(natsPort, networkingv1.NetworkPolicyPeerArray{labelPeer(clusteredHubTailnetSelector(s.name))}))
	}
	ingress = append(ingress, ingressFrom(clusteredHubClusterPort, networkingv1.NetworkPolicyPeerArray{labelPeer(clusteredHubSelector(s.name))}))
	if len(s.monitorNamespaces) > 0 {
		ingress = append(ingress, ingressFrom(monitorPort, namespacePeers(s.monitorNamespaces)))
	}
	return append(ingress, ingressFrom(clusteredHubExporterPort, namespacePeers(s.metricsNamespaces)))
}

// protectClusteredHubStatefulSet is the chart's transform: it protects the chart's StatefulSet and
// leaves every other child as the chart renders it. A change to the StatefulSet's volume claim
// templates, selector, service name or pod management policy replaces it, which stops all three
// servers at once; protected, such a change fails the preview instead.
func protectClusteredHubStatefulSet(_ context.Context, args *pulumi.ResourceTransformArgs) *pulumi.ResourceTransformResult {
	if args.Type != statefulSetType {
		return nil
	}
	opts := args.Opts
	opts.Protect = true
	return &pulumi.ResourceTransformResult{Props: args.Props, Opts: opts}
}

// clusteredHubChartValues is the nats chart's values for the hub (chart 2.15.1's values.yaml and
// templates). The reloader and the exporter take no resources value in this chart version, so
// their fixed sizes are merged into their containers (reloader.merge, promExporter.merge).
// nats-server's GOMEMLIMIT is a container.env entry, which the chart renders as a plain env var
// of the nats container (files/stateful-set/nats-container.yaml, templates/_helpers.tpl nats.env).
func clusteredHubChartValues(s clusteredHubSettings) pulumi.Map {
	jetstreamPVC := pulumi.Map{"size": gibibytes(s.size.diskSizeGb)}
	if s.storageClassName != "" {
		jetstreamPVC["storageClassName"] = pulumi.String(s.storageClassName)
	}
	podTemplate := pulumi.Map{
		"configChecksumAnnotation": pulumi.Bool(false),
		// One server per zone, held hard: minDomains keeps two servers from sharing a zone when
		// only two zones have nodes.
		"topologySpreadConstraints": pulumi.Map{"topology.kubernetes.io/zone": pulumi.Map{
			"maxSkew": pulumi.Int(1), "whenUnsatisfiable": pulumi.String("DoNotSchedule"), "minDomains": pulumi.Int(clusteredHubServers),
		}},
	}
	podMerge := pulumi.Map{}
	if s.priorityClassName != "" {
		podMerge["spec"] = pulumi.Map{"priorityClassName": pulumi.String(s.priorityClassName)}
	}
	if s.sessionNKey != "" {
		podMerge["metadata"] = pulumi.Map{"annotations": pulumi.Map{clusteredHubNoAuthUserAnnotation: pulumi.String(s.sessionNKey)}}
	}
	if len(podMerge) > 0 {
		podTemplate["merge"] = podMerge
	}
	servicePorts := pulumi.Map{}
	for _, port := range []string{"nats", "leafnodes", "websocket", "mqtt", "cluster", "gateway", "monitor", "profiling"} {
		servicePorts[port] = pulumi.Map{"enabled": pulumi.Bool(port == "nats")}
	}
	return pulumi.Map{
		"fullnameOverride": pulumi.String(s.name),
		"natsBox":          pulumi.Map{"enabled": pulumi.Bool(false)},
		"config": pulumi.Map{
			"cluster": pulumi.Map{"enabled": pulumi.Bool(true), "replicas": pulumi.Int(clusteredHubServers), "noAdvertise": pulumi.Bool(true)},
			"jetstream": pulumi.Map{
				"enabled":     pulumi.Bool(true),
				"fileStore":   pulumi.Map{"pvc": jetstreamPVC, "maxSize": gibibytes(s.size.maxFileStoreGb)},
				"memoryStore": pulumi.Map{"enabled": pulumi.Bool(false)},
			},
			"monitor": pulumi.Map{"enabled": pulumi.Bool(true), "port": pulumi.Int(monitorPort)},
			"merge":   pulumi.Map{"$include": pulumi.String(clusteredHubAuthConf)},
		},
		"configMap": pulumi.Map{"merge": pulumi.Map{"data": pulumi.Map{clusteredHubAuthConf: pulumi.String(s.authConf)}}},
		"container": pulumi.Map{
			"image":     pulumi.Map{"fullImageName": pulumi.String(s.images.nats)},
			"resources": chartResources(s.size.natsCPU, s.size.natsMemory),
			"env":       pulumi.Map{"GOMEMLIMIT": pulumi.String(s.size.goMemLimit)},
		},
		"reloader": pulumi.Map{
			"image": pulumi.Map{"fullImageName": pulumi.String(s.images.reloader)},
			"merge": pulumi.Map{"resources": chartResources(clusteredHubReloaderCPU, clusteredHubReloaderMemory)},
		},
		"promExporter": pulumi.Map{
			"enabled":    pulumi.Bool(true),
			"image":      pulumi.Map{"fullImageName": pulumi.String(s.images.exporter)},
			"podMonitor": pulumi.Map{"enabled": pulumi.Bool(false)},
			"merge":      pulumi.Map{"resources": chartResources(clusteredHubExporterCPU, clusteredHubExporterMemory)},
		},
		"service":             pulumi.Map{"enabled": pulumi.Bool(true), "name": pulumi.String(s.serviceName), "ports": servicePorts},
		"podTemplate":         podTemplate,
		"podDisruptionBudget": pulumi.Map{"enabled": pulumi.Bool(true)},
	}
}

// chartResources is a container's resources as chart values: the request and the limit equal.
func chartResources(cpu, memory string) pulumi.Map {
	size := pulumi.Map{"cpu": pulumi.String(cpu), "memory": pulumi.String(memory)}
	return pulumi.Map{"requests": size, "limits": size}
}

// gibibytes is n GiB as a Kubernetes quantity, which nats-server also reads as a size.
func gibibytes(n int) pulumi.String {
	return pulumi.String(fmt.Sprintf("%dGi", n))
}
