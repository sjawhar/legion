package kvwatch_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// warnings is every WARN or ERROR record, each as its level, message and error.
func warnings(records []map[string]any) []string {
	var found []string
	for _, record := range records {
		level, _ := record["level"].(string)
		if level != "WARN" && level != "ERROR" {
			continue
		}
		message, _ := record["msg"].(string)
		errText, _ := record["error"].(string)
		found = append(found, level+" "+message+": "+errText)
	}
	return found
}

// testConsumers lists the test bucket's consumers.
func testConsumers(t *testing.T, conn *natsgo.Conn) []string {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	var names []string
	for name := range js.ConsumerNames("KV_kvwatch-test") {
		names = append(names, name)
	}
	return names
}

// A watch still starting when the shutdown stops the watcher arms a watcher after the stop. End
// waits for that watch and ends its watcher too, deleting its consumer, so nothing is left for the
// connection's drain to delete.
func TestEndEndsAWatcherArmedAfterTheStop(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	held := &heldWatchKV{KeyValue: kv, watched: make(chan struct{}), release: make(chan struct{})}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", bus.KeyValue{KeyValue: held}, newSeen().apply, func() {}, kvwatch.WithLogger(logger))
	w.Start()
	select {
	case <-held.watched:
	case <-time.After(5 * time.Second):
		close(held.release)
		t.Fatal("the watch never armed its watcher")
	}
	w.Stop()
	ended := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		w.End(ctx)
		close(ended)
	}()
	close(held.release)
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("End never returned")
	}
	if consumers := testConsumers(t, conn); len(consumers) != 0 {
		t.Fatalf("consumers after End = %v, want none", consumers)
	}
	if got := warnings(records()); len(got) != 0 {
		t.Fatalf("End logged %q, want nothing", got)
	}
}

// A watch still starting when End's deadline passes is an ERROR, since End cannot say its watcher
// was ended within it. The watch ends its watcher itself once it arms it, so its consumer is still
// deleted.
func TestAWatchStillStartingAtEndsDeadlineIsAnError(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	held := &heldWatchKV{KeyValue: kv, watched: make(chan struct{}), release: make(chan struct{})}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", bus.KeyValue{KeyValue: held}, newSeen().apply, func() {}, kvwatch.WithLogger(logger))
	w.Start()
	select {
	case <-held.watched:
	case <-time.After(5 * time.Second):
		close(held.release)
		t.Fatal("the watch never armed its watcher")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.End(ctx)
	close(held.release)
	eventually(t, "the late watch to delete its consumer", func() bool { return len(testConsumers(t, conn)) == 0 })
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end found a watch still starting: context deadline exceeded"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}

// A delete that fails for any reason but its consumer being gone stays an ERROR: here the bucket's
// stream was deleted under the watcher, so the server answers "stream not found".
func TestAFailedConsumerDeleteAtTheEndIsAnError(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {}, kvwatch.WithLogger(logger))
	w.Start()
	eventually(t, "the first scan", w.Ready)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.End(ctx)
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end failed: nats: stream not found"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}

// End is bounded by its context: against a server that does not answer, it returns at the
// deadline and logs the unanswered delete as an ERROR rather than holding the shutdown.
func TestEndIsBoundedByItsContext(t *testing.T) {
	ctr, uri := testnats.Start(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {}, kvwatch.WithLogger(logger))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	id := ctr.GetContainerID()
	if out, err := exec.Command("docker", "pause", id).CombinedOutput(); err != nil {
		t.Fatalf("pause NATS: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "unpause", id).Run() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	w.End(ctx)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("End took %s against an unresponsive server, want it to return at its deadline", elapsed)
	}
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end failed: delete its consumer: context deadline exceeded"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}
