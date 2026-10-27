package docs

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Opening a document socket is what every browser tab does on every load and every reconnect, and
// an eviction or a deploy reconnects every browser of a room at once. The tests here are about what
// one open costs the service beyond the socket itself: how often it renders the whole document, how
// it decides the caps without walking every room, and that the one read of the issue it keeps still
// sees an issue closed while the socket loads.

// socketURL is id's document socket on serverURL, at the schema version the dashboard presents, so
// the only thing that can make the socket read-only is the document's issue.
func socketURL(serverURL, id string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + id +
		"?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
}

// admitSocket opens id's document socket as alice's browser does and returns it once the room has
// sent the first frame of its sync, so the socket's admission, and a cold room's load, are done.
func admitSocket(t *testing.T, serverURL, id, what string) *gws.Conn {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("%s: the socket was refused (%s: %v), want it admitted", what, status, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, message, err := connection.ReadMessage(); err != nil {
		t.Fatalf("%s: the socket's first frame = %q (%v), want the room's sync", what, message, err)
	}
	_ = connection.SetReadDeadline(time.Time{})
	return connection
}

// refuseSocket opens id's document socket and requires the service to refuse it with
// ServiceUnavailable before any upgrade, as a cap refuses one.
func refuseSocket(t *testing.T, serverURL, id, what string) {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
	if connection != nil {
		leaveDocument(connection)
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%s: response=%v err=%v, want the socket refused with 503", what, response, err)
	}
}

// hocuspocusScope sends the Hocuspocus authentication frame a provider sends once its socket is
// open, carrying token as the dashboard carries its schema version, and returns the scope the
// server's authenticated reply names: "readonly" or "read-write".
func hocuspocusScope(t *testing.T, connection *gws.Conn, room, token string) string {
	t.Helper()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(2) // Hocuspocus authentication message.
		encoder.WriteVarUint(0) // Token authentication payload.
		encoder.WriteVarString(token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, frame); err != nil {
		t.Fatalf("send the Hocuspocus authentication frame: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the Hocuspocus authentication reply: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read the Hocuspocus document name: %v", err)
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 2 {
			continue
		}
		subtype, err := decoder.ReadVarUint()
		if err != nil || subtype != 2 {
			t.Fatalf("Hocuspocus authentication reply subtype = %d (%v), want authenticated", subtype, err)
		}
		scope, err := decoder.ReadVarString()
		if err != nil {
			t.Fatalf("read the Hocuspocus authentication scope: %v", err)
		}
		return scope
	}
}

// registeredConnections is how many browser connections the service's room records hold, read
// by the test itself rather than through the service's own count.
func registeredConnections(service *Service) int {
	count := 0
	service.rooms.Range(func(_, value any) bool {
		state := value.(*roomState)
		state.mu.Lock()
		count += len(state.connected)
		state.mu.Unlock()
		return true
	})
	return count
}

// A socket's admission refuses a room outside the Proof schema, which it decides by rendering the
// document. A warm room has already been rendered by its update observer after every update, so
// its verdict is known and the open renders nothing; a cold open renders the document it loads
// once, not once to admit the socket and again when the room it loads takes the same state. The
// verdict is still right: a room an update took outside the schema is refused, and the same room
// repaired by a replacement from markdown admits sockets again.
func TestAWarmSocketOpenRendersNothingAndAColdOpenRendersOnce(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var renders atomic.Int64
	service.admissionRendered = func(room string) {
		if room == artifactID {
			renders.Add(1)
		}
	}

	before := renders.Load()
	admitSocket(t, server.URL, artifactID, "a cold open")
	if cold := renders.Load() - before; cold != 1 {
		t.Errorf("a cold socket open rendered the whole document %d times, want once", cold)
	}

	before = renders.Load()
	admitSocket(t, server.URL, artifactID, "a warm open")
	if warm := renders.Load() - before; warm != 0 {
		t.Errorf("a warm socket open rendered the whole document %d times, want none: the room's update observer already rendered it", warm)
	}

	writeSchemaInvalidElement(t, service, artifactID)
	connection, response, err := gws.DefaultDialer.Dial(socketURL(server.URL, artifactID), http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("open the room an update took outside the schema: response=%v err=%v, want an upgrade the server closes", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, message, err := connection.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
		t.Fatalf("first frame of a socket to a room outside the schema = %q (%v), want close %d before any sync", message, err, documentSchemaCloseCode)
	}

	if _, err := joinedReplaceText(service, artifactID, "Repaired.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("repair the document by a replacement from markdown: %v", err)
	}
	admitSocket(t, server.URL, artifactID, "a socket to the repaired room")
}

// A socket's admission reads the document's issue once, before it loads the document. An issue
// closed while that load runs - a person closes the issue as a browser reconnects - still reaches
// the socket: it is admitted read-only, as a socket opened after the close is.
func TestAnIssueClosedWhileASocketLoadsAdmitsItReadOnly(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	var closed atomic.Bool
	service.afterSocketIssueRead = func(room string) {
		if room != artifactID || closed.Swap(true) {
			return
		}
		ctx := context.Background()
		if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
			t.Errorf("close the document's issue while the socket loads: %v", err)
			return
		}
		service.SetIssueClosed(ctx, "DOC-1", true)
	}

	connection := admitSocket(t, server.URL, artifactID, "a socket whose issue closes while it loads")
	if !closed.Load() {
		t.Fatal("the socket's admission never reached the hook that closes the issue, so nothing here is tested")
	}
	if scope := hocuspocusScope(t, connection, artifactID, strconv.Itoa(pmdoc.SchemaVersion())); scope != "readonly" {
		t.Fatalf("the socket whose issue closed while it loaded is authenticated %q, want readonly", scope)
	}
}

// warmRooms loads each document's room through the service, a few at a time, as API reads warm
// them.
func warmRooms(t *testing.T, service *Service, ids []string) {
	t.Helper()
	work := make(chan string)
	var failures sync.Map
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range work {
				if err := service.warmLiveDocument(context.Background(), id); err != nil {
					failures.Store(id, err)
				}
			}
		}()
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	workers.Wait()
	failures.Range(func(id, err any) bool {
		t.Fatalf("warm the room of document %s: %v", id, err)
		return false
	})
}

// The service admits at most maxLiveRooms live rooms and maxRoomConnections browser connections.
// Each socket open asks both, so each is a count the service keeps as rooms load and go and as
// browsers come and leave, not a walk over every room record and every live room name: an
// eviction or a deploy reconnects every browser at once, and each of those walks costs every room.
// The counts stay right across a failed room's eviction, whose record goes and hands the browsers
// registered on it to the record that replaces it (handOverFailedRoomLocked).
//
// The rooms are real: the service loads a thousand of them, as API reads do. The connections
// beyond the test's own sockets are registered through the service's own path for a browser that
// joins (addConnection), since a thousand real sockets would test the operating system's limits
// more than the service's count.
func TestTheConnectionAndRoomCapsCountWithoutScanning(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	// ygo's idle sweeper reads it when the first room starts it, so it is set before any room:
	// no room may idle out while the test fills the cap.
	service.srv.RoomIdleTimeout = time.Hour
	ids := createDocuments(t, database, maxLiveRooms+2)
	resident, spare := ids[:maxLiveRooms], ids[maxLiveRooms:]
	warmRooms(t, service, resident)
	if live := len(service.srv.Rooms()); live != maxLiveRooms {
		t.Fatalf("%d rooms are live after the test loaded %d, so the cap is not what is tested", live, maxLiveRooms)
	}
	scans := service.CountRoomScansForTest()

	// The room cap: the 1,001st room is refused, while a live room still admits a socket.
	refuseSocket(t, server.URL, spare[0], "the 1,001st room")
	leaveDocument(admitSocket(t, server.URL, resident[0], "a live room while the room cap is reached"))
	waitFor(t, roomWait(t), "the socket that left to be forgotten", func() bool {
		return registeredConnections(service) == 0
	})

	// A failed room's eviction: its room goes, so the cap admits one new room and only one, and
	// the browsers registered on its record move to the record that replaces it.
	failed := resident[1]
	const handedOver = 500
	alice := model.Actor{Kind: "user", ID: "alice"}
	registered := uint64(1) << 40 // far above the ids the service gives real sockets
	for range handedOver {
		registered++
		service.addConnection(failed, registered, alice)
	}
	firstHandedOver := registered
	service.failRoom(failed, errors.New("the test fails the room"))
	if err := service.awaitRoomRecovery(ctx, failed); err != nil {
		t.Fatalf("wait for the failed room's eviction: %v", err)
	}
	if service.srv.GetDoc(failed) != nil {
		t.Fatal("the failed room is still live after its eviction")
	}
	if got := registeredConnections(service); got != handedOver {
		t.Fatalf("the service's records hold %d connections after the failed room's hand-over, want the %d registered on it", got, handedOver)
	}
	admitSocket(t, server.URL, spare[0], "a new room once a failed room's eviction freed its place")
	refuseSocket(t, server.URL, spare[1], "the 1,001st room after the failed room's place was taken")

	// The connection cap, counting the browsers the hand-over carried: with 999 connections the
	// next socket is the 1,000th and is admitted, the one after it is refused, and once one of the
	// handed-over browsers leaves a socket is admitted again.
	for registeredConnections(service) < maxRoomConnections-1 {
		registered++
		service.addConnection(resident[2], registered, alice)
	}
	admitSocket(t, server.URL, resident[3], "the 1,000th connection")
	refuseSocket(t, server.URL, resident[4], "the 1,001st connection")
	service.removeConnection(failed, firstHandedOver)
	admitSocket(t, server.URL, resident[4], "a connection once a handed-over browser left")
	refuseSocket(t, server.URL, resident[5], "the 1,001st connection after a handed-over browser left")

	if scanned := scans.Load(); scanned != 0 {
		t.Errorf("the socket opens above walked the rooms %d times, want none: each cap is a count kept as rooms and connections come and go", scanned)
	}
}
