// Package testnats starts a JetStream container only after its API is ready for daemon tests.
package testnats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

const readinessTimeout = 30 * time.Second

// JetStream starts a disposable NATS container and returns only after the JetStream API answers.
func JetStream(t *testing.T) jetstream.JetStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), readinessTimeout)
	t.Cleanup(cancel)

	container, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("start NATS JetStream: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate NATS JetStream: %v", err)
		}
	})

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}

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
