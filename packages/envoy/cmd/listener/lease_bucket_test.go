package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The bucket is created with the configured replicas, file storage, a 10 minute MaxAge and one
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
		if config.Replicas != 1 || config.Storage != natsgo.FileStorage || config.MaxAge != 10*time.Minute || bucket.History() != 1 {
			t.Fatalf("replicas %d, storage %v, max age %s, history %d; want 1, file, 10m, 1",
				config.Replicas, config.Storage, config.MaxAge, bucket.History())
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
