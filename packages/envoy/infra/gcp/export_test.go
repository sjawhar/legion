package gcp

import "strconv"

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
