package main

import (
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's shutdown ends its KV watchers itself, before the connection drains: main adds the
// drain hook that does it (endListenerKVWatchers). Left to the connection's drain, a watcher's
// consumer delete races the drain and is refused, logged as an ERROR, or cut off by the close and
// logged nowhere, so a missing hook shows only some of the time. Deleting the interest and session
// watchers' consumers before the SIGTERM makes the hook's work visible every time: each watcher's
// end finds its consumer gone and says so in a line only kvwatch.Watcher.End writes, and a
// shutdown with every consumer present leaves none of the listener's behind.
func TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, test := range []struct {
		name         string
		consumerGone bool
	}{
		{name: "consumers present"},
		{name: "consumers already gone", consumerGone: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			testnats.Connect(t, uri).Close()
			listener := startListenerProcess(t, binary, uri, "drain-watchers")
			listener.waitHealthy(t)

			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			js, err := admin.JetStream()
			if err != nil {
				t.Fatalf("admin JetStream: %v", err)
			}
			buckets := []string{bus.InterestBucket, bus.SessionBucket}
			for _, bucket := range buckets {
				if consumers := streamConsumers(js, "KV_"+bucket); len(consumers) != 1 {
					t.Fatalf("%s carries consumers %v before the shutdown, want its cache watcher's one", bucket, consumers)
				}
				if test.consumerGone {
					for _, name := range streamConsumers(js, "KV_"+bucket) {
						if err := js.DeleteConsumer("KV_"+bucket, name); err != nil {
							t.Fatalf("delete %s's watcher consumer %s: %v", bucket, name, err)
						}
					}
				}
			}

			listener.Terminate(t)
			listener.WaitExit(t, "SIGTERM")
			output := listener.Output.String()
			if !strings.Contains(output, "envoy-listener shutdown complete") {
				t.Fatalf("the listener did not finish its ordered shutdown:\n%s", output)
			}
			for _, line := range strings.Split(output, "\n") {
				if errorLine.MatchString(line) {
					t.Fatalf("the shutdown logged an error: %s", line)
				}
			}
			if test.consumerGone {
				for _, cache := range []string{"interest registry", "session registry"} {
					if !strings.Contains(output, cache+" watcher's consumer was already gone at its end") {
						t.Fatalf("the shutdown did not end the %s watcher itself (no line from its end):\n%s", cache, output)
					}
				}
			}
			for _, bucket := range buckets {
				if consumers := streamConsumers(js, "KV_"+bucket); len(consumers) != 0 {
					t.Fatalf("%s carries consumers %v after the shutdown, want none", bucket, consumers)
				}
			}
		})
	}
}

// streamConsumers lists the stream's consumers.
func streamConsumers(js natsgo.JetStreamContext, stream string) []string {
	var names []string
	for name := range js.ConsumerNames(stream) {
		names = append(names, name)
	}
	return names
}
