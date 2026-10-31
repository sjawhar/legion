package contracts

// The normalizer's bounds on what an envelope copies from its webhook, for the external test that
// publishes the largest envelope of each kind (envelope_size_test.go). Compiled into the package's
// tests only.
const (
	MaxEnvelopeTextRunes    = maxEnvelopeTextRunes
	MaxPushChangedPaths     = maxPushChangedPaths
	MaxPushChangedPathRunes = maxPushChangedPathRunes
)
