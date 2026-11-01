package aws

// The refusal tables are in package aws_test, so they build from the same example inputs as the
// components' tests, and run against each component's resolver directly, as host's do.

// Validate refuses what NewNatsService refuses before it requires a provider.
func (args *NatsServiceArgs) Validate() error {
	_, err := args.validate()
	return err
}

// Validate refuses what NewWebhookListener refuses before it requires a provider.
func (args *WebhookListenerArgs) Validate() error { return args.validate() }
