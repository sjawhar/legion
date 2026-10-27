package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	clusteredHubType = "envoy-infra:gcp:ClusteredNatsHub"

	reloaderImage = "example.invalid/nats-server-config-reloader@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	exporterImage = "example.invalid/prometheus-nats-exporter@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	kubeconfig    = "apiVersion: v1\nkind: Config\n"
)

// clusteredArgs are a clustered hub serving exampleArgs' users and session user, with a tailnet
// entry whose listener takes the machine id envoy-hub by default, as the VM hub's listener named
// envoy-hub does, on placeholders.
func clusteredArgs() *gcp.ClusteredNatsHubArgs {
	return &gcp.ClusteredNatsHubArgs{
		Kubeconfig:       pulumi.String(kubeconfig),
		Namespace:        "envoy",
		Name:             "envoy-nats",
		ServiceName:      "envoy-nats",
		NatsImage:        natsImage,
		ReloaderImage:    reloaderImage,
		ExporterImage:    exporterImage,
		ClientNamespaces: []string{"envoy", "dispatch"},
		Users:            exampleArgs().Users,
		SessionUser:      &gcp.SessionUserArgs{Name: "example-sessions", NKey: keySessions},
		Tailnet: &gcp.ClusteredHubTailnetArgs{
			ServiceName:    "envoy-hub",
			TailnetDnsName: "example-tailnet.ts.net",
			Tags:           []string{"tag:envoy-hub-host"},
			TailscaleImage: tailscaleImage,
			Listener: &gcp.ClusteredHubListenerArgs{
				Image: listenerImage, NKey: keyListener, SeedSecretName: "envoy-hub-listener-seed", SeedSecretKey: "seed",
			},
		},
		TailnetAuthKey: pulumi.String(authKey),
	}
}

// withoutTailnet is args with no tailnet entry, and so no tailnetAuthKey.
func withoutTailnet(args *gcp.ClusteredNatsHubArgs) *gcp.ClusteredNatsHubArgs {
	args.Tailnet, args.TailnetAuthKey = nil, nil
	return args
}

// runClustered constructs a clustered hub named name from args under the mocks, and returns what
// they saw and the run's error. check, when not nil, runs on the constructed hub inside the
// program.
func runClustered(t *testing.T, name string, args *gcp.ClusteredNatsHubArgs, check func(*gcp.ClusteredNatsHub)) (*pulumitest.Mocks, error) {
	t.Helper()
	m := newMocks()
	err := m.Run(func(ctx *pulumi.Context) error {
		hub, err := gcp.NewClusteredNatsHub(ctx, name, args)
		if err != nil {
			return err
		}
		if check != nil {
			check(hub)
		}
		return nil
	})
	return m, err
}

// awaitAny returns the value o resolves to, nil for an output resolved to nothing.
func awaitAny(t *testing.T, o pulumi.Output) any {
	t.Helper()
	out, err := internals.UnsafeAwaitOutput(t.Context(), o)
	require.NoError(t, err)
	require.True(t, out.Known, "the output is known")
	return out.Value
}

// The outputs come from the plain inputs alone, so they are known at preview: the client
// Service's in-cluster URL, the chart's selector labels for release name, the StatefulSet's name,
// and with tailnet the Service's tailnet URLs for NATS and the listener.
func TestClusteredNatsHubOutputs(t *testing.T) {
	for _, tailnet := range []bool{true, false} {
		args := clusteredArgs()
		if !tailnet {
			withoutTailnet(args)
		}
		var outputs map[string]any
		m, err := runClustered(t, "example-clustered-hub", args, func(h *gcp.ClusteredNatsHub) {
			outputs = map[string]any{
				"serviceUrl":      awaitAny(t, h.ServiceUrl),
				"podSelector":     awaitAny(t, h.PodSelector),
				"statefulSetName": awaitAny(t, h.StatefulSetName),
				"tailnetNatsUrl":  awaitAny(t, h.TailnetNatsUrl),
				"tailnetEnvoyUrl": awaitAny(t, h.TailnetEnvoyUrl),
			}
		})
		require.NoError(t, err)
		types := clusteredHubTypes
		if !tailnet {
			types = clusteredHubBaseTypes
		}
		require.ElementsMatch(t, types, m.Types())
		want := map[string]any{
			"serviceUrl": "nats://envoy-nats.envoy.svc.cluster.local:4222",
			"podSelector": map[string]string{
				"app.kubernetes.io/name": "nats", "app.kubernetes.io/instance": "envoy-nats", "app.kubernetes.io/component": "nats",
			},
			"statefulSetName": "envoy-nats",
			"tailnetNatsUrl":  (*string)(nil),
			"tailnetEnvoyUrl": (*string)(nil),
		}
		if tailnet {
			want["tailnetNatsUrl"] = pulumi.StringRef("nats://envoy-hub.example-tailnet.ts.net:4222")
			want["tailnetEnvoyUrl"] = pulumi.StringRef("http://envoy-hub.example-tailnet.ts.net:9020")
		}
		require.Equal(t, want, outputs, "tailnet %t", tailnet)
	}
}

// The hub's auth.conf is what gcp:NatsHub renders for the same users, listener and session user:
// the one authorization both hubs call, under the same header. With machine id envoy-hub the
// listener user is the VM hub's own listener user, named after its hostname envoy-hub, so the
// restored durable listener-envoy-hub and the listener's grants carry over the move. Without
// tailnet there is no listener user, as gcp:NatsHub renders none without a listener.
func TestClusteredNatsHubRendersTheVMHubsAuthorization(t *testing.T) {
	hub := exampleArgs()
	hub.Hostname = "envoy-hub"
	want, err := gcp.NatsHubAuthConf(hub)
	require.NoError(t, err)
	require.Contains(t, want, keyListener, "the VM hub renders its listener's user")

	t.Run("the default machine id", func(t *testing.T) {
		s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
		require.NoError(t, err)
		require.Equal(t, "envoy-hub", s.Tailnet.MachineID)
		require.Equal(t, want, s.AuthConf)
	})
	t.Run("an explicit machine id", func(t *testing.T) {
		args := clusteredArgs()
		args.Tailnet.ServiceName = "envoy-nats-tailnet"
		args.Tailnet.Listener.MachineID = pulumi.StringRef("envoy-hub")
		s, err := gcp.ResolveClusteredNatsHub(args)
		require.NoError(t, err)
		require.Equal(t, want, s.AuthConf)
	})
	t.Run("another machine id renders another listener user", func(t *testing.T) {
		args := clusteredArgs()
		args.Tailnet.Listener.MachineID = pulumi.StringRef("envoy-hub-b")
		s, err := gcp.ResolveClusteredNatsHub(args)
		require.NoError(t, err)
		require.NotEqual(t, want, s.AuthConf)
	})
	t.Run("without tailnet", func(t *testing.T) {
		hub.Listener = nil
		want, err := gcp.NatsHubAuthConf(hub)
		require.NoError(t, err)
		s, err := gcp.ResolveClusteredNatsHub(withoutTailnet(clusteredArgs()))
		require.NoError(t, err)
		require.Nil(t, s.Tailnet)
		require.Equal(t, want, s.AuthConf)
		require.NotContains(t, s.AuthConf, keyListener)
	})
}

// Without streamMaxBytes, each pod's volume is 10 GiB and JetStream's file store 9 GiB, ⌊0.9 ×
// 10⌋, and nats-server runs at 1 CPU and 2Gi; the store follows an explicit volume, at least 1.
func TestClusteredNatsHubSizingDefaults(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	require.Equal(t, 10, s.DiskSizeGb)
	require.Equal(t, 9, s.MaxFileStoreGb)
	require.Equal(t, "1", s.NatsCPU)
	require.Equal(t, "2Gi", s.NatsMemory)
	require.Equal(t, "", s.StorageClassName)
	require.Equal(t, "", s.PriorityClassName)

	for disk, store := range map[int]int{2: 1, 11: 9, 19: 17, 20: 18, 100: 90} {
		args := clusteredArgs()
		args.DiskSizeGb = pulumi.IntRef(disk)
		s, err := gcp.ResolveClusteredNatsHub(args)
		require.NoError(t, err)
		require.Equal(t, store, s.MaxFileStoreGb, "diskSizeGb %d", disk)
	}

	args := clusteredArgs()
	args.DiskSizeGb, args.MaxFileStoreGb = pulumi.IntRef(24), pulumi.IntRef(21)
	args.NatsCpu, args.NatsMemory = pulumi.StringRef("2"), pulumi.StringRef("6Gi")
	args.StorageClassName, args.PriorityClassName = pulumi.StringRef("premium-rwo"), pulumi.StringRef("envoy-critical")
	s, err = gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Equal(t, []any{24, 21, "2", "6Gi", "premium-rwo", "envoy-critical"},
		[]any{s.DiskSizeGb, s.MaxFileStoreGb, s.NatsCPU, s.NatsMemory, s.StorageClassName, s.PriorityClassName})
}

// The other defaults: the GMP collectors' namespaces admitted to the exporter, two tailnet hosts,
// and the listener at 500m and 1Gi.
func TestClusteredNatsHubDefaults(t *testing.T) {
	s, err := gcp.ResolveClusteredNatsHub(clusteredArgs())
	require.NoError(t, err)
	require.Equal(t, []string{"gmp-system", "gke-gmp-system"}, s.MetricsNamespaces)
	require.Empty(t, s.MonitorNamespaces)
	require.Equal(t, []string{"envoy", "dispatch"}, s.ClientNamespaces)
	require.Equal(t, 2, s.Tailnet.Replicas)
	require.Equal(t, "500m", s.Tailnet.ListenerCPU)
	require.Equal(t, "1Gi", s.Tailnet.ListenerMemory)

	args := clusteredArgs()
	args.MetricsNamespaces = []string{"monitoring"}
	args.MonitorNamespaces = []string{"ops"}
	args.Tailnet.Replicas = pulumi.IntRef(3)
	args.Tailnet.Listener.Cpu, args.Tailnet.Listener.Memory = pulumi.StringRef("250m"), pulumi.StringRef("512Mi")
	s, err = gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Equal(t, []string{"monitoring"}, s.MetricsNamespaces)
	require.Equal(t, []string{"ops"}, s.MonitorNamespaces)
	require.Equal(t, 3, s.Tailnet.Replicas)
	require.Equal(t, "250m", s.Tailnet.ListenerCPU)
	require.Equal(t, "512Mi", s.Tailnet.ListenerMemory)
}

// The listener runs as its machine id on the pod's loopback at the listener's port, serves the
// Service's tailnet name before the operator's further hosts, and reaches NATS through the
// in-cluster Service, which Env marks remote. ENVOY_NATS_REPLICAS and ENVOY_STREAM_MAX_BYTES are
// rendered exactly when natsReplicas and streamMaxBytes are given.
func TestClusteredNatsHubListenerEnvironment(t *testing.T) {
	base := []string{
		"ENVOY_MACHINE_ID=envoy-hub",
		"PORT=9020",
		"ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_ALLOWED_HOSTS=envoy-hub.example-tailnet.ts.net,envoy.example.com",
		"NATS_URLS=nats://envoy-nats.envoy.svc.cluster.local:4222",
		"ENVOY_ALLOW_REMOTE_NATS=1",
	}
	for _, row := range []struct {
		name                         string
		natsReplicas, streamMaxBytes *int
		want                         []string
	}{
		{"neither", nil, nil, nil},
		{"natsReplicas", pulumi.IntRef(3), nil, []string{"ENVOY_NATS_REPLICAS=3"}},
		{"streamMaxBytes", nil, pulumi.IntRef(1 << 30), []string{"ENVOY_STREAM_MAX_BYTES=1073741824"}},
		{"both", pulumi.IntRef(1), pulumi.IntRef(5), []string{"ENVOY_NATS_REPLICAS=1", "ENVOY_STREAM_MAX_BYTES=5"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := clusteredArgs()
			args.Tailnet.Listener.AllowedHosts = []string{"envoy.example.com"}
			args.NatsReplicas, args.StreamMaxBytes = row.natsReplicas, row.streamMaxBytes
			s, err := gcp.ResolveClusteredNatsHub(args)
			require.NoError(t, err)
			env := s.Tailnet.ListenerEnv
			require.ElementsMatch(t, append(slices.Clone(base), row.want...), env)
			for _, name := range []string{"ENVOY_NATS_REPLICAS=", "ENVOY_STREAM_MAX_BYTES=", "ENVOY_API_TOKEN="} {
				present := slices.ContainsFunc(row.want, func(e string) bool { return strings.HasPrefix(e, name) })
				require.Equal(t, present, slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, name) }), name)
			}
			require.Equal(t, row.natsReplicas != nil, s.NatsReplicas > 0)
			require.Equal(t, row.streamMaxBytes != nil, s.StreamMaxBytes > 0)
		})
	}
}

// streamMaxBytes, unlike natsReplicas, is not the tailnet listener's alone: the hub takes it
// without tailnet.
func TestClusteredNatsHubTakesStreamMaxBytesWithoutTailnet(t *testing.T) {
	args := withoutTailnet(clusteredArgs())
	args.StreamMaxBytes = pulumi.IntRef(1 << 30)
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	require.Nil(t, s.Tailnet)
	require.Equal(t, 1<<30, s.StreamMaxBytes)
}

// A refused input stops the constructor, named as the component and its name, before it
// registers anything.
func TestClusteredNatsHubRefusesAnInvalidInputBeforeRegistering(t *testing.T) {
	args := clusteredArgs()
	args.NatsReplicas = pulumi.IntRef(4)
	m, err := runClustered(t, "example-clustered-hub", args, nil)
	require.ErrorContains(t, err, "envoy-infra:gcp:ClusteredNatsHub example-clustered-hub: natsReplicas 4 must be from 1 to 3")
	require.Empty(t, m.Types(), "nothing registered")

	m, err = runClustered(t, "example-clustered-hub", clusteredArgs(), nil)
	require.NoError(t, err)
	require.ElementsMatch(t, clusteredHubTypes, m.Types())
}

// NewClusteredNatsHub quotes the name in every refusal and registers the component under it, so a
// seed there is refused first, before anything registers, as NewNatsHub refuses one in its own
// name.
func TestClusteredNatsHubRefusesASeedInTheComponentName(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct{ name, hub string }{
		{"a whole seed", seed},
		{"a seed inside a longer name", "hub-" + seed},
		{"a lowercase seed", strings.ToLower(seed)},
		{"a seed split by a hyphen", splitSeed(seed, "-")},
	} {
		t.Run(row.name, func(t *testing.T) {
			// An invalid input beside it shows the name's refusal comes first.
			args := clusteredArgs()
			args.Name = "Invalid"
			m, err := runClustered(t, row.hub, args, nil)
			require.ErrorContains(t, err, "envoy-infra:gcp:ClusteredNatsHub: the component's name holds an nkey seed (not echoed)")
			require.NotContains(t, err.Error(), "name \"Invalid\"")
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			require.Empty(t, m.Types(), "nothing registered")
		})
	}
}
