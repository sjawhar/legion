// Package sse delivers Server-Sent Events to connected dashboard clients.
//
// Each client registers with a (login, watched-repos) pair. Events are
// published with a `repo` key (the <owner>/<repo> slug derived from the
// NATS subject); the hub fans out to clients whose watched-repo set
// contains that slug. A client with an empty watched set receives nothing —
// new users see "no repos selected" in the sidebar until they pick one.
package sse

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Event is one server-sent event payload.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Client is a registered SSE consumer. Each client has its own buffered
// channel; if a slow consumer fills its buffer, the hub drops the client
// rather than blocking other clients.
type Client struct {
	Login    string
	watched  map[string]struct{}
	Messages chan []byte
}

// Watches reports whether the client subscribed to events for this repo
// (case-insensitive).
func (c *Client) Watches(repo string) bool {
	if c == nil || c.watched == nil {
		return false
	}
	_, ok := c.watched[strings.ToLower(repo)]
	return ok
}

// Hub is a goroutine-safe registry of SSE clients.
type Hub struct {
	mu      sync.Mutex
	clients map[int]*Client
	nextID  int
}

// New returns an initialized hub.
func New() *Hub {
	return &Hub{clients: map[int]*Client{}}
}

// AddClient registers a new client and returns its id + receiving channel.
// The caller must call RemoveClient when the connection ends.
//
// watched is the client's list of <owner>/<repo> slugs. The hub stores a
// copy and lowercases for matching; passing nil means "deliver nothing"
// rather than "deliver everything".
func (h *Hub) AddClient(login string, watched []string) (int, *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	client := &Client{
		Login:    login,
		watched:  watchedSet(watched),
		Messages: make(chan []byte, 16),
	}
	h.clients[id] = client
	return id, client
}

// RemoveClient deregisters and closes the client's channel. Safe to call
// multiple times.
func (h *Hub) RemoveClient(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if client, ok := h.clients[id]; ok {
		close(client.Messages)
		delete(h.clients, id)
	}
}

// BroadcastRepo sends an event to every client whose watched-set contains
// repo. Slow clients are dropped, not awaited.
func (h *Hub) BroadcastRepo(repo string, event Event) {
	payload, err := encodeEvent(event)
	if err != nil {
		return
	}
	key := strings.ToLower(repo)
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, client := range h.clients {
		if _, ok := client.watched[key]; !ok {
			continue
		}
		select {
		case client.Messages <- payload:
		default:
			close(client.Messages)
			delete(h.clients, id)
		}
	}
}

// BroadcastAll sends to every client regardless of watched-repos. Reserved
// for system events ("you've been logged out", health pings, …) — not for
// GitHub event fan-out.
func (h *Hub) BroadcastAll(event Event) {
	payload, err := encodeEvent(event)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, client := range h.clients {
		select {
		case client.Messages <- payload:
		default:
			close(client.Messages)
			delete(h.clients, id)
		}
	}
}

// Size returns the number of registered clients (for tests and metrics).
func (h *Hub) Size() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func watchedSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

func encodeEvent(event Event) ([]byte, error) {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event.Type, data)), nil
}
