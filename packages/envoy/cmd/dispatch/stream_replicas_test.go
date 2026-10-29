package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// (serverClient, startDispatchStream), on an in-process three-server cluster
// (testnats.StartCluster), one per test or subtest, unless a test says single server.

// replicasTestClientID is the GitHub App the tests' webhook sweeper signs as.
const replicasTestClientID = "Iv1.replicas"

// reconcileWait bounds startDispatchStream's wait for the background reconcile of the stream.
const reconcileWait = 30 * time.Second

// startDispatchStream is the Dispatch server's publisher connect for the settings overrides
// names, exactly as main makes it on the servers at urls: ConnectOwningStream with serverClient
// over dispatchStreamOptions. The client returns before it has connected, and its first
// connection reconciles the stream and only then starts its keeper (bus.WithConnectInBackground),
// so the helper waits, at the latest for reconcileWait, until the stream holds what the settings
// ask of it (reconciledFor), or until the client logs that reconcile's failure: main's constructor
// no longer returns a refusal of the server, which reaches the log as the reconnect hook's failure
// (`envoy nats reconnect hook failed`). A refused start is closed, so it retries no more, and the
// logged error is returned. It sets the default logger to record the client's lines until the
// test ends: a test that calls it must not run in parallel.
func startDispatchStream(t *testing.T, urls []string, overrides map[string]string) (*bus.Client, error) {
	t.Helper()
	env := bootEnvironment(overrides)
	boot, err := resolveBootConfig(env)
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	var logged cmdtest.LockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	client, err := bus.ConnectOwningStream(urls, serverClient(dispatchStreamOptions(boot, env)...)...)
	if err != nil {
		return nil, err
	}
	t.Cleanup(client.Close)
	reconciled := reconciledFor(boot)
	var info *natsgo.StreamInfo
	for deadline := time.Now().Add(reconcileWait); ; {
		if refusal := reconcileFailure(logged.String()); refusal != "" {
			client.Close()
			return nil, errors.New(refusal)
		}
		if client.Connected() {
			if info, err = client.JS().StreamInfo(bus.Stream); err == nil && reconciled(info.Config) {
				return client, nil
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch's start for %v did not reconcile the stream within %s, nor log a failure: %+v (%v)", overrides, reconcileWait, info, err)
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

// reconcileFailure is the error of the first `envoy nats reconnect hook failed` record the
// Dispatch publisher logged in logs, JSON lines, and "" when it logged none.
func reconcileFailure(logs string) string {
	for line := range strings.Lines(logs) {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "envoy nats reconnect hook failed" && record["name"] == "dispatch" {
			return fmt.Sprint(record["error"])
		}
	}
	return ""
}

// startDispatchNATS is all the Dispatch server's boot writes to NATS for overrides when it holds
// an App key: the publisher's connect (startDispatchStream) and the webhook sweeper's state bucket
// (webhookSweeper's Open, which the first sweep calls), opened at the settings' replica count.
func startDispatchNATS(t *testing.T, urls []string, overrides map[string]string) error {
	t.Helper()
	client, err := startDispatchStream(t, urls, overrides)
	if err != nil {
		return err
	}
	_, pemText := githubapptest.Key(t)
	app := &auth.AppConfig{ClientID: replicasTestClientID, PEM: pemText}
	sweeper, err := webhookSweeper(client, app, "", resolveWith(t, overrides).NATSReplicas)
	if err != nil {
		return err
	}
	_, err = sweeper.Open()
	return err
}

// On an existing three-copy stream with a byte limit, DiscardOld and an id, a Dispatch start whose
// stream differs in its subjects, its age or its duplicate window keeps the three copies, the byte
// limit and the id, and corrects the subjects, the age, the window and discard, to DiscardNew: with
// the deployment's settings (ENVOY_NATS_REPLICAS=3 and the byte limit), and with both settings
// unset, a replica count of 1 and no byte limit, which neither lowers the copies nor drops the limit.
func TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream(t *testing.T) {
	for _, drift := range []struct {
		name   string
		change func(*natsgo.StreamConfig)
	}{
		{"a changed subject list", func(c *natsgo.StreamConfig) { c.Subjects = c.Subjects[:len(c.Subjects)-1] }},
		{"a one-minute age", func(c *natsgo.StreamConfig) { c.MaxAge, c.Duplicates = time.Minute, time.Minute }},
		{"no duplicate window", func(c *natsgo.StreamConfig) { c.Duplicates = 0 }},
	} {
		for _, settings := range []struct {
			name      string
			overrides map[string]string
		}{
			{"the deployment's settings", map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)}},
			{"the settings unset", nil},
		} {
			t.Run(drift.name+"/"+settings.name, func(t *testing.T) {
				cluster := testnats.StartCluster(t, 3)
				js := cluster.JetStream(t)
				deployed := bustest.DeployedThreeCopyStream(drift.change)
				if _, err := js.AddStream(&deployed); err != nil {
					t.Fatalf("deploy the stream: %v", err)
				}
				before := testnats.AwaitSettledStream(t, js, bus.Stream).Config

				if _, err := startDispatchStream(t, cluster.URLs(), settings.overrides); err != nil {
					t.Fatalf("start Dispatch's stream: %v", err)
				}

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
	writes := testnats.WatchStreamWrites(t, url)

	if err := startDispatchNATS(t, []string{url}, nil); err != nil {
		t.Fatalf("first start: %v", err)
	}
	streams := []string{bus.Stream, "KV_" + bus.WebhookRedeliveryBucket}
	var created []string
	for _, stream := range streams {
		created = append(created, "$JS.API.STREAM.CREATE."+stream)
	}
	if got := writes.Take(t); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(created))) {
		t.Fatalf("the first start wrote %v, want a create of each of %v", got, streams)
	}
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	for _, stream := range streams {
		info, err := js.StreamInfo(stream)
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		if info.Config.Replicas != 1 {
			t.Errorf("%s replicas = %d, want 1", stream, info.Config.Replicas)
		}
	}

	if err := startDispatchNATS(t, []string{url}, nil); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := writes.Take(t); len(got) != 0 {
		t.Fatalf("the second start wrote %v, want nothing", got)
	}
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
	if _, err := startDispatchStream(t, cluster.URLs(), map[string]string{"ENVOY_NATS_REPLICAS": "3", "ENVOY_STREAM_MAX_BYTES": strconv.Itoa(bustest.StreamMaxBytes)}); err != nil {
		t.Fatalf("start Dispatch's stream: %v", err)
	}
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
