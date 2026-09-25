package session

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// openWatchedRegistry opens a session registry whose KV watcher has finished its initial scan.
func openWatchedRegistry(t *testing.T, uri string) *SessionRegistry {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	registry, err := OpenSessionRegistry(conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	return registry
}

// StopWatch runs in the listener's shutdown before the NATS drain and outside its deadline, so it
// makes no request of the server: with the server unresponsive it still returns at once.
func TestStopWatchMakesNoRequestOfTheServer(t *testing.T) {
	ctr, uri := testnats.Start(t)
	registry := openWatchedRegistry(t, uri)

	id := ctr.GetContainerID()
	if out, err := exec.Command("docker", "pause", id).CombinedOutput(); err != nil {
		t.Fatalf("pause NATS: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "unpause", id).Run() })

	started := time.Now()
	registry.StopWatch()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("StopWatch took %s against an unresponsive server, want it to return at once", elapsed)
	}
}

// StopWatch is final: a Rewatch after it, from a recovery's reconnect hook or a self-health rebuild
// still running at shutdown, arms no watcher, so the drain that follows ends nothing the registry
// still expects and logs no error.
func TestStopWatchIsFinal(t *testing.T) {
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	registry, err := OpenSessionRegistry(conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}

	var logs bytes.Buffer
	var logsMu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logs, &logsMu}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	registry.StopWatch()
	if err := registry.Rewatch(conn); err != nil {
		t.Fatalf("Rewatch after StopWatch: %v", err)
	}
	if err := conn.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !conn.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	if err := registry.WatchErr(); err != nil {
		t.Fatalf("the registry reports a failed watcher after StopWatch: %v", err)
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a watcher armed after StopWatch logged an error when the drain ended it:\n%s", logs.String())
	}
}

type lockedWriter struct {
	buffer *bytes.Buffer
	mu     *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}
