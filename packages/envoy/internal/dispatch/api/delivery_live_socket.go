package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// GET /ws/delivery (LEGION-690) keeps an open Delivery or Pipeline page current. The page sends
// subscribe and unsubscribe frames (DeliveryLiveSubscribe, DeliveryLiveUnsubscribe); the socket
// hands each to the delivery hub and writes every frame the hub sends it. It keeps no view state of
// its own.
//
// It closes with:
//   - 1012 (service restart) when Dispatch shuts down (Deps.Lifetime) or the test hook drops it;
//   - 1008 (policy violation) once its caller no longer resolves (whileCallerResolves);
//   - 1013 (try again later) when the page falls deliverySocketQueue frames behind;
//   - 1009 (message too big) for a frame over deliverySocketReadLimit, which gorilla writes;
//   - 1011 (internal error) for a frame Dispatch cannot encode.
//
// A page that answers no ping for two heartbeats, or whose connection fails, has gone or stopped
// reading: its socket ends with no close frame, which the page reads as 1006.
//
// The page reconnects after each and resumes every subscription with its revisions, so what a close
// cut off reaches it as part of what changed since.

const (
	// deliverySocketQueue is how many frames a socket holds for a page that is slow to read them.
	deliverySocketQueue = 16
	// deliverySocketReadLimit bounds one frame from the page. A resume frame carries the page's
	// revisions, one 16-digit revision per pull request, run, map entry and count it holds.
	deliverySocketReadLimit = 8 << 20
	// deliverySocketWriteWait bounds every write to the page.
	deliverySocketWriteWait = 10 * time.Second
	// deliverySocketCloseWait is how long a socket that sent its close frame waits for the page's.
	deliverySocketCloseWait = time.Second
	// deliverySocketReadBuffer is the socket's own read buffer, gorilla's default size. Setting one
	// at all is what matters: gorilla must read the raw connection through a reader of its own, which
	// a nonzero ReadBufferSize guarantees (with zero, "buffers allocated by the HTTP server are used").
	// A read through net/http's reader cancels the request's context on a read error, and the writer
	// reads that context's end as the caller no longer resolving (1008).
	deliverySocketReadBuffer = 4096
)

// deliverySocket is one open GET /ws/delivery socket, the deliveryLiveConn the hub sends to.
type deliverySocket struct {
	conn  *gws.Conn
	queue chan any
	// full closes the first time Send finds the queue full.
	full     chan struct{}
	fullOnce sync.Once
	// dropped closes when the test hook drops the socket.
	dropped  chan struct{}
	dropOnce sync.Once
	// deadline orders the pong handler's extension of the read deadline with the last one the
	// writer sets once it has closed; closing stops further extensions.
	deadline sync.Mutex
	closing  bool
}

// Send queues frame for the page without blocking. A full queue answers false and closes the
// socket with 1013, since a page that far behind reads a delta of what changed since sooner by
// resuming than by reading every frame it missed.
func (socket *deliverySocket) Send(frame any) bool {
	select {
	case socket.queue <- frame:
		return true
	default:
		socket.fullOnce.Do(func() { close(socket.full) })
		return false
	}
}

// overflowed reports whether a Send has found the queue full.
func (socket *deliverySocket) overflowed() bool {
	select {
	case <-socket.full:
		return true
	default:
		return false
	}
}

// drop closes the socket with 1012, as a restart does.
func (socket *deliverySocket) drop() {
	socket.dropOnce.Do(func() { close(socket.dropped) })
}

// awaitPong gives the page wait to answer the next ping, unless the socket is closing.
func (socket *deliverySocket) awaitPong(wait time.Duration) {
	socket.deadline.Lock()
	defer socket.deadline.Unlock()
	if !socket.closing {
		_ = socket.conn.SetReadDeadline(time.Now().Add(wait))
	}
}

// endReads ends the page's reads within wait: the time its close frame has to arrive once the
// socket sent its own, or none when the connection failed.
func (socket *deliverySocket) endReads(wait time.Duration) {
	socket.deadline.Lock()
	defer socket.deadline.Unlock()
	socket.closing = true
	_ = socket.conn.SetReadDeadline(time.Now().Add(wait))
}

// close sends the page a close frame with code and waits for its reply.
func (socket *deliverySocket) close(code int) {
	err := socket.conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(code, ""), time.Now().Add(deliverySocketWriteWait))
	if err != nil {
		socket.endReads(0)
		return
	}
	socket.endReads(deliverySocketCloseWait)
}

// write sends the page each queued frame, and every heartbeat a ping beside a heartbeat frame (a
// browser shows the page no ping), until the reader ends or something closes the socket.
func (socket *deliverySocket) write(ctx context.Context, lifetime <-chan struct{}, heartbeat time.Duration, readerDone <-chan struct{}) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		// A full queue wins over the frames still in it: the page resumes from what it applied.
		if socket.overflowed() {
			slog.Info("dispatch: delivery socket closed: its page fell behind", "queued", deliverySocketQueue)
			socket.close(gws.CloseTryAgainLater)
			return
		}
		var frame any
		select {
		case <-readerDone:
			return
		case <-socket.full:
			continue
		case <-socket.dropped:
			socket.close(gws.CloseServiceRestart)
			return
		case <-lifetime:
			socket.close(gws.CloseServiceRestart)
			return
		case <-ctx.Done():
			socket.close(gws.ClosePolicyViolation)
			return
		case <-ticker.C:
			if err := socket.conn.WriteControl(gws.PingMessage, nil, time.Now().Add(deliverySocketWriteWait)); err != nil {
				socket.endReads(0)
				return
			}
			frame = deliveryLiveHeartbeat
		case frame = <-socket.queue:
		}
		data, err := json.Marshal(frame)
		if err != nil {
			slog.Error("dispatch: delivery socket closed: a frame did not encode", "frame", fmt.Sprintf("%T", frame), "error", err)
			socket.close(gws.CloseInternalServerErr)
			return
		}
		_ = socket.conn.SetWriteDeadline(time.Now().Add(deliverySocketWriteWait))
		if err := socket.conn.WriteMessage(gws.TextMessage, data); err != nil {
			socket.endReads(0)
			return
		}
	}
}

// serveDeliverySocket serves GET /ws/delivery to a signed-in person or an agent's bearer. The
// upgrade keeps gorilla's same-origin check, so another site's page cannot open the socket with a
// person's cookie.
func (s *server) serveDeliverySocket(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	upgrader := gws.Upgrader{EnableCompression: true, ReadBufferSize: deliverySocketReadBuffer}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has answered the request
	}
	socket := &deliverySocket{
		conn:    conn,
		queue:   make(chan any, deliverySocketQueue),
		full:    make(chan struct{}),
		dropped: make(chan struct{}),
	}
	s.deliverySockets.add(socket)
	defer s.deliverySockets.remove(socket)
	defer conn.Close()
	defer s.deliveryHub.Close(socket)

	pongWait := 2 * s.deps.StreamHeartbeat
	conn.SetReadLimit(deliverySocketReadLimit)
	socket.awaitPong(pongWait)
	conn.SetPongHandler(func(string) error {
		socket.awaitPong(pongWait)
		return nil
	})
	readerDone := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		socket.write(r.Context(), s.deps.Lifetime.Done(), s.deps.StreamHeartbeat, readerDone)
	}()

	var readErr error
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			break
		}
		if kind != gws.TextMessage {
			socket.Send(deliveryLiveRefusal("", "INVALID_FRAME", "a frame is one JSON object in a text message"))
			continue
		}
		s.deliverySocketFrame(socket, data)
	}
	close(readerDone)
	<-writerDone
	if errors.Is(readErr, gws.ErrReadLimit) {
		// The rest of the frame past the limit is still arriving. Reading it off before closing makes
		// the close a FIN behind the 1009 gorilla wrote, not a reset that can overtake it.
		_ = conn.NetConn().SetReadDeadline(time.Now().Add(deliverySocketCloseWait))
		_, _ = io.Copy(io.Discard, conn.NetConn())
	}
}

// deliverySocketFrame hands one frame from the page to the hub, or answers the error frame refusing
// it, which names the frame's sub when it has one Dispatch can send back. Every frame passes the
// input layer's checks first: a U+0000 anywhere in its JSON (unstorableJSON), and a view's query as
// the GET routes' wrapper checks theirs (unstorableQuery).
func (s *server) deliverySocketFrame(socket *deliverySocket, data []byte) {
	var head struct {
		Type string `json:"type"`
		Sub  string `json:"sub"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		socket.Send(deliveryLiveRefusal("", "INVALID_FRAME", "a frame is one JSON object with a string type and sub: "+err.Error()))
		return
	}
	sub := head.Sub
	if refusal := unstorableJSON("", data); refusal != nil {
		if !text.Storable(sub) {
			sub = ""
		}
		socket.Send(deliveryLiveRefusal(sub, refusal.code, refusal.message))
		return
	}
	switch head.Type {
	case "subscribe":
		var frame DeliveryLiveSubscribe
		if err := decodeSocketClientFrame(data, &frame); err != nil {
			socket.Send(deliveryLiveRefusal(sub, "INVALID_FRAME", "a subscribe frame: "+err.Error()))
			return
		}
		if frame.View.Query != nil {
			if refusal := unstorableQuery(*frame.View.Query); refusal != nil {
				socket.Send(deliveryLiveRefusal(sub, refusal.code, refusal.message))
				return
			}
		}
		s.deliveryHub.Subscribe(socket, frame)
	case "unsubscribe":
		var frame DeliveryLiveUnsubscribe
		if err := decodeSocketClientFrame(data, &frame); err != nil {
			socket.Send(deliveryLiveRefusal(sub, "INVALID_FRAME", "an unsubscribe frame: "+err.Error()))
			return
		}
		if frame.Sub == "" {
			socket.Send(deliveryLiveRefusal("", "INVALID_FRAME", "an unsubscribe frame names its sub"))
			return
		}
		s.deliveryHub.Unsubscribe(socket, frame.Sub)
	default:
		socket.Send(deliveryLiveRefusal(sub, "INVALID_FRAME", "type must be subscribe or unsubscribe"))
	}
}

// decodeSocketClientFrame decodes one frame from the page into its struct, refusing a member the
// struct does not declare, as decodeJSON refuses one in a body.
func decodeSocketClientFrame(data []byte, frame any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(frame)
}

// disconnectDeliverySockets closes every open GET /ws/delivery socket with 1012, as a restart does,
// so a test can drive the page's reconnect and resume. Test-only: mounted only under
// Deps.TestHooksEnabled.
func (s *server) disconnectDeliverySockets(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	for _, socket := range s.deliverySockets.all() {
		socket.drop()
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deliverySocketSet is every open GET /ws/delivery socket: the test hook drops them, and a test
// waits for every socket's handler to return before its database closes.
type deliverySocketSet struct {
	mu      sync.Mutex
	open    map[*deliverySocket]struct{}
	serving sync.WaitGroup
}

func (set *deliverySocketSet) add(socket *deliverySocket) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.open == nil {
		set.open = map[*deliverySocket]struct{}{}
	}
	set.open[socket] = struct{}{}
	set.serving.Add(1)
}

func (set *deliverySocketSet) remove(socket *deliverySocket) {
	set.mu.Lock()
	delete(set.open, socket)
	set.mu.Unlock()
	set.serving.Done()
}

// all is every socket open now.
func (set *deliverySocketSet) all() []*deliverySocket {
	set.mu.Lock()
	defer set.mu.Unlock()
	sockets := make([]*deliverySocket, 0, len(set.open))
	for socket := range set.open {
		sockets = append(sockets, socket)
	}
	return sockets
}

// wait returns once every socket's handler has returned.
func (set *deliverySocketSet) wait() { set.serving.Wait() }
