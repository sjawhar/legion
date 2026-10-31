package docs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// limitRoom is the document the sockets in these tests are on.
const limitRoom = "0c4b4f2e-9b1f-4f9a-9c3a-1f6a2b5d7e80"

// scriptedSocket is the connection a document socket's filter reads: it hands out the frames a
// test wrote for it and then ends, and discards what ygo writes back.
type scriptedSocket struct{ frames *bytes.Reader }

func (s *scriptedSocket) Read(p []byte) (int, error)       { return s.frames.Read(p) }
func (s *scriptedSocket) Write(p []byte) (int, error)      { return len(p), nil }
func (s *scriptedSocket) Close() error                     { return nil }
func (s *scriptedSocket) LocalAddr() net.Addr              { return nil }
func (s *scriptedSocket) RemoteAddr() net.Addr             { return nil }
func (s *scriptedSocket) SetDeadline(time.Time) error      { return nil }
func (s *scriptedSocket) SetReadDeadline(time.Time) error  { return nil }
func (s *scriptedSocket) SetWriteDeadline(time.Time) error { return nil }

// socketSending is the filter over a socket sending frames.
func socketSending(frames ...[]byte) *awarenessLimitConn {
	return newAwarenessLimitConn(&scriptedSocket{frames: bytes.NewReader(bytes.Join(frames, nil))}, limitRoom, nil)
}

// clientFrame is one websocket frame as a browser sends it (RFC 6455 section 5.2): masked, with
// its own opcode and FIN bit. It is written out byte by byte rather than through the filter's own
// encoder, so a fault in that encoder cannot cancel itself out here.
func clientFrame(opcode byte, final bool, payload []byte) []byte {
	key := []byte{0x11, 0x22, 0x33, 0x44}
	first := opcode
	if final {
		first |= 0x80
	}
	frame := []byte{first}
	switch length := len(payload); {
	case length < 126:
		frame = append(frame, 0x80|byte(length))
	case length <= 0xffff:
		frame = binary.BigEndian.AppendUint16(append(frame, 0x80|126), uint16(length))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 0x80|127), uint64(length))
	}
	frame = append(frame, key...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	return frame
}

// readMessage is one message ygo reads through the filter: the opcode of its first frame and the
// payload its frames carry.
type readMessage struct {
	opcode  byte
	payload []byte
}

// filtered is what ygo reads of the frames a socket sent: the filter's whole stream, split into
// the messages its frames make.
func filtered(t *testing.T, conn *awarenessLimitConn) []readMessage {
	t.Helper()
	stream, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the filtered stream: %v", err)
	}
	var messages []readMessage
	for rest := stream; len(rest) > 0; {
		if len(rest) < 2 {
			t.Fatalf("the filtered stream ends inside a frame header: % x", rest)
		}
		if rest[1]&0x80 == 0 {
			t.Fatalf("the filter wrote an unmasked frame, which a websocket server refuses: % x", rest[:2])
		}
		opcode := rest[0] & 0x0f
		length, header := int(rest[1]&^0x80), 2
		switch length {
		case 126:
			length, header = int(binary.BigEndian.Uint16(rest[2:4])), 4
		case 127:
			length, header = int(binary.BigEndian.Uint64(rest[2:10])), 10
		}
		key := rest[header : header+4]
		header += 4
		if len(rest) < header+length {
			t.Fatalf("the filtered stream ends inside a frame's payload")
		}
		payload := make([]byte, length)
		for i := range payload {
			payload[i] = rest[header+i] ^ key[i%4]
		}
		rest = rest[header+length:]
		if opcode == continuationFrame && len(messages) > 0 {
			last := &messages[len(messages)-1]
			last.payload = append(last.payload, payload...)
			continue
		}
		messages = append(messages, readMessage{opcode: opcode, payload: payload})
	}
	return messages
}

// documentMessage is one Hocuspocus-framed message of room: its type and body.
func documentMessage(room string, kind uint64, body []byte) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(kind)
		encoder.WriteRaw(body)
	})
}

// awarenessMessageOf is room's awareness message carrying update, as a socket sends one.
func awarenessMessageOf(room string, update []byte) []byte {
	return documentMessage(room, hocuspocusAwareness, encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarBytes(update)
	}))
}

// awarenessClients is the document an awareness message names and the clients it carries.
func awarenessClients(t *testing.T, message readMessage) (string, []uint64) {
	t.Helper()
	if message.opcode != gws.BinaryMessage {
		t.Fatalf("the filter wrote opcode %d, want a binary message", message.opcode)
	}
	decoder := encoding.NewDecoder(message.payload)
	room, err := decoder.ReadVarString()
	if err != nil {
		t.Fatalf("read the message's document name: %v", err)
	}
	kind, err := decoder.ReadVarUint()
	if err != nil {
		t.Fatalf("read the message's type: %v", err)
	}
	if kind != hocuspocusAwareness {
		t.Fatalf("the filter wrote a message of type %d, want an awareness message", kind)
	}
	update, err := decoder.ReadVarBytes()
	if err != nil {
		t.Fatalf("read the message's awareness update: %v", err)
	}
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		t.Fatalf("read the awareness update's clients: %v", err)
	}
	clients := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		clients = append(clients, entry.client)
	}
	return room, clients
}

// One tab's frame naming thousands of invented awareness clients reaches ygo as the clients the
// socket may hold, in the order the frame named them, under the document the frame named.
func TestADocumentSocketAnnouncesNoMoreAwarenessClientsThanItsBound(t *testing.T) {
	const invented = 2_048
	messages := filtered(t, socketSending(clientFrame(gws.BinaryMessage, true,
		awarenessMessageOf(limitRoom, awarenessStates(500_000, invented, 1, `{"user":{"name":"invented"}}`)))))

	if len(messages) != 1 {
		t.Fatalf("a frame of %d awareness clients reached ygo as %d messages, want one", invented, len(messages))
	}
	room, clients := awarenessClients(t, messages[0])
	if room != limitRoom {
		t.Errorf("the filtered awareness message names document %q, want %q", room, limitRoom)
	}
	want := []uint64{500_000, 500_001, 500_002, 500_003}
	if len(want) != maxAwarenessClientsPerSocket || !slices.Equal(clients, want) {
		t.Errorf("a frame of %d awareness clients reached ygo as clients %v, want the socket's first %d: %v",
			invented, clients, maxAwarenessClientsPerSocket, want)
	}
}

// A socket that takes one of its awareness clients away may announce another in its place, and
// the removal of a client it never held - which leaves an entry in every room the relay reaches -
// never leaves the socket.
func TestADocumentSocketTakingAnAwarenessClientAwayMayAnnounceAnother(t *testing.T) {
	removal := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(2)
		encoder.WriteVarUint(11) // held: this socket announced it
		encoder.WriteVarUint(2)
		encoder.WriteVarString("null")
		encoder.WriteVarUint(99) // never held: the socket invented it
		encoder.WriteVarUint(2)
		encoder.WriteVarString("null")
	})
	messages := filtered(t, socketSending(
		clientFrame(gws.BinaryMessage, true, awarenessMessageOf(limitRoom, awarenessStates(11, 4, 1, `{"user":{"name":"tab"}}`))),
		clientFrame(gws.BinaryMessage, true, awarenessMessageOf(limitRoom, removal)),
		clientFrame(gws.BinaryMessage, true, awarenessMessageOf(limitRoom, awarenessStates(21, 1, 1, `{"user":{"name":"tab"}}`))),
	))

	if len(messages) != 3 {
		t.Fatalf("three awareness frames reached ygo as %d messages, want three", len(messages))
	}
	if _, clients := awarenessClients(t, messages[0]); !slices.Equal(clients, []uint64{11, 12, 13, 14}) {
		t.Fatalf("the socket's first frame reached ygo as clients %v, want 11 to 14", clients)
	}
	if _, clients := awarenessClients(t, messages[1]); !slices.Equal(clients, []uint64{11}) {
		t.Errorf("the removal reached ygo as clients %v, want client 11 alone: a null state for a client the socket never held says nothing", clients)
	}
	if _, clients := awarenessClients(t, messages[2]); !slices.Equal(clients, []uint64{21}) {
		t.Errorf("the client announced after one left reached ygo as %v, want client 21: a client that leaves gives its place back", clients)
	}
}

// Every other message reaches ygo exactly as it arrived, whatever its size: the filter reads only
// as much of one as it takes to see that it is not awareness.
func TestADocumentSocketsOtherMessagesReachYgoUnchanged(t *testing.T) {
	update := documentMessage(limitRoom, 0, bytes.Repeat([]byte{0xa5, 0x5a, 0x3c}, 20_000))
	auth := documentMessage(limitRoom, 2, []byte(`{"token":"x"}`))
	ping := []byte("ping")
	messages := filtered(t, socketSending(
		clientFrame(gws.BinaryMessage, true, update),
		clientFrame(gws.PingMessage, true, ping),
		clientFrame(gws.BinaryMessage, true, auth),
	))

	if len(messages) != 3 {
		t.Fatalf("three messages reached ygo as %d, want three", len(messages))
	}
	if !bytes.Equal(messages[0].payload, update) {
		t.Errorf("a %d-byte sync message reached ygo as %d bytes, want it unchanged", len(update), len(messages[0].payload))
	}
	if messages[1].opcode != gws.PingMessage || !bytes.Equal(messages[1].payload, ping) {
		t.Errorf("a ping reached ygo as opcode %d carrying %q, want a ping carrying %q", messages[1].opcode, messages[1].payload, ping)
	}
	if !bytes.Equal(messages[2].payload, auth) {
		t.Errorf("an auth message reached ygo as %q, want it unchanged", messages[2].payload)
	}
}

// A socket that splits its awareness frame across websocket frames is held to the same bound: the
// filter reads the message its frames make, not each frame on its own.
func TestAFragmentedAwarenessFrameIsHeldToTheSameBound(t *testing.T) {
	message := awarenessMessageOf(limitRoom, awarenessStates(700_000, 64, 1, `{"user":{"name":"invented"}}`))
	messages := filtered(t, socketSending(
		clientFrame(gws.BinaryMessage, false, message[:3]),
		clientFrame(continuationFrame, false, message[3:40]),
		clientFrame(continuationFrame, true, message[40:]),
	))

	if len(messages) != 1 {
		t.Fatalf("a fragmented awareness frame reached ygo as %d messages, want one", len(messages))
	}
	room, clients := awarenessClients(t, messages[0])
	if room != limitRoom {
		t.Errorf("the filtered awareness message names document %q, want %q", room, limitRoom)
	}
	if want := []uint64{700_000, 700_001, 700_002, 700_003}; !slices.Equal(clients, want) {
		t.Errorf("a fragmented frame of 64 awareness clients reached ygo as %v, want the socket's first %d: %v",
			clients, maxAwarenessClientsPerSocket, want)
	}
}

// An awareness message past the size one carries reaches ygo naming no client, which it applies
// and broadcasts as no change, rather than being read whole to be filtered.
func TestAnAwarenessMessagePastTheSizeOneCarriesNamesNoClient(t *testing.T) {
	huge := awarenessMessageOf(limitRoom, awarenessStates(1, 1, 1, `{"user":{"name":"`+strings.Repeat("x", maxAwarenessMessageBytes)+`"}}`))
	messages := filtered(t, socketSending(clientFrame(gws.BinaryMessage, true, huge)))

	if len(messages) != 1 {
		t.Fatalf("a %d-byte awareness message reached ygo as %d messages, want one", len(huge), len(messages))
	}
	if _, clients := awarenessClients(t, messages[0]); len(clients) != 0 {
		t.Errorf("a %d-byte awareness message reached ygo naming %d clients, want none", len(huge), len(clients))
	}
}

// A socket whose message does not name its document and type in the bytes the filter reads for
// them is sending nothing this service reads, and its connection ends rather than carry a frame
// past the filter unread.
func TestADocumentSocketWhoseMessageNeverNamesItselfEnds(t *testing.T) {
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(100_000) // a document name longer than the message itself
		encoder.WriteRaw(bytes.Repeat([]byte{'x'}, 4*maxDocumentMessageHeader))
	})
	if _, err := io.ReadAll(socketSending(clientFrame(gws.BinaryMessage, true, message))); err == nil {
		t.Fatal("a message naming no document or type was read through, want the connection to end")
	}
}
