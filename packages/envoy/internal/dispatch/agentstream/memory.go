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
	watchSeen map[string]int
}

func NewMemory() *Memory {
	return &Memory{
		replay:    make(map[string]Frame),
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

func (m *Memory) Replay(_ context.Context, sessionID string) (Frame, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// SetReplay is what a session would answer a replay request with.
func (m *Memory) SetReplay(sessionID string, frame Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replay[sessionID] = frame
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
