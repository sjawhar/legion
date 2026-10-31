package docs

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// socketCloseWait is how long the tests here give the service to close a socket it refuses. The
// filter decides on the frame itself, so the close follows the frame by a round trip.
const socketCloseWait = 5 * time.Second

// A document socket whose first message names neither a document nor a message type within the
// bytes the filter reads for them (maxDocumentMessageHeader) is sending nothing this service
// reads, and its connection ends rather than carry a frame past the filter unread. The message's
// length is no part of it: three bytes holding one variable-length integer that never ends name
// as little as six hundred bytes claiming a document name of 100,000. It takes a real upgrade to
// show: the filter sits on the connection the upgrade hijacks, which gorilla may read through the
// hijacked buffered reader instead, so the reader is reset onto the filter
// (awarenessLimitWriter.Hijack) - without that the frame reaches ygo and the socket stays open.
func TestADocumentSocketWhoseFirstMessageNamesNoDocumentIsClosedThroughARealUpgrade(t *testing.T) {
	// 600 bytes claiming a document name of 100,000: the message names no document, let alone a
	// type, within the maxDocumentMessageHeader bytes the filter reads for them.
	unnameable := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(100_000)
		encoder.WriteRaw(bytes.Repeat([]byte{'x'}, 600))
	})
	if len(unnameable) <= maxDocumentMessageHeader {
		t.Fatalf("the message is %d bytes, which the filter reads whole; want more than %d", len(unnameable), maxDocumentMessageHeader)
	}
	for _, message := range []struct {
		name  string
		first []byte
	}{
		{name: "three bytes of an integer that never ends", first: []byte{0xff, 0xff, 0xff}},
		{name: "600 bytes naming no document", first: unnameable},
	} {
		t.Run(message.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "First.\n")
			server := newTestServer(t, service)
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID
			socket, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"mallory"}})
			if err != nil {
				t.Fatalf("connect a socket to document %s: response=%#v err=%v", artifactID, response, err)
			}
			t.Cleanup(func() { _ = socket.Close() })

			if err := socket.WriteMessage(gws.BinaryMessage, message.first); err != nil {
				t.Fatalf("send a first message that names no document: %v", err)
			}
			if err := socket.SetReadDeadline(time.Now().Add(socketCloseWait)); err != nil {
				t.Fatalf("set the socket's read deadline: %v", err)
			}
			for {
				_, _, err := socket.ReadMessage()
				if err == nil {
					continue // the document the room sends every socket, or a keep-alive
				}
				if errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("the socket is still open %v after a %d-byte first message that names neither a document nor a type: want it closed",
						socketCloseWait, len(message.first))
				}
				return
			}
		})
	}
}

// hijackedWriter is a response writer whose Hijack hands out conn and a buffered reader already
// holding buffered - what an HTTP server has read of the socket past the request, which a client
// that sends its first frame without waiting for the upgrade's answer leaves there.
type hijackedWriter struct {
	conn     net.Conn
	buffered []byte
}

func (w *hijackedWriter) Header() http.Header { return http.Header{} }
func (w *hijackedWriter) Write([]byte) (int, error) {
	return 0, http.ErrNotSupported
}
func (w *hijackedWriter) WriteHeader(int) {}

func (w *hijackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(w.buffered), w.conn))
	if _, err := reader.Peek(len(w.buffered)); err != nil {
		return nil, nil, err
	}
	return w.conn, bufio.NewReadWriter(reader, bufio.NewWriter(w.conn)), nil
}

// A socket's first frame, sent without waiting for the upgrade's answer, is read by the HTTP
// server before the hijack and sits in the hijacked reader. The filter is given those bytes to
// read before the connection's own, so the frame is filtered like any other rather than lost
// when the reader is reset onto the filter (awarenessLimitWriter.Hijack).
func TestADocumentSocketsFrameSentBeforeTheUpgradesAnswerIsFiltered(t *testing.T) {
	const invented = 10
	writer := &hijackedWriter{
		conn:     &scriptedSocket{frames: bytes.NewReader(nil)},
		buffered: clientFrame(gws.BinaryMessage, true, awarenessMessageOf(limitRoom, awarenessStates(500_000, invented, 1, `{"user":{"name":"invented"}}`))),
	}
	conn, buffers, err := (&awarenessLimitWriter{ResponseWriter: writer, room: limitRoom}).Hijack()
	if err != nil {
		t.Fatalf("hijack the socket: %v", err)
	}
	if held := buffers.Reader.Buffered(); held != 0 {
		t.Fatalf("the hijacked reader holds %d bytes of the socket's first frame after the hijack, which the filter never reads", held)
	}
	filter, isFilter := conn.(*awarenessLimitConn)
	if !isFilter {
		t.Fatalf("the hijack handed out a %T, want the socket's filter", conn)
	}

	messages := filtered(t, filter)
	if len(messages) != 1 {
		t.Fatalf("a first frame of %d awareness clients sent before the upgrade's answer reached ygo as %d messages, want one", invented, len(messages))
	}
	room, clients := awarenessClients(t, messages[0])
	if room != limitRoom {
		t.Errorf("the filtered awareness message names document %q, want %q", room, limitRoom)
	}
	if len(clients) != maxAwarenessClientsPerSocket {
		t.Errorf("a first frame of %d awareness clients reached ygo as clients %v, want the socket's first %d",
			invented, clients, maxAwarenessClientsPerSocket)
	}
}
