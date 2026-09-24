// Package bootprobe is what Legion's boot probes share: the retry that waits out a probe's
// transient failures (retryBootProbe, packages/daemon/src/daemon/boot-probes.ts:124-161), and the
// OK line `legion probe-image` prints inside the worker image, which the daemon's probe Sandbox
// reads back from the pod's log (internal/runtime/sandbox/probe.go). The daemon's plugin gate,
// the image's own probe, and the probe Sandbox all run through Run, so a probe's verdict means the
// same thing wherever it ran.
package bootprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"
)

// Retry is the wait between attempts whose failure is transient: after the i-th such failure the
// probe waits min(Initial·2^i, Max). Attempts bounds the number of attempts; zero leaves it
// unbounded, so the probe runs until it passes or is refused.
type Retry struct {
	Initial, Max time.Duration
	Attempts     int
}

// Daemon is a daemon's policy (DAEMON_PROBE_RETRY, boot-probes.ts:38-51): 10 s doubling to 5
// min, unbounded. A transient failure is Oh My Pi dying under host load, or an attempt cut off
// before it answered; neither says anything about what is probed, so the daemon waits the load
// out inside its process rather than exiting into a supervisor that relaunches it into the same
// load.
var Daemon = Retry{Initial: 10 * time.Second, Max: 5 * time.Minute}

// Image is `legion probe-image`'s (IMAGE_PROBE_RETRY, boot-probes.ts:53-55): the daemon's backoff
// bounded to six attempts, about five minutes of waiting at worst, because an image build has no
// supervisor and must finish.
var Image = Retry{Initial: 10 * time.Second, Max: 5 * time.Minute, Attempts: 6}

// Outcome is one attempt: passed; refused, an answer no retry changes; or neither — transient,
// with the detail its retry is logged with.
type Outcome struct {
	Passed  bool
	Refusal error
	Detail  string
}

// Run runs attempt until it passes, is refused, or has failed transiently retry.Attempts times,
// logging each transient failure with the wait before the next attempt. A refusal is returned as
// it is. A ctx that ends — during an attempt, whatever the attempt then returned, or during a
// wait — ends Run with an error wrapping ctx's, and starts no other attempt.
func Run(ctx context.Context, name string, retry Retry, log *slog.Logger, attempt func(context.Context) Outcome) error {
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return abandoned(name, err)
		}
		outcome := attempt(ctx)
		if err := ctx.Err(); err != nil {
			return abandoned(name, err)
		}
		switch {
		case outcome.Passed:
			return nil
		case outcome.Refusal != nil:
			return outcome.Refusal
		case retry.Attempts > 0 && i+1 >= retry.Attempts:
			message := fmt.Sprintf("the %s probe never completed within its retry budget (%d attempts)", name, retry.Attempts)
			if outcome.Detail != "" {
				message += ": " + outcome.Detail
			}
			return errors.New(message)
		}
		delay := retry.Initial
		for n := 0; n < i && delay < retry.Max; n++ {
			delay *= 2
		}
		delay = min(delay, retry.Max)
		label := strconv.Itoa(i + 1)
		if retry.Attempts > 0 {
			label += "/" + strconv.Itoa(retry.Attempts)
		}
		log.Warn("boot probe failed transiently; waiting to run it again",
			"probe", name, "attempt", label, "retryIn", delay.String(), "detail", outcome.Detail)
		wait := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			wait.Stop()
			return abandoned(name, ctx.Err())
		case <-wait.C:
		}
	}
}

func abandoned(name string, err error) error {
	return fmt.Errorf("the %s probe was abandoned: it was stopped while it ran or waited to run again: %w", name, err)
}

// sessionStorageMark is on the OK line once the session-storage probe has passed
// (SESSION_STORAGE_PROBE_MARK, boot-probes.ts:398-403): a CLI that predates that probe prints no
// such token, having checked nothing about the setting.
const sessionStorageMark = "session-storage=probed"

// OKPrefix begins the line `legion probe-image` prints when every probe passed.
const OKPrefix = "probe-image: OK"

// OKLine is that line: the OMP invocation probed, the session-storage mark, and the Go daemon API
// contract the image's plugin declared. The daemon's probe Sandbox passes the image only on a line
// that confirms the daemon's own contract (ConfirmedContract).
func OKLine(omp string, contract int) string {
	return fmt.Sprintf("%s (%s) %s go-daemon-api-version=%d", OKPrefix, omp, sessionStorageMark, contract)
}

// confirmation is an OK line ending with the Go contract token. The space before the token keeps
// the TypeScript CLI's own `daemon-api-version=<N>` from reading as one.
var confirmation = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(OKPrefix) + ` .* go-daemon-api-version=([0-9]+)$`)

// ConfirmedContract is the contract an OK line in output confirmed, and false when output holds
// none: no OK line, or one from a CLI that predates the Go contract check.
func ConfirmedContract(output string) (int, bool) {
	match := confirmation.FindStringSubmatch(output)
	if match == nil {
		return 0, false
	}
	contract, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return contract, true
}
