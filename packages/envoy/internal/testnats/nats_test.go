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
	"github.com/testcontainers/testcontainers-go"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// This protects the testcontainers boundary: a server can accept NATS's
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
// messages left by one test are not there when the next asks for the server, which hands it an
// account of its own.
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
			t.Errorf("the second subtest's account holds %d streams, want none of the first's", info.Streams)
		}
	})
}

// A test that publishes a burst without waiting for acks leaves messages the server routes after
// the test ends. They land in that test's own account, so the next test, creating a stream on the
// same subjects, finds none of them.
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

// A test gets one account however often it asks, so every connection it makes reaches the streams
// any other made; a subtest gets an account of its own, where none of its parent's streams is.
func TestATestGetsOneAccountAndEachSubtestItsOwn(t *testing.T) {
	uri := URL(t)
	if again := URL(t); again != uri {
		t.Fatalf("a second URL for the same test is %q, want its first, %q", again, uri)
	}
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "PARENT", Subjects: []string{"parent.>"}}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	t.Run("subtest", func(t *testing.T) {
		subURI := URL(t)
		if subURI == uri {
			t.Fatalf("the subtest got its parent's account, %q", uri)
		}
		sub := Connect(t, subURI)
		defer sub.Close()
		subJS, err := sub.JetStream()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := subJS.StreamInfo("PARENT"); !errors.Is(err, natsgo.ErrStreamNotFound) {
			t.Fatalf("the subtest's account answers its parent's stream with %v, want not found", err)
		}
	})
}

// namedTB is a test under another name, so one test can ask a server for accounts as many tests.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// ownAccountServer starts an account server of the test's own, declaring accounts accounts.
func ownAccountServer(t *testing.T, accounts int) *accountServer {
	t.Helper()
	server, err := startAccountServer(accounts)
	if server != nil {
		testcontainers.CleanupContainer(t, server.ctr)
	}
	if err != nil {
		t.Fatalf("start an account server: %v", err)
	}
	return server
}

// A server whose accounts are all handed out declares twice as many by a reload and hands out the
// next, and an account a test is still using keeps its connection and its watcher across the
// reload.
func TestAServerOutOfAccountsDoublesThemWithoutDisturbingOneInUse(t *testing.T) {
	server := ownAccountServer(t, 2)
	live := Connect(t, server.url(namedTB{t, "live"}))
	defer live.Close()
	js, err := live.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "live"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = watcher.Stop() }()
	if entry := <-watcher.Updates(); entry != nil {
		t.Fatalf("the empty bucket's scan delivered %v before its marker", entry)
	}

	server.url(namedTB{t, "second"})
	third := server.url(namedTB{t, "third"})
	if server.declared != 4 {
		t.Fatalf("the server declares %d accounts after handing out its third, want 4", server.declared)
	}
	if want := fmt.Sprintf("nats://t3:t3@%s", server.host); third != want {
		t.Fatalf("the third test's URL is %q, want %q", third, want)
	}
	if _, err := kv.Put("after-the-reload", []byte("1")); err != nil {
		t.Fatalf("put after the reload: %v", err)
	}
	select {
	case entry := <-watcher.Updates():
		if entry == nil || entry.Key() != "after-the-reload" {
			t.Fatalf("the watcher delivered %v after the reload, want the put", entry)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher of an account in use delivered nothing after the reload")
	}
	if status := live.Status(); status != natsgo.CONNECTED {
		t.Fatalf("the connection of an account in use is %v after the reload, want CONNECTED", status)
	}
}

// StartNkeyAuthorized's readiness wait refuses a server that admits a client with no credential:
// such a server enforces no nkey users, and every refusal a test expects of it would pass for the
// wrong reason. A server started with no configuration, which asks for no credential, stands in
// for one.
func TestTheNkeyReadinessWaitRefusesAServerThatAdmitsAnyone(t *testing.T) {
	_, uri := Start(t)
	wait := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		answering(wait, uri)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the readiness wait is still waiting on a server that admits anyone")
	}
	if want := "admitted a client with no credential"; !strings.Contains(wait.message, want) {
		t.Errorf("the readiness wait ended with %q, want it to say %q", wait.message, want)
	}
}

// A restart keeps its server's URL even when something else takes the URL's port while the server is
// stopped, as another test's container or a free-port pick can on a busy host.
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

// A second Grant on one server judges the outcome of its own reload, not the first Grant's, which
// the server's log still holds: a second grant the server refuses fails the test with the server's
// reason instead of passing on the first reload's line.
func TestASecondGrantWaitsForItsOwnReload(t *testing.T) {
	_, public := User(t)
	grant := StartNkeyGranted(t, public, "first.>")
	grant.Grant(t, "second.>")

	refused := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		grant.Grant(refused, "not..a.subject")
	}()
	<-done
	if !strings.Contains(refused.message, "NATS refused the new grant") ||
		!strings.Contains(refused.message, `subject "not..a.subject" is not a valid subject`) {
		t.Fatalf("a second Grant the server refused ended with %q, want the server's refusal", refused.message)
	}
}
