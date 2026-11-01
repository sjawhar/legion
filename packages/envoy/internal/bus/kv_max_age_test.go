package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// maxAgeBucket creates a KV bucket keeping one value per key for ttl and returns a JetStream
// context on it, so a test can raise the bucket and read its stream back.
func maxAgeBucket(t *testing.T, bucket string, ttl time.Duration) nats.JetStreamContext {
	t.Helper()
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bucket, History: 1, TTL: ttl, Replicas: 1}); err != nil {
		t.Fatalf("create the bucket with a max age of %s: %v", ttl, err)
	}
	return js
}

// maxAge reads the age the bucket keeps a key's latest value for.
func maxAge(t *testing.T, js nats.JetStreamContext, bucket string) time.Duration {
	t.Helper()
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the stream of %s: %v", bucket, err)
	}
	return info.Config.MaxAge
}

// JetStream writes "keep every value forever" as a MaxAge of 0, which is longer than any age a
// raise asks for, so a bucket an operator set to keep its values without limit is left alone: a
// raise raises an age and never lowers one.
func TestRaiseMaxAgeLeavesABucketWithNoAgeLimitAlone(t *testing.T) {
	bucket := "envoy_test_unlimited_age"
	js := maxAgeBucket(t, bucket, 0)
	if err := RaiseMaxAge(js, bucket, 20*time.Minute); err != nil {
		t.Fatalf("raise the max age of a bucket that keeps its values forever: %v", err)
	}
	if age := maxAge(t, js, bucket); age != 0 {
		t.Fatalf("the bucket keeps a value for %s after the raise, want the unlimited age (0) the operator set", age)
	}
}

// A bucket that keeps its values for less than the age the caller names is raised to it, and one
// that already keeps them as long or longer is left alone.
func TestRaiseMaxAgeRaisesAShorterAgeAndKeepsALongerOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"shorter is raised", 10 * time.Minute, 20 * time.Minute},
		{"longer stands", 30 * time.Minute, 30 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bucket := "envoy_test_raise_age"
			js := maxAgeBucket(t, bucket, tc.ttl)
			if err := RaiseMaxAge(js, bucket, 20*time.Minute); err != nil {
				t.Fatalf("raise the max age of a bucket that keeps its values for %s: %v", tc.ttl, err)
			}
			if age := maxAge(t, js, bucket); age != tc.want {
				t.Fatalf("the bucket keeps a value for %s after the raise, want %s", age, tc.want)
			}
		})
	}
}
