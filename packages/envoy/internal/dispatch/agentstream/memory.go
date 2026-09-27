package agentstream

import (
	"context"
	"sync"
)

// Memory is an in-process Source. The e2e harness runs Dispatch with NATS disabled, so a browser
// test of the conversation view has no bus to take frames from; a test hook publishes into this
// instead. It is mounted only where Deps.TestHooksEnabled is set, and it holds a session's
// frames no longer than the session's viewers are attached.
type Memory struct {
	mu        sync.Mutex
	next      int
	viewers   map[string]map[int]func(Frame)
	replay    map[string]Frame
	responder map[string]bool
	watchSeen map[string]int
}

func NewMemory() *Memory {
	return &Memory{
		replay:    make(map[string]Frame),
		responder: make(map[string]bool),
		viewers:   make(map[string]map[int]func(Frame)),
		watchSeen: make(map[string]int),
	}
}

func (m *Memory) Subscribe(sessionID string, deliver func(Frame)) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	session, found := m.viewers[sessionID]
	if !found {
		session = make(map[int]func(Frame))
		m.viewers[sessionID] = session
	}
	session[id] = deliver
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.viewers[sessionID], id)
	}, nil
}

// Replay answers as a session would. A session this harness has not given a responder to is one
// nobody is listening for, which is the state a plugin that cannot stream leaves on the bus.
func (m *Memory) Replay(_ context.Context, sessionID string) (Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.responder[sessionID] {
		return nil, ErrNoResponder
	}
	return m.replay[sessionID], nil
}

func (m *Memory) Watch(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watchSeen[sessionID]++
	return nil
}

// Watches reports how many times a viewer has told sessionID it is attached.
func (m *Memory) Watches(sessionID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watchSeen[sessionID]
}

// SetReplay is what a session would answer a replay request with. Answering at all makes it a
// responder, exactly as a running session with a streaming plugin is.
func (m *Memory) SetReplay(sessionID string, frame Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replay[sessionID] = frame
	m.responder[sessionID] = true
}

// SetResponder says whether anyone is listening on the session's control subject, without
// giving it a history: a live session that has produced nothing yet is a responder with no
// frames, and one on a plugin that predates the live view is not a responder at all.
func (m *Memory) SetResponder(sessionID string, responding bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responder[sessionID] = responding
	if !responding {
		delete(m.replay, sessionID)
	}
}

// Publish fans a frame out to every viewer of sessionID, as the bus would.
func (m *Memory) Publish(sessionID string, frame Frame) {
	m.mu.Lock()
	delivers := make([]func(Frame), 0, len(m.viewers[sessionID]))
	for _, deliver := range m.viewers[sessionID] {
		delivers = append(delivers, deliver)
	}
	m.mu.Unlock()
	for _, deliver := range delivers {
		deliver(frame)
	}
}
