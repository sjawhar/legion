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
// to wait for a repair, and every callback, in the order the watcher armed them, which the test
// fires itself. The production wait is a minute, so a test that slept for it could not run at
// all. Each callback is kept, not just the last, because a real one can still run after a later
// stop has armed its own: time.Timer.Stop does not wait for a callback already started.
type armed struct {
	mu    sync.Mutex
	delay time.Duration
	fires []func()
}

// afterFunc is the AfterFunc WithAfterFunc takes.
func (a *armed) afterFunc() kvwatch.AfterFunc {
	return func(d time.Duration, f func()) kvwatch.Timer {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.delay = d
		a.fires = append(a.fires, f)
		return (*armedTimer)(a)
	}
}

// waits reports how many waits the watcher armed.
func (a *armed) waits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.fires)
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
	a.fireAt(t, a.waits()-1)
}

// fireAt runs the callback of the wait the watcher armed i-th, counting from 0, and fails when it
// armed no such wait.
func (a *armed) fireAt(t *testing.T, i int) {
	t.Helper()
	a.mu.Lock()
	if i < 0 || i >= len(a.fires) {
		count := len(a.fires)
		a.mu.Unlock()
		t.Fatalf("the watcher armed %d waits, so no wait %d to fire", count, i)
	}
	fire := a.fires[i]
	a.mu.Unlock()
	fire()
}

// armedTimer is the Timer an armed hands back. Its Stop does nothing: the fake fires a wait only
// when the test asks, which is what lets a test fire one a repair has already stopped.
type armedTimer armed

func (t *armedTimer) Stop() bool { return true }

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

// stopNoticeBudget is how long waitForStopWarns waits for a stop. Against a real nats-server
// v2.11.4 the notice was measured at about 10 s (10.017 s, 10.022 s and 10.006 s) and at 18.81 s
// twice, so this is six times the usual notice and about three times the worst observed, which
// leaves a slow runner room.
const stopNoticeBudget = 60 * time.Second

// waitForStopWarns waits until the stops' WARNs number want: each stop writes one. It polls on
// its own budget rather than through the package's eventually, because nats.go's ordered consumer
// reports its stream gone only after two missed 5 s idle heartbeats: the notice was measured at
// about 10 s, and at 18.81 s, so eventually's 5 s deadline can never see this WARN. Polling ends
// a passing case as soon as the WARN is written.
func waitForStopWarns(t *testing.T, records func() []map[string]any, want int) {
	t.Helper()
	deadline := time.Now().Add(stopNoticeBudget)
	for len(matching(records(), "WARN", stoppedWarn)) != want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d stopped watcher WARNs", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	waitForStopWarns(t, records, 1)

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
	waitForStopWarns(t, records, 1)

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
	waitForStopWarns(t, records, 1)

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

// A wait reports only the stop that armed it. A repair clears the wait, but time.Timer.Stop does
// not wait for a callback already started, so the callback can still run once the replacement
// has stopped too and armed a wait of its own. Without knowing its stop, that late callback took
// the second stop's report: an ERROR saying no repair came within a minute of a stop moments
// old, and the second wait, finding the stop reported, then said nothing when its minute passed.
func TestAWaitARepairClearedReportsNotTheNextStop(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {},
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	waitForStopWarns(t, records, 1)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Rewatch(conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	deleteTestBucket(t, js)
	waitForStopWarns(t, records, 2)
	if got := arm.waits(); got != 2 {
		t.Fatalf("two stops armed %d repair waits, want two", got)
	}

	// The repair cleared the first wait; its callback runs now, as one already started would.
	arm.fireAt(t, 0)
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("the repaired stop's wait logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}

	arm.fireAt(t, 1)
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("the second stop's passed wait logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
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
