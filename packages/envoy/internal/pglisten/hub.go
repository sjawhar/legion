package pglisten

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hub shares one Listener, and so one held Postgres connection, among every subscriber in a
// process that registers on it, however many channels they name between them: a connection per
// feature would cost a held backend and a keepalive each. Dispatch builds one for its
// change-driven background loops, each of which waits on a Wake.
//
// Postgres does not replay a notification sent while nothing listened, so every Wake is also
// signalled after each successful LISTEN, the first one included: a subscriber catches up then
// what it missed while the listener had no connection (the redial backoff, RedialBackoff). A
// subscriber therefore needs no timer of its own for an outage, and must not gate its own start
// on the hub - a loop runs its boot pass whether or not the listener has connected yet, and the
// signal after the first LISTEN costs it one more pass at most.
//
// Everything is registered before Run: Handle, Wake, OnConnected and OnDisconnected panic once Run
// has started, because the listener's LISTEN statement and callbacks are fixed when it starts and
// a channel registered later would never be heard.
type Hub struct {
	// Owner names the subsystem in the listener's log line (Listener.Owner).
	Owner string
	// Dial opens the listener's connection (Listener.Dial): store.Pool.Listener in the server.
	Dial func(ctx context.Context) (*pgx.Conn, error)
	// ListenIdle, PingTimeout and Backoff pass through to the Listener; zero or nil is its
	// default. A test sets them.
	ListenIdle  time.Duration
	PingTimeout time.Duration
	Backoff     func(attempt int) time.Duration

	mu           sync.Mutex
	started      bool
	channels     []string
	handlers     map[string][]func(Notification)
	wakes        map[string][]chan struct{}
	connected    []func()
	disconnected []func()
}

// Handle calls fn with each notification on channel, on the listener's goroutine, so slow work in
// fn delays every channel's notifications behind it. channel is spliced into the LISTEN statement
// as written (Listener.Channels): a package constant, never a value read from a request.
func (h *Hub) Handle(channel string, fn func(Notification)) {
	h.register(func() {
		h.listenOn(channel)
		if h.handlers == nil {
			h.handlers = make(map[string][]func(Notification))
		}
		h.handlers[channel] = append(h.handlers[channel], fn)
	})
}

// Wake returns a channel of its own, with a buffer of one, that is signalled on each notification
// on channel and after every successful LISTEN. The send never blocks, so any number of signals
// before the subscriber reads collapse into one, and a signal that arrives while the subscriber is
// busy waits in the buffer. Each call is a separate subscriber.
func (h *Hub) Wake(channel string) <-chan struct{} {
	wake := make(chan struct{}, 1)
	h.register(func() {
		h.listenOn(channel)
		if h.wakes == nil {
			h.wakes = make(map[string][]chan struct{})
		}
		h.wakes[channel] = append(h.wakes[channel], wake)
	})
	return wake
}

// OnConnected adds fn to what runs after every successful LISTEN, on the listener's goroutine,
// after every Wake has been signalled.
func (h *Hub) OnConnected(fn func()) {
	h.register(func() { h.connected = append(h.connected, fn) })
}

// OnDisconnected adds fn to what runs when a connection that had listened is lost, on the
// listener's goroutine (Listener.OnDisconnected): from then until the next OnConnected,
// notifications are missed.
func (h *Hub) OnDisconnected(fn func()) {
	h.register(func() { h.disconnected = append(h.disconnected, fn) })
}

// Run runs one Listener on every registered channel until ctx ends. A hub with no channel
// registered opens no connection, and Run only waits for ctx.
func (h *Hub) Run(ctx context.Context) {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		panic("pglisten: Hub.Run called twice")
	}
	h.started = true
	h.mu.Unlock()
	if len(h.channels) == 0 {
		<-ctx.Done()
		return
	}
	listener := &Listener{
		Owner:          h.Owner,
		Dial:           h.Dial,
		Channels:       h.channels,
		ListenIdle:     h.ListenIdle,
		PingTimeout:    h.PingTimeout,
		Backoff:        h.Backoff,
		OnConnected:    h.onConnected,
		OnDisconnected: h.onDisconnected,
		OnNotification: h.onNotification,
	}
	listener.Run(ctx)
}

// register runs add under the hub's lock, or panics once Run has started. After that nothing
// writes the registrations, so the listener's goroutine reads them without the lock.
func (h *Hub) register(add func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		panic("pglisten: Hub registration after Run")
	}
	add()
}

// listenOn adds channel to the LISTEN statement once, however many subscribers name it.
func (h *Hub) listenOn(channel string) {
	for _, listened := range h.channels {
		if listened == channel {
			return
		}
	}
	h.channels = append(h.channels, channel)
}

func (h *Hub) onConnected() {
	for _, wakes := range h.wakes {
		for _, wake := range wakes {
			signalWake(wake)
		}
	}
	for _, fn := range h.connected {
		fn()
	}
}

func (h *Hub) onDisconnected() {
	for _, fn := range h.disconnected {
		fn()
	}
}

// onNotification signals the channel's wakes before calling its handlers, so a slow handler does
// not hold back a loop waiting on the same change.
func (h *Hub) onNotification(n Notification) {
	for _, wake := range h.wakes[n.Channel] {
		signalWake(wake)
	}
	for _, fn := range h.handlers[n.Channel] {
		fn(n)
	}
}

func signalWake(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
