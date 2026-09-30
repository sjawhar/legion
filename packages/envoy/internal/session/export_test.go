package session

// SetupNATS is setupNATS for the package's external tests, so they open their registries on the
// package's shared test NATS, on buckets no other test uses.
var SetupNATS = setupNATS
