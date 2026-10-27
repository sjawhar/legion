// Package bustest holds the deployed stream the start tests of envoy-listener and envoy-dispatch
// both begin from, and the check each makes of what a start left of it. The bus's own tests cannot
// import it (it imports the bus) and keep their own fixture, deployedClusterStream.
package bustest

import (
	"maps"
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// StreamMaxBytes is the deployed stream's byte limit, one every member of a test cluster can
	// reserve.
	StreamMaxBytes = 8 << 20
	// DeployedStreamID is the deployed stream's id (bus.StreamIDMetadataKey).
	DeployedStreamID = "deployed-id"
)

// DeployedThreeCopyStream is the stream as a three-copy deployment with a byte limit
// (StreamMaxBytes) and an id (DeployedStreamID) holds it, discarding old messages as a stream an
// earlier build created does, changed by change.
func DeployedThreeCopyStream(change func(*natsgo.StreamConfig)) natsgo.StreamConfig {
	deployed := natsgo.StreamConfig{
		Name:       bus.Stream,
		Subjects:   bus.StreamSubjects(),
		Retention:  natsgo.LimitsPolicy,
		MaxAge:     contracts.DeliveryDuplicateWindow,
		Duplicates: contracts.DeliveryDuplicateWindow,
		Discard:    natsgo.DiscardOld,
		Storage:    natsgo.FileStorage,
		Replicas:   3,
		MaxBytes:   StreamMaxBytes,
		Metadata:   map[string]string{bus.StreamIDMetadataKey: DeployedStreamID},
	}
	change(&deployed)
	return deployed
}

// RequireKeptThreeCopyStream fails the test unless after, the DeployedThreeCopyStream after a
// start, holds three copies, the byte limit and before's metadata, its id among it, with every
// subject this binary compiles in and its age, its duplicate window and discard corrected to the
// binary's: a full stream refuses new messages (DiscardNew).
func RequireKeptThreeCopyStream(t testing.TB, before natsgo.StreamConfig, after *natsgo.StreamInfo) {
	t.Helper()
	got := after.Config
	if got.Replicas != 3 || testnats.Copies(after.Cluster) != 3 {
		t.Errorf("stream replicas = %d with %d copies, want 3", got.Replicas, testnats.Copies(after.Cluster))
	}
	if got.MaxBytes != StreamMaxBytes || got.Discard != natsgo.DiscardNew {
		t.Errorf("stream byte limit = %d discarding %v, want %d discarding new", got.MaxBytes, got.Discard, StreamMaxBytes)
	}
	if !maps.Equal(got.Metadata, before.Metadata) || got.Metadata[bus.StreamIDMetadataKey] != DeployedStreamID {
		t.Errorf("stream metadata = %v, want the deployed %v", got.Metadata, before.Metadata)
	}
	if got.MaxAge != contracts.DeliveryDuplicateWindow || got.Duplicates != contracts.DeliveryDuplicateWindow {
		t.Errorf("stream age = %s and duplicate window = %s, want %s", got.MaxAge, got.Duplicates, contracts.DeliveryDuplicateWindow)
	}
	for _, subject := range bus.StreamSubjects() {
		if !slices.Contains(got.Subjects, subject) {
			t.Errorf("stream subjects = %v, want %s among them", got.Subjects, subject)
		}
	}
}
