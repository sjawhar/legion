package gcp

// CloudConfig is the hub VM's cloud-config document, for the tests outside the package.
type CloudConfig = cloudConfig

// CloudConfigOf returns the cloud-config args give the hub's VM: the document, and the user data
// that holds it.
func CloudConfigOf(args *NatsHubArgs) (CloudConfig, string, error) {
	s, err := newSettings(args)
	if err != nil {
		return CloudConfig{}, "", err
	}
	text, err := s.vm.userData()
	return s.vm.document(), text, err
}
