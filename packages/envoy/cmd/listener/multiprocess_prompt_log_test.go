//go:build multiprocess

// The accessors of the two-pod suite's prompt log (mpPromptLog, prompt_log_test.go) that
// only the tagged harness and its rows read. The log's type and its counts, and the
// exactly-once check the rows end on, stay outside the tag, so envoy-go's untagged unit run
// exercises that check; these are under it because a build without it calls none of them.

package main

import (
	"slices"
	"time"
)

// took records that sessionID took event at a moment.
func (l *mpPromptLog) took(sessionID, event string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.prompts == nil {
		l.prompts = map[string][]mpPrompt{}
	}
	l.prompts[sessionID] = append(l.prompts[sessionID], mpPrompt{event: event, at: at})
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
