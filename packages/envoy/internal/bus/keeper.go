package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
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
	return func(o *connectOpts) { o.keepInterval = interval }
}

// startKeeper starts the keeper of a client whose start reconciled the stream with cfg
// (ensureStreamWithConfig), which keeps buckets (WithKeptBuckets) at cfg's copies too. The
// client's stop ends it (Close and Drain wait for it: awaitKeeper).
func (c *Client) startKeeper(cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	c.keeperDone = make(chan struct{})
	go c.keep(cfg, buckets, interval)
}

// keep is the keeper: every interval it puts back what the deployment's settings ask of the stream
// (keepStream) and raises each of buckets, the program's own (WithKeptBuckets), back to the
// deployment's copies (raiseReplicas), until the client stops. A start's reconcile can lose to a
// concurrent start with other settings, since JetStream's stream update has no compare-and-swap
// (ensureStreamWithConfig), and an operator can lower either by hand; the keeper ends both within
// one period. A pass waits for a live connection: a pass while the client is not connected is
// skipped. A pass that fails is logged and the next period tries again: the keeper never stops the
// client or the process.
func (c *Client) keep(cfg nats.StreamConfig, buckets []string, interval time.Duration) {
	defer close(c.keeperDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
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
	if err != nil {
		slog.Warn("envoy nats keeper could not restore the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("error", err.Error()))
	} else if len(restored) > 0 {
		slog.Warn("envoy nats keeper restored the stream", slog.String("name", c.name), slog.String("stream", Stream), slog.String("restored", strings.Join(restored, ", ")))
	}
	for _, bucket := range buckets {
		if c.stopped() {
			return
		}
		from, err := raiseReplicas(js, bucket, cfg.Replicas)
		switch {
		case errors.Is(err, nats.ErrStreamNotFound):
		case err != nil:
			slog.Warn("envoy nats keeper could not raise a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("error", err.Error()))
		case from > 0:
			slog.Warn("envoy nats keeper raised a bucket", slog.String("name", c.name), slog.String("bucket", bucket), slog.String("replicas", fmt.Sprintf("%d -> %d", from, cfg.Replicas)))
		}
	}
}

// awaitKeeper waits for the keeper of a stopped client to end, so it never outlives the client.
// The caller has closed the connection, or is about to have nats.go close it, which ends a
// request a pass has in flight at once.
func (c *Client) awaitKeeper() {
	if c.keeperDone != nil {
		<-c.keeperDone
	}
}

// keepStream puts back what cfg, the deployment's settings, asks of the stream and a concurrent
// start or an operator took away (keptStream): copies below cfg's count, a byte limit other than
// cfg's when it sets one, and a missing StreamIDMetadataKey. It returns each field it restored,
// old and new value, none when the stream already held them, in which case it sends nothing. It
// never touches the subjects, the age, the duplicate window or discard: only a start reconciles
// those, since two releases with different subject lists would otherwise switch the stream back
// and forth every period. An update whose answer never came succeeds when the stream, read again,
// holds it (unanswered), as at a start.
func keepStream(js nats.JetStreamContext, cfg *nats.StreamConfig) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return nil, err
	}
	kept := keptStream(info, cfg)
	if reflect.DeepEqual(kept, info.Config) {
		return nil, nil
	}
	if _, err := js.UpdateStream(&kept); err != nil && !(unanswered(err) && streamHolds(js, cfg, keptStream)) {
		return nil, refusedSettings(err, &info.Config, &kept)
	}
	return restoredFields(&info.Config, &kept), nil
}

// restoredFields names each field kept differs in from deployed, with its old and new value:
// `replicas 1 -> 3`, `max_bytes -1 -> 2000000`, `envoy_stream_id added as <id>`.
func restoredFields(deployed, kept *nats.StreamConfig) []string {
	var restored []string
	if kept.Replicas != deployed.Replicas {
		restored = append(restored, fmt.Sprintf("replicas %d -> %d", deployed.Replicas, kept.Replicas))
	}
	if kept.MaxBytes != deployed.MaxBytes {
		restored = append(restored, fmt.Sprintf("max_bytes %d -> %d", deployed.MaxBytes, kept.MaxBytes))
	}
	if id := kept.Metadata[StreamIDMetadataKey]; id != deployed.Metadata[StreamIDMetadataKey] {
		restored = append(restored, fmt.Sprintf("%s added as %s", StreamIDMetadataKey, id))
	}
	return restored
}
