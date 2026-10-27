package gcp

import (
	"strconv"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// CloudConfig is the hub VM's cloud-config document, for the tests outside the package.
type CloudConfig = cloudConfig

// ResolveNatsHub is newSettings' refusal, for gcp_test's refusal table, which needs no engine.
func ResolveNatsHub(args *NatsHubArgs) error {
	_, err := newSettings(args)
	return err
}

// CloudConfigOf returns the cloud-config args give the hub's VM: the document, and the user data
// the resolver rendered from it.
func CloudConfigOf(args *NatsHubArgs) (CloudConfig, string, error) {
	s, err := newSettings(args)
	if err != nil {
		return CloudConfig{}, "", err
	}
	return s.vm.document(), s.userData, nil
}

// NatsHubAuthConf is the authorization args render for the hub's NATS server, for the test that
// holds gcp:ClusteredNatsHub's to the same rendering.
func NatsHubAuthConf(args *NatsHubArgs) (string, error) {
	s, err := newSettings(args)
	if err != nil {
		return "", err
	}
	return s.vm.natsConf, nil
}

// ClusteredNatsHubSettings is the resolved gcp:ClusteredNatsHub configuration the external tests
// assert. Tailnet is nil without tailnet.
type ClusteredNatsHubSettings struct {
	Name, Namespace, ServiceName, ServiceURL string
	NatsImage, ReloaderImage, ExporterImage  string
	NatsReplicas, StreamMaxBytes             int
	DiskSizeGb, MaxFileStoreGb               int
	NatsCPU, NatsMemory                      string
	StorageClassName, PriorityClassName      string
	ClientNamespaces                         []string
	MonitorNamespaces                        []string
	MetricsNamespaces                        []string
	AuthConf                                 string
	PodSelector                              map[string]string
	Tailnet                                  *ClusteredHubTailnetSettings
}

// ClusteredHubTailnetSettings is the resolved tailnet entry, its listener's environment included.
type ClusteredHubTailnetSettings struct {
	ServiceName, TailnetDNSName     string
	Tags                            []string
	Replicas                        int
	TailscaleImage                  string
	ListenerImage, MachineID        string
	SeedSecretName, SeedSecretKey   string
	ListenerCPU, ListenerMemory     string
	ListenerEnv                     []string
	TailnetNatsURL, TailnetEnvoyURL string
}

// ResolveClusteredNatsHub resolves gcp:ClusteredNatsHub's plain inputs for the external refusal
// and default tests, which need no engine.
func ResolveClusteredNatsHub(args *ClusteredNatsHubArgs) (ClusteredNatsHubSettings, error) {
	s, err := newClusteredHubSettings(args)
	if err != nil {
		return ClusteredNatsHubSettings{}, err
	}
	settings := ClusteredNatsHubSettings{
		Name: s.name, Namespace: s.namespace, ServiceName: s.serviceName, ServiceURL: s.serviceURL,
		NatsImage: s.images.nats, ReloaderImage: s.images.reloader, ExporterImage: s.images.exporter,
		NatsReplicas: s.natsReplicas, StreamMaxBytes: s.streamMaxBytes,
		DiskSizeGb: s.size.diskSizeGb, MaxFileStoreGb: s.size.maxFileStoreGb,
		NatsCPU: s.size.natsCPU, NatsMemory: s.size.natsMemory,
		StorageClassName: s.storageClassName, PriorityClassName: s.priorityClassName,
		ClientNamespaces: s.clientNamespaces, MonitorNamespaces: s.monitorNamespaces, MetricsNamespaces: s.metricsNamespaces,
		AuthConf:    s.authConf,
		PodSelector: clusteredHubSelector(s.name),
	}
	if t := s.tailnet; t != nil {
		l := t.listener
		settings.Tailnet = &ClusteredHubTailnetSettings{
			ServiceName: t.serviceName, TailnetDNSName: t.tailnetDNSName, Tags: t.tags, Replicas: t.replicas,
			TailscaleImage: t.tailscaleImage, ListenerImage: l.image, MachineID: l.machineID,
			SeedSecretName: l.seedSecretName, SeedSecretKey: l.seedSecretKey, ListenerCPU: l.cpu, ListenerMemory: l.memory,
			ListenerEnv: l.env, TailnetNatsURL: t.natsURL, TailnetEnvoyURL: t.envoyURL,
		}
	}
	return settings, nil
}

// RefuseClusteredHubSecret is value, gcp:ClusteredNatsHub's secret Input input (kubeconfig or
// tailnetAuthKey), as the hub named name reads it once it resolves: blank and seed refused by the
// rule gcp:Dispatch's credentials run, refuseDispatchSeedInSecret.
func RefuseClusteredHubSecret(value pulumi.StringInput, name, input string) pulumi.StringOutput {
	return refuseDispatchSeedInSecret(value, clusteredHubType+" "+name, input)
}

// DispatchRollingUpdate is the resolved rolling update's bounds, for the tests outside the package.
type DispatchRollingUpdate struct {
	MaxUnavailable int
	MaxSurge       int
}

// DispatchSettings is the resolved Dispatch configuration the external tests assert without
// exporting the component's internal representation. RollingUpdate is nil unless StrategyType is
// RollingUpdate.
type DispatchSettings struct {
	DatabaseVersion    string
	DatabaseTier       string
	DeletionProtection bool
	Namespace          string
	Replicas           int
	StrategyType       string
	RollingUpdate      *DispatchRollingUpdate
	ServeConfig        string
	AgentSecretsURL    string
	AppClientID        string
	AppID              string
	AppKeySecretName   string
	AppKeySecretKey    string
	AppCPU             string
	AppMemory          string
	PprofPort          int
}

// ResolveDispatch resolves Dispatch's plain inputs for the external refusal and default tests.
func ResolveDispatch(args *DispatchArgs) (DispatchSettings, error) {
	s, err := newDispatchSettings(args)
	if err != nil {
		return DispatchSettings{}, err
	}
	settings := DispatchSettings{
		DatabaseVersion:    s.databaseVersion,
		DatabaseTier:       s.databaseTier,
		DeletionProtection: s.deletionProtection,
		Namespace:          s.namespace,
		Replicas:           s.replicas,
		StrategyType:       s.strategy.strategyType,
		ServeConfig:        s.serveConfig,
		AgentSecretsURL:    s.agentSecretsURL,
		AppClientID:        s.appClientID,
		AppID:              s.appID,
		AppKeySecretName:   s.appKeySecretName,
		AppKeySecretKey:    s.appKeySecretKey,
		AppCPU:             s.appCPU,
		AppMemory:          s.appMemory,
		PprofPort:          s.pprofPort,
	}
	if rolling := s.strategy.rollingUpdate; rolling != nil {
		settings.RollingUpdate = &DispatchRollingUpdate{MaxUnavailable: rolling.maxUnavailable, MaxSurge: rolling.maxSurge}
	}
	return settings, nil
}

// DispatchKubeconfig renders the component's stable exec-plugin kubeconfig for the golden test.
func DispatchKubeconfig(endpoint, ca, name string) string {
	return renderKubeconfig(endpoint, ca, name)
}

// DispatchListenerEnvError resolves the listener container's environment from the one NATS URL,
// for the refusal tests, with the front door's proxy in the default namespace: nil when the URL
// renders an environment.
func DispatchListenerEnvError(hostname, url string) error {
	proxy := clusterLocal(hostname+"-tailnet", defaultDispatchNamespace) + ":" + strconv.Itoa(dispatchSOCKS5Port)
	_, err := dispatchListenerEnv(hostname, proxy, hostname+"-listener", url)
	return err
}
