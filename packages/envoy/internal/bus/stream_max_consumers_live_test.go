//go:build natslive

// The two transitions of the consumer cap that no test in this process can show, each against a
// real nats-server image. packages/envoy/go.mod links nats-server v2.10.29, the server every
// in-process test of this package runs: it is older than streamMaxConsumersFloor, so it refuses
// every update that changes MaxConsumers on a stream that already exists (maxConsumersRefusal),
// and older than 2.15, so it caps no stream that sets no cap and this binary leaves such a stream
// alone there (serverDefaultsAnUnsetCap). Every in-process fixture therefore deploys the stream
// already holding the cap (deployStream, publish_only_test.go), which leaves both transitions
// proved nowhere. They are proved here instead: the raise of an uncapped stream on the image the
// hub runs, which caps such a stream at 1000 itself and takes the update, and on the copy of
// nats:2.10 both the stream this binary must not touch and the one it must raise and cannot.
//
// A real image needs a container runtime, which Envoy's agents do not have, so the file sits
// behind the natslive tag and the envoy-go CI job runs it by name
// (.github/workflows/ci.yaml, "Test the container-backed NATS matrix (natslive)"). The runtime is
// reached only through testnats.StartImageConfig, as every package outside the three container
// rigs must (TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime, testnats/nats_test.go).

package bus

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// liveServerArgs are the JetStream arguments the GCP hub runs nats-server with, without its
// monitor port: the store the server keeps for the run lives at /data, as the hub's does.
var liveServerArgs = []string{"-js", "--store_dir", "/data"}

// deployedStreamFixtureID is the id the deployed fixture already carries, so a start finds the
// stream identified and the consumer cap is the one setting it has left to write.
const deployedStreamFixtureID = "deployed-stream-fixture"

// olderServerDeployedCap is the cap the stream carries where a subtest wants a start to raise one
// rather than leave it: 1000, the number the hub's own uncapped stream behaved as, and below the
// cap this binary compiles, so a start raises it and a server before streamMaxConsumersFloor
// refuses that raise.
const olderServerDeployedCap = 1000

// deployStreamWithCap creates ENVOY_NOTIFICATIONS on js as a deployment stands before this
// release: this binary's own configuration (streamCfg) with maxConsumers as its cap and an id
// already in its metadata, so nothing but the cap differs from what a start writes. A maxConsumers
// of 0 deploys a stream that sets no cap. It returns the cap the server answers for what it was
// given, which the caller records rather than assumes.
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

// liveServerEnvironment is the environment the starts below read (WithEnvironment): the opt-in to
// a NATS server that is not this machine's, and nothing else. A container's client URL is a
// mapped port on the Docker host, which is this machine on a CI runner and whatever DOCKER_HOST
// names anywhere else, and a start refuses every URL it cannot read as this machine's
// (LocalNATSURL, refuseRemoteNATS). Handing the environment also keeps a NATS_NKEY_SEED the run
// happens to hold out of a connect to a server that wants no credential.
func liveServerEnvironment(key string) (string, bool) {
	if key == AllowRemoteEnvVar {
		return "1", true
	}
	return "", false
}

// liveServerJetStream starts image, connects to it and answers its JetStream context. The
// connection is closed when the test ends.
func liveServerJetStream(t *testing.T, image string) (string, nats.JetStreamContext) {
	t.Helper()
	_, public := testnats.User(t)
	config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\nno_auth_user: %q\n", public, public)
	_, uri := testnats.StartImageConfig(t, image, config, liveServerArgs, true)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return uri, js
}

// What a stream carrying no consumer cap means is the server's, and this test puts both answers
// to a real server. On the image the hub runs, nats-server 2.15.1, such a stream is capped at the
// server's own default of 1000: a start raises it to the cap this binary compiles
// (StreamMaxConsumers), and the keeper of the client it returns puts that cap back after an
// operator lowers it. On the copy of nats:2.10 the same stream has no limit at all, and the two
// subtests there hold the two halves of the gate this binary applies:
//
//   - "a stream with no cap" proves the gate itself (serverDefaultsAnUnsetCap): the start writes
//     no cap change, so it succeeds where a start of the branch without the gate was refused, and
//     the stream still carries no cap once the keeper has had its periods;
//   - "a deployed cap below the compiled one" proves the refusal: that stream's cap does bind its
//     consumers on every server, so the start raises it, the server before streamMaxConsumersFloor
//     refuses the update, and the refusal names the field, the cap asked for, the first server
//     that can take the change (refusedSettings) and the server's own words, so an operator
//     reading it knows both what was refused and what takes it.
//
// Neither the raise nor the refusal can be shown in this process: the module links nats-server
// v2.10.29, and every in-process fixture deploys the stream already capped. The envoy-go CI job
// runs this test by name (.github/workflows/ci.yaml), so the name stays as that step spells it.
func TestAStartCapsAnUncappedStreamOnTheHubsServerAndIsRefusedOnTheOlderOne(t *testing.T) {
	t.Run("hub", func(t *testing.T) {
		uri, js := liveServerJetStream(t, testnats.HubImage)
		if deployed := deployStreamWithCap(t, js, 0); deployed != -1 && deployed != 0 {
			t.Fatalf("the deployed stream's consumer cap = %d, want the -1 or 0 a server answers for a stream deployed without one", deployed)
		}

		client, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment), WithKeepInterval(testKeepInterval))
		if err != nil {
			t.Fatalf("start owning the stream: %v", err)
		}
		t.Cleanup(client.Close)

		capped, err := js.StreamInfo(Stream)
		if err != nil {
			t.Fatalf("read the stream after the start: %v", err)
		}
		if capped.Config.MaxConsumers != StreamMaxConsumers {
			t.Fatalf("the stream's consumer cap after the start = %d, want %d", capped.Config.MaxConsumers, StreamMaxConsumers)
		}

		// An operator lowers the cap the start wrote; the client's keeper raises it back
		// within a period, as it does every other setting this binary owns (keepStream).
		lowered := capped.Config
		lowered.MaxConsumers = StreamMaxConsumers / 2
		if _, err := js.UpdateStream(&lowered); err != nil {
			t.Fatalf("operator lowering of the cap: %v", err)
		}
		awaitKept(t, js, Stream, "the consumer cap", func(info *nats.StreamInfo) bool {
			return info.Config.MaxConsumers == StreamMaxConsumers
		})
	})

	t.Run("2.10", func(t *testing.T) {
		t.Run("a stream with no cap is left exactly as it is", func(t *testing.T) {
			uri, js := liveServerJetStream(t, testnats.Image)
			deployed := deployStreamWithCap(t, js, 0)
			if deployed != -1 && deployed != 0 {
				t.Fatalf("the deployed stream's consumer cap = %d, want the -1 or 0 a server answers for a stream deployed without one", deployed)
			}

			client, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment), WithKeepInterval(testKeepInterval))
			if err != nil {
				t.Fatalf("the start against a stream with no cap on a server that caps none: %v, want no update and no refusal", err)
			}
			t.Cleanup(client.Close)

			// Three keeper periods, so the stream is read back after passes the keeper has run
			// rather than before its first.
			time.Sleep(3 * testKeepInterval)
			left, err := js.StreamInfo(Stream)
			if err != nil {
				t.Fatalf("read the stream after the start: %v", err)
			}
			if left.Config.MaxConsumers != deployed {
				t.Fatalf("the stream's consumer cap after the start and the keeper = %d, want the %d it was deployed with", left.Config.MaxConsumers, deployed)
			}
		})

		t.Run("a deployed cap below the compiled one is raised, and the server refuses it", func(t *testing.T) {
			uri, js := liveServerJetStream(t, testnats.Image)
			if deployed := deployStreamWithCap(t, js, olderServerDeployedCap); deployed != olderServerDeployedCap {
				t.Fatalf("the deployed stream's consumer cap = %d, want the %d it was deployed with", deployed, olderServerDeployedCap)
			}

			client, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment), WithKeepInterval(testKeepInterval))
			if err == nil {
				client.Close()
				t.Fatal("the start raised the cap on a server that cannot take the change, want a refusal")
			}
			// The first three names are the bus's own (refusedSettings) and the fourth is the
			// server's own words, which the bus writes nowhere itself. The floor is written only
			// where maxConsumersRefused holds, the predicate the production path reads, so a
			// refusal naming it is that refusal and no other answer this server happened to give:
			// asserting the floor asserts the predicate, and the test checks it no second time.
			for _, want := range []string{"max_consumers", strconv.Itoa(StreamMaxConsumers), streamMaxConsumersFloor, maxConsumersRefusal} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the start's refusal %q does not name %q", err, want)
				}
			}
		})
	})
}
