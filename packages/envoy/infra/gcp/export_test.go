package gcp

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

// DispatchSettings is the resolved Dispatch configuration the external tests assert without
// exporting the component's internal representation.
type DispatchSettings struct {
	DatabaseVersion    string
	DatabaseTier       string
	DeletionProtection bool
	Namespace          string
	ServeConfig        string
	AgentSecretsURL    string
	AppClientID        string
	AppID              string
	AppKeySecretName   string
	AppKeySecretKey    string
}

// ResolveDispatch resolves Dispatch's plain inputs for the external refusal and default tests.
func ResolveDispatch(args *DispatchArgs) (DispatchSettings, error) {
	s, err := newDispatchSettings(args)
	if err != nil {
		return DispatchSettings{}, err
	}
	return DispatchSettings{
		DatabaseVersion:    s.databaseVersion,
		DatabaseTier:       s.databaseTier,
		DeletionProtection: s.deletionProtection,
		Namespace:          s.namespace,
		ServeConfig:        s.serveConfig,
		AgentSecretsURL:    s.agentSecretsURL,
		AppClientID:        s.appClientID,
		AppID:              s.appID,
		AppKeySecretName:   s.appKeySecretName,
		AppKeySecretKey:    s.appKeySecretKey,
	}, nil
}

// DispatchKubeconfig renders the component's stable exec-plugin kubeconfig for the golden test.
func DispatchKubeconfig(endpoint, ca, name string) string {
	return renderKubeconfig(endpoint, ca, name)
}

// DispatchListenerEnvError resolves the listener container's environment from the one NATS URL,
// for the refusal tests: nil when the URL renders an environment.
func DispatchListenerEnvError(hostname, url string) error {
	_, err := dispatchListenerEnv(hostname, url)
	return err
}
