package docs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// heardFrame is a message a test's peer read, and when.
type heardFrame struct {
	at      time.Time
	message []byte
}

// A browser's provider closes a document socket that has delivered nothing for 30 s, and an idle
// peer is sent nothing by the room once its sync is done. So the socket sends each peer a keep-alive
// at least every documentKeepAlive: here a peer that opens as the dashboard's provider does - its
// token, then its sync step 1 - and then says nothing hears a frame within 15 s of the last, over
// 90 s, and is still served at the end, its sync step 1 answered. Each keep-alive is an awareness
// update naming no client, which the provider applies as no change at all.
func TestAnIdleDocumentSocketIsSentAKeepAliveTheProviderCounts(t *testing.T) {
	service, artifactID := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	schemaVersion := strconv.Itoa(pmdoc.SchemaVersion())
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID + "?schema_version=" + schemaVersion
	connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("connect a peer: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	opened := time.Now()
	var writes sync.Mutex
	send := func(kind uint64, body []byte) {
		t.Helper()
		message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
			encoder.WriteVarString(artifactID)
			encoder.WriteVarUint(kind)
		})
		writes.Lock()
		defer writes.Unlock()
		if err := connection.WriteMessage(gws.BinaryMessage, append(message, body...)); err != nil {
			t.Fatalf("send a message of kind %d: %v", kind, err)
		}
	}
	send(2, encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(0) // a token
		encoder.WriteVarString(schemaVersion)
	}))
	send(0, ygsync.EncodeSyncStep1(crdt.New()))

	frames := make(chan heardFrame, 64)
	go func() {
		defer close(frames)
		for {
			_, message, err := connection.ReadMessage()
			if err != nil {
				return
			}
			frames <- heardFrame{at: time.Now(), message: message}
		}
	}()

	// applied is the awareness a provider holds, which every awareness frame is applied to, and
	// changes counts the updates applying them made it emit.
	applied := awareness.New(7)
	changes := 0
	applied.OnUpdate(func(awareness.UpdateEvent) { changes++ })
	const idle = 90 * time.Second
	allowed := documentKeepAlive + time.Second
	end := opened.Add(idle)
	last, keepAlives := opened, 0
	for {
		timer := time.NewTimer(min(time.Until(last.Add(allowed)), time.Until(end)))
		select {
		case frame, open := <-frames:
			timer.Stop()
			if !open {
				t.Fatalf("the idle socket closed %v after it opened", time.Since(opened).Round(time.Millisecond))
			}
			if gap := frame.at.Sub(last); gap > allowed {
				t.Fatalf("the idle socket heard nothing for %v, past the %v the keep-alive allows", gap.Round(time.Millisecond), allowed)
			}
			last = frame.at
			if awarenessUpdate, ok := awarenessMessage(t, frame.message, artifactID); ok {
				before := changes
				if err := applied.ApplyUpdate(awarenessUpdate, nil); err != nil {
					t.Fatalf("apply the awareness frame %x: %v", frame.message, err)
				}
				// The room's own awareness goes out as the sync ends; a keep-alive comes later.
				if bytes.Equal(awarenessUpdate, noAwarenessChange) && frame.at.Sub(opened) > documentKeepAlive/2 {
					keepAlives++
					if changes != before {
						t.Fatalf("a keep-alive changed the provider's awareness: states %v", applied.GetStates())
					}
				}
			}
			continue
		case <-timer.C:
		}
		if !time.Now().Before(end) {
			break
		}
		t.Fatalf("the idle socket heard nothing for %v, past the %v the keep-alive allows; the provider closes at 30 s",
			time.Since(last).Round(time.Millisecond), allowed)
	}
	if keepAlives < 5 {
		t.Fatalf("the idle socket was sent %d keep-alives in %v, want one every %v", keepAlives, idle, documentKeepAlive)
	}

	send(0, ygsync.EncodeSyncStep1(crdt.New()))
	answered := time.After(5 * time.Second)
	for {
		select {
		case frame, open := <-frames:
			if !open {
				t.Fatal("the socket closed after 90 s idle instead of answering its sync step 1")
			}
			if isSyncStep2(frame.message) {
				return
			}
		case <-answered:
			t.Fatal("the socket, idle 90 s, did not answer its sync step 1 within 5 s")
		}
	}
}

// awarenessMessage is the awareness update message carries when it is a Hocuspocus awareness
// message, and whether it is one. Every message the room sends names its document first.
func awarenessMessage(t *testing.T, message []byte, artifactID string) ([]byte, bool) {
	t.Helper()
	decoder := encoding.NewDecoder(message)
	name, err := decoder.ReadVarString()
	if err != nil || name != artifactID {
		t.Fatalf("a frame %x names document %q (%v), want %s", message, name, err, artifactID)
	}
	if kind, err := decoder.ReadVarUint(); err != nil || kind != hocuspocusAwareness {
		return nil, false
	}
	update, err := decoder.ReadVarBytes()
	if err != nil {
		t.Fatalf("an awareness frame %x carries no update: %v", message, err)
	}
	return update, true
}

// isSyncStep2 is whether message is a Hocuspocus sync message carrying a sync step 2.
func isSyncStep2(message []byte) bool {
	decoder := encoding.NewDecoder(message)
	if _, err := decoder.ReadVarString(); err != nil {
		return false
	}
	if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
		return false
	}
	kind, _, err := ygsync.ReadSyncMessage(decoder.RemainingBytes())
	return err == nil && kind == ygsync.MsgSyncStep2
}

// keepAliveShape is one message writeEveryShape writes, numbered.
func keepAliveShape(number, size int) []byte {
	message := bytes.Repeat([]byte{byte(number)}, size)
	binary.BigEndian.PutUint32(message, uint32(number))
	return message
}

// writeEveryShape writes count numbered messages through conn in each way gorilla/websocket writes
// one - inside its buffer, in one write; past it, a header and the buffer's worth in one write and the
// rest in another; streamed, in several frames - while pings go out among them, and then closes.
func writeEveryShape(conn *gws.Conn, count int) error {
	pinging := make(chan struct{})
	pinged := make(chan struct{})
	go func() {
		defer close(pinged)
		for {
			select {
			case <-pinging:
				return
			case <-time.After(200 * time.Microsecond):
				_ = conn.WriteControl(gws.PingMessage, []byte("p"), time.Now().Add(time.Second))
			}
		}
	}()
	defer func() {
		close(pinging)
		<-pinged
	}()
	for number := range count {
		switch number % 3 {
		case 0:
			if err := conn.WriteMessage(gws.BinaryMessage, keepAliveShape(number, 100)); err != nil {
				return err
			}
		case 1:
			if err := conn.WriteMessage(gws.BinaryMessage, keepAliveShape(number, 70_000)); err != nil {
				return err
			}
		case 2:
			writer, err := conn.NextWriter(gws.BinaryMessage)
			if err != nil {
				return err
			}
			message := keepAliveShape(number, 20_000)
			for len(message) > 0 {
				chunk := message[:min(3_000, len(message))]
				if _, err := writer.Write(chunk); err != nil {
					return err
				}
				message = message[len(chunk):]
			}
			if err := writer.Close(); err != nil {
				return err
			}
		}
		if number%20 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	return conn.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, "done"), time.Now().Add(time.Second))
}

// The keep-alive goes onto the connection under gorilla/websocket's writer, so it must go out only
// between that writer's messages: never inside a frame whose payload gorilla writes in two parts,
// never between the frames of a message it streams, and never after its close. A writer sending
// every shape of message gorilla writes, with a keep-alive due every 100 µs, reaches a reader that
// refuses any frame out of place with every message whole and in order, and keep-alives among them.
func TestAKeepAliveGoesOutOnlyBetweenTheWritersMessages(t *testing.T) {
	const count = 600
	written := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keepAlive := newKeepAliveWriter(w, "room", 100*time.Microsecond)
		defer keepAlive.stop()
		var upgrader gws.Upgrader
		conn, err := upgrader.Upgrade(keepAlive, r, nil)
		if err != nil {
			written <- err
			return
		}
		defer conn.Close()
		written <- writeEveryShape(conn, count)
		// The reader ends the connection once it has read the close.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	conn, response, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: response=%#v err=%v", response, err)
	}
	defer conn.Close()
	keepAlive := keepAliveFrame("room")[2:]
	next, keepAlives := 0, 0
	for {
		_, message, err := conn.ReadMessage()
		if closed := (*gws.CloseError)(nil); errors.As(err, &closed) && closed.Code == gws.CloseNormalClosure {
			break
		}
		if err != nil {
			t.Fatalf("read after %d messages and %d keep-alives: %v", next, keepAlives, err)
		}
		if bytes.Equal(message, keepAlive) {
			keepAlives++
			continue
		}
		want := keepAliveShape(next, len(message))
		if next >= count || !bytes.Equal(message, want) || len(message) != []int{100, 70_000, 20_000}[next%3] {
			t.Fatalf("message %d read as %d bytes beginning %x, want message %d whole", next, len(message), message[:min(8, len(message))], next)
		}
		next++
	}
	if err := <-written; err != nil {
		t.Fatalf("write: %v", err)
	}
	if next != count || keepAlives == 0 {
		t.Fatalf("read %d of %d messages and %d keep-alives, want all of them and keep-alives among them", next, count, keepAlives)
	}
}

// The frame watch finds the end of each message the server writes, wherever a write splits its
// bytes: after the upgrade's response, after a final data frame, never inside a frame's header or
// payload, never between the frames of a fragmented message (a control frame among them included),
// and never once a close frame has begun.
func TestTheFrameWatchFindsTheEndOfEachMessage(t *testing.T) {
	stream := []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	ends := []int{len(stream)}
	frame := func(first byte, payload int, masked, endsMessage bool) {
		second := byte(0)
		if masked {
			second = maskedFrame
		}
		header := []byte{first, second}
		switch {
		case payload < 126:
			header[1] |= byte(payload)
		case payload <= 0xffff:
			header[1] |= 126
			header = binary.BigEndian.AppendUint16(header, uint16(payload))
		default:
			header[1] |= 127
			header = binary.BigEndian.AppendUint64(header, uint64(payload))
		}
		if masked {
			header = append(header, 1, 2, 3, 4)
		}
		stream = append(append(stream, header...), bytes.Repeat([]byte{'x'}, payload)...)
		if endsMessage {
			ends = append(ends, len(stream))
		}
	}
	frame(finalFrame|gws.BinaryMessage, 5, false, true)
	frame(gws.TextMessage, 3, false, false)              // a message's first frame
	frame(finalFrame|gws.PingMessage, 2, false, false)   // a control frame inside it
	frame(0, 200, false, false)                          // a continuation, two-byte length
	frame(finalFrame, 70_000, false, true)               // its final frame, eight-byte length
	frame(finalFrame|gws.BinaryMessage, 4, true, true)   // masked
	frame(finalFrame|gws.PongMessage, 0, false, true)    // a control frame between messages
	frame(finalFrame|gws.CloseMessage, 2, false, false)  // the close
	frame(finalFrame|gws.BinaryMessage, 1, false, false) // nothing is between messages after it
	var watch frameWatch
	for written := 0; written <= len(stream); written++ {
		if between, want := watch.betweenMessages(), slices.Contains(ends, written); between != want {
			t.Fatalf("after %d of %d bytes betweenMessages = %v, want %v", written, len(stream), between, want)
		}
		if written < len(stream) {
			watch.wrote(stream[written : written+1])
		}
	}
	var whole frameWatch
	whole.wrote(stream[:ends[len(ends)-1]])
	if !whole.betweenMessages() {
		t.Fatal("the stream up to its last message's end, written at once, is not between messages")
	}
}
