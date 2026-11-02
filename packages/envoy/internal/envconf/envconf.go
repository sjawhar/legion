// Package envconf reads a duration an operator may override through the environment, for every
// Envoy program that takes one: the listener's session windows and its two reaper periods, and
// the same two session windows in Dispatch, which judges a role route's holder by them
// (roles.Windows, cmd/dispatch's roleHolderWindows). Both programs read a setting they share
// with the same rules, so a deployment that moves a window moves it for both.
//
// The environment is handed in rather than read here, so each program keeps its own one reader
// of it: the listener's os.LookupEnv, and Dispatch's settings table, which is the only place
// Dispatch reads its environment (cmd/dispatch/settings.go, TestNoReaderBypassesTheSettingsTable).
package envconf

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/session"
)

// Duration reads the duration name holds in environment: an unset or empty value leaves fallback
// standing, and so does one time.ParseDuration refuses, after a single WARN naming the variable -
// a program never refuses to start over a malformed duration. The WARN's message and fields are
// what an operator's log filter matches, so they name the variable exactly as it is written in
// the environment.
//
// It takes any duration it can read, zero and negative ones included, so it is the reader for a
// setting where zero says something: ENVOY_UNPLACEABLE_RETRY_WINDOW=0s asks for no retry of an
// unplaceable delivery at all (cmd/listener/unplaceable_delivery.go), and the acceptance test of
// the exception lane sets exactly that. A setting zero cannot mean is read through
// PositiveDuration instead.
func Duration(logger *slog.Logger, environment func(string) (string, bool), name string, fallback time.Duration) time.Duration {
	v, _ := environment(name)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		logger.Warn("invalid "+name+"; using default", slog.String("value", v), slog.String("error", err.Error()))
		return fallback
	}
	return d
}

// PositiveDuration is Duration for a period or a window no program can run on at zero: the live
// window and the gone horizon, which decide every session's state, and the reap intervals, each
// a ticker's period (time.NewTicker panics on a non-positive one, and both of the listener's
// reaper loops take one). A value that parses but is zero or negative is refused by name and the
// value, as bus.ParseSessionMaxAge refuses its own, and the program does not start: an operator
// who wrote a duration meant it, so starting on the default instead would serve a listener
// nobody asked for. An unset or malformed value is still the default, as above.
func PositiveDuration(logger *slog.Logger, environment func(string) (string, bool), name string, fallback time.Duration) (time.Duration, error) {
	d := Duration(logger, environment, name, fallback)
	if d <= 0 {
		value, _ := environment(name)
		return 0, fmt.Errorf("invalid %s %q: want a positive Go duration such as 1m", name, value)
	}
	return d, nil
}

// SessionWindows reads the two session windows both programs carry: ENVOY_SESSION_LIVE_WINDOW,
// how long after its last registration a session reads live, and ENVOY_SESSION_GONE_HORIZON,
// how long it then reads unresponsive before it reads gone. Each is read as PositiveDuration
// reads a window - the package's own compiled default for an unset variable, a WARN and that
// default for one no Go duration can be read from, and a refusal naming the variable for one
// that is not positive - and each defaults to session.LiveWindow or session.GoneHorizon, the
// declarations every reader of a session record judges its age against.
//
// The listener and Dispatch both judge the same holder, so both read these two the same way
// (dispatch://INFRA-392). They are read here, once, rather than spelled out in each program, so
// a change to either variable's name, default or rules reaches both. environment is the caller's
// own one reader of its environment, as everything in this package takes: the listener's
// os.LookupEnv, and Dispatch's settings table.
func SessionWindows(logger *slog.Logger, environment func(string) (string, bool)) (live, horizon time.Duration, err error) {
	live, err = PositiveDuration(logger, environment, "ENVOY_SESSION_LIVE_WINDOW", session.LiveWindow)
	if err != nil {
		return 0, 0, err
	}
	horizon, err = PositiveDuration(logger, environment, "ENVOY_SESSION_GONE_HORIZON", session.GoneHorizon)
	if err != nil {
		return 0, 0, err
	}
	return live, horizon, nil
}
