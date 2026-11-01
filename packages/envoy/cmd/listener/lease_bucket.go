package main

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// The listener state bucket (bus.ListenerStateBucket) keeps one value per key and drops a key
// nobody has written for listenerStateMaxAge, so a lease record nobody renews leaves the bucket
// after 20 minutes. That age is what every pod answers a lapsed session's last_seen for: the
// session's `seen.` record is the one answer they share (session.SessionRegistry.LastSeen), and
// the registry reports it for the session TTL and the interest reaper's allowance past its write
// (store.InterestReapAllowance, the retention main sets), 20 minutes with the default 5-minute
// TTL. The record must outlive that window, because every pod reads the record alone and keeps
// no answer of its own: once the bucket drops it, the whole fleet answers nothing for a session
// the registry still reports. A record nobody renews living twice as long changes
// nothing else: a lease record is taken over once it has stood unchanged for the lease's ttl,
// whatever its age, and the delivery ledger judges every record by its own ages (dedupe.KV).
// listenerStateRequestWait is the JetStream wait of every request on the bucket's handle: a
// renewal that hangs must give up well inside the holder's validity (leaseTTL - leaseMargin) so
// its retry still lands in it. A bucket create the wait cuts short is a connection error the
// caller retries, and the retry finds the bucket the server made.
const (
	listenerStateMaxAge      = 20 * time.Minute
	listenerStateRequestWait = 2 * time.Second
)

// errListenerStateRefused marks a listener whose NATS user may not use the listener state
// bucket: no retry opens it until the grant changes. It wraps the server's own violation.
var errListenerStateRefused = errors.New("NATS refused KV bucket " + bus.ListenerStateBucket +
	": the listener's NATS user lacks the grant for KV bucket " + bus.ListenerStateBucket +
	" (" + listenerGrant + ")")

// openListenerState opens the listener state bucket, creating it at replicas with file storage,
// listenerStateMaxAge and one value per key when it does not exist, with wait as the JetStream
// wait of every request on the handle it returns (listenerStateRequestWait in production). A
// bucket an earlier build created keeps its keys for a shorter age, so the open raises it to
// listenerStateMaxAge (bus.RaiseMaxAge): a `seen.` record must outlive the window every pod
// answers a lapsed session's last_seen for, and a create that finds the bucket there changes
// nothing of it.
//
// A refusal under the connection's grant is errListenerStateRefused, which errors.Is tells apart
// from a connection error. nats-server drops a publish the grant refuses and answers it only with
// `-ERR 'Permissions Violation for Publish to "<subject>"'`, which nats.go records as the
// connection's last error and reports to no request, so the refused request itself runs out at
// wait. So a failed open is a refusal when its own error is such a violation, or when the
// connection's last error changed across the open to a violation of a subject of this bucket
// (its stream KV_envoy_listener, or its keys $KV.envoy_listener.>). nats.go keeps one last error
// per connection, so a violation another goroutine causes in between hides this one, and the
// open reads as a connection error its caller retries.
func openListenerState(conn *nats.Conn, replicas int, wait time.Duration) (bus.KeyValue, error) {
	before := conn.LastError()
	js, err := conn.JetStream(nats.MaxWait(wait))
	if err != nil {
		return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.ListenerStateBucket,
		History:  1,
		TTL:      listenerStateMaxAge,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
	if err == nil {
		err = bus.RaiseMaxAge(js, bus.ListenerStateBucket, listenerStateMaxAge)
	}
	if err == nil {
		return kv, nil
	}
	if violation := listenerStateRefusal(err, before, conn.LastError()); violation != nil {
		return bus.KeyValue{}, fmt.Errorf("%w: %w", errListenerStateRefused, violation)
	}
	return bus.KeyValue{}, fmt.Errorf("open %s KV bucket: %w", bus.ListenerStateBucket, err)
}

// publishViolation captures the subject of the server's refusal of a publish.
var publishViolation = regexp.MustCompile(`Publish to "([^"]*)"`)

// listenerStateRefusal returns the permissions violation that refused a failed open of the
// listener state bucket, or nil when the open failed for another reason (bucketRefusal over that
// one bucket).
func listenerStateRefusal(err, before, after error) error {
	_, violation := bucketRefusal([]string{bus.ListenerStateBucket}, err, before, after)
	return violation
}

// sameLastError reports whether a and b are the same error value. nats.go records each
// violation as a new error, so a new refusal never equals the one it replaced. A value that
// cannot be compared counts as different rather than panicking.
func sameLastError(a, b error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
