// Package testnats starts a JetStream container only after its API is ready for daemon tests.
package testnats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// readinessTimeout bounds the wait for the JetStream API once the container runs. The container
// start is not bounded by it: how long Docker takes to create a container is the daemon's load.
const readinessTimeout = 30 * time.Second

// JetStream starts a disposable NATS container and returns only after the JetStream API answers.
func JetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	container, err := start(t)
	if err != nil {
		t.Fatalf("start NATS JetStream: %v", err)
	}

	url, err := container.ConnectionString(t.Context())
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	t.Cleanup(cancel)

	for {
		conn, js, err := ready(ctx, url)
		if err == nil {
			t.Cleanup(conn.Close)
			return js
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for NATS JetStream readiness: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func ready(ctx context.Context, url string) (*nats.Conn, jetstream.JetStream, error) {
	conn, err := nats.Connect(url, nats.Timeout(time.Second))
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(conn)
	if err == nil {
		_, err = js.AccountInfo(ctx)
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, js, nil
}

// start runs a NATS container for t, and removes it when t ends. The removal is registered before
// the start's error is looked at: a container Docker created and never saw ready — a readiness wait
// that timed out under load — comes back beside the error, and would otherwise outlive the test.
func start(t *testing.T, options ...testcontainers.ContainerCustomizer) (*tcnats.NATSContainer, error) {
	t.Helper()
	container, err := tcnats.Run(t.Context(), "nats:2.10", options...)
	testcontainers.CleanupContainer(t, container)
	return container, err
}
