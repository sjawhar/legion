//go:build multiprocess

// The accessors of the two-pod suite's frame log (mpFrameLog, frame_log_test.go) that
// only the tagged harness and its rows read. The log's type and its counts, and the
// exactly-once check the rows end on, stay outside the tag, so envoy-go's untagged unit run
// exercises that check; these are under it because a build without it calls none of them.

package main

import (
	"slices"
	"time"
)

// took records that sessionID took event at a moment.
func (l *mpFrameLog) took(sessionID, event string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.frames == nil {
		l.frames = map[string][]mpFrame{}
	}
	l.frames[sessionID] = append(l.frames[sessionID], mpFrame{event: event, at: at})
}

// framesTo is how many frames sessionID answered.
func (l *mpFrameLog) framesTo(sessionID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames[sessionID])
}

// first is when event's first frame was answered, and whether it was.
func (l *mpFrameLog) first(sessionID, event string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, frame := range l.frames[sessionID] {
		if frame.event == event {
			return frame.at, true
		}
	}
	return time.Time{}, false
}

// last is when event's last frame was answered, and whether any was.
func (l *mpFrameLog) last(sessionID, event string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var at time.Time
	var took bool
	for _, frame := range l.frames[sessionID] {
		if frame.event == event {
			at, took = frame.at, true
		}
	}
	return at, took
}

// order is the event ids sessionID took, each at its first frame, in the order it took them.
func (l *mpFrameLog) order(sessionID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var order []string
	for _, frame := range l.frames[sessionID] {
		if !slices.Contains(order, frame.event) {
			order = append(order, frame.event)
		}
	}
	return order
}
