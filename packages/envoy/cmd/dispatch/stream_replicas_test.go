package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The Dispatch server's starts of the stream here go through dispatchStreamOptions over what
// resolveBootConfig read from the settings table, connecting in the background as main's do
// (serverClient, dispatchStreamStart), on an in-process three-server cluster
// (testnats.StartCluster), one per test or subtest, unless a test says single server.

// replicasTestClientID is the GitHub App the tests' webhook sweeper signs as.
const replicasTestClientID = "Iv1.replicas"

// reconcileWait bounds the wait of the start helpers for the background reconcile of the stream.
const reconcileWait = 30 * time.Second

// dispatchStreamStart is the Dispatch server's publisher connect for the settings overrides
// names, exactly as main makes it on the servers at urls: ConnectOwningStream with serverClient
// over dispatchStreamOptions, with no option and no clock of the test's own. It returns the
// settings it resolved, the client, and the buffer that holds the client's log lines. The client
// returns before it has connected, and its first connection reconciles the stream and only then
// starts its keeper (bus.WithConnectInBackground), so a caller waits for that reconcile itself.
// The helper sets the default logger to record the client's lines until the test ends: a test
// that calls it must not run in parallel.
func dispatchStreamStart(t *testing.T, urls []string, overrides map[string]string) (bootConfig, *bus.Client, *cmdtest.LockedBuffer) {
	t.Helper()
	env := bootEnvironment(overrides)
	boot, err := resolveBootConfig(env)
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	logged := &cmdtest.LockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	client, err := bus.ConnectOwningStream(urls, serverClient(dispatchStreamOptions(boot, env)...)...)
	if err != nil {
		t.Fatalf("connect Dispatch's publisher for %v: %v", overrides, err)
	}
	t.Cleanup(client.Close)
	return boot, client, logged
}

// startDispatchStream is startDispatchStreamRepairing with no repair: the Dispatch server's
// publisher connect for the settings overrides names, built exactly as main builds it
// (dispatchStreamStart), waited out until this client's own first connection has reconciled the
// stream to what the settings ask of it. It sets the default logger (dispatchStreamStart), so a
// test that calls it must not run in parallel.
func startDispatchStream(t *testing.T, urls []string, overrides map[string]string) *bus.Client {
	t.Helper()
	return startDispatchStreamRepairing(t, urls, overrides, nil)
}

// startDispatchStreamRepairing is the Dispatch server's publisher connect for the settings
// overrides names on the servers at urls (dispatchStreamStart), waited out, at the latest for
// reconcileWait, until the stream holds what the settings ask of it (reconciledFor). The helper
// waits for this client's own reconcile, not for a stream that already looks reconciled: it
// returns only once the client's hooks have covered a connection event
// (bus.Client.HookedConnectionEvents). An earlier start of the same test can leave the stream
// holding what the settings ask of it, and a helper that returned on that alone would let a check
// that the next start wrote nothing pass before this start had written at all.
//
// A failed run of the reconnect hooks does not end the wait, because the bus runs those hooks
// again (reconcileFailure). The first time the wait reads such a failure from the log, it calls
// repair once with that failure, and then starts the wait over, so that reconcileWait bounds the
// reconcile the repair makes possible rather than the repair itself: a repair that restarts a
// cluster member waits for the member, and testnats.Cluster.AwaitReady spends 15 seconds on a
// member no peer hears from before it restarts that member (measured here: one run in three).
// A nil repair is no repair. repair runs on the helper's own goroutine, which is the test's
// goroutine, because testnats.Server.Start, testnats.Server.Stop and testnats.Cluster.AwaitReady
// call t.Fatal, which Go forbids off the test goroutine.
//
// The helper sets the default logger (dispatchStreamStart), so a test that calls it must not run
// in parallel.
func startDispatchStreamRepairing(t *testing.T, urls []string, overrides map[string]string, repair func(hookFailure)) *bus.Client {
	t.Helper()
	boot, client, logged := dispatchStreamStart(t, urls, overrides)
	reconciled := reconciledFor(boot)
	var info *natsgo.StreamInfo
	var err error
	var failure hookFailure
	for deadline := time.Now().Add(reconcileWait); ; {
		if client.Connected() && client.HookedConnectionEvents() >= 1 {
			if info, err = client.JS().StreamInfo(bus.Stream); err == nil && reconciled(info.Config) {
				return client
			}
		}
		if failure.Message == "" {
			if failure = reconcileFailure(logged.String()); failure.Message != "" && repair != nil {
				repair(failure)
				deadline = time.Now().Add(reconcileWait)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch's start for %v did not reconcile the stream within %s: %+v (%v), hooked events %d, first logged failure of the reconnect hooks %q (%s)", overrides, reconcileWait, info, err, client.HookedConnectionEvents(), failure.Message, failure.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// refusedDispatchStream is the error of the first failed run of the reconnect hooks that the
// Dispatch server's publisher logs for the settings overrides names on the servers at urls
// (dispatchStreamStart, reconcileFailure), for a start whose settings the servers can never
// satisfy. It closes the client as soon as that failure reaches the log, so the client retries the
// reconcile no more. It fails the test when the stream reconciles instead, and when the publisher
// logs no such failure within reconcileWait. It sets the default logger (dispatchStreamStart), so
// a test that calls it must not run in parallel.
func refusedDispatchStream(t *testing.T, urls []string, overrides map[string]string) string {
	t.Helper()
	boot, client, logged := dispatchStreamStart(t, urls, overrides)
	reconciled := reconciledFor(boot)
	for deadline := time.Now().Add(reconcileWait); ; {
		if failure := reconcileFailure(logged.String()); failure.Message != "" {
			client.Close()
			return failure.Error
		}
		if client.Connected() && client.HookedConnectionEvents() >= 1 {
			if info, err := client.JS().StreamInfo(bus.Stream); err == nil && reconciled(info.Config) {
				t.Fatalf("Dispatch's start for %v reconciled the stream to %+v, want the server to refuse the start", overrides, info.Config)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch's start for %v logged no failed run of the reconnect hooks within %s, hooked events %d", overrides, reconcileWait, client.HookedConnectionEvents())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reconciledFor reports whether a stream holds what a Dispatch start with boot's settings writes:
// every subject this binary compiles in, at least boot's copies, at least its byte limit when it
// sets one, the binary's age, duplicate window and DiscardNew, and an id.
func reconciledFor(boot bootConfig) func(natsgo.StreamConfig) bool {
	return func(c natsgo.StreamConfig) bool {
		for _, subject := range bus.StreamSubjects() {
			if !slices.Contains(c.Subjects, subject) {
				return false
			}
		}
		return c.Replicas >= boot.NATSReplicas && (boot.StreamMaxBytes == 0 || c.MaxBytes >= boot.StreamMaxBytes) &&
			c.MaxAge == contracts.DeliveryDuplicateWindow && c.Duplicates == contracts.DeliveryDuplicateWindow &&
			c.Discard == natsgo.DiscardNew && c.Metadata[bus.StreamIDMetadataKey] != ""
	}
}

// hookFailure is one failed run of the Dispatch publisher's reconnect hooks, as the bus logged it:
// the message the bus chose for it, and the error of the run. The zero value says that the
// publisher logged no such run (Message == "").
type hookFailure struct {
	Message string
	Error   string
}

// reconcileFailure is the first failed run of the reconnect hooks that the Dispatch publisher
// logged in logs, JSON lines, and the zero hookFailure when it logged none. The bus reports such a
// run by how long its hooks have been uncovered (internal/bus, bus.RepairGrace): under that grace
// the WARN `envoy nats reconnect hook failed; it is retried`, at or past it the ERROR `envoy nats
// reconnect hooks have not succeeded`. Both messages are read, so that a failure a slow start logs
// past the grace is read as well, and the Message a caller gets back names which of the two the
// bus chose.
func reconcileFailure(logs string) hookFailure {
	for line := range strings.Lines(logs) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["name"] != "dispatch" {
			continue
		}
		switch message := record["msg"]; message {
		case "envoy nats reconnect hook failed; it is retried", "envoy nats reconnect hooks have not succeeded":
			return hookFailure{Message: fmt.Sprint(message), Error: fmt.Sprint(record["error"])}
		}
	}
	return hookFailure{}
}

// startDispatchNATS is all the Dispatch server's boot writes to NATS for overrides when it holds
// an App key: the publisher's connect (startDispatchStream, which fails the test itself when it
// does not reconcile) and the webhook sweeper's state bucket (webhookSweeper's Open, which the
// first sweep calls), opened at the settings' replica count. The error it returns is the sweeper's.
func startDispatchNATS(t *testing.T, urls []string, overrides map[string]string) error {
	t.Helper()
	client := startDispatchStream(t, urls, overrides)
	_, pemText := githubapptest.Key(t)
	app := &auth.AppConfig{ClientID: replicasTestClientID, PEM: pemText}
	sweeper, err := webhookSweeper(client, app, "", resolveWith(t, overrides).NATSReplicas)
	if err != nil {
		return err
	}
	_, err = sweeper.Open()
	return err
}

// A Dispatch start whose first reconcile fails on a cluster with a member down waits that failure
// out: the bus runs the reconnect hooks again, the start's own log line for the first failure is
// the retried WARN, and an operator who starts the missing member again within reconcileWait
// finds the start's stream at the three copies the settings ask of it.
func TestADispatchStartWaitsOutAReconcileFailureTheBusRetries(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.Servers[2].Stop(t)
	cluster.AwaitReady(t)
	overrides := map[string]string{"ENVOY_NATS_REPLICAS": "3"}

	var repaired hookFailure
	client := startDispatchStreamRepairing(t, cluster.URLs(), overrides, func(failure hookFailure) {
		repaired = failure
		cluster.Servers[2].Start(t)
		cluster.AwaitReady(t)
	})

	if want := "envoy nats reconnect hook failed; it is retried"; repaired.Message != want {
		t.Errorf("the start's first logged failure of the reconnect hooks is %q (%s), want the retried %q", repaired.Message, repaired.Error, want)
	}
	if events := client.HookedConnectionEvents(); events < 1 {
		t.Errorf("the client's hooks covered %d connection events, want at least 1", events)
	}
	info := testnats.AwaitSettledStream(t, cluster.JetStream(t), bus.Stream)
	if !reconciledFor(resolveWith(t, overrides))(info.Config) {
		t.Errorf("the repaired start left the stream at %+v with %d copies, want what %v asks of it", info.Config, testnats.Copies(info.Cluster), overrides)
	}
}

// On an existing three-copy stream with a byte limit, DiscardOld and an id, a Dispatch start whose
// stream differs in its subjects, its age or its duplicate window keeps the three copies, the byte
// limit and the id, and corrects the subjects, the age, the window and discard, to DiscardNew: with
// the deployment's settings (ENVOY_NATS_REPLICAS=3 and the byte limit), and with both settings
// unset, a replica count of 1 and no byte limit, which neither lowers the copies nor drops the limit.
func TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	for _, drift := range bustest.StreamDrifts() {
		for _, settings := range []struct {
			name      string
			overrides map[string]string
		}{
			{"the deployment's settings", map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)}},
			{"the settings unset", nil},
		} {
			t.Run(drift.Name+"/"+settings.name, func(t *testing.T) {
				cluster := testnats.StartCluster(t, 3)
				js := cluster.JetStream(t)
				deployed := bustest.DeployedThreeCopyStream(drift.Change)
				if _, err := js.AddStream(&deployed); err != nil {
					t.Fatalf("deploy the stream: %v", err)
				}
				before := testnats.AwaitSettledStream(t, js, bus.Stream).Config

				startDispatchStream(t, cluster.URLs(), settings.overrides)

				bustest.RequireKeptThreeCopyStream(t, before, testnats.AwaitSettledStream(t, js, bus.Stream))
			})
		}
	}
}

// `redeliver-webhooks` with ENVOY_NATS_REPLICAS=3 raises the R1 webhook redelivery bucket an
// earlier sweep at the default created to three copies, as the server's sweeper does.
func TestRedeliverWebhooksRaisesAnR1RedeliveryBucketToTheReplicaSetting(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	js := cluster.JetStream(t)
	if _, err := redeliver.OpenState(js, 1); err != nil {
		t.Fatalf("create the R1 redelivery bucket: %v", err)
	}
	stream := "KV_" + bus.WebhookRedeliveryBucket
	if info := testnats.AwaitSettledStream(t, js, stream); testnats.Copies(info.Cluster) != 1 {
		t.Fatalf("the redelivery bucket has %d copies, want 1", testnats.Copies(info.Cluster))
	}
	key, pemText := githubapptest.Key(t)
	github := githubapptest.NewWebhook(t, &key.PublicKey, replicasTestClientID, nil)

	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"redeliver-webhooks", "--since", "1h", "--dry-run"}, envGetter(map[string]string{
		"NATS_URLS":                cluster.URL(),
		"XDG_CONFIG_HOME":          t.TempDir(),
		"DISPATCH_APP_CLIENT_ID":   replicasTestClientID,
		"DISPATCH_APP_PEM_B64":     pemText,
		"DISPATCH_GITHUB_API_BASE": github.URL(),
		"ENVOY_NATS_REPLICAS":      "3",
	}), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("redeliver-webhooks --dry-run: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if info := testnats.AwaitSettledStream(t, js, stream); info.Config.Replicas != 3 || testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("the redelivery bucket has replicas %d with %d copies, want 3", info.Config.Replicas, testnats.Copies(info.Cluster))
	}
}

// On a single server with the default settings, the Dispatch server's start creates the stream and
// the webhook redelivery bucket at one copy, and its next start creates and updates nothing.
func TestADefaultDispatchStartOnASingleServerCreatesEverythingAtOneCopyAndTheNextWritesNothing(t *testing.T) {
	_, url := testnats.Start(t)
	streams := []string{bus.Stream, "KV_" + bus.WebhookRedeliveryBucket}

	bustest.RequireFirstStartAtOneCopyAndNoSecondWrite(t, url, streams, func() {
		t.Helper()
		if err := startDispatchNATS(t, []string{url}, nil); err != nil {
			t.Fatalf("start the Dispatch server's NATS work: %v", err)
		}
	})
}

// keeperWait bounds the wait for the Dispatch publisher's keeper to restore a lowered stream: its
// period is the bus's default of 30 seconds, which no exported option shortens, and a replica
// change the cluster takes to apply.
const keeperWait = 45 * time.Second

// The Dispatch server's publisher, started as main starts it, keeps the stream once its background
// reconcile has run: an operator who lowers the stream to one copy and half the byte limit finds
// both back at the deployment's settings within one keeper period.
func TestTheDispatchPublishersKeeperRestoresALoweredStream(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	js := cluster.JetStream(t)
	startDispatchStream(t, cluster.URLs(), map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)})
	lowered := testnats.AwaitSettledStream(t, js, bus.Stream).Config
	lowered.Replicas = 1
	lowered.MaxBytes = bustest.StreamMaxBytes / 2
	if _, err := js.UpdateStream(&lowered); err != nil {
		t.Fatalf("operator lowering: %v", err)
	}
	for deadline := time.Now().Add(keeperWait); ; {
		info, err := js.StreamInfo(bus.Stream)
		if err == nil && info.Config.Replicas == 3 && info.Config.MaxBytes == bustest.StreamMaxBytes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream was not restored to three copies and a byte limit of %d within %s: %+v (%v)", bustest.StreamMaxBytes, keeperWait, info, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if info := testnats.AwaitSettledStream(t, js, bus.Stream); testnats.Copies(info.Cluster) != 3 {
		t.Fatalf("the restored stream has %d copies, want 3", testnats.Copies(info.Cluster))
	}
}
