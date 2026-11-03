package main

import (
	"os"
	"strconv"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's starts of the stream here go through listenerStreamOptions over what config.Load
// read, as main's do, on an in-process three-server cluster (testnats.StartCluster), one per test
// or subtest, unless a test says single server.

// listenerSettings is the configuration config.Load reads with NATS_URLS urls and ENVOY_NATS_REPLICAS
// and ENVOY_STREAM_MAX_BYTES as values names them: a setting values lacks is unset.
func listenerSettings(t *testing.T, urls string, values map[string]string) config.Service {
	t.Helper()
	t.Setenv("ENVOY_MACHINE_ID", "replicas-test")
	t.Setenv("NATS_URLS", urls)
	for _, name := range []string{bus.ReplicasEnvVar, bus.StreamMaxBytesEnvVar} {
		// t.Setenv restores the variable after the test; Unsetenv then leaves it unset for this one.
		t.Setenv(name, "")
		if value, ok := values[name]; ok {
			t.Setenv(name, value)
		} else if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	cfg, err := config.Load(9020)
	if err != nil {
		t.Fatalf("load the listener's settings: %v", err)
	}
	return cfg
}

// startListenerStream is a listener start's connect, which reconciles the stream as main's does.
func startListenerStream(t *testing.T, cfg config.Service) *bus.Client {
	t.Helper()
	client, err := bus.ConnectOwningStream(cfg.NATSURLs, listenerStreamOptions(cfg)...)
	if err != nil {
		t.Fatalf("start the listener's stream with %d replicas and byte limit %d: %v", cfg.NATSReplicas, cfg.StreamMaxBytes, err)
	}
	t.Cleanup(client.Close)
	return client
}

// On an existing three-copy stream with a byte limit, DiscardOld and an id, a listener start whose
// settings are the deployment's (ENVOY_NATS_REPLICAS=3 and the byte limit) and whose stream differs
// in its subjects, its age or its duplicate window keeps the three copies, the byte limit and the
// id, and corrects the subjects, the age, the window and discard, to DiscardNew.
func TestAListenerStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	for _, drift := range bustest.StreamDrifts() {
		t.Run(drift.Name, func(t *testing.T) {
			cluster := testnats.StartCluster(t, 3)
			js := cluster.JetStream(t)
			deployed := bustest.DeployedThreeCopyStream(drift.Change)
			if _, err := js.AddStream(&deployed); err != nil {
				t.Fatalf("deploy the stream: %v", err)
			}
			before := testnats.AwaitSettledStream(t, js, bus.Stream).Config

			startListenerStream(t, listenerSettings(t, cluster.URL(), map[string]string{
				bus.ReplicasEnvVar:       "3",
				bus.StreamMaxBytesEnvVar: strconv.Itoa(bustest.StreamMaxBytes),
			}))

			bustest.RequireKeptThreeCopyStream(t, before, testnats.AwaitSettledStream(t, js, bus.Stream))
		})
	}
}

// A listener start with ENVOY_NATS_REPLICAS=3 raises the R1 stream an earlier start at the default
// created, and the durable the listener created and bound on it (startListenerSubscription), which
// sets no replica count of its own, moves to three copies with it.
func TestRaisingTheStreamMovesTheListenersDurableToThreeCopies(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	js := cluster.JetStream(t)
	first := listenerSettings(t, cluster.URL(), nil)
	client := startListenerStream(t, first)
	durable := bus.ListenerDurable(first.MachineID)
	if _, err := startListenerSubscription(client, durable, func(*natsgo.Msg) {}); err != nil {
		t.Fatalf("create and bind the listener's durable: %v", err)
	}
	if info := testnats.AwaitSettledStream(t, js, bus.Stream); info.Config.Replicas != 1 || testnats.Copies(info.Cluster) != 1 {
		t.Fatalf("the default start's stream has replicas %d with %d copies, want 1", info.Config.Replicas, testnats.Copies(info.Cluster))
	}

	startListenerStream(t, listenerSettings(t, cluster.URL(), map[string]string{bus.ReplicasEnvVar: "3"}))

	if info := testnats.AwaitSettledStream(t, js, bus.Stream); info.Config.Replicas != 3 || testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("stream replicas = %d with %d copies after the raise, want 3", info.Config.Replicas, testnats.Copies(info.Cluster))
	}
	testnats.AwaitConsumerCopies(t, js, bus.Stream, durable, 3)
}

// On a single server with the default settings, the listener binary creates the stream and its
// four buckets (interests, role claims, sessions, CI state; the GitHub route opens the last) at one
// copy, and its next start creates and updates nothing.
func TestADefaultListenerOnASingleServerCreatesEverythingAtOneCopyAndItsNextStartWritesNothing(t *testing.T) {
	const machineID = "replicas-default"
	uri := testnats.URL(t)
	binary := cmdtest.Build(t, "envoy-listener")
	streams := []string{bus.Stream, "KV_" + bus.InterestBucket, "KV_" + bus.RoleClaimBucket, "KV_" + bus.SessionBucket, "KV_" + bus.CIStateBucket}

	bustest.RequireFirstStartAtOneCopyAndNoSecondWrite(t, uri, streams, func() {
		t.Helper()
		listener := startListenerProcess(t, binary, uri, machineID,
			"ENVOY_WEBHOOKS=github", "ENVOY_GITHUB_WEBHOOK_SECRET=replicas-secret", "ENVOY_REVIEWER_APP_ID=1")
		listener.waitHealthy(t)
		listener.Terminate(t)
		listener.WaitExit(t, "SIGTERM")
	})
}
