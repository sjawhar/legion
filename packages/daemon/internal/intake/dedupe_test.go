package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

// GitHub redelivers a delivery under its own X-GitHub-Delivery, and Dispatch's redelivery sweep asks
// for one up to 72 hours late, past the stream's duplicate window, while the listener mints a new
// event_id for every request it is sent. A delivery the daemon has applied is dropped by its
// dedupe_key when it arrives again, before any handler runs, and its consumer's position still
// advances past it. A review that mentions the trigger, which the listener publishes on its two
// mention topics beside its own, is one fact; another delivery with the same body is another.
func TestARedeliveredGitHubDeliveryIsAppliedOnceByItsDedupeKey(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.ReviewPermission = func(context.Context, PullRequestReview) (bool, error) { return true, nil }
	stop := startConsume(t, js, spec, pool, writeHandler("review", nil))
	defer stop()
	recordPullRequest(t, pool, "LEGION-208", 42)

	captured := capturedGitHubEnvelope(t, "review.json")
	delivery := bytes.ReplaceAll(captured, []byte(`\"body\":\"Captured review\"`), []byte(`\"body\":\"Captured review @legion\"`))
	if bytes.Equal(delivery, captured) {
		t.Fatal("review.json carries no review body to mention the trigger in")
	}
	// The listener's publication order: the most specific mention topic, the repository's, the review's.
	topics := []string{
		"notifications.github.sjawhar.legion.pr.42.mention",
		"notifications.github.sjawhar.legion.mention",
		"notifications.github.sjawhar.legion.pr.42.review",
	}
	consumer := githubConsumerName(spec.Project)
	deliver := func(envelope []byte, fields map[string]string, topics ...string) uint64 {
		t.Helper()
		var last uint64
		for _, topic := range topics {
			fields["topic"] = topic
			last = publishAt(t, js, topic, reenveloped(t, envelope, fields))
		}
		waitAcknowledged(t, stream, consumer, last)
		return last
	}
	applied := func(want int) {
		t.Helper()
		if got := writeCount(t, pool); got != want {
			t.Fatalf("the review handler ran %d times, want %d", got, want)
		}
		if got := processedEvents(t, pool); got != want {
			t.Fatalf("processed_events holds %d rows, want %d, one for each delivery", got, want)
		}
	}

	deliver(delivery, map[string]string{}, topics...)
	applied(1)

	last := deliver(delivery, map[string]string{"event_id": "redelivered-review", "trace_id": "redelivered-review-trace"}, topics...)
	applied(1)
	var seq int64
	if err := pool.QueryRow(context.Background(), `select stream_seq from consumer_positions where consumer = $1`, consumer).Scan(&seq); err != nil {
		t.Fatalf("read %s's position: %v", consumer, err)
	}
	if seq != int64(last) {
		t.Fatalf("%s's position = stream sequence %d, want %d: the redelivery it dropped is behind it", consumer, seq, last)
	}

	deliver(delivery, map[string]string{"event_id": "another-review", "source_event_id": "another-delivery", "dedupe_key": "github.another-delivery"}, topics[2])
	applied(2)
}

// reenveloped is the Envoy envelope data with each of fields set to its value.
func reenveloped(t *testing.T, data []byte, fields map[string]string) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode the envelope: %v", err)
	}
	for field, value := range fields {
		envelope[field] = value
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return out
}
