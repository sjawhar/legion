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
