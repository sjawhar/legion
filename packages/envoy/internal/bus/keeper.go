package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// defaultKeepInterval is the keeper's period (keep): how long, at most, a stream or bucket a
// concurrent start or an operator lowered stays below the deployment's settings.
const defaultKeepInterval = 30 * time.Second

// withKeepInterval sets the keeper's period in place of defaultKeepInterval, so a test sees a
// period pass without waiting one.
func withKeepInterval(interval time.Duration) ConnectOption {
	return clientOption(func(o *connectOpts) { o.keepInterval = interval })
}

// startKeeper starts the keeper of a client whose start reconciled the stream with cfg
// (ensureStreamWithConfig), which keeps buckets (WithKeptBuckets) at cfg's copies too: the
// constructor's start once install has returned, and the first connection's on a client that
// connects in the background (ensureStreamOnce), which a Close or Drain can stop first. A client
// already stopped starts none. The client's stop ends it (Close and Drain wait for it:
// awaitKeeper). The check and keeperDone's write are one step under mu, and Close and Drain stop
// the client before awaitKeeper reads keeperDone under mu, so a keeper either sees the stop or is
// waited for.
func (c *Client) startKeeper(cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped() {
		return
	}
	c.keeperDone = make(chan struct{})
	go c.keep(c.keeperDone, cfg, buckets, interval)
}

// keep is the keeper: every interval it puts back what the deployment's settings and this binary
// ask of the stream (keepStream) and raises each of buckets, the program's own (WithKeptBuckets),
// back to the deployment's copies (raiseReplicas), until the client stops. A start's reconcile can
// lose to a concurrent start with other settings or subjects, since JetStream's stream update has
// no compare-and-swap (ensureStreamWithConfig), and an operator can lower either by hand; the
// keeper ends both within one period. A pass waits for a live connection: a pass while the client
// is not connected is skipped, and none starts once the client has stopped, though a tick and the
// stop came together. A pass that fails is logged and the next period tries again: the keeper never
// stops the client or the process. A failure once the client has stopped is the stop's, and is not
// logged.
func (c *Client) keep(done chan struct{}, cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}
		if c.stopped() {
			return
		}
		if c.Connected() {
			c.keepPass(&cfg, buckets)
		}
	}
}

// keepPass is one pass of the keeper, through the client's current JetStream context. A bucket of
// buckets that does not exist yet, one the program opens only for a feature it has not configured,
// is skipped without a line: the program creates it at the deployment's copies when it opens it.
func (c *Client) keepPass(cfg *nats.StreamConfig, buckets []string) {
	js := c.JS()
	restored, err := keepStream(js, cfg)
	switch {
	case err != nil && c.stopped():
		return
	case err != nil:
		slog.Warn("envoy nats keeper could not restore the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("error", err.Error()))
	case len(restored) > 0:
		slog.Warn("envoy nats keeper restored the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("restored", strings.Join(restored, ", ")))
	}
	for _, bucket := range buckets {
		if c.stopped() {
			return
		}
		from, err := raiseReplicas(js, bucket, cfg.Replicas)
		switch {
		case errors.Is(err, nats.ErrStreamNotFound):
		case err != nil && c.stopped():
			return
		case err != nil:
			slog.Warn("envoy nats keeper could not raise a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("error", err.Error()))
		case from > 0:
			slog.Warn("envoy nats keeper raised a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("replicas", fmt.Sprintf("%d -> %d", from, cfg.Replicas)))
		}
	}
}

// awaitKeeper waits for the keeper of a stopped client to end, so it never outlives the client.
// The caller has closed the connection, or is about to have nats.go close it, which ends a
// request a pass has in flight at once. A client that never started one, as a client connecting
// in the background that stopped before its first reconcile, has none to wait for.
func (c *Client) awaitKeeper() {
	c.mu.Lock()
	done := c.keeperDone
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// keepStream puts back what cfg, this binary's stream with the deployment's settings, asks of the
// stream and a concurrent start or an operator took away. It writes keptStream: the stream as it
// read it, with copies below cfg's count raised to it, a byte limit below cfg's, or none, raised to
// cfg's when cfg sets one, a missing StreamIDMetadataKey added, the age, the duplicate window and
// discard set to cfg's (the same in every release, ownedStream), and each of cfg's subjects the
// stream lacks added unless it overlaps a deployed subject (keptSubjects). It does not check cfg's
// subjects for one that captures a role lane, because no subject this binary compiles captures
// one, which TestStreamConfigExcludesRoleLanes in stream_config_test.go guarantees. It never
// lowers the copies or the byte limit, so an operator's raise of either stands, and never
// removes or replaces a subject: only a start does, since two releases with different subject lists
// would otherwise switch the stream back and forth every period. Its write still carries every
// other field as it read it, the other subjects included, so a concurrent write that lands between
// its read and its write is lost again; the keeper of the program whose write it was puts its part
// back a period later. One part never comes back that way: the keeper skips a subject this
// binary's start put in place of an overlapping one while that overlapping subject is back
// (keptSubjects), so a stale write that put the replaced subject back stands until this binary's
// next start. It returns each field it restored, old and new value, none when the stream
// already held them, in which case it sends nothing. A stream whose subjects capture the role lanes
// it leaves to a start, sending nothing and returning an error when it needs anything: only a start
// moves the role lanes off the stream, their consumers' filters first (migrateRoleLanesOffStream),
// and a write of the list the keeper read could put them back after a start removed them. A stream
// stamped with a generation higher than streamConfigGeneration belongs to a newer build and is
// left exactly as it is, as a start leaves it (ensureStreamWithConfig), so the two builds a
// release runs at once never put their own settings back in turn. An update whose answer never
// came succeeds when the stream, read again, needs nothing more, and one the server refused as
// overlapping the stream itself is sent again until the stream needs nothing more or its sends
// run out (updateStream).
func keepStream(js nats.JetStreamContext, cfg *nats.StreamConfig) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return nil, err
	}
	if MetadataGeneration(info.Config.Metadata, streamGenerationKey) > streamConfigGeneration {
		return nil, nil
	}
	kept := keptStream(info, cfg)
	if reflect.DeepEqual(kept, info.Config) {
		return nil, nil
	}
	if roleLanes := slices.DeleteFunc(slices.Clone(info.Config.Subjects), func(subject string) bool { return !subjectCapturesRoleLanes(subject) }); len(roleLanes) > 0 {
		return nil, fmt.Errorf("the stream carries subjects that capture the role lanes, which only a start moves off it: %s", strings.Join(roleLanes, ", "))
	}
	if err := updateStream(js, &kept, func(again *nats.StreamInfo) bool {
		return reflect.DeepEqual(keptStream(again, cfg), again.Config)
	}); err != nil {
		return nil, refusedSettings(err, &info.Config, &kept)
	}
	return restoredFields(&info.Config, &kept), nil
}

// restoredFields names each field kept differs in from deployed, with its old and new value:
// `replicas 1 -> 3`, `max_bytes -1 -> 2000000` or `max_bytes 1000000 -> 2000000`,
// `max_age 73h0m0s -> 72h0m0s`, `duplicate_window 1m0s -> 72h0m0s`,
// `discard DiscardOld -> DiscardNew`, `envoy_stream_id added as <id>`,
// `subjects added notifications.x.>`. The copies and the byte limit only ever rise (ownedStream).
// kept's subjects are deployed's with the keeper's additions after them (keptSubjects).
func restoredFields(deployed, kept *nats.StreamConfig) []string {
	var restored []string
	if kept.Replicas != deployed.Replicas {
		restored = append(restored, fmt.Sprintf("replicas %d -> %d", deployed.Replicas, kept.Replicas))
	}
	if kept.MaxBytes != deployed.MaxBytes {
		restored = append(restored, fmt.Sprintf("max_bytes %d -> %d", deployed.MaxBytes, kept.MaxBytes))
	}
	if kept.MaxAge != deployed.MaxAge {
		restored = append(restored, fmt.Sprintf("max_age %s -> %s", deployed.MaxAge, kept.MaxAge))
	}
	if kept.Duplicates != deployed.Duplicates {
		restored = append(restored, fmt.Sprintf("duplicate_window %s -> %s", deployed.Duplicates, kept.Duplicates))
	}
	if kept.Discard != deployed.Discard {
		restored = append(restored, fmt.Sprintf("discard %s -> %s", deployed.Discard, kept.Discard))
	}
	if id := kept.Metadata[StreamIDMetadataKey]; id != deployed.Metadata[StreamIDMetadataKey] {
		restored = append(restored, fmt.Sprintf("%s added as %s", StreamIDMetadataKey, id))
	}
	if added := kept.Subjects[len(deployed.Subjects):]; len(added) > 0 {
		restored = append(restored, "subjects added "+strings.Join(added, " "))
	}
	return restored
}
