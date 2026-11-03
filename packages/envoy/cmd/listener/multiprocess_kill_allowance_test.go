//go:build multiprocess

package main

import (
	"testing"
	"time"
)

// The check the SIGKILL rows end on (mpServingPort.killAllowanceViolations, which
// requireOnceExceptInFlight fails on) allows the one repeat a hard take-over can make: a message
// the killed pod pushed, whose session took it, and whose delivered record the pod died before
// writing. A message its session never took before the kill was never pushed by the killed pod -
// it was queued behind a push still in flight, or not yet pulled - so nothing but the pod taking
// over pushes it, and a second prompt of it is a fault of the take-over, as a second prompt of any
// message published after the kill is. The rows publish to sessions whose first message is held at
// the port, so every later message of such a session is one of these. At 119087ff the check lets
// such a message reach its session twice: it counts a message as in flight at the kill when its
// session had not taken it at all (`!taken || took.After(...)`), so the row passes over exactly the
// repeat it exists to refuse (INFRA-557 tester's round 25).
func TestTheKillCheckRefusesARepeatOfAMessageTheKilledPodNeverPushed(t *testing.T) {
	killed := time.Now()
	queued := mpMessage{event: "evt-mp-queued", session: "ses_mp_kill_queued", published: killed.Add(-5 * time.Second)}
	serving := &mpServingPort{prompts: map[string][]mpPrompt{
		queued.session: {
			{event: queued.event, at: killed.Add(10 * time.Second)},
			{event: queued.event, at: killed.Add(11 * time.Second)},
		},
	}}
	if violations := serving.killAllowanceViolations([]mpMessage{queued}, killed, mpKillInFlight); len(violations) == 0 {
		t.Fatalf("a message published 5 s before the kill and first taken 10 s after it, by the pod taking over, was prompted twice and the check passed it; the killed pod never pushed it, so it must reach its session once")
	}
}
