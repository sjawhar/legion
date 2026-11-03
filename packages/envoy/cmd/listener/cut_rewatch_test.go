package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// cutRewatchCaches are the warm-up lines of the three caches the listener keeps. The reconnect
// after a cut rewatches every one of them (rewatchListenerKVWatchers), and each replacement writes
// one warm-up line, so all three say the caches came back.
var cutRewatchCaches = []string{
	"interest registry cache warm-up",
	"role claims cache warm-up",
	"session registry cache warm-up",
}

// A hub that stops answering runs every KV cache of the listener through kvwatch's replacement
// path, and a watcher a rewatch replaced is no operator's page. The listener declares such a hub
// lost about two seconds into the silence (bus.WithSilentCutDetection in listenerStreamOptions),
// and the reconnect that follows rewatches every cache. The freeze is what reaches this path now:
// on nats.go's default pings the same silence left the listener reading connected for four to six
// minutes, so no reconnect and no rewatch ran at all.
//
// The test asserts four things over the real binary, each read from the output the freeze and the
// thaw wrote:
//   - both of the listener's clients report the cut, so the run is in the rewatch path;
//   - each of the three caches logs a warm-up line after the thaw, so a rewatch replaced each
//     watcher;
//   - an interest another listener writes after the thaw reaches this listener's cache;
//   - the listener logs no ERROR line, so no watcher a rewatch replaced reports its end as a
//     failure of the cache (kvwatch.Watcher.Start).
//
// This listener reaches one of the two branches of kvwatch.Watcher.Start: main opens its stores
// and waits out their warm-ups before it adds the reconnect hook, so a first start of this process
// always ends before a rewatch can replace it, and the ERROR it writes is a live cache's. The
// branch a rewatch replaced is pinned by the unit test of internal/kvwatch. What this test holds
// is the whole cut path, every cache's replacement included, to no ERROR at all.
func TestASilentCutRewatchesTheCachesWithNoErrorLine(t *testing.T) {
	server, uri := testnats.Start(t)
	// The live topics lane opens only under ENVOY_SUBSCRIBE_ALL_TOPICS (liveLaneEnv), and both
	// clients connect before the listener reports itself healthy.
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "cut-rewatch", liveLaneEnv...)
	listener.waitHealthy(t)

	thaw, output := freezeUntilBothClientsReportTheCut(t, server, listener)
	thaw()

	// Every cache is rewatched on the reconnect, and each replacement's warm-up line says so.
	thawed := time.Now()
	for _, warmUp := range cutRewatchCaches {
		for !strings.Contains(output(), warmUp) {
			if time.Since(thawed) > 30*time.Second {
				t.Fatalf("no %q line %s after the thaw, so the cache did not come back:\n%s",
					warmUp, time.Since(thawed).Round(time.Millisecond), output())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// The cache follows the bucket again: an interest another listener writes after the thaw is
	// listed by this listener, which answers /v1/interests/ from its cache alone.
	writer := testnats.Connect(t, uri)
	t.Cleanup(writer.Close)
	registry, err := store.Open(writer)
	if err != nil {
		t.Fatalf("open the interest registry as another listener: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessionID := "ses_cut_rewatch_writer"
	if _, err := registry.Upsert(store.Interest{SessionID: sessionID, MachineID: "other-listener"}, []string{"notifications.agent." + sessionID}); err != nil {
		t.Fatalf("subscribe through another listener: %v", err)
	}
	written := time.Now()
	for !listsInterest(t, listener.port, sessionID) {
		if time.Since(written) > 10*time.Second {
			t.Fatalf("%s after the thaw, the listener's cache still missed an interest another listener wrote:\n%s",
				time.Since(written).Round(time.Millisecond), output())
		}
		time.Sleep(100 * time.Millisecond)
	}

	for line := range strings.SplitSeq(output(), "\n") {
		if errorLine.MatchString(line) {
			t.Fatalf("a silent cut and its rewatch logged an error: %s", line)
		}
	}
}
