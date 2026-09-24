package session

// SharedTestNATSURI is the package's shared test NATS for its external tests, so one test binary
// starts, and TestMain terminates, one container.
var SharedTestNATSURI = sharedTestNATSURI
