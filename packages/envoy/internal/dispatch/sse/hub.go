// Package sse delivers Server-Sent Events to connected dashboard clients.
//
// Each client registers with a (login, owner-set) pair, where owners are the
// GitHub account logins (users/orgs) the client's Envoy App installations
// cover. Events are published with a `repo` key (the <owner>/<repo> slug
// derived from the NATS subject); the hub fans out to clients whose owner
// set contains that slug's account. A client with an empty owner set
// receives nothing — a user with no App installations sees nothing until
// they install the App somewhere.
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
	owners   map[string]struct{}
	Messages chan []byte
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
// owners is the client's list of GitHub account logins — the accounts its
// Envoy App installations cover. The hub stores a copy and lowercases for
// matching; passing nil means "deliver nothing" rather than "deliver
// everything".
func (h *Hub) AddClient(login string, owners []string) (int, *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	client := &Client{
		Login:    login,
		owners:   ownerSet(owners),
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

// BroadcastRepo sends an event to every client whose owner set covers
// repo's account. Slow clients are dropped, not awaited.
func (h *Hub) BroadcastRepo(repo string, event Event) {
	owner, ok := ownerOf(repo)
	if !ok {
		return
	}
	payload, err := encodeEvent(event)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, client := range h.clients {
		if _, watched := client.owners[owner]; !watched {
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

// BroadcastAll sends to every client regardless of owner set. Reserved
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

// ownerOf returns the lowercased account segment of an "<owner>/<repo>"
// slug. Returns false when repo doesn't have that shape.
func ownerOf(repo string) (string, bool) {
	idx := strings.Index(repo, "/")
	if idx <= 0 {
		return "", false
	}
	return strings.ToLower(repo[:idx]), true
}

func ownerSet(in []string) map[string]struct{} {
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
