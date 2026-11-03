// The frames a row's sessions answered, and the kill allowance the SIGKILL rows of the
// two-pod suite end on. They are outside the multiprocess tag, so envoy-go's untagged unit run
// exercises the check: the only CI lane of that tag runs the two-pod rows alone, and a guard no
// lane runs guards nothing. The session stand-in that fills the log in, and the row helpers that
// read it, are the tagged harness's (multiprocess_harness_test.go).

package main

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// mpKillInFlight is how far on either side of a SIGKILL a message's first prompt may land and
// still count as the killed pod's push in flight at the kill (requireOnceExceptInFlight). A pod
// that pushed a message earlier than this had time to record it delivered, so the pod taking
// over skips it and a repeat of it is a fault, while one pushed inside the window may have died
// with its record unwritten; a first prompt later than this is the successor's push. A
// delivered record is one KV write, milliseconds on the box the rows run on; a second is three
// orders of magnitude over that and still far under the 6 s ack wait that brings the message
// back, and under the successor's first push after the kill, which the 6 s ack wait and the
// settle hold put seconds later.
const mpKillInFlight = time.Second

// mpMessage is one message a row published to a session.
type mpMessage struct {
	event     string
	session   string
	published time.Time
}

// mpPrompt is one frame a session took: the event id it carried, and when the session answered
// it.
type mpPrompt struct {
	event string
	at    time.Time
}

// mpPromptLog is the frames the row's sessions answered, by session id, in the order they
// answered them: the part of the harness's session stand-in (mpNATSSession, which embeds it)
// that the row checks read. A frame held or naked is no entry here: the session never took it.
type mpPromptLog struct {
	mu      sync.Mutex
	prompts map[string][]mpPrompt
}

// took records that sessionID took event at a moment.
func (l *mpPromptLog) took(sessionID, event string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.prompts == nil {
		l.prompts = map[string][]mpPrompt{}
	}
	l.prompts[sessionID] = append(l.prompts[sessionID], mpPrompt{event: event, at: at})
}

// counts is how many frames were answered for each event id.
func (l *mpPromptLog) counts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := map[string]int{}
	for _, prompts := range l.prompts {
		for _, prompt := range prompts {
			counts[prompt.event]++
		}
	}
	return counts
}

// promptsTo is how many frames sessionID answered.
func (l *mpPromptLog) promptsTo(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prompts[sessionID])
}

// first is when event's first prompt was answered, and whether it was.
func (l *mpPromptLog) first(sessionID, event string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, prompt := range l.prompts[sessionID] {
		if prompt.event == event {
			return prompt.at, true
		}
	}
	return time.Time{}, false
}

// last is when event's last prompt was answered, and whether any was.
func (l *mpPromptLog) last(sessionID, event string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var at time.Time
	var took bool
	for _, prompt := range l.prompts[sessionID] {
		if prompt.event == event {
			at, took = prompt.at, true
		}
	}
	return at, took
}

// order is the event ids sessionID took, each at its first prompt, in the order it took them.
func (l *mpPromptLog) order(sessionID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var order []string
	for _, prompt := range l.prompts[sessionID] {
		if !slices.Contains(order, prompt.event) {
			order = append(order, prompt.event)
		}
	}
	return order
}

// killAllowanceViolations reports, one line each, the messages that did not reach their session,
// and those that reached it more often than the kill allowance permits: every message reaches its
// session exactly once, except that a message whose delivery was still in flight at moment - the
// instant a pod was killed - may have reached it twice: that is the one repeat a hard take-over
// allows (cmd/listener/README.md, "A repeat with the same event id is possible after a hard
// take-over"), because the killed pod died between the push its session took and the delivered
// record that would have told the pod taking over.
//
// A message counts as in flight at moment when it was published before it and its session took it,
// by its first prompt, within window on either side of it. Only such a first prompt can be the
// killed pod's own push: one more than window before the kill is a push the killed pod had time to
// record, and one more than window after it is a push of the pod taking over, since a message the
// session never took before the kill (queued behind a held push, or not pulled yet) was pushed by
// nothing but the successor, and a repeat of either is a fault of the take-over. The window
// reaches past the kill too because the port records a prompt when its request arrives, and the
// killed pod's last push can still be on its way, or in the port's handler, at the kill instant.
// A prompt names no pod, and attributing one exactly would take a socket probe this harness does
// not carry - the request's source port, its socket inode in /proc/net/tcp, and the pod holding
// that socket under /proc/<pid>/fd - so the window around the kill is what marks a first prompt as
// the killed pod's.
func (l *mpPromptLog) killAllowanceViolations(messages []mpMessage, moment time.Time, window time.Duration) []string {
	counts := l.counts()
	var wrong []string
	for _, m := range messages {
		count, limit, want := counts[m.event], 1, "once"
		took, taken := l.first(m.session, m.event)
		if m.published.Before(moment) && taken && took.After(moment.Add(-window)) && took.Before(moment.Add(window)) {
			limit, want = 2, "once, or twice as a push in flight at the kill"
		}
		if count >= 1 && count <= limit {
			continue
		}
		wrong = append(wrong, fmt.Sprintf("%s to %s prompted %d times, want %s", m.event, m.session, count, want))
	}
	return wrong
}

// notOnce reports, one line each, the messages a session did not take exactly once: what
// requireOnce fails on. A frame whose reply reached its relay, and whose acknowledgement
// reached the server, is one of these wherever the row puts it - the pod taking over restarts
// the session's consumer at an ack floor past it - so a repeat of such a frame is a fault
// however the links behaved around it.
func (l *mpPromptLog) notOnce(messages []mpMessage) []string {
	counts := l.counts()
	var wrong []string
	for _, m := range messages {
		if counts[m.event] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s to %s prompted %d times", m.event, m.session, counts[m.event]))
		}
	}
	return wrong
}

// The check the links-dropped row ends on for the frames around the drop (mpPromptLog.notOnce,
// which requireOnce fails on) refuses a repeat of a frame the session answered before its links
// dropped: its reply reached the relay and its acknowledgement the server, so the pod that
// restarts the consumer at the ack floor passes over it. Only the frame whose reply the drop
// lost may come again, under its own event id
// (TestTwoPodsARelayedFrameTheLinksDroppedUnderIsRepeatedAtMostOnceWithItsId).
func TestTheExactlyOnceCheckRefusesARepeatOfAFrameAnsweredBeforeTheDrop(t *testing.T) {
	dropped := time.Now()
	justBefore := mpMessage{event: "evt-mp-just-before", session: "ses_mp_relay_drop_once", published: dropped.Add(-time.Second)}
	log := &mpPromptLog{prompts: map[string][]mpPrompt{
		justBefore.session: {
			{event: justBefore.event, at: dropped.Add(-900 * time.Millisecond)},
			{event: justBefore.event, at: dropped.Add(2 * time.Second)},
		},
	}}
	if wrong := log.notOnce([]mpMessage{justBefore}); len(wrong) == 0 {
		t.Fatal("a frame the session answered a moment before the drop was taken twice and the check passed it; only the frame whose reply the drop lost may come again")
	}
}

// The check the SIGKILL rows end on (mpPromptLog.killAllowanceViolations, which
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
	log := &mpPromptLog{prompts: map[string][]mpPrompt{
		queued.session: {
			{event: queued.event, at: killed.Add(10 * time.Second)},
			{event: queued.event, at: killed.Add(11 * time.Second)},
		},
	}}
	if violations := log.killAllowanceViolations([]mpMessage{queued}, killed, mpKillInFlight); len(violations) == 0 {
		t.Fatalf("a message published 5 s before the kill and first taken 10 s after it, by the pod taking over, was prompted twice and the check passed it; the killed pod never pushed it, so it must reach its session once")
	}
}
