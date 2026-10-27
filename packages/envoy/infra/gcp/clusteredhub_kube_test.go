package gcp_test

import (
	"context"
	"slices"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	kubernetesProviderType = "pulumi:providers:kubernetes"
	chartType              = "kubernetes:helm.sh/v4:Chart"
	networkPolicyType      = "kubernetes:networking.k8s.io/v1:NetworkPolicy"
	podMonitoringType      = "kubernetes:monitoring.googleapis.com/v1:PodMonitoring"
	kubeSecretType         = "kubernetes:core/v1:Secret"
	configMapType          = "kubernetes:core/v1:ConfigMap"
	serviceAccountType     = "kubernetes:core/v1:ServiceAccount"
	deploymentType         = "kubernetes:apps/v1:Deployment"

	clusteredHubURN = "urn:pulumi:s::p::" + clusteredHubType + "::example-clustered-hub"
	// clusteredProviderRef starts the reference to the provider the hub builds, which every
	// Kubernetes child names.
	clusteredProviderRef = "urn:pulumi:s::p::" + clusteredHubType + "$" + kubernetesProviderType + "::envoy-nats-kubernetes::"
)

var (
	// clusteredHubBaseTypes are what a hub registers without tailnet or a priority class: itself,
	// its Kubernetes provider, the chart, the hub pods' NetworkPolicy and their PodMonitoring.
	clusteredHubBaseTypes = []string{clusteredHubType, kubernetesProviderType, chartType, networkPolicyType, podMonitoringType}
	// clusteredHubTailnetTypes are what tailnet adds: the tailnet hosts' auth key Secret, serve
	// ConfigMap, ServiceAccount, Deployment and budget.
	clusteredHubTailnetTypes = []string{kubeSecretType, configMapType, serviceAccountType, deploymentType, podDisruptionBudgetType}
	// clusteredHubTypes are what clusteredArgs' hub registers: with tailnet, without a priority
	// class.
	clusteredHubTypes = slices.Concat(clusteredHubBaseTypes, clusteredHubTailnetTypes)
)

// runClusteredHub constructs a clustered hub named example-clustered-hub from args under the
// mocks, failing the test on an error.
func runClusteredHub(t *testing.T, args *gcp.ClusteredNatsHubArgs) *pulumitest.Mocks {
	t.Helper()
	m, err := runClustered(t, "example-clustered-hub", args, nil)
	require.NoError(t, err)
	return m
}

// chartValues is the values the hub gives the chart, as the mocks recorded them.
func chartValues(t *testing.T, m *pulumitest.Mocks) map[string]any {
	t.Helper()
	return normalizedProperty(m.One(t, chartType).Inputs["values"]).(map[string]any)
}

// requireClusteredKubernetesChild asserts that r is a child of the hub on the provider the hub
// built, and on no other.
func requireClusteredKubernetesChild(t *testing.T, r pulumitest.Registration) {
	t.Helper()
	require.Equal(t, clusteredHubURN, r.RPC.GetParent(), "%s %s is the hub's child", r.Type, r.Name)
	require.Contains(t, r.RPC.GetProvider(), clusteredProviderRef, "%s %s runs on the hub's provider", r.Type, r.Name)
}

// The hub builds its own Kubernetes provider, the component's child, from kubeconfig, kept
// secret: server-side apply, and mutable ConfigMaps, so a users change updates the chart's
// ConfigMap in place rather than deleting and recreating it under the running pods. Every
// Kubernetes child, the chart and the priority class read among them, runs on that provider.
func TestClusteredNatsHubBuildsItsOwnKubernetesProvider(t *testing.T) {
	args := clusteredArgs()
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	m := runClusteredHub(t, args)

	provider := m.Named(t, kubernetesProviderType, "envoy-nats-kubernetes")
	require.Equal(t, clusteredHubURN, provider.RPC.GetParent())
	inputs := provider.Inputs.Mappable()
	require.Equal(t, true, inputs["enableServerSideApply"])
	require.Equal(t, true, inputs["enableConfigMapMutable"])
	value, secret := pulumitest.Plain(provider.Inputs["kubeconfig"])
	require.True(t, secret, "the kubeconfig stays secret")
	require.Equal(t, kubeconfig, value.StringValue())

	for _, r := range m.Registrations() {
		switch r.Type {
		case clusteredHubType, kubernetesProviderType:
		case chartType:
			// The chart's children take the kubernetes entry of its providers.
			requireClusteredKubernetesChild(t, r)
			require.Len(t, r.RPC.GetProviders(), 1)
			require.Contains(t, r.RPC.GetProviders()["kubernetes"], clusteredProviderRef)
		case priorityClassType:
			require.Equal(t, clusteredHubURN, r.ReadRPC.GetParent())
			require.Contains(t, r.ReadRPC.GetProvider(), clusteredProviderRef)
		default:
			requireClusteredKubernetesChild(t, r)
		}
	}
	require.ElementsMatch(t, slices.Concat(clusteredHubTypes, []string{priorityClassType}), m.Types())
}

// The chart is nats 2.15.1 from the NATS project's repository, installed under the release name
// name, so the chart's selector labels are clusteredHubSelector's, in the hub's namespace.
func TestClusteredNatsHubInstallsThePinnedChart(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	chart := m.Named(t, chartType, "envoy-nats").Inputs.Mappable()
	delete(chart, "values")
	require.Equal(t, map[string]any{
		"chart":          "nats",
		"version":        "2.15.1",
		"repositoryOpts": map[string]any{"repo": "https://nats-io.github.io/k8s/helm/charts/"},
		"name":           "envoy-nats",
		"namespace":      "envoy",
	}, chart)
}

// wantChartValues is the chart's values for clusteredArgs, given no storage class, no priority
// class and its session user: three clustered servers that advertise no pod address, JetStream
// on a 10Gi claim holding at most 9Gi, no memory store, monitoring on 8222, auth.conf included
// and held in the chart's ConfigMap, the three images by digest, the sizes, nats-server's
// GOMEMLIMIT at 80% of its 2Gi (⌊1638.4⌋ MiB), the client Service with the nats port alone, one
// server per zone, the session user's key on the pods, the budget, and no nats-box.
func wantChartValues(authConf string) map[string]any {
	ports := map[string]any{}
	for _, port := range []string{"nats", "leafnodes", "websocket", "mqtt", "cluster", "gateway", "monitor", "profiling"} {
		ports[port] = map[string]any{"enabled": port == "nats"}
	}
	return map[string]any{
		"fullnameOverride": "envoy-nats",
		"natsBox":          map[string]any{"enabled": false},
		"config": map[string]any{
			"cluster": map[string]any{"enabled": true, "replicas": 3.0, "noAdvertise": true},
			"jetstream": map[string]any{
				"enabled":     true,
				"fileStore":   map[string]any{"pvc": map[string]any{"size": "10Gi"}, "maxSize": "9Gi"},
				"memoryStore": map[string]any{"enabled": false},
			},
			"monitor": map[string]any{"enabled": true, "port": 8222.0},
			"merge":   map[string]any{"$include": "auth.conf"},
		},
		"configMap": map[string]any{"merge": map[string]any{"data": map[string]any{"auth.conf": authConf}}},
		"container": map[string]any{
			"image":     map[string]any{"fullImageName": natsImage},
			"resources": appResources("1", "2Gi"),
			"env":       map[string]any{"GOMEMLIMIT": "1638MiB"},
		},
		"reloader": map[string]any{
			"image": map[string]any{"fullImageName": reloaderImage},
			"merge": map[string]any{"resources": appResources("50m", "64Mi")},
		},
		"promExporter": map[string]any{
			"enabled":    true,
			"image":      map[string]any{"fullImageName": exporterImage},
			"podMonitor": map[string]any{"enabled": false},
			"merge":      map[string]any{"resources": appResources("50m", "128Mi")},
		},
		"service": map[string]any{"enabled": true, "name": "envoy-nats", "ports": ports},
		"podTemplate": map[string]any{
			"configChecksumAnnotation": false,
			"topologySpreadConstraints": map[string]any{"topology.kubernetes.io/zone": map[string]any{
				"maxSkew": 1.0, "whenUnsatisfiable": "DoNotSchedule", "minDomains": 3.0,
			}},
			"merge": map[string]any{"metadata": map[string]any{"annotations": map[string]any{
				"envoy-infra.sjawhar.dev/no-auth-user": keySessions,
			}}},
		},
		"podDisruptionBudget": map[string]any{"enabled": true},
	}
}

// The chart's values, whole: wantChartValues says what each is for. auth.conf is the resolver's
// rendering, the one TestClusteredNatsHubRendersTheVMHubsAuthorization holds to gcp:NatsHub's.
func TestClusteredNatsHubChartValues(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	values := chartValues(t, runClusteredHub(t, clusteredArgs()))
	require.Equal(t, wantChartValues(s.AuthConf), values)
	require.NotEmpty(t, s.AuthConf)
}

// storageClassName names the claims' class, beside their size, and the sizes follow diskSizeGb,
// maxFileStoreGb, natsCpu and natsMemory, GOMEMLIMIT following natsMemory (⌊0.8 × 4096⌋ MiB);
// unset, the claim names no class (the cluster's default), as TestClusteredNatsHubChartValues
// pins.
func TestClusteredNatsHubChartValuesFollowTheSizes(t *testing.T) {
	args := clusteredArgs()
	args.StorageClassName = pulumi.StringRef("premium-rwo")
	args.DiskSizeGb = pulumi.IntRef(40)
	args.MaxFileStoreGb = pulumi.IntRef(30)
	args.NatsCpu = pulumi.StringRef("2")
	args.NatsMemory = pulumi.StringRef("4Gi")
	values := chartValues(t, runClusteredHub(t, args))

	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	want := wantChartValues(s.AuthConf)
	want["config"].(map[string]any)["jetstream"].(map[string]any)["fileStore"] = map[string]any{
		"pvc": map[string]any{"size": "40Gi", "storageClassName": "premium-rwo"}, "maxSize": "30Gi",
	}
	want["container"].(map[string]any)["resources"] = appResources("2", "4Gi")
	want["container"].(map[string]any)["env"] = map[string]any{"GOMEMLIMIT": "3276MiB"}
	require.Equal(t, want, values)
}

// With INFRA-546's streamMaxBytes the chart takes the sizes it gives
// (TestClusteredNatsHubSizingFollowsStreamMaxBytes): a 53Gi claim holding at most 47Gi, and
// nats-server at 5Gi under a GOMEMLIMIT of 80% of it, 4096MiB; an explicit natsMemory of 4.5Gi
// gives ⌊0.8 × 4608⌋ = 3686MiB. Without streamMaxBytes, TestClusteredNatsHubChartValues pins
// 1638MiB for 2Gi.
func TestClusteredNatsHubChartValuesFollowStreamMaxBytes(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	for _, row := range []struct {
		name, natsMemory, memory, goMemLimit string
	}{
		{"default natsMemory", "", "5Gi", "4096MiB"},
		{"natsMemory 4.5Gi", "4.5Gi", "4.5Gi", "3686MiB"},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := clusteredArgs()
			args.StreamMaxBytes = pulumi.IntRef(infra546StreamMaxBytes)
			if row.natsMemory != "" {
				args.NatsMemory = pulumi.StringRef(row.natsMemory)
			}
			want := wantChartValues(s.AuthConf)
			want["config"].(map[string]any)["jetstream"].(map[string]any)["fileStore"] = map[string]any{
				"pvc": map[string]any{"size": "53Gi"}, "maxSize": "47Gi",
			}
			want["container"].(map[string]any)["resources"] = appResources("1", row.memory)
			want["container"].(map[string]any)["env"] = map[string]any{"GOMEMLIMIT": row.goMemLimit}
			require.Equal(t, want, chartValues(t, runClusteredHub(t, args)))
		})
	}
}

// priorityClassName reaches the hub's pods through the chart's pod template, and the hub reads
// the class first, on its own provider, with the chart waiting on the read, so a class the cluster
// lacks fails the preview. Unset, nothing is read and the pods name no class.
func TestClusteredNatsHubReadsAndAppliesThePriorityClass(t *testing.T) {
	defaults := runClusteredHub(t, clusteredArgs())
	require.Zero(t, countRegistrations(defaults, priorityClassType))
	require.NotContains(t, chartValues(t, defaults)["podTemplate"].(map[string]any)["merge"], "spec")

	args := clusteredArgs()
	args.PriorityClassName = pulumi.StringRef("envoy-critical")
	m := runClusteredHub(t, args)
	read := m.Named(t, priorityClassType, "envoy-nats-priority-class")
	require.Equal(t, "envoy-critical", read.ID)
	require.Nil(t, read.RPC, "the PriorityClass is read, never registered")
	require.Contains(t, m.One(t, chartType).RPC.GetDependencies(),
		"urn:pulumi:s::p::"+clusteredHubType+"$"+priorityClassType+"::envoy-nats-priority-class")

	want := chartValues(t, defaults)
	want["podTemplate"].(map[string]any)["merge"].(map[string]any)["spec"] = map[string]any{"priorityClassName": "envoy-critical"}
	require.Equal(t, want, chartValues(t, m))
}

// withoutKeys is values with each of paths removed, a path being the keys from the top.
func withoutKeys(values map[string]any, paths ...[]string) map[string]any {
	for _, path := range paths {
		parent := values
		for _, key := range path[:len(path)-1] {
			parent = parent[key].(map[string]any)
		}
		delete(parent, path[len(path)-1])
	}
	return values
}

var (
	authConfPath   = []string{"configMap", "merge", "data", "auth.conf"}
	noAuthUserPath = []string{"podTemplate", "merge", "metadata", "annotations", "envoy-infra.sjawhar.dev/no-auth-user"}
)

// A users change that keeps the session user rewrites auth.conf alone, which the reloader applies
// without a restart: every other value, the pod template's among them, stays as it was, so the
// StatefulSet does not roll. A session-user change changes the pods' annotation too, which rolls
// them, since nats-server refuses to reload a changed no_auth_user. Without a session user the
// pods carry no annotation.
func TestClusteredNatsHubRollsThePodsOnlyForASessionUserChange(t *testing.T) {
	before := chartValues(t, runClusteredHub(t, clusteredArgs()))

	_, added := testnats.User(t)
	args := clusteredArgs()
	args.Users = append(args.Users, gcp.NATSUserArgs{Name: "example-host-b", NKey: added, MachineID: pulumi.StringRef("example-host-b")})
	users := chartValues(t, runClusteredHub(t, args))
	require.NotEqual(t, before["configMap"], users["configMap"], "the added user reaches auth.conf")
	require.Contains(t, users["configMap"].(map[string]any)["merge"].(map[string]any)["data"].(map[string]any)["auth.conf"], added)
	require.Equal(t, withoutKeys(chartValues(t, runClusteredHub(t, clusteredArgs())), authConfPath), withoutKeys(users, authConfPath))

	_, session := testnats.User(t)
	args = clusteredArgs()
	args.SessionUser.NKey = session
	changed := chartValues(t, runClusteredHub(t, args))
	require.Equal(t, session, changed["podTemplate"].(map[string]any)["merge"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)["envoy-infra.sjawhar.dev/no-auth-user"])
	require.NotEqual(t, before["configMap"], changed["configMap"])
	require.Equal(t, withoutKeys(chartValues(t, runClusteredHub(t, clusteredArgs())), authConfPath, noAuthUserPath),
		withoutKeys(changed, authConfPath, noAuthUserPath))

	args = clusteredArgs()
	args.SessionUser = nil
	podTemplate := chartValues(t, runClusteredHub(t, args))["podTemplate"].(map[string]any)
	require.NotContains(t, podTemplate, "merge", "no session user, no annotation and no priority class")
}

// The chart takes one transform, which protects the chart's StatefulSet, so a change that would
// replace it, and stop all three servers at once, fails the preview; it leaves every other child,
// the chart's ConfigMap, Services and budget, as the chart renders it. Chart children are not
// registered under the mocks, so the transform is called directly.
func TestClusteredNatsHubProtectsTheStatefulSetAlone(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	require.Len(t, m.One(t, chartType).RPC.GetTransforms(), 1)

	props := pulumi.Map{"metadata": pulumi.Map{"name": pulumi.String("envoy-nats")}}
	got := gcp.ProtectClusteredHubStatefulSet(context.Background(), &pulumi.ResourceTransformArgs{
		Custom: true, Type: "kubernetes:apps/v1:StatefulSet", Name: "envoy-nats:envoy/envoy-nats", Props: props,
		Opts: pulumi.ResourceOptions{IgnoreChanges: []string{"example"}},
	})
	require.NotNil(t, got)
	require.True(t, got.Opts.Protect)
	require.Equal(t, props, got.Props)
	require.Equal(t, []string{"example"}, got.Opts.IgnoreChanges, "the chart's own options stay")

	for _, typ := range []string{
		configMapType, "kubernetes:core/v1:Service", podDisruptionBudgetType, deploymentType,
	} {
		require.Nil(t, gcp.ProtectClusteredHubStatefulSet(context.Background(), &pulumi.ResourceTransformArgs{
			Custom: true, Type: typ, Name: "envoy-nats:envoy/envoy-nats", Props: props,
		}), typ)
	}
}

// clusteredPolicyRules is the hub's NetworkPolicy's ingress rules, failing unless the policy is
// an Ingress policy on the hub pods alone.
func clusteredPolicyRules(t *testing.T, m *pulumitest.Mocks) []any {
	t.Helper()
	r := m.Named(t, networkPolicyType, "envoy-nats")
	spec := r.Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, map[string]any{"name": "envoy-nats", "namespace": "envoy"}, r.Inputs.Mappable()["metadata"])
	require.Equal(t, map[string]any{
		"app.kubernetes.io/name": "nats", "app.kubernetes.io/instance": "envoy-nats", "app.kubernetes.io/component": "nats",
	}, spec["podSelector"].(map[string]any)["matchLabels"])
	require.Equal(t, []any{"Ingress"}, spec["policyTypes"])
	return spec["ingress"].([]any)
}

// namespaceRule is an ingress rule admitting each of namespaces to port.
func namespaceRule(port float64, namespaces ...string) map[string]any {
	from := make([]any, len(namespaces))
	for i, namespace := range namespaces {
		from[i] = map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": namespace}}}
	}
	return map[string]any{"from": from, "ports": []any{map[string]any{"port": port}}}
}

// podRule is an ingress rule admitting the pods of the policy's namespace wearing labels to port.
func podRule(port float64, labels map[string]any) map[string]any {
	return map[string]any{
		"from":  []any{map[string]any{"podSelector": map[string]any{"matchLabels": labels}}},
		"ports": []any{map[string]any{"port": port}},
	}
}

var (
	hubPodLabels     = map[string]any{"app.kubernetes.io/name": "nats", "app.kubernetes.io/instance": "envoy-nats", "app.kubernetes.io/component": "nats"}
	tailnetPodLabels = map[string]any{"app.kubernetes.io/name": "nats-tailnet", "app.kubernetes.io/instance": "envoy-nats", "app.kubernetes.io/component": "tailnet"}
)

// The hub pods admit NATS's client port from each client namespace and from the hub's own
// tailnet hosts, which forward the tailnet's port to it; the route port from the hub pods alone,
// since routes carry no credential; the monitoring port from the monitor namespaces; and the
// exporter's port from the metrics namespaces, GMP's by default. Nothing else reaches them.
func TestClusteredNatsHubNetworkPolicy(t *testing.T) {
	args := clusteredArgs()
	args.MonitorNamespaces = []string{"monitoring"}
	require.Equal(t, []any{
		namespaceRule(4222, "envoy", "dispatch"),
		podRule(4222, tailnetPodLabels),
		podRule(6222, hubPodLabels),
		namespaceRule(8222, "monitoring"),
		namespaceRule(7777, "gmp-system", "gke-gmp-system"),
	}, clusteredPolicyRules(t, runClusteredHub(t, args)))
}

// With no monitor namespace the policy has no monitoring rule, since a rule with an empty from
// admits every source; without tailnet no pod but the clients' reaches NATS's client port.
func TestClusteredNatsHubNetworkPolicyWithoutMonitorsOrTailnet(t *testing.T) {
	require.Equal(t, []any{
		namespaceRule(4222, "envoy", "dispatch"),
		podRule(4222, tailnetPodLabels),
		podRule(6222, hubPodLabels),
		namespaceRule(7777, "gmp-system", "gke-gmp-system"),
	}, clusteredPolicyRules(t, runClusteredHub(t, clusteredArgs())))

	args := withoutTailnet(clusteredArgs())
	args.MetricsNamespaces = []string{"example-metrics"}
	require.Equal(t, []any{
		namespaceRule(4222, "envoy", "dispatch"),
		podRule(6222, hubPodLabels),
		namespaceRule(7777, "example-metrics"),
	}, clusteredPolicyRules(t, runClusteredHub(t, args)))
}

// Google Managed Prometheus scrapes the exporter of each hub pod: the PodMonitoring selects the
// hub pods by the chart's selector labels and scrapes the exporter's named port every 30 s.
func TestClusteredNatsHubPodMonitoring(t *testing.T) {
	m := runClusteredHub(t, clusteredArgs())
	r := m.Named(t, podMonitoringType, "envoy-nats")
	requireClusteredKubernetesChild(t, r)
	require.Equal(t, map[string]any{
		"apiVersion": "monitoring.googleapis.com/v1",
		"kind":       "PodMonitoring",
		"metadata":   map[string]any{"name": "envoy-nats", "namespace": "envoy"},
		"spec": map[string]any{
			"selector":  map[string]any{"matchLabels": hubPodLabels},
			"endpoints": []any{map[string]any{"port": "prom-metrics", "interval": "30s"}},
		},
	}, r.Inputs.Mappable())
}
