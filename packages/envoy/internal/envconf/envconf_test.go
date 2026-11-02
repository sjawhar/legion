package envconf_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/envconf"
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
