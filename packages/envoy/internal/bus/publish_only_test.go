package bus_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// deployedSubjects is the subject list a deployment of the stream carries here: one subject no
// binary of this checkout compiles, so a caller that reconciled the stream is seen doing it
// whatever this binary's own list holds.
var deployedSubjects = []string{"notifications.legion.>", "notifications.publishonly.>"}

// deployStream creates ENVOY_NOTIFICATIONS as a deployment left it: the subjects above, and
// retention and duplicate windows no binary of this checkout would install.
func deployStream(t *testing.T, uri string) nats.JetStreamContext {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	_, err = js.AddStream(&nats.StreamConfig{
		Name:       bus.Stream,
		Subjects:   slices.Clone(deployedSubjects),
		Retention:  nats.LimitsPolicy,
		MaxAge:     72 * time.Hour,
		Duplicates: 72 * time.Hour,
		Storage:    nats.FileStorage,
		Replicas:   1,
	})
	if err != nil {
		t.Fatalf("deploy the stream: %v", err)
	}
	return js
}

func streamConfig(t *testing.T, js nats.JetStreamContext) nats.StreamConfig {
	t.Helper()
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	return info.Config
}

// A caller that only publishes or only tails - natstail, the MCP bridge, envoy-dispatch's
// operator commands - owns nothing on the bus it reaches, so its connect leaves the shared
// stream's configuration exactly as the deployment that owns it left it (LEGION-249).
func TestConnectLeavesADeployedStreamAlone(t *testing.T) {
	uri := testnats.URL(t)
	js := deployStream(t, uri)
	before := streamConfig(t, js)

	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	after := streamConfig(t, js)
	if !slices.Equal(after.Subjects, before.Subjects) {
		t.Fatalf("stream subjects = %v, want the deployed %v", after.Subjects, before.Subjects)
	}
	if after.MaxAge != before.MaxAge || after.Duplicates != before.Duplicates {
		t.Fatalf("stream retention = (max age %s, duplicates %s), want the deployed (%s, %s)", after.MaxAge, after.Duplicates, before.MaxAge, before.Duplicates)
	}
}

// The same caller against a server with no stream creates none: a scratch NATS is not a tool's to
// lay out, and a stream it created would stand in for the one the owner configures.
func TestConnectCreatesNoStream(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	if _, err := client.JS().StreamInfo(bus.Stream); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("stream info after a publish-only connect = %v, want %v", err, nats.ErrStreamNotFound)
	}
}
