package kvwatch_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A kvwatch subscription can end in ways kvwatch does not choose: the connection closes or drains
// under it, or the listener shuts down mid-scan. None of them may panic the process. nats.go's
// legacy KV watcher (v1.50.0 kv.go) arms an init timer while a scan is pending, and its callback
// sends on the watcher's errCh under the watcher's mutex; the subscription's end (pDone) closes that
// channel under the same mutex and only Stops the timer, which does not wait for a callback already
// running. A callback that fires while the message handler holds the mutex - the handler blocks
// holding it once the 256-entry updates channel is full, as it is while the cache is slow to take a
// record - and loses the mutex to pDone sends on a closed channel and panics the process (INFRA-261).
//
// Each round holds the cache's first record, so the updates channel fills and the handler blocks,
// closes the connection the watcher reads through, and lets the record go at a moment swept across
// the init timer's firing (the connection's JetStream wait, 20 ms here), so in some round the
// timer's callback starts waiting on the mutex just before the handler releases it to pDone.
func TestASubscriptionEndingMidScanNeverPanics(t *testing.T) {
	const (
		jsWait  = 20 * time.Millisecond
		records = 600
		rounds  = 300
	)
	uri := testnats.URL(t)
	_, seed := bucket(t, uri)
	for i := range records {
		if _, err := seed.Put(fmt.Sprintf("k%04d", i), []byte("v")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	for round := range rounds {
		// The release moves 100 µs later each round, from 15 ms to 45 ms after the first record.
		hold := 15*time.Millisecond + time.Duration(round)*100*time.Microsecond
		conn := testnats.Connect(t, uri)
		js, err := conn.JetStream(natsgo.MaxWait(jsWait))
		if err != nil {
			t.Fatalf("jetstream: %v", err)
		}
		// The bucket lookup is setup, not what the round tests, and the 20 ms wait the round needs
		// bounds the legacy STREAM.INFO it makes too, which a loaded machine can miss: try again.
		var handle bus.KeyValue
		for attempt := 0; ; attempt++ {
			if handle, err = bus.OpenKeyValue(conn, js, testBucketName); err == nil {
				break
			}
			if attempt == 50 {
				t.Fatalf("round %d: bucket: %v", round, err)
			}
		}
		release := make(chan struct{})
		first := make(chan time.Time, 1)
		var once sync.Once
		into := withApply{seen: newSeen(), before: func(natsgo.KeyValueEntry) {
			once.Do(func() {
				first <- time.Now()
				<-release
			})
		}}
		w := kvwatch.New("test cache", handle, into, kvwatch.WithStreamProbeInterval(0))
		w.Start()
		var started time.Time
		select {
		case started = <-first:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: the first record never reached the cache", round)
		}
		time.Sleep(time.Until(started.Add(hold)))
		conn.Close()
		close(release)
		w.Stop()
		time.Sleep(2 * jsWait)
	}
}
