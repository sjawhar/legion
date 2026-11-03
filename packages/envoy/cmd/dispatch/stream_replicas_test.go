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
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The Dispatch server's starts of the stream here go through dispatchStreamOptions over what
// resolveBootConfig read from the settings table, connecting in the background as main's do
// (serverClient, connectDispatchStream), on an in-process three-server cluster
// (testnats.StartCluster), one per test or subtest, unless a test says single server.

// replicasTestClientID is the GitHub App the tests' webhook sweeper signs as.
const replicasTestClientID = "Iv1.replicas"

// reconcileWait bounds the wait of the start helpers for a start's first outcome: the stream's
// background reconcile, or the first failed run of the reconnect hooks.
const reconcileWait = 30 * time.Second

// repairedReconcileWait bounds the wait for the reconcile after a test has repaired what the
// first run failed on. reconcileWait alone is not enough: the bus retries the hooks with a
// backoff that reaches 30 s (maxBackoff, internal/bus/recovery.go:309), so a repair that lands
// just after a failed attempt waits that backoff out before the next attempt runs.
const repairedReconcileWait = reconcileWait + 30*time.Second

// dispatchStart is one Dispatch publisher start under test: the settings it resolved, the
// client, the buffer that holds the client's log lines, and the overrides a failure names.
type dispatchStart struct {
	boot      bootConfig
	client    *bus.Client
	logged    *cmdtest.LockedBuffer
	overrides map[string]string
}

// connectDispatchStream is the Dispatch server's publisher connect for the settings overrides
// names, exactly as main makes it on the servers at urls: ConnectOwningStream with serverClient
// over dispatchStreamOptions, with no option and no clock of the test's own. It returns the start
// it made (dispatchStart). The client returns before it has connected, and its first connection
// reconciles the stream and only then starts its keeper (bus.WithConnectInBackground), so a caller
// waits for what it expects itself (dispatchStart.awaitReconciled, dispatchStart.awaitHookFailure).
// The helper sets the default logger to record the client's lines until the test ends: a test that
// calls it must not run in parallel.
func connectDispatchStream(t *testing.T, urls []string, overrides map[string]string) dispatchStart {
	t.Helper()
	env := bootEnvironment(overrides)
	boot, err := resolveBootConfig(env)
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	logged := &cmdtest.LockedBuffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(logged, nil))
	client, err := bus.ConnectOwningStream(urls, serverClient(dispatchStreamOptions(boot, env)...)...)
	if err != nil {
		t.Fatalf("connect Dispatch's publisher for %v: %v", overrides, err)
	}
	t.Cleanup(client.Close)
	return dispatchStart{boot: boot, client: client, logged: logged, overrides: overrides}
}

// awaitReconciled waits, at the latest for within, until the stream holds what the start's
// settings ask of it (reconciledFor), and fails the test when it does not. The wait is for this
// client's own reconcile, not for a stream that already looks reconciled: it ends only once the
// client's hooks have covered a connection event (bus.Client.HookedConnectionEvents). An earlier
// start of the same test can leave the stream holding what the settings ask of it, and a wait
// that ended on that alone would let a check that the next start wrote nothing pass before this
// start had written at all.
//
// A failed run of the reconnect hooks does not end the wait, because the bus runs those hooks
// again. The wait reads no log at all. Its failure message names how long the hooks have been due
// and the error of the last run that failed (bus.Client.HookStall).
func (s dispatchStart) awaitReconciled(t *testing.T, within time.Duration) {
	t.Helper()
	reconciled := reconciledFor(s.boot)
	var info *natsgo.StreamInfo
	var err error
	for deadline := time.Now().Add(within); ; {
		if s.client.Connected() && s.client.HookedConnectionEvents() >= 1 {
			if info, err = s.client.JS().StreamInfo(bus.Stream); err == nil && reconciled(info.Config) {
				return
			}
		}
		if time.Now().After(deadline) {
			stall, hookErr := s.client.HookStall()
			t.Fatalf("Dispatch's start for %v did not reconcile the stream within %s: %+v (%v), hooked events %d, reconnect hooks due for %s, last failed run %v", s.overrides, within, info, err, s.client.HookedConnectionEvents(), stall, hookErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitHookFailure is the first failed run of the start's reconnect hooks that the Dispatch
// publisher logs (reconcileFailure), for a start whose settings the servers refuse. It leaves the
// client open and the caller decides what follows: a caller that wants no further reconcile closes
// the client, a caller that repairs what the start failed on waits the repaired reconcile out
// (awaitReconciled, repairedReconcileWait). It fails the test when the start's hooks run through
// instead, and when the publisher logs no such failure within reconcileWait.
func (s dispatchStart) awaitHookFailure(t *testing.T) hookFailure {
	t.Helper()
	for deadline := time.Now().Add(reconcileWait); ; {
		if failure := reconcileFailure(s.logged.String()); failure.Message != "" {
			return failure
		}
		if s.client.HookedConnectionEvents() >= 1 {
			t.Fatalf("Dispatch's start for %v ran its reconnect hooks through, so the servers accepted the start, want them to refuse it", s.overrides)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch's start for %v logged no failed run of the reconnect hooks within %s, hooked events %d", s.overrides, reconcileWait, s.client.HookedConnectionEvents())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startDispatchStream is the Dispatch server's publisher connect for the settings overrides names,
// built exactly as main builds it (connectDispatchStream), waited out for reconcileWait until this
// client's own first connection has reconciled the stream to what the settings ask of it
// (dispatchStart.awaitReconciled). It sets the default logger (connectDispatchStream), so a test
// that calls it must not run in parallel.
func startDispatchStream(t *testing.T, urls []string, overrides map[string]string) *bus.Client {
	t.Helper()
	start := connectDispatchStream(t, urls, overrides)
	start.awaitReconciled(t, reconcileWait)
	return start.client
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

// retriedHookFailure and uncoveredHookFailure are the two messages the bus logs a failed run of
// the reconnect hooks under, by how long the hooks have been uncovered (internal/bus,
// bus.RepairGrace): the WARN under that grace, the ERROR at or past it.
const (
	retriedHookFailure   = "envoy nats reconnect hook failed; it is retried"
	uncoveredHookFailure = "envoy nats reconnect hooks have not succeeded"
)

// hookFailure is one failed run of the Dispatch publisher's reconnect hooks, as the bus logged it:
// the message the bus chose for it, and the error of the run. The zero value says that the
// publisher logged no such run (Message == "").
type hookFailure struct {
	Message string
	Error   string
}

// reconcileFailure is the first failed run of the reconnect hooks that the Dispatch publisher
// logged in logs, JSON lines, and the zero hookFailure when it logged none. Both messages are
// read, so that a failure a slow start logs past the grace is read as well, and the Message a
// caller gets back names which of the two the bus chose.
func reconcileFailure(logs string) hookFailure {
	for line := range strings.Lines(logs) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["name"] != "dispatch" {
			continue
		}
		switch message := record["msg"]; message {
		case retriedHookFailure, uncoveredHookFailure:
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
	// The store only carries the GitHub rate limits a send reads; nothing here sends, and the
	// bucket this opens reads none, so an empty one is every database this needs.
	sweeper, err := webhookSweeper(client, &store.Store{}, app, "", resolveWith(t, overrides).NATSReplicas)
	if err != nil {
		return err
	}
	_, err = sweeper.Open()
	return err
}

// A Dispatch start whose first reconcile fails on a cluster with a member down waits that failure
// out: the bus runs the reconnect hooks again, the start's own log line for the first failure is
// the retried WARN, and an operator who starts the missing member again finds the start's stream
// at the three copies the settings ask of it, once the cluster is ready again. Every step runs on
// the test's own goroutine, because testnats.Server.Start, testnats.Server.Stop and
// testnats.Cluster.AwaitReady call t.Fatal, which Go forbids off the test goroutine.
func TestADispatchStartWaitsOutAReconcileFailureTheBusRetries(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.Servers[2].Stop(t)
	cluster.AwaitReady(t)
	overrides := map[string]string{"ENVOY_NATS_REPLICAS": "3"}

	start := connectDispatchStream(t, cluster.URLs(), overrides)
	failure := start.awaitHookFailure(t)
	if failure.Message != retriedHookFailure {
		t.Fatalf("the start's first logged failure of the reconnect hooks is %q (%s), want the retried %q", failure.Message, failure.Error, retriedHookFailure)
	}
	cluster.Servers[2].Start(t)
	cluster.AwaitReady(t)
	start.awaitReconciled(t, repairedReconcileWait)

	testnats.AwaitStreamCopies(t, cluster.JetStream(t), bus.Stream, 3)
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
	// redeliver-webhooks opens the database that holds the GitHub rate limits every process obeys.
	database := storetest.Open(t)

	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"redeliver-webhooks", "--since", "1h", "--dry-run"}, envGetter(map[string]string{
		"NATS_URLS":                cluster.URL(),
		"DATABASE_URL":             database.Pool.Config().ConnString(),
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
