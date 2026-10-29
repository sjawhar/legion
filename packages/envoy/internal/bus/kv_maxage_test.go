package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// RaiseKeyValueMaxAge raises a bucket that holds less than the caller wants, leaves one that
// already holds as much or more, and leaves one holding no MaxAge at all, which expires nothing:
// raising that would shorten it. Each answer is the MaxAge the bucket holds when the call returns.
func TestRaiseKeyValueMaxAge(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	for _, c := range []struct {
		name    string
		bucket  string
		created time.Duration
		want    time.Duration
		after   time.Duration
	}{
		{name: "raises a shorter window", bucket: "kv_maxage_short", created: 2 * time.Minute, want: 10 * time.Minute, after: 10 * time.Minute},
		{name: "leaves a longer window", bucket: "kv_maxage_long", created: 20 * time.Minute, want: 10 * time.Minute, after: 20 * time.Minute},
		{name: "leaves a window it already holds", bucket: "kv_maxage_equal", created: 10 * time.Minute, want: 10 * time.Minute, after: 10 * time.Minute},
		{name: "leaves a bucket that expires nothing", bucket: "kv_maxage_none", created: 0, want: 10 * time.Minute, after: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: c.bucket, TTL: c.created, Storage: nats.MemoryStorage}); err != nil {
				t.Fatalf("create bucket: %v", err)
			}
			t.Cleanup(func() { _ = js.DeleteKeyValue(c.bucket) })

			got, err := RaiseKeyValueMaxAge(js, c.bucket, c.want)
			if err != nil {
				t.Fatalf("raise: %v", err)
			}
			if got != c.after {
				t.Fatalf("RaiseKeyValueMaxAge answered %v, want %v", got, c.after)
			}
			info, err := js.StreamInfo(KeyValueStream(c.bucket))
			if err != nil {
				t.Fatalf("stream info: %v", err)
			}
			if info.Config.MaxAge != c.after {
				t.Fatalf("bucket holds MaxAge %v, want %v", info.Config.MaxAge, c.after)
			}
		})
	}
}

// A bucket that is not there has no MaxAge to answer, and the read's error is returned as it is.
func TestRaiseKeyValueMaxAgeReportsABucketThatIsNotThere(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	got, err := RaiseKeyValueMaxAge(js, "kv_maxage_missing", time.Minute)
	if err == nil {
		t.Fatal("expected an error for a bucket that is not there")
	}
	if got != 0 {
		t.Fatalf("expected no MaxAge, got %v", got)
	}
}
