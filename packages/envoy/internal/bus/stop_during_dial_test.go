package bus_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// A recovery that is dialling when Drain stops the client neither installs the connection its dial
// produces nor reports the stop as a failure: the connection would be one nothing drains, and the
// stop is a shutdown. client.Conn.Close() stands in for nats.go closing the connection itself (the
// same authorization error twice, or an unclassified server -ERR), which is what starts that
// recovery in production.
func TestARecoveryStoppedMidDialInstallsNothingAndLogsNoError(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	var logs bytes.Buffer
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	proxy.delay.Store(int64(time.Second))
	client.Conn.Close()
	select {
	case <-proxy.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the recovery never dialled")
	}
	_ = client.Drain(5 * time.Second)
	time.Sleep(2 * time.Second)

	if client.Connected() {
		t.Fatal("the recovery installed the connection it dialled after Drain stopped the client")
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Drain logged an error: %s", line)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A recovery whose dial is failing when Close stops the client ends at the stop: it logs the
// cancellation and never a reconnect failure. Its dial retries a lost server for up to ten
// attempts, so without that the recovery would outlive the client by seconds and then report a
// shutdown as an ERROR. client.Conn.Close() stands in for nats.go closing the connection itself,
// as in the test above.
func TestARecoveryDiallingALostServerEndsAtTheStop(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	var logs bytes.Buffer
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	logged := func(fragment string) bool {
		logsMu.Lock()
		defer logsMu.Unlock()
		return strings.Contains(logs.String(), fragment)
	}

	proxy.refuse.Store(true)
	proxy.delay.Store(int64(500 * time.Millisecond))
	client.Conn.Close()
	select {
	case <-proxy.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the recovery never dialled")
	}
	client.Close()

	deadline := time.Now().Add(3 * time.Second)
	for !logged("envoy nats recovery cancelled") {
		if time.Now().After(deadline) {
			t.Fatal("the recovery was still dialling 3s after Close stopped the client")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	logsMu.Lock()
	defer logsMu.Unlock()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Fatalf("a recovery stopped by Close logged an error: %s", line)
		}
	}
}
