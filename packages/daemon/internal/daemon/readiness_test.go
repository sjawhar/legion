package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// fastReadiness overrides readinessRetry for a test's lifetime, restored on cleanup, so a gate
// that would otherwise wait up to a minute between attempts costs the suite nothing.
func fastReadiness(t *testing.T, retry bootprobe.Retry) {
	t.Helper()
	previous := readinessRetry
	readinessRetry = retry
	t.Cleanup(func() { readinessRetry = previous })
}

// readinessAttempt's three outcomes: passed, retried (bootprobe.Run's retry branch) when its
// judge recognizes the failure, and refused when it does not.
// TestRunWaitsThroughADispatch503BeforeServing (daemon_test.go) proves the real wiring end to
// end, through a real dispatch.HTTPClient and a real daemon.Run(); this proves the adapter's own
// three-way logic in isolation.
func TestReadinessAttempt(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		unreachable func(error) bool
		wantPassed  bool
		wantDetail  string
		wantRefusal bool
	}{
		{name: "passed", wantPassed: true},
		{
			name:        "retried when its judge recognizes the failure",
			unreachable: func(error) bool { return true },
			wantDetail:  "unavailable",
		},
		{
			name:        "refused when its judge does not recognize the failure",
			unreachable: func(error) bool { return false },
			wantRefusal: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			attemptErr := error(nil)
			if !testCase.wantPassed {
				attemptErr = errors.New("unavailable")
			}
			got := readinessAttempt(func(context.Context) error { return attemptErr }, testCase.unreachable)(context.Background())
			if got.Passed != testCase.wantPassed {
				t.Fatalf("Outcome.Passed = %t, want %t", got.Passed, testCase.wantPassed)
			}
			if got.Detail != testCase.wantDetail {
				t.Fatalf("Outcome.Detail = %q, want %q", got.Detail, testCase.wantDetail)
			}
			if (got.Refusal != nil) != testCase.wantRefusal {
				t.Fatalf("Outcome.Refusal = %v, want non-nil = %t", got.Refusal, testCase.wantRefusal)
			}
		})
	}
}
