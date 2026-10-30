package docs

import (
	"bufio"
	"encoding/binary"
	"net"
	"net/http"
	"sync"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// A browser's document provider (@hocuspocus/provider 2.x) closes its socket once it has received
// nothing on it for 30 s (messageReconnectTimeout): its checkConnection runs every 3 s and counts any
// message the socket delivers, whatever it carries, and nothing else, so a websocket ping, which a
// browser shows no page, counts for nothing. @hocuspocus/server answers each awareness update a peer
// sends, its 15 s presence renewal among them, to every peer of the document, the sender too, which is
// what the provider's 30 s counts on. ygo relays one peer's awareness only to the others, so a lone
// peer heard nothing once its sync was done, and an idle document tab closed its socket and opened it
// again, with a whole sync, every 33 s.
//
// So every documentKeepAlive the document socket sends its peer an awareness update naming no client:
// the protocol's own message, which the provider counts as received and applies as no change, as
// /ws/live sends its heartbeat frame. ygo owns the socket and gives no application a way to write to
// one of its peers, so the frame goes onto the connection under ygo's websocket writer (keepAliveConn),
// between two of that writer's messages.

const (
	// documentKeepAlive is how often a document socket's peer is sent the keep-alive: as often as
	// /ws/live sends its heartbeat (the API's defaultStreamHeartbeat), half the provider's 30 s.
	documentKeepAlive = 15 * time.Second
	// keepAliveWriteWait bounds the keep-alive's write, as ygo bounds each of its own.
	keepAliveWriteWait = 10 * time.Second
	// hocuspocusAwareness is the Hocuspocus message type of an awareness update.
	hocuspocusAwareness = 1
)

// noAwarenessChange is an awareness update naming no client - y-protocols' encoding of an update
// over no clients, a count of zero - which the provider's applyAwarenessUpdate reads as no change,
// emitting nothing and answering nothing.
var noAwarenessChange = []byte{0}

// keepAliveFrame is room's keep-alive as one websocket frame: a final binary frame, unmasked as a
// server writes every frame, whose message is the room's name, as Hocuspocus frames every message,
// the awareness type and noAwarenessChange.
func keepAliveFrame(room string) []byte {
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(hocuspocusAwareness)
		encoder.WriteVarBytes(noAwarenessChange)
	})
	frame := []byte{finalFrame | gws.BinaryMessage}
	switch length := len(message); {
	case length < 126:
		frame = append(frame, byte(length))
	case length <= 0xffff:
		frame = binary.BigEndian.AppendUint16(append(frame, 126), uint16(length))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 127), uint64(length))
	}
	return append(frame, message...)
}

// keepAliveWriter is a document socket's response writer. The connection ygo's websocket upgrade
// hijacks from it is a keepAliveConn, to which it writes the keep-alive every every until stop.
type keepAliveWriter struct {
	http.ResponseWriter
	frame   []byte
	every   time.Duration
	stopped chan struct{}
	// ended closes once the keep-alive has stopped; it is nil until the upgrade hijacks the
	// connection, which runs on the handler's goroutine, as stop does.
	ended chan struct{}
}

func newKeepAliveWriter(w http.ResponseWriter, room string, every time.Duration) *keepAliveWriter {
	return &keepAliveWriter{ResponseWriter: w, frame: keepAliveFrame(room), every: every, stopped: make(chan struct{})}
}

// Hijack hands the websocket upgrade the request's connection as a keepAliveConn, the hijacked
// buffers' writer writing through it too, and starts the keep-alive.
func (w *keepAliveWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	watched := &keepAliveConn{Conn: conn}
	buffers.Writer.Reset(watched)
	w.ended = make(chan struct{})
	go w.keepAlive(watched)
	return watched, buffers, nil
}

// Unwrap is the request's own writer, for http.ResponseController.
func (w *keepAliveWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *keepAliveWriter) keepAlive(conn *keepAliveConn) {
	defer close(w.ended)
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopped:
			return
		case <-ticker.C:
			if err := conn.writeBetweenMessages(w.frame, keepAliveWriteWait); err != nil {
				// A keep-alive cut short leaves part of a frame on the connection, and a peer that
				// takes none for keepAliveWriteWait has stopped reading: the socket is done either way,
				// and its close ends ygo's read of it.
				_ = conn.Close()
				return
			}
		}
	}
}

// stop ends the keep-alive once ygo has served the socket, and waits for it to end.
func (w *keepAliveWriter) stop() {
	close(w.stopped)
	if w.ended != nil {
		<-w.ended
	}
}

// keepAliveConn is a document socket's connection under ygo's websocket writer (gorilla/websocket).
// It follows what that writer puts on it (frameWatch), so the keep-alive goes out between two of
// the writer's messages and never inside one: gorilla writes a frame's header and the start of its
// payload in one write and the rest in another once the payload outgrows its buffer, and a message
// it streams in several frames, between which no other message may go. The connection's last frame,
// the close frame a revoked socket ends with, takes the same boundary, and waits for it
// (closeBetweenMessages).
type keepAliveConn struct {
	net.Conn
	mu    sync.Mutex
	watch frameWatch
	// writeDeadline is the write deadline the websocket writer last set, which a keep-alive's own
	// gives back.
	writeDeadline time.Time
	// betweenWrites wakes everyone waiting for the websocket writer to reach the end of a message.
	// It is broadcast after each write and once the last frame has had its turn, and it is made on
	// the first wait, since a connection no one closes this way never has a waiter.
	betweenWrites *sync.Cond
	// closing is whether someone is waiting for the end of a message to write the connection's
	// last frame, and closed whether that turn has passed: from then on nothing more is written.
	closing, closed bool
}

func (c *keepAliveConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.writeLocked(p)
}

// writeLocked writes p on the connection, follows it, and wakes everyone waiting for the end of a
// message. Its caller holds mu and has found the connection still open.
func (c *keepAliveConn) writeLocked(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.watch.wrote(p[:n])
	c.wake()
	return n, err
}

// wake wakes everyone waiting for the end of a message, and nobody when none waits. Its caller
// holds mu.
func (c *keepAliveConn) wake() {
	if c.betweenWrites != nil {
		c.betweenWrites.Broadcast()
	}
}

// waiters is what everyone waiting for the end of a message waits on, made on the first wait.
// Its caller holds mu, which is the condition's own lock.
func (c *keepAliveConn) waiters() *sync.Cond {
	if c.betweenWrites == nil {
		c.betweenWrites = sync.NewCond(&c.mu)
	}
	return c.betweenWrites
}

func (c *keepAliveConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return c.Conn.SetDeadline(t)
}

func (c *keepAliveConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
}

// writeBetweenMessages writes frame, a whole message of its own, within wait, when the websocket
// writer is between two messages. While one of its messages is part way out it writes nothing, since
// that message reaches the peer anyway, and once its close frame has gone out nothing more.
func (c *keepAliveConn) writeBetweenMessages(frame []byte, wait time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.watch.betweenMessages() {
		return nil
	}
	if err := c.Conn.SetWriteDeadline(time.Now().Add(wait)); err != nil {
		return err
	}
	if _, err := c.writeLocked(frame); err != nil {
		return err
	}
	return c.Conn.SetWriteDeadline(c.writeDeadline)
}

// closeBetweenMessages writes frame as the connection's last bytes: when the websocket writer is
// between two messages, so the frame never cuts one of its messages, and nothing after it, since
// every later write - the keep-alive's included - is refused (net.ErrClosed). It waits for that
// boundary for at most wait, which the caller gives as the deadline ygo gives each of its own
// writes: a message that holds the boundary has that long to go out, and a peer that takes none of
// it in that time has stopped reading, so the connection closes without the frame. It writes
// nothing once the websocket writer has sent a close frame of its own. A second caller writes no
// second frame: it waits for the first to finish, so no close of the connection cuts that frame,
// and then returns.
func (c *keepAliveConn) closeBetweenMessages(frame []byte, wait time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.closed {
		for c.closing {
			c.waiters().Wait()
		}
		return nil
	}
	c.closing = true
	defer func() {
		c.closing, c.closed = false, true
		c.wake()
	}()
	until := time.Now().Add(wait)
	timer := time.AfterFunc(wait, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.wake()
	})
	defer timer.Stop()
	for !c.watch.betweenMessages() && !c.watch.closed && time.Now().Before(until) {
		c.waiters().Wait()
	}
	if !c.watch.betweenMessages() {
		return nil
	}
	if err := c.Conn.SetWriteDeadline(until); err != nil {
		return err
	}
	_, err := c.writeLocked(frame)
	return err
}

const (
	// finalFrame is the FIN bit of a websocket frame's first byte: the frame ends its message.
	finalFrame = 0x80
	// maskedFrame is the MASK bit of a frame's second byte: a masking key follows the length.
	maskedFrame = 0x80
	// maxFrameHeader is the longest frame header: two bytes, an eight-byte length, a masking key.
	maxFrameHeader = 14
)

// frameWatch follows the server's side of an upgraded HTTP/1.1 connection as it is written: the
// upgrade's response, up to the blank line that ends it, then websocket frames (RFC 6455 section
// 5.2), each a header that names its payload's length, then that payload.
type frameWatch struct {
	upgraded bool
	// blankLine is how much of the "\r\n\r\n" that ends the upgrade's response the response's
	// bytes so far end with.
	blankLine  int
	header     [maxFrameHeader]byte
	headerRead int
	// payloadLeft is how much of the current frame's payload is still to be written.
	payloadLeft uint64
	// fragmented is whether a message's first frames have been written and its final one has not.
	fragmented bool
	// closed is whether a close frame has been written.
	closed bool
}

// wrote follows p, the next bytes written to the connection.
func (w *frameWatch) wrote(p []byte) {
	for len(p) > 0 {
		switch {
		case !w.upgraded:
			p = w.response(p)
		case w.payloadLeft > 0:
			n := min(uint64(len(p)), w.payloadLeft)
			w.payloadLeft -= n
			p = p[n:]
		default:
			w.header[w.headerRead] = p[0]
			w.headerRead++
			p = p[1:]
			if w.headerRead == frameHeaderLength(w.header[:w.headerRead]) {
				w.frameBegins()
			}
		}
	}
}

// response follows p as the upgrade's response, and answers what of p follows its blank line.
func (w *frameWatch) response(p []byte) []byte {
	const blankLine = "\r\n\r\n"
	for i, b := range p {
		switch {
		case b == blankLine[w.blankLine]:
			w.blankLine++
		case b == '\r':
			w.blankLine = 1
		default:
			w.blankLine = 0
		}
		if w.blankLine == len(blankLine) {
			w.upgraded = true
			return p[i+1:]
		}
	}
	return nil
}

// frameHeaderLength is the length of the frame header that begins with header, at least two bytes.
func frameHeaderLength(header []byte) int {
	if len(header) < 2 {
		return 2
	}
	length := 2
	switch header[1] &^ maskedFrame {
	case 126:
		length += 2
	case 127:
		length += 8
	}
	if header[1]&maskedFrame != 0 {
		length += 4
	}
	return length
}

// frameBegins reads the frame header just written: the frame's payload follows it.
func (w *frameWatch) frameBegins() {
	header := w.header[:w.headerRead]
	w.headerRead = 0
	payload := uint64(header[1] &^ maskedFrame)
	switch payload {
	case 126:
		payload = uint64(binary.BigEndian.Uint16(header[2:4]))
	case 127:
		payload = binary.BigEndian.Uint64(header[2:10])
	}
	w.payloadLeft = payload
	switch opcode := int(header[0] & 0x0f); {
	case opcode == gws.CloseMessage:
		w.closed = true
	case opcode < gws.CloseMessage:
		// A data frame - text, binary or a continuation - ends its message when it is final.
		w.fragmented = header[0]&finalFrame == 0
	}
}

// betweenMessages is whether the connection has been written up to the end of a message: its
// upgrade done, no frame part way out, no message waiting for its final frame, and no close.
func (w *frameWatch) betweenMessages() bool {
	return w.upgraded && w.headerRead == 0 && w.payloadLeft == 0 && !w.fragmented && !w.closed
}
