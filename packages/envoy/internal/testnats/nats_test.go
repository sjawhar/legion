package testnats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// This protects the testcontainers boundary that failed on #1243: a server can accept NATS's
// protocol handshake while JetStream is still starting, so a KV test that returns at the handshake
// races CreateKeyValue. The helper must retry the JetStream operation itself.
func TestWaitForJetStreamRetriesTheOperationUntilItIsReady(t *testing.T) {
	attempts := 0
	waitForJetStream(t, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("JetStream is starting")
		}
		return nil
	})
	if attempts != 3 {
		t.Fatalf("JetStream readiness attempts = %d, want 3", attempts)
	}
}

// The package's tests share one server, and each gets it as a fresh container was: a stream and its
// messages left by one test are gone when the next asks for the server.
func TestEachTestGetsTheSharedServerEmpty(t *testing.T) {
	t.Run("leaves a stream and a message", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: "LEFT_BEHIND", Subjects: []string{"left.>"}}); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		if _, err := js.Publish("left.one", []byte("x")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	})
	t.Run("finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		info, err := js.AccountInfo()
		if err != nil {
			t.Fatalf("account info: %v", err)
		}
		if info.Streams != 0 {
			t.Errorf("the shared server holds %d streams from the previous test", info.Streams)
		}
	})
}

// A test that publishes a burst without waiting for acks leaves messages the server routes after
// the test ends. The next test, recreating a stream on the same subjects, finds none of them: the
// reset waits for the previous test's connections to be gone first.
func TestABurstLeftInFlightDoesNotReachTheNextTest(t *testing.T) {
	const burst = 20000
	config := &natsgo.StreamConfig{Name: "BURST", Subjects: []string{"burst.>"}}
	t.Run("publishes a burst and ends", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		payload := []byte(strings.Repeat("x", 64))
		for i := range burst {
			if err := conn.Publish(fmt.Sprintf("burst.%d", i%16), payload); err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
		}
	})
	t.Run("recreates the stream and finds none of it", func(t *testing.T) {
		conn := Connect(t, URL(t))
		defer conn.Close()
		js, err := conn.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.AddStream(config); err != nil {
			t.Fatalf("add stream: %v", err)
		}
		time.Sleep(time.Second)
		info, err := js.StreamInfo(config.Name)
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		if info.State.Msgs != 0 {
			t.Errorf("the recreated stream holds %d of the previous test's %d messages", info.State.Msgs, burst)
		}
	})
}

// fatalRecorder is t with Fatal recorded and the goroutine ended, as t.Fatal does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) { f.message = fmt.Sprint(args...); runtime.Goexit() }
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// A test that takes the shared server twice is refused at once, naming itself, instead of waiting
// on itself for the whole test binary's timeout.
func TestASecondTakeByTheSameTestFailsAtOnce(t *testing.T) {
	URL(t)
	again := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		URL(again)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second take by the same test waited on itself")
	}
	if !strings.Contains(again.message, "already holds the shared NATS server") {
		t.Errorf("a second take by the same test failed with %q, not the re-entry refusal", again.message)
	}
}

// A restart keeps its server's URL even when something else takes the URL's port while the server is
// stopped, as another test's container or a free-port pick can on a busy host: #1387's CI lost the
// port between a stop and a start ("address already in use" on restart 3).
func TestARestartKeepsItsURLWhenThePortIsContendedWhileStopped(t *testing.T) {
	ctr, uri := StartRestartable(t)
	Stop(t, ctr)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", uri, err)
	}
	if taken, err := net.Listen("tcp", u.Host); err == nil {
		t.Cleanup(func() { _ = taken.Close() })
	}
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("start NATS again: %v", err)
	}
	Connect(t, uri).Close()
}
