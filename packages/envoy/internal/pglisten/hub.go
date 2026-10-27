package pglisten

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hub is a process's one shared LISTEN connection: every subsystem that learns of another
// process's work through a NOTIFY channel registers that channel here, and one Listener carries
// them all. A process that gave each such subsystem a Listener of its own would hold one
// connection per subsystem for the life of the process, each one fewer for everything else on
// the database; and the connection is also what other processes read to learn which processes
// are listening (the caller names it through Dial), which only one connection per process can
// answer.
//
// Every registration - Handle, Wake, OnConnected, OnDisconnected - happens before Run is called.
// One made after Run has started panics: the channels a Listener names are fixed when it dials,
// so a channel registered late would never be listened on, and a handler registered late would
// race the goroutine that reads the registrations without a lock. A misordered start is a
// programming error, the kind that should fail the first time it runs rather than drop
// notifications without a word.
//
// Every handler and callback runs on Run's goroutine, the one that reads the connection and
// pings it, and must not block: a handler that waits delays every notification behind it on
// every channel of the process, and a long enough wait holds off the ping that finds a half-open
// connection. A handler with slow work queues it for a goroutine of its own.
type Hub struct {
	// Dial opens the connection the hub listens on. The caller names it - the connection is the
	// process's membership, read by the other processes on the database - and owns its pool.
	Dial func(ctx context.Context) (*pgx.Conn, error)
	// Owner names the process's subsystem in the listener's one log line (Listener.Owner); empty
	// is "dispatch".
	Owner string

	mu           sync.Mutex
	started      bool
	channels     []string
	handlers     map[string][]func(payload string)
	wakes        map[string][]chan struct{}
	connected    []func()
	disconnected []func()
	// established is whether a LISTEN stands on the current connection. Only Run's goroutine
	// touches it.
	established bool

	// listenIdle, pingTimeout and backoff are the Listener's; zero or nil is its default. A test
	// of this package sets them before Run.
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
}

// Handle registers onNotify for channel: the hub LISTENs on channel and calls onNotify with the
// payload of each notification on it, in arrival order, on Run's goroutine (it must not block).
// Several handlers may register for one channel; each is called, in registration order. It panics
// once Run has started.
func (h *Hub) Handle(channel string, onNotify func(payload string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.register("Handle", channel)
	if h.handlers == nil {
		h.handlers = map[string][]func(string){}
	}
	h.handlers[channel] = append(h.handlers[channel], onNotify)
}

// Wake registers channel, as Handle does, and returns a channel of its own with a buffer of one.
// It is signalled, without blocking, after each notification on channel and after each LISTEN
// that succeeds, so a caller that reads its state again on each wake catches up on what the
// connection missed while it had none. A signal while one is already buffered is folded into
// it: a wake says "look again", never how many times. Each call returns a new channel. It panics
// once Run has started.
func (h *Hub) Wake(channel string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.register("Wake", channel)
	if h.wakes == nil {
		h.wakes = map[string][]chan struct{}{}
	}
	wake := make(chan struct{}, 1)
	h.wakes[channel] = append(h.wakes[channel], wake)
	return wake
}

// OnConnected registers callback to run after each LISTEN succeeds on a new connection, the
// first included: where a caller catches up on the notifications sent while the hub had no
// connection, which reached nobody. It runs on Run's goroutine and must not block. It panics once
// Run has started.
func (h *Hub) OnConnected(callback func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseStarted("OnConnected")
	h.connected = append(h.connected, callback)
}

// OnDisconnected registers callback to run when a connection whose LISTEN succeeded is lost -
// after the hub has closed it and before it redials - and when Run returns having held one. It
// runs on Run's goroutine and must not block. It panics once Run has started.
func (h *Hub) OnDisconnected(callback func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseStarted("OnDisconnected")
	h.disconnected = append(h.disconnected, callback)
}

// register adds channel to the channels the hub LISTENs on, once; h.mu is held.
func (h *Hub) register(method, channel string) {
	h.refuseStarted(method)
	for _, registered := range h.channels {
		if registered == channel {
			return
		}
	}
	h.channels = append(h.channels, channel)
}

// refuseStarted panics once Run has started; h.mu is held.
func (h *Hub) refuseStarted(method string) {
	if h.started {
		panic("pglisten: Hub." + method + " called after Run started; register before Run")
	}
}

// Run listens on every registered channel until ctx ends, redialing on any failure as Listener
// does, and returns once its connection is closed. It panics if called twice.
func (h *Hub) Run(ctx context.Context) {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		panic("pglisten: Hub.Run called twice")
	}
	h.started = true
	h.mu.Unlock()

	listener := &Listener{
		Owner:       h.Owner,
		Dial:        h.Dial,
		Channels:    h.channels,
		ListenIdle:  h.listenIdle,
		PingTimeout: h.pingTimeout,
		OnConnected: h.onConnected,
		OnNotify:    h.onNotify,
		// Listener asks for the backoff once per failure, on this goroutine, right after it has
		// closed the connection that failed and before it waits to redial - which is the moment
		// an established connection is lost.
		Backoff: func(attempt int) time.Duration {
			h.lost()
			if h.backoff != nil {
				return h.backoff(attempt)
			}
			return RedialBackoff(attempt)
		},
	}
	listener.Run(ctx)
	h.lost()
}

func (h *Hub) onConnected() {
	h.established = true
	for _, callback := range h.connected {
		callback()
	}
	for _, wakes := range h.wakes {
		for _, wake := range wakes {
			signal(wake)
		}
	}
}

func (h *Hub) onNotify(channel, payload string) {
	for _, handler := range h.handlers[channel] {
		handler(payload)
	}
	for _, wake := range h.wakes[channel] {
		signal(wake)
	}
}

// lost runs the OnDisconnected callbacks if a LISTEN stood on the connection just closed.
func (h *Hub) lost() {
	if !h.established {
		return
	}
	h.established = false
	for _, callback := range h.disconnected {
		callback()
	}
}

func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
