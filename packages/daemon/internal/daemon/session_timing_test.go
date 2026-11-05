package daemon

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/sessiontiming"

	"github.com/sjawhar/legion/daemon/internal/controller"
)

// refreshesBeforeExpiry is how many refreshes the renewal window leaves room for: a record that
// expires unless it is refreshed is renewed at a third of its lifetime, so two whole refreshes
// may fail — a listener restart, a dropped connection, a pass that held the plugin — and the
// third still renews the record before it expires.
const refreshesBeforeExpiry = 3

// sessionTiming is one set of the numbers the session-timing rule weighs: how often a pane
// refreshes its listener registration (interval), how long the listener keeps that registration
// after the write it accepted (lifetime), what one whole listener call costs (attempts of
// timeout, with retryDelay between them), and how many intervals wide the daemon's staleness
// window is (windowFactor).
type sessionTiming struct {
	interval     time.Duration
	lifetime     time.Duration
	attempts     int
	timeout      time.Duration
	retryDelay   time.Duration
	windowFactor int
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

// stalenessWindow is the heartbeat arm of controller.LivenessWindow: how stale the controller
// holder's last_seen may be before the daemon counts the controller gone.
func (s sessionTiming) stalenessWindow() time.Duration {
	return time.Duration(s.windowFactor) * s.interval
}

// sessionTimingProblems is the session-timing rule itself: it returns one line for each part of
// the rule s breaks, each naming both sides of its comparison with their numbers, and nothing at
// all when s holds the rule. It lives in this test file because no part of the product weighs
// these numbers against each other at runtime; a function only a test reads would be dead code in
// the package it was declared in.
func sessionTimingProblems(s sessionTiming) []string {
	var problems []string
	if renewal := s.renewalWindow(); renewal >= s.lifetime {
		problems = append(problems, fmt.Sprintf(
			"the renewal window is %s (%d refreshes of %s, plus one listener call of %s), which is not strictly inside the record lifetime %s",
			millis(renewal), refreshesBeforeExpiry, millis(s.interval), millis(s.callBudget()), millis(s.lifetime),
		))
	}
	if staleness := s.stalenessWindow(); staleness < s.lifetime {
		problems = append(problems, fmt.Sprintf(
			"the daemon's staleness window is %s (%d heartbeats of %s), which does not cover the record lifetime %s",
			millis(staleness), s.windowFactor, millis(s.interval), millis(s.lifetime),
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
// A live pane keeps its Envoy registration by refreshing it: the pane's session record in the
// listener's session bucket expires on its own, and every refresh renews it. The numbers that
// decide whether a live pane stays registered are declared once each in packages/contracts and
// generated into both languages, and this test holds the arithmetic over them. When that
// arithmetic breaks, a pane whose process is still running has its record expire, and every other
// agent asking the listener for it is told there is no live session: messages to it are refused,
// its role lookups answer gone, and nothing logs an error.
//
// The rule has two parts, and sessionTimingProblems is both of them:
//
//  1. Three refresh intervals plus one whole listener call fall strictly short of the record
//     lifetime. A sum equal to the lifetime fails as surely as one past it. The record expires
//     that long after the listener accepted a write, while the third refresh is only posted three
//     intervals after that acceptance and still owes its round trip, and setInterval only ever
//     fires late; at equality the third refresh's budgeted completion reaches the very instant
//     the record expires, leaving no scheduling margin at all. The call is budgeted as every
//     attempt's timeout plus the retry delays between them (resolveCallTiming,
//     packages/envoy-client/src/transport.ts).
//
//     The pass deadline is not in that sum, and that is not an oversight. A pass that stalls does
//     not hold the refresh back until its deadline: the heartbeat's own backstop posts the
//     refresh at the first tick where a pass is still running and the last accepted registration
//     is a whole interval old, whatever the deadline is. So the gap a stalled pass can open
//     between two accepted registrations is at most two intervals and one listener call, which
//     the sum above already covers. MIN_PASS_DEADLINE_MS, and ENVOY_PASS_DEADLINE_MS over it,
//     only floor how long one pass may run, and raising either must not fail this test.
//
//  2. The daemon's staleness window covers the record lifetime: windowFactor intervals are at
//     least one lifetime. A live holder's last_seen can reach the lifetime, since that is how
//     long the listener keeps the record, so a narrower window reports a live controller gone.
//
// Four copies of these numbers are then checked against the declarations they must equal, because
// each one feeds a different decision:
//
//   - controller.Heartbeat is the heartbeat arm of controller.LivenessWindow, part 2's window.
//   - pluginHeartbeat and the two listener windows, listenerSessionTTL and listenerClaimStale,
//     are noticeReholdDelays (notice_exceptions.go): when each copy of a notice the listener
//     could not forward is re-sent. The second copy goes one pane heartbeat after the first, to
//     catch a pane that has just relaunched and registered; the last goes past both listener
//     windows with a minute to spare, when any registration a stopped session left behind must
//     have lapsed. A copy that drifted low sends that last notice while the listener still names
//     a holder that is gone, so the forward is refused and the copy dropped; a copy that drifted
//     high holds the notice past the window it could have been caught in.
//
// What this test cannot bind:
//
//   - A pane started with ENVOY_HEARTBEAT_MS set: that overrides the interval for that pane
//     alone, and no declaration records it.
//   - The bootTimeout arm of controller.LivenessWindow, which is deployment configuration no test
//     of the source can see. Only the heartbeat arm is weighed here.
//   - The live session bucket's own MaxAge. The bucket is created only when it does not exist, and
//     OpenSessionRegistry adopts whatever MaxAge the live bucket reports
//     (packages/envoy/internal/session/registry.go), so a bucket an older build created keeps its
//     old lifetime however the declaration moves. The live value is in the listener's boot log,
//     which names the record lifetime it resolved.
func TestDAEMONTESTTheSessionTimingRuleHoldsAtTheValuesTheBuildUses(t *testing.T) {
	build := sessionTiming{
		interval:     sessiontiming.Heartbeat,
		lifetime:     sessiontiming.RecordLifetime,
		attempts:     sessiontiming.CallAttempts,
		timeout:      sessiontiming.CallTimeout,
		retryDelay:   sessiontiming.CallRetryDelay,
		windowFactor: controller.StalenessWindowHeartbeats,
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
		{"controller.Heartbeat", controller.Heartbeat, "sessiontiming.Heartbeat", sessiontiming.Heartbeat},
		{"pluginHeartbeat", pluginHeartbeat, "sessiontiming.Heartbeat", sessiontiming.Heartbeat},
		{"listenerSessionTTL", listenerSessionTTL, "sessiontiming.RecordLifetime", sessiontiming.RecordLifetime},
		{"listenerClaimStale", listenerClaimStale, "sessiontiming.ClaimStale", sessiontiming.ClaimStale},
	} {
		if copied.value != copied.want {
			t.Errorf("%s is %s, but %s is %s", copied.name, millis(copied.value), copied.wantName, millis(copied.want))
			continue
		}
		t.Logf("%s is %s, the same as %s", copied.name, millis(copied.value), copied.wantName)
	}

	// The failing answer, proved in the same green run: a tree whose refresh interval has been
	// moved past what the lifetime can hold must be named by the rule, with both sides of the
	// comparison in the line it returns. Only part 1 breaks here, since a wider interval only
	// widens the staleness window.
	broken := build
	broken.interval = 290_000 * time.Millisecond
	brokenSum, brokenLifetime := millis(broken.renewalWindow()), millis(broken.lifetime)
	problems := sessionTimingProblems(broken)
	named := false
	for _, problem := range problems {
		if strings.Contains(problem, brokenSum) && strings.Contains(problem, brokenLifetime) {
			named = true
			t.Logf("an interval of %s is rejected: %s", millis(broken.interval), problem)
		}
	}
	if !named {
		t.Errorf("an interval of %s at a lifetime of %s was not rejected naming both %s and %s; the rule returned %v",
			millis(broken.interval), brokenLifetime, brokenSum, brokenLifetime, problems)
	}

	// And the other part, so neither half of the rule can be written backwards unnoticed: a
	// staleness window three heartbeats wide is 270,000 ms, short of the 300,000 ms a live
	// holder's last_seen can reach, which is why StalenessWindowHeartbeats is four.
	narrow := build
	narrow.windowFactor = 3
	narrowWindow, narrowLifetime := millis(narrow.stalenessWindow()), millis(narrow.lifetime)
	problems = sessionTimingProblems(narrow)
	named = false
	for _, problem := range problems {
		if strings.Contains(problem, narrowWindow) && strings.Contains(problem, narrowLifetime) {
			named = true
			t.Logf("a staleness window of %d heartbeats is rejected: %s", narrow.windowFactor, problem)
		}
	}
	if !named {
		t.Errorf("a staleness window of %d heartbeats (%s) at a lifetime of %s was not rejected naming both; the rule returned %v",
			narrow.windowFactor, narrowWindow, narrowLifetime, problems)
	}

	// The numbers this build weighed, logged only where every check above held, so no line here
	// can read as "strictly inside" a lifetime the rule has just reported it outside of. They
	// appear under `go test -v` alone, which is why CI's own step is unaffected by them.
	if t.Failed() {
		return
	}

	t.Logf("refresh interval %s (sessiontiming.Heartbeat)", millis(build.interval))
	t.Logf("record lifetime %s (sessiontiming.RecordLifetime)", millis(build.lifetime))
	t.Logf("claim stale window %s (sessiontiming.ClaimStale)", millis(sessiontiming.ClaimStale))
	t.Logf("one listener call: %d attempts (sessiontiming.CallAttempts) of %s (sessiontiming.CallTimeout), %s between them (sessiontiming.CallRetryDelay)",
		build.attempts, millis(build.timeout), millis(build.retryDelay))
	t.Logf("staleness window %d heartbeats wide (controller.StalenessWindowHeartbeats)", build.windowFactor)
	t.Logf("call budget %s = %d * %s + %d * %s",
		millis(build.callBudget()), build.attempts, millis(build.timeout), build.attempts-1, millis(build.retryDelay))
	t.Logf("renewal window %s = %d * %s + %s, strictly inside the record lifetime %s",
		millis(build.renewalWindow()), refreshesBeforeExpiry, millis(build.interval), millis(build.callBudget()), millis(build.lifetime))
	t.Logf("staleness window %s = %d * %s, covering the record lifetime %s",
		millis(build.stalenessWindow()), build.windowFactor, millis(build.interval), millis(build.lifetime))
}
