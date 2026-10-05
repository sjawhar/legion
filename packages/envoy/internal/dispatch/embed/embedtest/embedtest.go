// Package embedtest holds fakes shared by this module's own tests and its callers' tests, kept
// out of the embed package proper so no test-only type ships in the production binary.
package embedtest

// FakeThrottleError stands in for a Bedrock throttle/capacity error well enough for
// embed.IsThrottled (and the AWS SDK's own retry classifier beneath it) to actually recognize it,
// without needing a live Bedrock call or a hand-built smithy type: it carries the ErrorCode()
// string method that classifier looks for. Shared by embed's own tests, embedqueue's, and api's -
// before this, two independent copies were hardcoded to "ThrottlingException" alone and a third,
// parameterized one in embed_test.go duplicated the same handful of lines a third time.
type FakeThrottleError struct{ Code string }

func (e FakeThrottleError) Error() string     { return "simulated: " + e.Code }
func (e FakeThrottleError) ErrorCode() string { return e.Code }
