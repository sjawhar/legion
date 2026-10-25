// Package embedtest holds fakes shared by this module's own tests and its callers' tests, kept
// out of the embed package proper so no test-only type ships in the production binary.
package embedtest

// FakeThrottleError stands in for a model service's throttle/capacity error, as a client hands it
// back: it carries the Throttled() marker embed.IsThrottled reads, so a caller's test means
// "the provider said back off" without a live call. Code names the condition in the error's text
// and through ErrorCode, the AWS SDK's error-code shape a Bedrock throttle also has. Shared by
// embed's own tests, embedqueue's, and api's - before this, two independent copies were hardcoded
// to "ThrottlingException" alone and a third, parameterized one in embed_test.go duplicated the
// same handful of lines a third time.
type FakeThrottleError struct{ Code string }

func (e FakeThrottleError) Error() string     { return "simulated: " + e.Code }
func (e FakeThrottleError) ErrorCode() string { return e.Code }
func (e FakeThrottleError) Throttled() bool   { return true }
