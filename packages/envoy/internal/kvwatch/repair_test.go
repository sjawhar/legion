package kvwatch_test

import (
	"context"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// armed stands in for time.AfterFunc and records what the watcher asked of it: how long it wanted
// to wait for a repair, and the callback, which the test fires itself. The production wait is a
// minute, so a test that slept for it could not run at all.
type armed struct {
	mu    sync.Mutex
	count int
	stops int
	delay time.Duration
	fire  func()
}

// afterFunc is the AfterFunc WithAfterFunc takes.
func (a *armed) afterFunc() kvwatch.AfterFunc {
	return func(d time.Duration, f func()) kvwatch.Timer {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.count++
		a.delay = d
		a.fire = f
		return (*armedTimer)(a)
	}
}

// waits reports how many waits the watcher armed.
func (a *armed) waits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

// asked is the delay of the last wait the watcher armed.
func (a *armed) asked() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.delay
}

// fireIt runs the last armed wait's callback, as the real timer would once its delay passed, and
// fails when nothing was armed.
func (a *armed) fireIt(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	fire := a.fire
	a.mu.Unlock()
	if fire == nil {
		t.Fatal("the watcher armed no wait to fire")
	}
	fire()
}

// armedTimer is the Timer an armed hands back, counting the stops a repair makes.
type armedTimer armed

func (t *armedTimer) Stop() bool {
	a := (*armed)(t)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops++
	return true
}

// matching is every record whose message and level are both these. A case is matched by the two
// together, so no other line the watcher writes - its warm-up, a failed first start, a recreated
// bucket - can satisfy it.
func matching(records []map[string]any, level, msg string) []map[string]any {
	var found []map[string]any
	for _, record := range records {
		if haveLevel, _ := record["level"].(string); haveLevel != level {
			continue
		}
		if haveMsg, _ := record["msg"].(string); haveMsg == msg {
			found = append(found, record)
		}
	}
	return found
}

const (
	stoppedWarn    = "test cache watcher stopped"
	unrepairedErr  = "test cache watcher was not repaired"
	testBucketName = "kvwatch-test"
)

// jetStream is the JetStream context of conn.
func jetStream(t *testing.T, conn *natsgo.Conn) natsgo.JetStreamContext {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// deleteTestBucket deletes the test bucket, which deletes the stream the running watcher's ordered
// consumer reads and so ends that watcher while the connection to NATS stays up.
func deleteTestBucket(t *testing.T, js natsgo.JetStreamContext) {
	t.Helper()
	if err := js.DeleteKeyValue(testBucketName); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
}

// waitForTheStopWarn waits for the one WARN the stop writes.
func waitForTheStopWarn(t *testing.T, records func() []map[string]any) {
	t.Helper()
	eventually(t, "the stopped watcher's WARN", func() bool {
		return len(matching(records(), "WARN", stoppedWarn)) == 1
	})
}

// A stop a Rewatch repairs a moment later is a WARN and nothing more. The repair disarms the wait,
// so even firing it afterwards writes no ERROR. Without that, every reconnect and every
// self-health rebuild left an ERROR an operator had to learn to ignore (INFRA-182).
func TestAWatcherRepairedAfterItsConsumerWentLogsOneWarn(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, into.apply, into.reset,
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	waitForTheStopWarn(t, records)

	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	arm.fireIt(t)

	if warns := matching(records(), "WARN", stoppedWarn); len(warns) != 1 {
		t.Fatalf("WARN %q logged %d times, want once", stoppedWarn, len(warns))
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("a repaired watcher logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}
}

// A repair that fails reports the stop at once rather than waiting out the minute: the bucket is
// gone, so nothing will repair this watcher. The report is made once, so a self-health rebuild
// retrying every few seconds adds no second ERROR for the same stop.
func TestAWatcherWhoseBucketIsGoneLogsTheFailedRepair(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {},
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	deleteTestBucket(t, jetStream(t, conn))
	waitForTheStopWarn(t, records)

	if err := w.Rewatch(conn); err == nil {
		t.Fatal("a rewatch onto a deleted bucket succeeded")
	}
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("a failed repair logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
	}
	if reason, _ := errs[0]["reason"].(string); reason != "repair failed" {
		t.Fatalf("reason = %q, want %q", reason, "repair failed")
	}

	if err := w.Rewatch(conn); err == nil {
		t.Fatal("a second rewatch onto a deleted bucket succeeded")
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 1 {
		t.Fatalf("a second failed repair logged %d %q errors in all, want one: %v", len(errs), unrepairedErr, errs)
	}
}

// A stop nothing repairs is the outage an operator has to see, so it becomes an ERROR once the
// minute the watcher waits for a repair has passed, and not before: an ERROR at the stop itself
// could not tell this apart from a stop a rebuild repairs a moment later (INFRA-182).
func TestAWatcherNothingRepairsLogsWhenItsMinutePasses(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {},
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	deleteTestBucket(t, jetStream(t, conn))
	waitForTheStopWarn(t, records)

	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("the stop logged %d %q errors before its wait passed, want none: %v", len(errs), unrepairedErr, errs)
	}
	if got := arm.asked(); got != time.Minute {
		t.Fatalf("the watcher waited %v for a repair, want %v", got, time.Minute)
	}

	arm.fireIt(t)
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("the passed wait logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
	}
	want := "no repair within " + time.Minute.String()
	if reason, _ := errs[0]["reason"].(string); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// A shutdown's own stop is neither a stop to report nor a repair to wait for: End retires the
// watcher first, so it writes no WARN, arms no wait and can leave no ERROR behind. The listener's
// shutdown test greps its whole output for ERROR across ten runs
// (cmd/listener/shutdown_test.go), so an arm left by an ordered stop would fail it.
func TestAWatcherStoppedOnPurposeLogsNeither(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {},
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.End(ctx)

	if warns := matching(records(), "WARN", stoppedWarn); len(warns) != 0 {
		t.Fatalf("an ordered stop logged %d %q warnings, want none: %v", len(warns), stoppedWarn, warns)
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("an ordered stop logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}
	if arm.waits() != 0 {
		t.Fatalf("an ordered stop armed %d repair waits, want none", arm.waits())
	}
}
