package docs

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"
)

// The leak registry answers one question this package's tests cannot otherwise ask: when the test
// binary ends, how many of the services its tests built are still alive? A test that builds a
// service and never shuts it down keeps that service - and every room it loaded, with that room's
// document in memory - for the rest of the binary's run, which is the memory this check exists to
// catch. The registry holds each service through a weak pointer, so it pins nothing itself: what
// stays alive stays alive because a test left it so.

// leakWaitBudget bounds how long the report waits for the services the tests released to be
// collected. A service is unreachable the moment its test returns, but it is only reported as
// collected once a GC has cleared its weak pointer, and its shutdown's goroutines may still be
// unwinding when the last test returns.
const leakWaitBudget = 30 * time.Second

// leakWaitStep is how long the report sleeps between the pairs of collections it forces.
const leakWaitStep = 250 * time.Millisecond

// builtService is one service New built, named by the test that built it.
type builtService struct {
	service weak.Pointer[Service]
	test    string
}

var (
	builtServicesMu sync.Mutex
	builtServices   []builtService
)

// New runs from tests that run in parallel, so the hook appends under the mutex.
func init() { newServiceHook = recordBuiltService }

func recordBuiltService(service *Service) {
	entry := builtService{service: weak.Make(service), test: buildingTest()}
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	builtServices = append(builtServices, entry)
}

// buildingTest names the nearest frame of the call to New that belongs to a test of this package:
// the test to fix when the service it built is still alive at the end of the run. Helpers of this
// package that build a service for a test are skipped, so the name is the test's own.
func buildingTest() string {
	var pcs [64]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	for {
		frame, more := frames.Next()
		if name := frame.Function; strings.Contains(name, "docs.Test") {
			if cut := strings.LastIndex(name, "/"); cut >= 0 {
				name = name[cut+1:]
			}
			return name
		}
		if !more {
			return "an unnamed caller"
		}
	}
}

// liveServices reports how many services the tests built and names the ones a weak pointer still
// resolves, in the order they were built.
func liveServices() (built int, live []string) {
	builtServicesMu.Lock()
	defer builtServicesMu.Unlock()
	for _, entry := range builtServices {
		if entry.service.Value() != nil {
			live = append(live, entry.test)
		}
	}
	return len(builtServices), live
}

// reportLeakedServices forces collections until no service the tests built is still reachable, or
// until the budget runs out, and prints what it found to stderr. It never fails the run: it
// reports, and the exit code stays the one the test run earned. A non-zero count of live services
// means a test built a service and never shut it down, so that service, its rooms and their
// documents stayed in memory for the rest of the binary's life; the lines after the count name the
// tests to give a t.Cleanup that shuts their service down.
func reportLeakedServices() {
	start := time.Now()
	deadline := start.Add(leakWaitBudget)
	var built int
	var live []string
	for {
		// Twice: the first collection can leave a service reachable from a finalizer it queued.
		runtime.GC()
		runtime.GC()
		built, live = liveServices()
		if len(live) == 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(leakWaitStep)
	}
	fmt.Fprintf(os.Stderr, "docs: %d services built, %d still alive after %s\n",
		built, len(live), time.Since(start).Round(time.Millisecond))
	for _, test := range live {
		fmt.Fprintf(os.Stderr, "docs: a service built by %s is still alive\n", test)
	}
}
