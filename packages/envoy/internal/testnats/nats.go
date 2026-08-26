// Package testnats provides bounded NATS readiness helpers for integration tests.
package testnats

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

const (
	connectTimeout = 30 * time.Second
	retryInterval  = 100 * time.Millisecond
	dialTimeout    = time.Second
)

// Connect waits for a test NATS server to accept a real connection before
// returning it. Testcontainers can report a mapped port before NATS accepts
// its protocol handshake, so a one-shot dial is not a readiness check.
func Connect(t testing.TB, uri string) *natsgo.Conn {
	t.Helper()

	deadline := time.Now().Add(connectTimeout)
	var conn *natsgo.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = natsgo.Connect(uri, natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
		if err == nil {
			return conn
		}
		time.Sleep(retryInterval)
	}
	t.Fatalf("failed to connect to NATS within %s: %v", connectTimeout, err)
	return nil
}
