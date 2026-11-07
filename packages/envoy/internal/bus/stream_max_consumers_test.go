package bus

import (
	"slices"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// deployedStreamFixtureID is the id the deployed fixture already carries, so a start finds the
// stream identified and the consumer cap is the one setting it has left to write.
const deployedStreamFixtureID = "deployed-stream-fixture"

// deployStreamWithCap creates ENVOY_NOTIFICATIONS on js as a deployment stands before this
// release: this binary's own configuration (streamCfg) with maxConsumers as its cap and an id
// already in its metadata, so nothing but the cap differs from what a start writes. A maxConsumers
// of 0 deploys a stream that sets no cap. It returns the cap the server answers for what it was
// given, which the caller records rather than assumes. The cases below deploy it on the server
// this binary links, and the live test's on a real image (stream_max_consumers_live_test.go).
func deployStreamWithCap(t *testing.T, js nats.JetStreamContext, maxConsumers int) int {
	t.Helper()
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamCfg.Subjects)
	deployed.MaxConsumers = maxConsumers
	deployed.Replicas = 1
	deployed.Metadata = map[string]string{StreamIDMetadataKey: deployedStreamFixtureID}
	info, err := js.AddStream(&deployed)
	if err != nil {
		t.Fatalf("deploy the stream with a consumer cap of %d: %v", maxConsumers, err)
	}
	return info.Config.MaxConsumers
}

// consumerCapFixture is the package's in-process nats-server as a case below reaches it: the
// client URL a start takes, and a JetStream context the writes an operator makes by hand go
// through. A test gets one JetStream account however often it asks for a URL (testnats.URL), so
// both reach the same stream, and each subtest is a test of its own and gets an account with no
// stream in it yet.
func consumerCapFixture(t *testing.T) (string, nats.JetStreamContext) {
	t.Helper()
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return uri, js
}

// The consumer cap against the server this binary links, nats-server v2.15.1 (go.mod): the
// release the hub runs, which caps a stream that sets no cap at a default of its own and takes a
// change of MaxConsumers on a stream that already exists. The three cases this release has to get
// right on such a server are proved here, each on an account and a start of its own:
//
//   - a deployed stream carrying no cap is raised to streamMaxConsumers by the start, the
//     transition the hub's own stream needed;
//   - a cap an operator lowered below the compiled one is put back by the keeper, within a period;
//   - a cap the same operator takes off altogether is put back by the keeper too. The cap goes
//     back to the -1 this server answers for a stream that sets none, and only a pass whose read
//     of the server's version makes it one that defaults an unset cap writes 24000 over it; a pass
//     that read it wrongly leaves the stream at -1 and the wait runs out.
//
// Each case drives ConnectOwningStream, a deployed program's own start, so the version the raise
// turns on is the one the start (install) and each keeper pass (keepPass) read off their live
// connection rather than one a test hands in: serverDefaultsAnUnsetCap answering wrongly fails the
// first and the last case here, and ownedStream no longer raising fails all three.
//
// What is left for a real nats-server image is the two 2.10 cases, a stream with no cap left
// exactly as it is and the refusal of the raise of a lower cap, and the hub's reconcile on the
// image the deployment runs (stream_max_consumers_live_test.go).
func TestAStartRaisesAnUncappedStreamAndTheKeeperPutsBackACapAnOperatorLoweredOrUnset(t *testing.T) {
	t.Run("a deployed stream with no cap is raised to the compiled cap by the start", func(t *testing.T) {
		uri, js := consumerCapFixture(t)
		if deployed := deployStreamWithCap(t, js, 0); deployed != -1 && deployed != 0 {
			t.Fatalf("the deployed stream's consumer cap = %d, want the -1 or 0 a server answers for a stream deployed without one", deployed)
		}

		// The start runs with no keeper period of its own, so the keeper's first pass is a
		// defaultKeepInterval away, 30 seconds, and the cap read below is the start's own write.
		client, err := ConnectOwningStream([]string{uri})
		if err != nil {
			t.Fatalf("start owning the stream: %v", err)
		}
		t.Cleanup(client.Close)

		if capped := deployedStream(t, js); capped.Config.MaxConsumers != streamMaxConsumers {
			t.Fatalf("the stream's consumer cap after the start = %d, want %d", capped.Config.MaxConsumers, streamMaxConsumers)
		}
	})

	t.Run("a cap an operator lowered is put back by the keeper", func(t *testing.T) {
		uri, js := consumerCapFixture(t)
		if deployed := deployStreamWithCap(t, js, streamMaxConsumers); deployed != streamMaxConsumers {
			t.Fatalf("the deployed stream's consumer cap = %d, want the %d it was deployed with", deployed, streamMaxConsumers)
		}
		client, err := ConnectOwningStream([]string{uri}, WithKeepInterval(testKeepInterval))
		if err != nil {
			t.Fatalf("start owning the stream: %v", err)
		}
		t.Cleanup(client.Close)

		// An operator lowers the cap the deployment carries; the client's keeper raises it back
		// within a period, as it does every other setting this binary owns (keepStream).
		lowered := deployedStream(t, js).Config
		lowered.MaxConsumers = streamMaxConsumers / 2
		down, err := js.UpdateStream(&lowered)
		if err != nil {
			t.Fatalf("operator lowering of the cap: %v", err)
		}
		if down.Config.MaxConsumers != streamMaxConsumers/2 {
			t.Fatalf("the stream's consumer cap after the operator lowered it = %d, want %d", down.Config.MaxConsumers, streamMaxConsumers/2)
		}
		awaitKept(t, js, Stream, "the consumer cap over a stream the operator lowered", func(info *nats.StreamInfo) bool {
			return info.Config.MaxConsumers == streamMaxConsumers
		})
	})

	t.Run("a cap an operator unset altogether is put back by the keeper", func(t *testing.T) {
		uri, js := consumerCapFixture(t)
		if deployed := deployStreamWithCap(t, js, streamMaxConsumers); deployed != streamMaxConsumers {
			t.Fatalf("the deployed stream's consumer cap = %d, want the %d it was deployed with", deployed, streamMaxConsumers)
		}
		client, err := ConnectOwningStream([]string{uri}, WithKeepInterval(testKeepInterval))
		if err != nil {
			t.Fatalf("start owning the stream: %v", err)
		}
		t.Cleanup(client.Close)

		// The operator takes the cap off instead of lowering it, which is the stream this binary
		// raises only where the server caps it itself. The update carries the -1 this server
		// answers for a stream that sets none, so the cap the keeper reads is the one a deployment
		// that never set one leaves, and the pass's own version read is what makes it a cap to
		// raise (keepPass).
		unset := deployedStream(t, js).Config
		unset.MaxConsumers = -1
		off, err := js.UpdateStream(&unset)
		if err != nil {
			t.Fatalf("operator unsetting of the cap: %v", err)
		}
		if off.Config.MaxConsumers != -1 {
			t.Fatalf("the stream's consumer cap after the operator unset it = %d, want the -1 this server answers for a stream with no cap", off.Config.MaxConsumers)
		}
		awaitKept(t, js, Stream, "the consumer cap over a stream the operator uncapped", func(info *nats.StreamInfo) bool {
			return info.Config.MaxConsumers == streamMaxConsumers
		})
	})
}
