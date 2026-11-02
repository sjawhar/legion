package session

import "time"

// SetupNATS is setupNATS for the package's external tests, so they connect to the package's shared
// test NATS in an account of their own.
var SetupNATS = setupNATS

// withKnownSessionRetry shortens the waits between the attempts to open a bucket that did not
// open, so a test meets the attempt that opens one without waiting out the production schedule
// (knownBucketRetryFirst, knownBucketRetryMax). It is test-only: no deployment changes them.
func withKnownSessionRetry(first, longest time.Duration) KnownSessionsOption {
	return func(o *knownSessionsOpts) {
		o.retryFirst, o.retryMax = first, longest
	}
}
