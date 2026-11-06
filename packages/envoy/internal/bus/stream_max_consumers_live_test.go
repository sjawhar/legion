//go:build natslive

// The one transition production takes that no test in this process can show: a deployed
// ENVOY_NOTIFICATIONS carrying no consumer cap raised by a start to the cap this binary compiles
// (StreamMaxConsumers). packages/envoy/go.mod links nats-server v2.10.29, the server every
// in-process test of this package runs, and every nats-server before streamMaxConsumersFloor
// refuses an update that changes MaxConsumers on a stream that already exists ("nats: stream
// configuration update can not change MaxConsumers"); only a create carries the cap on such a
// server. Every in-process fixture therefore deploys the stream already holding the cap
// (deployStream, publish_only_test.go), which leaves the raise itself proved nowhere. It is
// proved here instead, against a real server image: on the image the hub runs, which takes the
// update, and on the copy of nats:2.10, which cannot and must say so in words an operator can act
// on (refusedSettings).
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

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// liveServerArgs are the JetStream arguments the GCP hub runs nats-server with, without its
// monitor port: the store the server keeps for the run lives at /data, as the hub's does.
var liveServerArgs = []string{"-js", "--store_dir", "/data"}

// uncappedStreamID is the id the deployed fixture already carries, so a start finds the stream
// identified and the consumer cap is the one setting it has left to write.
const uncappedStreamID = "uncapped-stream-fixture"

// deployUncappedStream creates ENVOY_NOTIFICATIONS on js as the hub's stream stands today: this
// binary's own configuration (streamCfg) with no MaxConsumers at all and an id already in its
// metadata, so nothing but the cap differs from what a start writes. It returns the cap the
// server answers for a stream deployed without one, which the caller records rather than assumes.
func deployUncappedStream(t *testing.T, js nats.JetStreamContext) int {
	t.Helper()
	deployed := *streamCfg
	deployed.Subjects = slices.Clone(streamCfg.Subjects)
	deployed.MaxConsumers = 0
	deployed.Replicas = 1
	deployed.Metadata = map[string]string{StreamIDMetadataKey: uncappedStreamID}
	info, err := js.AddStream(&deployed)
	if err != nil {
		t.Fatalf("deploy the stream with no consumer cap: %v", err)
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

// A start against a deployed stream with no consumer cap raises it to the cap this binary
// compiles (StreamMaxConsumers), and the keeper of the client it returns puts that cap back after
// an operator lowers it - on the image the hub runs, which takes an update that changes
// MaxConsumers. On the copy of nats:2.10, which refuses such an update whatever it carries, the
// same start fails, its refusal is the server's own refusal of the change (maxConsumersRefused)
// and not some other answer that server happened to give, and it names the field, the cap it
// asked for and the first nats-server that can take the change (refusedSettings), so an operator
// reading it knows both what was refused and what takes it. Neither half can be shown in this
// process: the module links nats-server v2.10.29.
func TestAStartCapsAnUncappedStreamOnTheHubsServerAndIsRefusedOnTheOlderOne(t *testing.T) {
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			_, public := testnats.User(t)
			config := fmt.Sprintf("authorization {\n  users: [ { nkey: %q } ]\n}\nno_auth_user: %q\n", public, public)
			_, uri := testnats.StartImageConfig(t, image.Ref, config, liveServerArgs, true)
			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			js, err := conn.JetStream()
			if err != nil {
				t.Fatalf("open JetStream: %v", err)
			}
			if deployed := deployUncappedStream(t, js); deployed != -1 && deployed != 0 {
				t.Fatalf("the deployed stream's consumer cap = %d, want the -1 or 0 a server answers for a stream deployed without one", deployed)
			}

			client, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment), withKeepInterval(testKeepInterval))
			if image.Ref != testnats.HubImage {
				if err == nil {
					client.Close()
					t.Fatal("the start raised the cap on a server that cannot take the change, want a refusal")
				}
				// The first three names below are the bus's own (refusedSettings), so a subtest
				// that asserted only them would pass for a refusal with another cause reaching
				// the same lines of this file. The fourth is the server's own words, which the
				// bus writes nowhere itself, and maxConsumersRefused, what the production path
				// reads to add the floor at all, is asserted after them, so this test and that
				// path agree on what the refusal is.
				for _, want := range []string{"max_consumers", strconv.Itoa(StreamMaxConsumers), streamMaxConsumersFloor, maxConsumersRefusal} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the start's refusal %q does not name %q", err, want)
					}
				}
				if !maxConsumersRefused(err) {
					t.Errorf("the start's refusal %q is not the server's refusal of a change of MaxConsumers, so the subtest proves nothing about the cap", err)
				}
				return
			}
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
	}
}
