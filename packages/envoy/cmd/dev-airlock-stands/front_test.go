// packages/envoy/cmd/dev-airlock-stands/front_test.go
// The front as the local stack drives it: bound and reporting its address before any backend
// exists, then sending each connection to the next of the backends a file names, which the
// stack writes once its two brokers have bound.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// answering is a TCP server that writes name to every connection and closes it, so a client
// reads which backend it reached.
func answering(t *testing.T, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, name+"\n")
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// reached is the name the backend behind the front answered one connection with.
func reached(t *testing.T, front string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", front, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the front at %s: %v", front, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the backend's answer through the front: %v", err)
	}
	return line[:len(line)-1]
}

// TestTheFrontRoundRobinsOverBackendsItLearnsAfterItBound: the stack binds the front first,
// names it in each broker's BROKER_PUBLIC_URL, and only then knows the addresses its brokers
// took - so a front that needed its backends at start would be of no use to it.
func TestTheFrontRoundRobinsOverBackendsItLearnsAfterItBound(t *testing.T) {
	backendsFile := filepath.Join(t.TempDir(), "backends")
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"front", "--listen", "127.0.0.1:0", "--backends-file", backendsFile}, writer, os.Stderr)
	}()

	var report frontReport
	if err := json.NewDecoder(reader).Decode(&report); err != nil {
		t.Fatalf("the front wrote no report: %v", err)
	}
	if report.Address == "" || report.URL != "http://"+report.Address {
		t.Fatalf("the front's report is %+v; want an address and that address as a URL", report)
	}
	// Drained, so the front is never blocked writing into the pipe.
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	first, second := answering(t, "first"), answering(t, "second")
	if err := os.WriteFile(backendsFile, []byte("# the two brokers\n"+first+"\n"+second+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Four connections over two backends: each is reached twice, in turn.
	var got []string
	for range 4 {
		got = append(got, reached(t, report.Address))
	}
	want := []string{"first", "second", "first", "second"}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("the front sent its four connections to %v; want %v, one after the other", got, want)
		}
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("the front exited %d on a cancelled context; want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the front did not stop when its context ended")
	}
}

// TestTheFrontRefusesAUsageError: no backend at all is a usage error, and so is an argument the
// front does not take, each exit 2 as every other mode of this command refuses one.
func TestTheFrontRefusesAUsageError(t *testing.T) {
	for name, args := range map[string][]string{
		"no backend":             {"front", "--listen", "127.0.0.1:0"},
		"a stray argument":       {"front", "--listen", "127.0.0.1:0", "--backend", "127.0.0.1:1", "what"},
		"a backend with no port": {"front", "--listen", "127.0.0.1:0", "--backend", "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			if code := run(t.Context(), args, io.Discard, io.Discard); code != exitUsageError {
				t.Fatalf("dev-airlock-stands %v exited %d; want %d", args, code, exitUsageError)
			}
		})
	}
}
