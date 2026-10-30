package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The bucket is created with the configured replicas, file storage, listenerStateMaxAge and one
// value per key.
func TestListenerStateBucketIsCreatedAtTheConfiguredReplicas(t *testing.T) {
	t.Run("one replica", func(t *testing.T) {
		kv := leaseBucket(t)
		status, err := kv.Status()
		if err != nil {
			t.Fatalf("read the bucket's status: %v", err)
		}
		bucket, ok := status.(*natsgo.KeyValueBucketStatus)
		if !ok {
			t.Fatalf("status is %T", status)
		}
		config := bucket.StreamInfo().Config
		if bucket.Bucket() != "envoy_listener" || bus.ListenerStateBucket != "envoy_listener" {
			t.Fatalf("bucket %q, constant %q, want envoy_listener", bucket.Bucket(), bus.ListenerStateBucket)
		}
		if config.Replicas != 1 || config.Storage != natsgo.FileStorage || config.MaxAge != listenerStateMaxAge || bucket.History() != 1 {
			t.Fatalf("replicas %d, storage %v, max age %s, history %d; want 1, file, %s, 1",
				config.Replicas, config.Storage, config.MaxAge, bucket.History(), listenerStateMaxAge)
		}
	})
	// A single test server cannot hold three replicas, and refuses the create for exactly that,
	// which shows the configured count is what the create asks for.
	t.Run("three replicas reach the server", func(t *testing.T) {
		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		_, err := openListenerState(conn, 3, listenerStateRequestWait)
		if err == nil || !strings.Contains(err.Error(), "replicas > 1 not supported") {
			t.Fatalf("open at 3 replicas on one server: %v, want the server's refusal of replicas > 1", err)
		}
		if errors.Is(err, errListenerStateRefused) {
			t.Fatalf("a refused replica count reads as a missing grant: %v", err)
		}
	})
}

// The bucket keeps a session's `seen.` record for at least as long as every pod answers that
// session's last_seen for, so the record every pod reads outlives the answer: the session
// registry reports a lapsed session for the session TTL and the retention main gives it
// (store.InterestReapAllowance of the reaper's grace) past the last registration write
// (session.SessionRegistry.LastSeen). Every pod reads that record alone and keeps no answer of
// its own, so a shorter bucket age would end the answer for the whole fleet while the registry
// still reports the session, and /v1/interests/ would list rows the reaper still holds with no
// last heartbeat.
func TestTheListenerStateBucketOutlivesTheLastSeenWindow(t *testing.T) {
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	sessions, err := session.OpenSessionRegistry(conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	window := sessions.TTL() + store.InterestReapAllowance(store.InterestReapGrace)
	if listenerStateMaxAge < window {
		t.Fatalf("the listener state bucket keeps a record for %s, want at least the %s a lapsed session's last_seen is answered for", listenerStateMaxAge, window)
	}
}

// A bucket an earlier build created with a shorter MaxAge is raised to listenerStateMaxAge when
// a listener opens it: a create that finds the bucket there changes nothing of it, so without the
// raise a deployment that ran an earlier build would drop every `seen.` record inside the window
// its pods answer last_seen for.
func TestOpeningTheListenerStateBucketRaisesAnEarlierBuildsMaxAge(t *testing.T) {
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      10 * time.Minute,
		Storage:  natsgo.FileStorage,
		Replicas: 1,
	}); err != nil {
		t.Fatalf("create the bucket as an earlier build did: %v", err)
	}
	if _, err := openListenerState(conn, 1, listenerStateRequestWait); err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	if info.Config.MaxAge != listenerStateMaxAge {
		t.Fatalf("the bucket keeps a record for %s after the open, want %s", info.Config.MaxAge, listenerStateMaxAge)
	}
}

// A user whose grant lacks the bucket is refused with errListenerStateRefused, naming the bucket
// and the grant; once the grant has it the same connection opens the bucket and takes the lease;
// a connection error is not the refusal.
func TestListenerStateBucketRefusalIsTheMissingGrant(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, "_INBOX.>", "$JS.API.INFO")
	// The server's violation is expected; the handler keeps nats.go from printing it.
	conn, err := testnats.ConnectSeed(t, grant.URL, seed, natsgo.ErrorHandler(func(*natsgo.Conn, *natsgo.Subscription, error) {}))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	_, err = openListenerState(conn, 1, 500*time.Millisecond)
	if !errors.Is(err, errListenerStateRefused) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("open without the grant: %v, want errListenerStateRefused wrapping the server's violation", err)
	}
	for _, want := range []string{"envoy_listener", "grant", `Publish to "$JS.API.STREAM.INFO.KV_envoy_listener"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}

	grant.Grant(t, "_INBOX.>", "$JS.API.>", "$KV.envoy_listener.>")
	kv, err := openListenerState(conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open with the grant: %v", err)
	}
	l := testLease(kv, "pod-a", productionLeaseConfig(), time.Now)
	l.step()
	if !l.Holding() {
		t.Fatal("the lease was not taken under the grant")
	}

	conn.Close()
	if _, err := openListenerState(conn, 1, 500*time.Millisecond); err == nil || errors.Is(err, errListenerStateRefused) {
		t.Fatalf("open on a closed connection: %v, want a connection error that is not the refusal", err)
	}
}

// Only a violation of a subject of the listener state bucket, as the open's own error or as a
// last error that changed across the open, is the refusal.
func TestListenerStateBucketRefusalClassification(t *testing.T) {
	violation := func(subject string) error {
		return fmt.Errorf("%w: Permissions Violation for Publish to %q", natsgo.ErrPermissionViolation, subject)
	}
	stale := violation("$JS.API.STREAM.INFO.KV_envoy_listener")
	for _, tc := range []struct {
		name               string
		err, before, after error
		refused            bool
	}{
		{name: "stream info refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "stream create refused", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.CREATE.KV_envoy_listener"), refused: true},
		{name: "direct get refused", err: natsgo.ErrTimeout, after: violation("$JS.API.DIRECT.GET.KV_envoy_listener.$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "key write refused", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listener.lease.listener-box-a"), refused: true},
		{name: "the open's own error", err: violation("$JS.API.STREAM.INFO.KV_envoy_listener"), refused: true},
		{name: "another bucket", err: natsgo.ErrTimeout, after: violation("$JS.API.STREAM.INFO.KV_envoy_listener_other")},
		{name: "another bucket's keys", err: natsgo.ErrTimeout, after: violation("$KV.envoy_listeners.x")},
		{name: "a subscription refusal", err: natsgo.ErrTimeout, after: fmt.Errorf("%w: Permissions Violation for Subscription to %q", natsgo.ErrPermissionViolation, "$KV.envoy_listener.>")},
		{name: "unchanged last error", err: natsgo.ErrTimeout, before: stale, after: stale},
		{name: "a timeout alone", err: natsgo.ErrTimeout},
		{name: "not a violation", err: natsgo.ErrTimeout, after: errors.New(`Permissions Violation for Publish to "$KV.envoy_listener.x"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listenerStateRefusal(tc.err, tc.before, tc.after) != nil; got != tc.refused {
				t.Fatalf("refused = %v, want %v", got, tc.refused)
			}
		})
	}
}
