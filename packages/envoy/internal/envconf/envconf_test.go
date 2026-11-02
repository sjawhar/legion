package envconf_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/envconf"
	"github.com/sjawhar/envoy/internal/session"
)

// environment is a program's own read of its environment, as os.LookupEnv and Dispatch's
// settings table both answer: the value and whether it is set.
func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// logged is a logger writing to the buffer a test reads back, as each program's own logger
// writes to its stderr.
func logged() (*slog.Logger, *bytes.Buffer) {
	var buffer bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buffer, nil)), &buffer
}

// A period or a window an operator wrote as zero or a negative value is refused by name, and the
// program does not start: ENVOY_SESSION_REAP_INTERVAL=0 reached time.NewTicker and panicked the
// listener's reaper loops before these readers existed. A value time.ParseDuration cannot read
// is a WARN and the default, since a program never refuses to start over a malformed duration.
// A setting where zero says something - ENVOY_UNPLACEABLE_RETRY_WINDOW=0s, which asks for no
// retry at all - is read through Duration, which takes it.
func TestPositiveDurationRefusesADurationThatIsNotPositive(t *testing.T) {
	const name = "ENVOY_SESSION_REAP_INTERVAL"
	for _, value := range []string{"0", "0s", "-1m"} {
		t.Run(value, func(t *testing.T) {
			logger, _ := logged()
			got, err := envconf.PositiveDuration(logger, environment(map[string]string{name: value}), name, time.Minute)
			if want := fmt.Sprintf("invalid %s %q", name, value); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("PositiveDuration(%q) = %s, %v; want a refusal naming %s and %q", value, got, err, name, value)
			}
		})
	}
	t.Run("a malformed duration still starts on the default", func(t *testing.T) {
		logger, logs := logged()
		got, err := envconf.PositiveDuration(logger, environment(map[string]string{name: "1 hour"}), name, time.Minute)
		if got != time.Minute || err != nil {
			t.Fatalf("PositiveDuration(\"1 hour\") = %s, %v; want the default and no refusal", got, err)
		}
		if !strings.Contains(logs.String(), "invalid "+name) {
			t.Fatalf("the log does not warn about the malformed value:\n%s", logs.String())
		}
	})
	t.Run("a positive duration is taken", func(t *testing.T) {
		logger, _ := logged()
		got, err := envconf.PositiveDuration(logger, environment(map[string]string{name: "30s"}), name, time.Minute)
		if got != 30*time.Second || err != nil {
			t.Fatalf("PositiveDuration(\"30s\") = %s, %v; want 30s", got, err)
		}
	})
	t.Run("an unset variable is the default", func(t *testing.T) {
		logger, logs := logged()
		got, err := envconf.PositiveDuration(logger, environment(nil), name, time.Minute)
		if got != time.Minute || err != nil || logs.Len() != 0 {
			t.Fatalf("PositiveDuration with %s unset = %s, %v, logging %q; want the default, no refusal and no warning", name, got, err, logs.String())
		}
	})
	t.Run("a setting zero speaks for keeps taking it", func(t *testing.T) {
		// ENVOY_UNPLACEABLE_RETRY_WINDOW=0s asks the listener to retry an unplaceable delivery
		// not at all; its own default is five minutes (defaultUnplaceableRetryWindow).
		const window = "ENVOY_UNPLACEABLE_RETRY_WINDOW"
		logger, _ := logged()
		if got := envconf.Duration(logger, environment(map[string]string{window: "0s"}), window, 5*time.Minute); got != 0 {
			t.Fatalf("Duration(\"0s\") = %s, want the zero window that asks for no retry", got)
		}
	})
}

// The listener and Dispatch judge the same role holder, so both read the two session windows
// through SessionWindows: the same variables, the same compiled defaults (session.LiveWindow and
// session.GoneHorizon) and the same refusal of a value that is not positive. Each hands it its
// own reader of the environment - the listener's os.LookupEnv, Dispatch's settings table - so
// neither program spells the pair out again.
func TestSessionWindowsReadsTheTwoWindowsBothProgramsCarry(t *testing.T) {
	t.Run("unset, each window is the package's own declaration", func(t *testing.T) {
		logger, logs := logged()
		live, horizon, err := envconf.SessionWindows(logger, environment(nil))
		if live != session.LiveWindow || horizon != session.GoneHorizon || err != nil {
			t.Fatalf("SessionWindows with neither variable set = %s, %s, %v; want %s, %s and no refusal", live, horizon, err, session.LiveWindow, session.GoneHorizon)
		}
		if logs.Len() != 0 {
			t.Fatalf("the log warns about a variable nobody set:\n%s", logs.String())
		}
	})
	t.Run("a deployment's own windows are taken", func(t *testing.T) {
		logger, _ := logged()
		live, horizon, err := envconf.SessionWindows(logger, environment(map[string]string{
			"ENVOY_SESSION_LIVE_WINDOW":  "30m",
			"ENVOY_SESSION_GONE_HORIZON": "3h",
		}))
		if live != 30*time.Minute || horizon != 3*time.Hour || err != nil {
			t.Fatalf("SessionWindows(30m, 3h) = %s, %s, %v; want 30m, 3h and no refusal", live, horizon, err)
		}
	})
	for _, name := range []string{"ENVOY_SESSION_LIVE_WINDOW", "ENVOY_SESSION_GONE_HORIZON"} {
		t.Run(name+" at zero refuses the start", func(t *testing.T) {
			logger, _ := logged()
			live, horizon, err := envconf.SessionWindows(logger, environment(map[string]string{name: "0s"}))
			if want := fmt.Sprintf("invalid %s %q", name, "0s"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("SessionWindows with %s=0s = %s, %s, %v; want a refusal naming %s", name, live, horizon, err, name)
			}
		})
	}
}
