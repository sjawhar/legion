//go:build natslive

// The cases of the consumer cap that no server in this process can show, each against a real
// nats-server image. packages/envoy/go.mod links nats-server v2.15.2, the server every in-process
// test of this package runs, and the hub runs v2.15.1 of the same line: the raise of an uncapped
// stream, the keeper's restore of a cap an operator lowered or unset and the version read each
// pass makes are therefore proved in process
// (TestAStartRaisesAnUncappedStreamAndTheKeeperPutsBackACapAnOperatorLoweredOrUnset). What is
// left needs a server this binary cannot link. One is the copy of nats:2.10
// (testnats.LegacyImage): it is older than streamMaxConsumersFloor, so it refuses every update
// that changes MaxConsumers on a stream that already exists (maxConsumersRefusal), and older
// than 2.15, so it caps no stream that sets no cap and this binary leaves such a stream alone
// there (serverDefaultsAnUnsetCap), and both of those cases are proved here. The other is the
// image the deployment itself runs, nats:2.15.1-alpine, which the hub half below reconciles the
// stream on: only a real image is the image the hub runs, and the issue's c-hub-reconcile check
// names it.
//
// A real image needs a container runtime, which Envoy's agents do not have, so the file sits
// behind the natslive tag, and the envoy-go CI job runs the tests that tag adds to this package,
// their names read off the source by .github/scripts/go-tagged-tests.sh (.github/workflows/ci.yaml,
// "Test the container-backed NATS matrix (natslive)"). The runtime is
// reached only through testnats.StartImageConfig, as every package outside the three container
// rigs must (TestNoPackageOutsideTheRigsImportsOrDrivesAContainerRuntime, testnats/nats_test.go).

package bus

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// liveServerArgs are the JetStream arguments the GCP hub runs nats-server with, without its
// monitor port: the store the server keeps for the run lives at /data, as the hub's does.
var liveServerArgs = []string{"-js", "--store_dir", "/data"}

// olderServerDeployedCap is the cap the stream carries where a subtest wants a start to raise one
// rather than leave it: 1000, the number the hub's own uncapped stream behaved as, and below the
// cap this binary compiles, so a start raises it and a server before streamMaxConsumersFloor
// refuses that raise.
const olderServerDeployedCap = 1000

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
// server's own default of 1000, so this binary raises it to the cap it compiles
// (streamMaxConsumers); on the copy of nats:2.10 the same stream has no limit at all, so the
// binary leaves it exactly as it is. The gate between the two is serverDefaultsAnUnsetCap, read
// from the version the connected server answers, and each case below says which direction of
// that read it fails on:
//
//   - on "hub", the start's raise and the keeper's raise of a cap an operator lowered to a
//     positive value prove the raise and the keeper's period, and no more: a positive cap below
//     the compiled one is raised whatever the gate answers;
//   - on "hub", the keeper's raise of a cap the same operator then takes off altogether proves
//     the gate's yes, and the keeper's own version read (keepPass) with it. The cap goes back to
//     the -1 this server answers for a stream that sets none, and only a pass whose read makes
//     2.15.1 a server that defaults an unset cap writes 24000 over it; a pass that read it
//     wrongly leaves the stream at -1 and the wait runs out;
//   - "2.10"/"a stream with no cap is left exactly as it is" proves the gate's no, in both
//     halves of the pass. The cap stays as it was deployed, and the keeper logged no refusal
//     while its periods ran: a pass that wrongly sent this server a raise has it refused, which
//     leaves the cap at -1 for the first assertion to pass as well and is the only reason the
//     second assertion exists, since that refusal reaches nothing but
//     `envoy nats keeper could not restore the stream` at WARN;
//   - "2.10"/"a deployed cap below the compiled one" proves the refusal: that stream's cap does
//     bind its consumers on every server, so the start raises it, the server before
//     streamMaxConsumersFloor refuses the update, and the refusal names the field, the cap asked
//     for, the first server that can take the change (refusedSettings) and the server's own
//     words, so an operator reading it knows both what was refused and what takes it.
//
// Only the "2.10" cases need a server this process cannot run: the module links nats-server
// v2.15.1, so the "hub" cases above are each proved in process as well
// (TestAStartRaisesAnUncappedStreamAndTheKeeperPutsBackACapAnOperatorLoweredOrUnset). They stay
// here because the image the hub runs is the one the deployment reconciles its stream on, and
// only a real image is that.
func TestAnUncappedStreamIsCappedOnTheHubsServerAndLeftAloneOnTheOlderOneWhereALowerCapIsRefused(t *testing.T) {
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
		if capped.Config.MaxConsumers != streamMaxConsumers {
			t.Fatalf("the stream's consumer cap after the start = %d, want %d", capped.Config.MaxConsumers, streamMaxConsumers)
		}

		// An operator lowers the cap the start wrote; the client's keeper raises it back
		// within a period, as it does every other setting this binary owns (keepStream).
		lowered := capped.Config
		lowered.MaxConsumers = streamMaxConsumers / 2
		if _, err := js.UpdateStream(&lowered); err != nil {
			t.Fatalf("operator lowering of the cap: %v", err)
		}
		raised := awaitKept(t, js, Stream, "the consumer cap", func(info *nats.StreamInfo) bool {
			return info.Config.MaxConsumers == streamMaxConsumers
		})

		// The same operator now takes the cap off instead of lowering it, which is the stream
		// this binary raises only where the server caps it itself. This server takes a -1 and a
		// 0 alike and answers -1 for both, normalising the 0 as it reads the update
		// (server/stream.go:1992 in v2.15.1), so the update carries the -1 it answers, and the
		// value the keeper reads back is the one a deployment that never set a cap leaves.
		unset := raised.Config
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

	t.Run("2.10", func(t *testing.T) {
		t.Run("a stream with no cap is left exactly as it is", func(t *testing.T) {
			// Captured before the start, so every keeper pass of this subtest logs into it.
			logs := captureLogs(t)
			uri, js := liveServerJetStream(t, testnats.LegacyImage)
			deployed := deployStreamWithCap(t, js, 0)
			if deployed != -1 && deployed != 0 {
				t.Fatalf("the deployed stream's consumer cap = %d, want the -1 or 0 a server answers for a stream deployed without one", deployed)
			}

			client, err := ConnectOwningStream([]string{uri}, WithEnvironment(liveServerEnvironment), WithKeepInterval(testKeepInterval))
			if err != nil {
				t.Fatalf("the start against a stream with no cap on a server that caps none: %v, want no update and no refusal", err)
			}
			t.Cleanup(client.Close)

			heldFor(t, js, Stream, "the consumer cap", 6, func(info *nats.StreamInfo) bool {
				return info.Config.MaxConsumers == deployed
			})
			// The cap above is unchanged whether the keeper sent nothing or sent a raise this
			// server refused, and a refusal reaches no caller: the pass logs it and the next
			// period tries again (keepPass). So the passes that just ran are read for it here.
			if refusals := logs.recordsWithMsg(t, "envoy nats keeper could not restore the stream"); len(refusals) != 0 {
				t.Fatalf("the keeper logged %v, want no pass that sent this server a cap change it refuses", refusals)
			}
		})

		t.Run("a deployed cap below the compiled one is raised, and the server refuses it", func(t *testing.T) {
			uri, js := liveServerJetStream(t, testnats.LegacyImage)
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
			for _, want := range []string{"max_consumers", strconv.Itoa(streamMaxConsumers), streamMaxConsumersFloor, maxConsumersRefusal} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the start's refusal %q does not name %q", err, want)
				}
			}
		})
	})
}
