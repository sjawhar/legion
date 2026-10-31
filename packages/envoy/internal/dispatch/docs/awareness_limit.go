package docs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// Awareness - a reader's cursor and presence - reaches a document room through the socket that
// sends it, and a socket sends more than its own client. @hocuspocus/provider 2.15.3 listens on
// its awareness (hocuspocus-provider.cjs:2522) and sends every client that a change touched,
// whatever made that change (:2595-2601). A tab applies the awareness the server sends it for
// the room's other readers (:2265), and that apply fires the same change (:1582-1586), so every
// tab sends the other readers' client ids back on its own socket. Nothing in the protocol bounds
// what a socket names. ygo applies a peer's awareness frame whole and broadcasts it to the room's
// other peers whatever it names (provider/websocket/peer.go:96-117), and the room's relay then
// sends it to every other process (awareness_relay.go), so one tab's frame naming thousands of
// invented client ids used to cost every reader of every process an entry each, and the room it
// filled had nothing left for the reader who joined next.
//
// So the bound is the socket's: a document socket may hold maxAwarenessClientsPerSocket awareness
// clients at once, and the entries of the clients past that are taken out of its frame before ygo
// reads it. ygo offers no hook on a peer's inbound awareness - peer.handleMessage applies and
// broadcasts what it reads - so the filter sits on the connection the websocket upgrade hijacks
// (awarenessLimitWriter, awarenessLimitConn), where the process header's wrapper sits
// (websocket.go). Filtering there, before ygo reads the frame, is what makes the room's own
// readers and the readers on the other processes see the same clients: ygo broadcasts and the
// relay sends the frame the filter wrote, not the one the socket sent.
//
// A socket's clients go with it: ygo tracks the client ids a peer announced and removes them as
// the peer disconnects (provider/websocket/peer.go:438-465, 467-653), which the relay carries to
// the other processes like any other removal, and the filter frees a client's place as the socket
// takes that client away. So a room's awareness holds its sockets' clients at most, and the
// sockets of a process are themselves bounded (Service.canAddConnection).

const (
	// maxAwarenessClientsPerSocket is how many awareness clients one document socket may hold at
	// once. The first client a tab sends is its own: startSync announces it at the socket's first
	// sync (hocuspocus-provider.cjs:2669-2679), and an echo can only follow an awareness message
	// the server sent that tab. The tab then echoes the room's other readers back on the same
	// socket (see the top of this file), so the three places left fill with echoes, and a room of
	// more than four readers loses the echoes past them. The room loses nothing by that: an echo
	// carries the state and clock the tab read from the room, which the room holds already. Four
	// also leaves room for a tab that swaps the document under its provider and announces the new
	// client before the old one is taken away, and is far below any number a room would notice.
	// The clients a frame names past it are dropped, so a tab inventing thousands reaches its
	// room, its readers and the other processes as four.
	maxAwarenessClientsPerSocket = 4
	// maxAwarenessMessageBytes is the largest awareness message the filter reads whole, to rewrite
	// it. One client's cursor is a few hundred bytes and a room holds no more state than
	// maxAwarenessBytesPerRoom, so nothing a reader sends comes near it; a larger one reaches ygo
	// as an awareness message naming no client, which applies and broadcasts as no change. It
	// bounds what the filter buffers for a socket, well under the 64 MiB message ygo's own reader
	// buffers (provider/websocket/server.go:103, 1998).
	maxAwarenessMessageBytes = 256 << 10
	// maxDocumentMessageHeader is how many bytes of a message the filter reads to learn what the
	// message is: Hocuspocus frames each one with the document's name and the message's type
	// (docs/service.go sets HocuspocusFraming), which is 38 bytes for a document of this service.
	// A socket whose message does not name both within this many bytes, and one whose message
	// ends without naming both however short it is, is sending nothing this service can read,
	// and its connection ends rather than carry a frame past the filter.
	maxDocumentMessageHeader = 512
	// continuationFrame is the opcode of a websocket frame continuing the message before it
	// (RFC 6455 section 5.4); gorilla names every other opcode this package uses.
	continuationFrame = 0x0
)

// awarenessLimitMode is what the message a socket is part way through sending is to the filter.
type awarenessLimitMode int

const (
	// limitIdle: no message is part way in.
	limitIdle awarenessLimitMode = iota
	// limitDeciding: a message's first bytes are in and its Hocuspocus header is not complete, so
	// what the message is is not yet known.
	limitDeciding
	// limitAwareness: an awareness message is being read whole, to be rewritten once it ends.
	limitAwareness
	// limitPassing: the message is not an awareness message; its frames reach ygo unread.
	limitPassing
	// limitSkipping: an awareness message past maxAwarenessMessageBytes; its bytes are discarded
	// and ygo is given an awareness message naming no client in its place.
	limitSkipping
)

// awarenessLimitWriter is a document socket's response writer: the connection ygo's websocket
// upgrade hijacks through it is an awarenessLimitConn, which holds the socket to
// maxAwarenessClientsPerSocket awareness clients. The upgrade reads the socket either through
// that connection or through the hijacked buffered reader, whichever gorilla this build
// resolves: v1.5.3 resets that reader onto the connection the hijack returned
// (server.go:330-341, bufioReaderSize), while the v1.5.4 pre-release the workspace builds keeps
// it on the connection underneath (server.go:192-195), where the filter would never see a frame.
// So Hijack resets the reader onto the filter itself, which holds whichever of the two reads it.
type awarenessLimitWriter struct {
	http.ResponseWriter
	room string
}

func (w *awarenessLimitWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffers, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// What the HTTP server read past the request and has not handed on - a client that sent a
	// frame without waiting for the upgrade's answer - is the socket's first bytes, and the
	// reset below drops the buffer holding them. So the filter is given them to read first.
	// Peek of what the reader already holds reads nothing and cannot fail.
	var pending []byte
	if held := buffers.Reader.Buffered(); held > 0 {
		head, _ := buffers.Reader.Peek(held)
		pending = bytes.Clone(head)
	}
	filtered := newAwarenessLimitConn(conn, w.room, pending)
	buffers.Reader.Reset(filtered)
	return filtered, buffers, nil
}

// Unwrap gives http.ResponseController the writer underneath.
func (w *awarenessLimitWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// awarenessLimitConn is a document socket's connection under ygo's websocket reader. It reads the
// frames the socket's peer sends and hands ygo an awareness message holding only the clients the
// socket may hold; every other message reaches ygo as it arrived.
type awarenessLimitConn struct {
	net.Conn
	room string
	// live is the awareness clients this socket holds now: a client announced takes a place and a
	// client taken away gives it back, so a socket that announces and removes one client at a
	// time is never refused.
	live map[uint64]struct{}
	src  *bufio.Reader
	// out is the rewritten bytes ygo's reader has yet to take.
	out bytes.Buffer
	// through is how much of the frame being read reaches ygo unread, straight from src.
	through int64
	// message is the payload of the message being read, unmasked: its Hocuspocus header while the
	// filter is deciding what the message is, and the whole of it for an awareness message.
	message []byte
	mode    awarenessLimitMode
	// name is the document the message being read names, which the rewritten message carries.
	name string
	// key is the masking key of the frame last read; the rewritten frames reuse it, as a frame a
	// client sends must be masked (gorilla/websocket conn.go:861).
	key [4]byte
}

// newAwarenessLimitConn is the filter over conn, reading pending - what the upgrade's hijack
// held of the socket already (Hijack) - before conn itself.
func newAwarenessLimitConn(conn net.Conn, room string, pending []byte) *awarenessLimitConn {
	var source io.Reader = conn
	if len(pending) > 0 {
		source = io.MultiReader(bytes.NewReader(pending), conn)
	}
	return &awarenessLimitConn{Conn: conn, room: room, live: make(map[uint64]struct{}), src: bufio.NewReader(source)}
}

// Read hands ygo the rewritten stream: it reads whole frames from the socket until it has bytes
// for ygo, as the socket's own reads would block until a frame arrives.
func (c *awarenessLimitConn) Read(p []byte) (int, error) {
	for c.out.Len() == 0 && c.through == 0 {
		if err := c.readFrame(); err != nil {
			return 0, err
		}
	}
	if c.out.Len() > 0 {
		return c.out.Read(p)
	}
	if int64(len(p)) > c.through {
		p = p[:c.through]
	}
	read, err := c.src.Read(p)
	c.through -= int64(read)
	return read, err
}

// readFrame reads the next websocket frame the socket sent and puts what ygo is to read of it on
// out, or leaves it to be copied through unread (through).
func (c *awarenessLimitConn) readFrame() error {
	var header [maxFrameHeader]byte
	if _, err := io.ReadFull(c.src, header[:2]); err != nil {
		return err
	}
	length := frameHeaderLength(header[:2])
	if _, err := io.ReadFull(c.src, header[2:length]); err != nil {
		return err
	}
	payload := framePayloadLength(header[:length])
	opcode := header[0] & 0x0f
	final := header[0]&finalFrame != 0
	starts := opcode == gws.BinaryMessage
	if payload < 0 || header[1]&maskedFrame == 0 || (!starts && opcode != continuationFrame) || starts == (c.mode != limitIdle) {
		// A control frame, a text frame, a frame a client left unmasked, a frame out of order or
		// one whose length does not fit an int64: ygo's own reader answers each as it does today,
		// so it passes unread (gorilla/websocket conn.go:800-930).
		c.out.Write(header[:length])
		c.through = max(payload, 0)
		return nil
	}
	copy(c.key[:], header[length-4:length])
	switch c.mode {
	case limitPassing:
		c.out.Write(header[:length])
		c.through = payload
		if final {
			c.mode = limitIdle
		}
		return nil
	case limitSkipping:
		return c.skip(payload, final)
	case limitAwareness:
		return c.collect(payload, final)
	default:
		return c.decide(payload, final)
	}
}

// decide reads as much of the frame as it takes to learn what the message is - the document it
// names and its type - and sets the mode the rest of the message is read in. A message that is
// not awareness is handed to ygo as it stands: the bytes already read go out as one frame and the
// rest of the frame is copied through unread. A message that ends without naming both, or that
// has not named them within maxDocumentMessageHeader bytes, ends the connection: whatever its
// length, it is not a message this service reads, and ygo, which reads the same header, would
// make nothing of it either.
func (c *awarenessLimitConn) decide(payload int64, final bool) error {
	want := payload
	if room := int64(maxDocumentMessageHeader - len(c.message)); want > room {
		// A frame split here leaves the rest of its payload to be copied through under the same
		// masking key, which only lines up on a multiple of the key's four bytes.
		want = room &^ 3
	}
	if want <= 0 {
		return fmt.Errorf("dispatch: a document socket's message does not name its document and type within %d bytes", maxDocumentMessageHeader)
	}
	start := len(c.message)
	c.message = append(c.message, make([]byte, want)...)
	if _, err := io.ReadFull(c.src, c.message[start:]); err != nil {
		return err
	}
	c.unmask(start)
	rest := payload - want
	name, kind, read, named := documentMessageKind(c.message)
	switch {
	case !named && rest > 0:
		return fmt.Errorf("dispatch: a document socket's message does not name its document and type within %d bytes", maxDocumentMessageHeader)
	case !named && final:
		// The whole message is in hand - a message of three bytes as much as one of six hundred -
		// and names neither a document nor a type.
		return fmt.Errorf("dispatch: a document socket's %d-byte message names neither a document nor a type", len(c.message))
	case !named:
		// The header runs into the next frame of the message.
		c.mode = limitDeciding
		return nil
	case kind == hocuspocusAwareness:
		c.name = name
		c.message = append(c.message[:0], c.message[read:]...)
		c.mode = limitAwareness
		return c.collect(rest, final)
	default:
		// Not an awareness message: ygo reads it as it arrived.
		c.pass(rest, final)
		return nil
	}
}

// collect reads the rest of an awareness message - n more bytes of the frame in hand, and the
// frames after it - and rewrites the message once it ends.
func (c *awarenessLimitConn) collect(n int64, final bool) error {
	if int64(len(c.message))+n > maxAwarenessMessageBytes {
		slog.Debug("dispatch: drop a document socket's awareness message past the size one carries",
			"room", c.room, "bytes", int64(len(c.message))+n, "limit", maxAwarenessMessageBytes)
		c.message = c.message[:0]
		return c.skip(n, final)
	}
	start := len(c.message)
	c.message = append(c.message, make([]byte, n)...)
	if _, err := io.ReadFull(c.src, c.message[start:]); err != nil {
		return err
	}
	c.unmask(start)
	c.mode = limitAwareness
	if !final {
		return nil
	}
	c.mode = limitIdle
	c.writeAwareness(c.allowed(c.message))
	c.message = c.message[:0]
	return nil
}

// skip reads n more bytes of an awareness message being dropped - one too large to rewrite
// (collect) and the frames after it - and hands ygo none of what it carried. The message ends
// with its final frame, where ygo is given an awareness message naming no client in its place
// (writeAwareness), which it applies and broadcasts as no change.
func (c *awarenessLimitConn) skip(n int64, final bool) error {
	c.mode = limitSkipping
	if _, err := io.CopyN(io.Discard, c.src, n); err != nil {
		return err
	}
	if final {
		c.mode = limitIdle
		c.writeAwareness(nil)
	}
	return nil
}

// pass hands ygo the message being read as it arrived: the bytes already read as one frame of
// their own, then rest, the bytes of the frame in hand that follow them, copied through unread.
func (c *awarenessLimitConn) pass(rest int64, final bool) {
	c.writeFrame(gws.BinaryMessage, final && rest == 0, c.message)
	c.message = c.message[:0]
	if rest > 0 {
		c.out.Write(frameHeader(continuationFrame, final, rest, c.key[:]))
		c.through = rest
	}
	c.mode = limitIdle
	if !final {
		c.mode = limitPassing
	}
}

// unmask takes the mask off the message's bytes from start, the first of the frame in hand: each
// stands at its own offset within that frame, which the key repeats over every four bytes.
func (c *awarenessLimitConn) unmask(start int) {
	for i := start; i < len(c.message); i++ {
		c.message[i] ^= c.key[(i-start)%4]
	}
}

// allowed is the awareness update a socket may send of the one it sent: the entries of the
// clients it holds, and of the clients it may still take, in the order they arrived. A client it
// takes away gives its place back, and the removal of a client it never held says nothing and is
// dropped with the rest.
func (c *awarenessLimitConn) allowed(message []byte) []byte {
	update, err := encoding.NewDecoder(message).ReadVarBytes()
	if err != nil {
		slog.Debug("dispatch: drop a document socket's awareness message the filter cannot read", "room", c.room, "error", err)
		return nil
	}
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		slog.Debug("dispatch: drop a document socket's awareness update the filter cannot read", "room", c.room, "error", err)
		return nil
	}
	kept := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		_, held := c.live[entry.client]
		switch {
		case entry.removed:
			if !held {
				continue
			}
			delete(c.live, entry.client)
		case held:
		case len(c.live) < maxAwarenessClientsPerSocket:
			c.live[entry.client] = struct{}{}
		default:
			continue
		}
		kept = append(kept, entry.raw)
	}
	if dropped := len(entries) - len(kept); dropped > 0 {
		slog.Debug("dispatch: drop the awareness clients a document socket may not hold",
			"room", c.room, "dropped", dropped, "held", len(c.live), "limit", maxAwarenessClientsPerSocket)
	}
	return encodeAwarenessEntries(kept)
}

// writeAwareness puts the awareness message carrying update - the socket's, with the clients it
// may not hold taken out - on the stream ygo reads. A nil update names no client, which ygo
// applies and broadcasts as no change, as the keep-alive's does (keepalive.go).
func (c *awarenessLimitConn) writeAwareness(update []byte) {
	if update == nil {
		update = noAwarenessChange
	}
	c.writeFrame(gws.BinaryMessage, true, encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(c.name)
		encoder.WriteVarUint(hocuspocusAwareness)
		encoder.WriteVarBytes(update)
	}))
}

// writeFrame puts payload on the stream ygo reads as one frame, masked under the frame's own key
// as every frame a client sends is.
func (c *awarenessLimitConn) writeFrame(opcode byte, final bool, payload []byte) {
	c.out.Write(frameHeader(opcode, final, int64(len(payload)), c.key[:]))
	for i, b := range payload {
		c.out.WriteByte(b ^ c.key[i%4])
	}
}

// documentMessageKind reads a document message's Hocuspocus header - the document it names and
// its type - from the start of the message, and how many bytes the header takes. named is false
// while message is too short to hold both.
func documentMessageKind(message []byte) (name string, kind uint64, read int, named bool) {
	decoder := encoding.NewDecoder(message)
	name, err := decoder.ReadVarString()
	if err != nil {
		return "", 0, 0, false
	}
	kind, err = decoder.ReadVarUint()
	if err != nil {
		return "", 0, 0, false
	}
	return name, kind, len(message) - decoder.Remaining(), true
}
