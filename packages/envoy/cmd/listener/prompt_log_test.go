// The frames a row's sessions answered, the exactly-once check the rows of the two-pod suite end
// on, and mpLineWith, the log-line reader both suites read a line through. They are outside the
// multiprocess tag, so envoy-go's untagged unit run exercises the check and its own rows read
// their log lines through the one helper: the only CI lane of that tag runs the two-pod rows
// alone, and a guard no lane runs guards nothing. The session stand-in that fills the log in, and
// the row helpers that read it, are the tagged harness's (multiprocess_harness_test.go).

package main

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

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

// notOnce reports, one line each, the messages a session did not take exactly once: what
// requireOnce fails on. A row puts it on the frames whose stream sequences are at or below the
// ack floor every restart of the session's consumer reads: a repeat of one of those is a fault,
// however the links behaved around it. A frame above that floor is another matter, since every
// restart sends again every frame above it (bus.SessionConsumers.RestartAtFloor), so a row
// bounds those with requireAtMostTwice instead.
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

// The check the links-dropped row ends on for the frames below the ack floor (mpPromptLog.notOnce,
// which requireOnce fails on) refuses a repeat of a frame the session answered, and whose
// acknowledgement reached the server, while nothing below it was still held: the restart at the
// ack floor starts above such a frame. Only the frame whose reply the drop lost may come again,
// under its own event id
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

// mpLineWith is the first line of output containing every one of parts, or "".
func mpLineWith(output string, parts ...string) string {
	for line := range strings.SplitSeq(output, "\n") {
		if !slices.ContainsFunc(parts, func(part string) bool { return !strings.Contains(line, part) }) {
			return line
		}
	}
	return ""
}
