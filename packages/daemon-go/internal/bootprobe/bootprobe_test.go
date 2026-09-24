package bootprobe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

// plan answers one outcome per attempt, repeating the last, and counts the attempts.
type plan struct {
	outcomes []Outcome
	attempts int
}

func (p *plan) attempt(context.Context) Outcome {
	p.attempts++
	if p.attempts <= len(p.outcomes) {
		return p.outcomes[p.attempts-1]
	}
	return p.outcomes[len(p.outcomes)-1]
}

func transient(detail string) Outcome { return Outcome{Detail: detail} }

var passed = Outcome{Passed: true}

// quick is a policy whose waits cost the suite nothing: 1 ms doubling to a 4 ms cap.
func quick(attempts int) Retry {
	return Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond, Attempts: attempts}
}

func logger(buffer *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(buffer, nil)) }

func TestRunStopsAtTheFirstPass(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("database is locked"), passed, transient("never run")}}
	var logged bytes.Buffer

	if err := Run(context.Background(), "OMP pi.agents", quick(6), logger(&logged), p.attempt); err != nil {
		t.Fatalf("Run = %v, want the pass", err)
	}
	if p.attempts != 2 {
		t.Errorf("attempts = %d, want a transient failure and the pass", p.attempts)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 1 {
		t.Errorf("logged %d transient failures, want 1:\n%s", got, logged.String())
	}
}

// A refusal is an answer no retry changes: it is returned as it is, at once.
func TestRunReturnsARefusalWithoutRetrying(t *testing.T) {
	refusal := errors.New("pi-legion-envoy 1.57.0 is installed but not loaded by omp")
	p := &plan{outcomes: []Outcome{{Refusal: refusal}, passed}}

	err := Run(context.Background(), "pi-legion-envoy load", quick(6), logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, refusal) || err.Error() != refusal.Error() {
		t.Fatalf("Run = %v, want the refusal itself", err)
	}
	if p.attempts != 1 {
		t.Errorf("attempts = %d, want 1", p.attempts)
	}
}

// A bounded policy gives up after its attempts, naming the probe, the bound, and what the last
// attempt said — a probe that never answered, not one that answered no.
func TestRunGivesUpAfterItsAttemptsNamingTheLastDetail(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("first"), transient("second"), transient("command timed out after 300s")}}
	var logged bytes.Buffer

	err := Run(context.Background(), "OMP session storage setting", quick(3), logger(&logged), p.attempt)

	want := "the OMP session storage setting probe never completed within its retry budget (3 attempts): command timed out after 300s"
	if err == nil || err.Error() != want {
		t.Fatalf("Run = %v, want %q", err, want)
	}
	if p.attempts != 3 {
		t.Errorf("attempts = %d, want the bound, 3", p.attempts)
	}
	if got := strings.Count(logged.String(), "failed transiently"); got != 2 {
		t.Errorf("logged %d transient failures, want 2 (none after the last attempt):\n%s", got, logged.String())
	}
}

// Without a bound, transient failures are waited out however many there are, each wait doubling
// from Initial and capped at Max.
func TestRunWithoutABoundWaitsOutEveryTransientFailure(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("1"), transient("2"), transient("3"), transient("4"), transient("5"), passed}}
	var logged bytes.Buffer

	if err := Run(context.Background(), "pi-legion-envoy load", quick(0), logger(&logged), p.attempt); err != nil {
		t.Fatalf("Run = %v, want the pass after five transient failures", err)
	}
	if p.attempts != 6 {
		t.Errorf("attempts = %d, want 6", p.attempts)
	}
	var waits []string
	for _, match := range regexp.MustCompile(`retryIn=(\S+)`).FindAllStringSubmatch(logged.String(), -1) {
		waits = append(waits, match[1])
	}
	if got, want := strings.Join(waits, " "), "1ms 2ms 4ms 4ms 4ms"; got != want {
		t.Errorf("waits = %q, want %q", got, want)
	}
}

// A probe stopped while it waits to run again runs no other attempt, and says why it ended.
func TestRunStopsWithItsContext(t *testing.T) {
	p := &plan{outcomes: []Outcome{transient("hung")}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	err := Run(ctx, "worker image", Retry{Initial: time.Minute, Max: time.Minute}, logger(&bytes.Buffer{}), p.attempt)

	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the worker image probe was abandoned") {
		t.Fatalf("Run = %v, want the abandoned-probe error wrapping the cancellation", err)
	}
	if p.attempts != 1 {
		t.Errorf("attempts = %d, want the one before the stop", p.attempts)
	}
}

// An attempt the stop interrupted is not judged: whatever it returned, Run reports the stop.
func TestRunReportsTheStopOverAnInterruptedAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	refused := func(context.Context) Outcome {
		cancel()
		return Outcome{Refusal: errors.New("OMP launch probe failed (exit -1)")}
	}

	err := Run(ctx, "pi-legion-envoy load", quick(6), logger(&bytes.Buffer{}), refused)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the stop, not the interrupted attempt's refusal", err)
	}
}

// The OK line is the wire between the image's `legion probe-image` and the daemon's probe
// Sandbox: the session-storage mark, the model that answered the round trip through the gateway
// when the probe made one, and the contract last.
func TestOKLineCarriesTheMarksAndTheContract(t *testing.T) {
	for model, want := range map[string]string{
		"": "probe-image: OK (/opt/omp/bin/omp) session-storage=probed go-daemon-api-version=3",
		"anthropic/claude-fable-5-1-legion": "probe-image: OK (/opt/omp/bin/omp) session-storage=probed " +
			"model-gateway=anthropic/claude-fable-5-1-legion go-daemon-api-version=3",
	} {
		if got := OKLine("/opt/omp/bin/omp", model, 3); got != want {
			t.Errorf("OKLine(model %q) = %q, want %q", model, got, want)
		}
	}
}

// The daemon requires the model an image's round trip was answered by, read from its OK line: a
// line without the token is an image that made no round trip (its CLI predates it, or its pod had
// no gateway), and the token counts only on the OK line, before the contract.
func TestConfirmedModelReadsTheTokenOnAnOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		output string
		model  string
	}{
		{"the line among other output", "[legion] model route probe failed transiently\n" + OKLine("/opt/omp/bin/omp", "anthropic/claude-fable-5-1-legion", 3) + "\n", "anthropic/claude-fable-5-1-legion"},
		{"no round trip", OKLine("/opt/omp/bin/omp", "", 3), ""},
		{"the token on a line that is not the OK line", "[legion] model-gateway=anthropic/claude-fable-5-1-legion", ""},
		{"the token after the contract", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed go-daemon-api-version=3 model-gateway=anthropic/x", ""},
		{"an empty token", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed model-gateway= go-daemon-api-version=3", ""},
		{"nothing", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model, ok := ConfirmedModel(testCase.output)
			if model != testCase.model || ok != (testCase.model != "") {
				t.Fatalf("ConfirmedModel = (%q, %t), want %q", model, ok, testCase.model)
			}
		})
	}
}

// The daemon reads the contract an image confirmed from the probe pod's log. Only an OK line that
// ends with the Go token confirms one: the TypeScript CLI's line ends with its own
// `daemon-api-version=`, and a CLI that predates the Go check prints none.
func TestConfirmedContractReadsOnlyTheGoTokenOnAnOKLine(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		output   string
		contract int
		ok       bool
	}{
		{"the Go line among other output", "[legion] OMP pi.agents probe failed transiently\n" + OKLine("/opt/omp/bin/omp", "", 3) + "\n", 3, true},
		{"another number", OKLine("/opt/omp/bin/omp", "", 12), 12, true},
		{"a line with the model token", OKLine("/opt/omp/bin/omp", "anthropic/claude-fable-5-1-legion", 3), 3, true},
		{"the TypeScript CLI's line", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=8", 0, false},
		{"a CLI that predates the contract check", "probe-image: OK (/opt/omp/bin/omp) session-storage=probed", 0, false},
		{"a CLI that predates the session-storage probe", "probe-image: OK (/opt/omp/bin/omp)", 0, false},
		{"the token before the line's end", "probe-image: OK (/opt/omp/bin/omp) go-daemon-api-version=3 session-storage=probed", 0, false},
		{"the token on a line that is not the OK line", "[legion] go-daemon-api-version=3", 0, false},
		{"no digits", "probe-image: OK (/opt/omp/bin/omp) go-daemon-api-version=", 0, false},
		{"a number no int holds", "probe-image: OK (/opt/omp/bin/omp) go-daemon-api-version=99999999999999999999", 0, false},
		{"nothing", "", 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			contract, ok := ConfirmedContract(testCase.output)
			if contract != testCase.contract || ok != testCase.ok {
				t.Fatalf("ConfirmedContract = (%d, %t), want (%d, %t)", contract, ok, testCase.contract, testCase.ok)
			}
		})
	}
}
