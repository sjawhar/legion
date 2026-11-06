package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/sessiontiming"
)

// refreshesBeforeExpiry is how many refreshes the renewal window leaves room for: a registration
// that keeps its session live only until the live window closes is renewed at under a third of
// that window, so two whole refreshes may fail — a listener restart, a dropped connection, a pass
// that held the plugin — and the third still renews the registration before the window closes.
const refreshesBeforeExpiry = 3

// sessionTiming is one set of the numbers the session-timing rule weighs: how often a pane
// refreshes its listener registration (interval), how long the registration the listener accepted
// keeps that session live (liveWindow), and what one whole listener call costs (attempts of
// timeout, with retryDelay between them).
type sessionTiming struct {
	interval   time.Duration
	liveWindow time.Duration
	attempts   int
	timeout    time.Duration
	retryDelay time.Duration
}

// callBudget is one whole listener call's round trip: every attempt's timeout, plus the retry
// delays between them.
func (s sessionTiming) callBudget() time.Duration {
	return time.Duration(s.attempts)*s.timeout + time.Duration(s.attempts-1)*s.retryDelay
}

// renewalWindow is how long after an accepted registration the refresh that renews it has
// finished, in the worst case the rule makes room for: refreshesBeforeExpiry intervals until that
// refresh is posted, and one whole call for it to land.
func (s sessionTiming) renewalWindow() time.Duration {
	return refreshesBeforeExpiry*s.interval + s.callBudget()
}

// sessionTimingProblems is the session-timing rule itself: it returns one line for each part of
// the rule s breaks, each naming both sides of its comparison with their numbers, and nothing at
// all when s holds the rule. It lives in this test file because no part of the product weighs
// these numbers against each other at runtime; a function only a test reads would be dead code in
// the package it was declared in.
func sessionTimingProblems(s sessionTiming) []string {
	var problems []string
	if renewal := s.renewalWindow(); renewal >= s.liveWindow {
		problems = append(problems, fmt.Sprintf(
			"the renewal window is %s (%d refreshes of %s, plus one listener call of %s), which is not strictly inside the live window %s",
			millis(renewal), refreshesBeforeExpiry, millis(s.interval), millis(s.callBudget()), millis(s.liveWindow),
		))
	}
	return problems
}

// millis renders d as whole milliseconds, grouped in threes: 280250 reads as "280,250 ms", the
// way the rule's own numbers are written down.
func millis(d time.Duration) string {
	digits := strconv.FormatInt(int64(d/time.Millisecond), 10)
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return grouped.String() + " ms"
}

// TestDAEMONTESTTheSessionTimingRuleHoldsAtTheValuesTheBuildUses weighs the session-timing rule
// over the values this build compiles, not over the text they are written in.
//
// A live pane keeps its Envoy registration by refreshing it: a session's last registration keeps
// it live only until the listener's live window closes, and every refresh renews it. The numbers
// that decide whether a live pane keeps reading live are declared once each in packages/contracts
// and generated into both languages, and this test holds the arithmetic over them. When that
// arithmetic breaks, a pane whose process is still running reads unresponsive, and every reader
// that asks the listener for live sessions alone is told it is not there: messages to it are
// refused, its role lookups answer gone, and nothing logs an error.
//
// The rule has one part, and sessionTimingProblems is it: three refresh intervals plus one whole
// listener call fall strictly short of the live window. A sum equal to the window fails as surely
// as one past it. The window closes that long after the listener accepted a write, while the
// third refresh is only posted three intervals after that acceptance and still owes its round
// trip, and setInterval only ever fires late; at equality the third refresh's budgeted completion
// reaches the very instant the window closes, leaving no scheduling margin at all. The call is
// budgeted as every attempt's timeout plus the retry delays between them (resolveCallTiming,
// packages/envoy-client/src/transport.ts).
//
// The pass deadline is not in that sum, and that is not an oversight. A pass that stalls does not
// hold the refresh back until its deadline: the heartbeat's own backstop posts the refresh at the
// first tick where a pass is still running and the last accepted registration is a whole interval
// old, whatever the deadline is. So the gap a stalled pass can open between two accepted
// registrations is at most two intervals and one listener call, which the sum above already
// covers. MIN_PASS_DEADLINE_MS, and ENVOY_PASS_DEADLINE_MS over it, only floor how long one pass
// may run, and raising either must not fail this test.
//
// Three copies of these numbers are then checked against the declarations they must equal,
// because each one feeds a decision of the daemon's own: pluginHeartbeat and the two listener
// windows, listenerSessionTTL and listenerClaimStale, are noticeReholdDelays
// (notice_exceptions.go): when each copy of a notice the listener could not forward is re-sent.
// The second copy goes one pane heartbeat after the first, to catch a pane that has just
// relaunched and registered; the last goes past both listener windows with a minute to spare,
// when any registration a stopped session left behind must have lapsed. A copy that drifted low
// sends that last notice while the listener still names a holder that is gone, so the forward is
// refused and the copy dropped; a copy that drifted high holds the notice past the window it
// could have been caught in.
//
// What this test cannot bind:
//
//   - A pane started with ENVOY_HEARTBEAT_MS set: that overrides the interval for that pane
//     alone, and no declaration records it.
//   - A listener started with ENVOY_SESSION_LIVE_WINDOW set: envconf.SessionWindows gives that
//     deployment a live window of its own, which no test of the source can see. Only the
//     declared window is weighed here.
func TestDAEMONTESTTheSessionTimingRuleHoldsAtTheValuesTheBuildUses(t *testing.T) {
	build := sessionTiming{
		interval:   sessiontiming.Heartbeat,
		liveWindow: sessiontiming.LiveWindow,
		attempts:   sessiontiming.CallAttempts,
		timeout:    sessiontiming.CallTimeout,
		retryDelay: sessiontiming.CallRetryDelay,
	}
	for _, problem := range sessionTimingProblems(build) {
		t.Error(problem)
	}

	for _, copied := range []struct {
		name     string
		value    time.Duration
		wantName string
		want     time.Duration
	}{
		{"pluginHeartbeat", pluginHeartbeat, "sessiontiming.Heartbeat", sessiontiming.Heartbeat},
		{"listenerSessionTTL", listenerSessionTTL, "sessiontiming.LiveWindow", sessiontiming.LiveWindow},
		{"listenerClaimStale", listenerClaimStale, "sessiontiming.ClaimStale", sessiontiming.ClaimStale},
	} {
		if copied.value != copied.want {
			t.Errorf("%s is %s, but %s is %s", copied.name, millis(copied.value), copied.wantName, millis(copied.want))
			continue
		}
		t.Logf("%s is %s, the same as %s", copied.name, millis(copied.value), copied.wantName)
	}

	// The failing answer, proved in the same green run: a set the rule must reject has to be
	// named by it, with both sides of the comparison in the line it returns. The second set is
	// the boundary the doc comment above turns on — a renewal window exactly equal to the live
	// window — which a rule weakened from `>=` to `>` would pass in silence while the first set,
	// far past the window, still failed.
	pastTheWindow := build
	pastTheWindow.interval = 290_000 * time.Millisecond
	atTheWindow := build
	atTheWindow.liveWindow = build.renewalWindow()
	for _, rejected := range []struct {
		what string
		set  sessionTiming
	}{
		{"an interval of " + millis(pastTheWindow.interval), pastTheWindow},
		{"a live window of " + millis(atTheWindow.liveWindow) + ", the renewal window exactly", atTheWindow},
	} {
		sum, window := millis(rejected.set.renewalWindow()), millis(rejected.set.liveWindow)
		problems := sessionTimingProblems(rejected.set)
		named := false
		for _, problem := range problems {
			if strings.Contains(problem, sum) && strings.Contains(problem, window) {
				named = true
				t.Logf("%s is rejected: %s", rejected.what, problem)
			}
		}
		if !named {
			t.Errorf("%s was not rejected naming both %s and %s; the rule returned %v",
				rejected.what, sum, window, problems)
		}
	}

	// The numbers this build weighed, logged only where every check above held, so no line here
	// can read as "strictly inside" a live window the rule has just reported it outside of. They
	// appear under `go test -v` alone, which is why CI's own step is unaffected by them.
	if t.Failed() {
		return
	}

	t.Logf("refresh interval %s (sessiontiming.Heartbeat)", millis(build.interval))
	t.Logf("live window %s (sessiontiming.LiveWindow)", millis(build.liveWindow))
	t.Logf("claim stale window %s (sessiontiming.ClaimStale)", millis(sessiontiming.ClaimStale))
	t.Logf("one listener call: %d attempts (sessiontiming.CallAttempts) of %s (sessiontiming.CallTimeout), %s between them (sessiontiming.CallRetryDelay)",
		build.attempts, millis(build.timeout), millis(build.retryDelay))
	t.Logf("call budget %s = %d * %s + %d * %s",
		millis(build.callBudget()), build.attempts, millis(build.timeout), build.attempts-1, millis(build.retryDelay))
	t.Logf("renewal window %s = %d * %s + %s, strictly inside the live window %s",
		millis(build.renewalWindow()), refreshesBeforeExpiry, millis(build.interval), millis(build.callBudget()), millis(build.liveWindow))
}
