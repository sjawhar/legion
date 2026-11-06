//go:build !race

// This file holds HoldsSeed's allocation test, which the race build must not compile: under the
// race detector testing.AllocsPerRun no longer counts only the function under test, as the test
// says below.

package bus_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// HoldsSeed allocates at most one decoded copy per decoding, at most three in all, and nothing for
// the window check, so however deep a value's encoding its memory and its decoding work are
// bounded. Allocations are counted, not timed, so this does not depend on the machine's load.
// testing.AllocsPerRun counts every malloc in the process, so the test measures in a process of its
// own, the test binary again running only this test. In the package's process, a drain test whose
// connection closes while nats.go drains it leaves nats.go's drain goroutine running after the test
// ends: it waits for the connection's subscriptions to fall to its one request subscription, which
// the close removes too, so on a connection that has made a request it waits out its 30 s
// DrainTimeout, then marks the closed connection draining again and waits 5 s for a PONG
// (Client.Drain says why). It allocates as it ends, and those allocations would count as
// HoldsSeed's. This file's //go:build !race keeps the test out of the race build, as the standard
// library keeps its allocation tests out of it: the race runtime allocates on its own and disables
// the tiny allocator, so AllocsPerRun would count those allocations as HoldsSeed's. Its cost
// counterpart, TestHoldsSeedCostIsLinear, still runs there.
func TestHoldsSeedAllocatesAtMostOneCopyPerDecoding(t *testing.T) {
	if os.Getenv(allocsChildEnv) == "" {
		child := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count=1", "-test.v")
		child.Env = append(os.Environ(), allocsChildEnv+"=1")
		output, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: "+t.Name()+" (") {
			t.Fatalf("measured in a process of its own, %s did not pass: %v\n%s", t.Name(), err, output)
		}
		return
	}
	for _, row := range []struct {
		name string
		text string
		max  float64
	}{
		{"a percent sign nested 1,024 times", "%" + fill("25", 2048), 3},
		{"windows that start like a seed", fill("SUA", 1<<16), 0},
		{"ordinary text", fill("the quick brown fox jumps over the lazy dog. ", 1<<16), 0},
		{"a letter percent-encoded three times", "%252553", 3},
	} {
		t.Run(row.name, func(t *testing.T) {
			if allocs := testing.AllocsPerRun(5, func() { bus.HoldsSeed(row.text) }); allocs > row.max {
				t.Fatalf("HoldsSeed allocated %v times, want at most %v", allocs, row.max)
			}
		})
	}
}

// allocsChildEnv marks the child process TestHoldsSeedAllocatesAtMostOneCopyPerDecoding measures
// in: set, the test measures in its own process instead of starting one.
const allocsChildEnv = "ENVOY_BUS_HOLDSSEED_ALLOCS_CHILD"
