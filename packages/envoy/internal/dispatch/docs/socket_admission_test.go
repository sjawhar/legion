package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
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

// openedSocket is what a dial off the test's own goroutine answers with (dialSocketAsync): the
// socket it opened, or why it was refused, with the response's status before it.
type openedSocket struct {
	connection *gws.Conn
	err        error
}

// dialSocketAsync dials id's document socket as alice's browser does, on a goroutine of its own,
// and answers on the channel it returns. A test dials this way when the open it is about to make
// holds inside the service - in an admission's rendering, in a room's load - where a dial on the
// test's own goroutine would wait for the very hold that goroutine has to release.
func dialSocketAsync(serverURL, id string) <-chan openedSocket {
	opened := make(chan openedSocket, 1)
	go func() {
		connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, id), http.Header{"X-Dispatch-User": []string{"alice"}})
		if err != nil {
			status := "no response"
			if response != nil {
				status = response.Status
			}
			err = fmt.Errorf("%s: %w", status, err)
		}
		opened <- openedSocket{connection, err}
	}()
	return opened
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

// addConnection registers a browser connected to room as a document socket's admission does: it
// takes a place under the connection cap (reserveConnection), as ServeHTTP does, and registers the
// browser against it (registerConnectionLocked), as authorize does. An id registered already keeps
// its one place, so the count counts each id once however often a test registers it. A test that
// registers past the cap panics, since the browser it means to connect would have been refused.
func (s *Service) addConnection(room string, id uint64, actor model.Actor) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	if _, registered := state.connected[id]; !registered && !s.reserveConnection() {
		panic(fmt.Sprintf("register connection %d on room %s: the connection cap (%d) is reached", id, room, s.connectionCap))
	}
	s.registerConnectionLocked(state, id, actor)
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

// A cold socket whose admission another socket's load overtakes - its own read of the store began
// while the room was cold, and the room became resident before it could offer what it read - keeps
// nothing of that read. The room's load will never read its offer, so the offer goes at that load:
// neither the whole history it carries nor its hold on the document's head-tracker entry, which the
// liveness ping follows (holdsWhatTheTrackerProtects), waits for the socket to close, which can be
// hours later. The socket itself is admitted and current all the same.
func TestAPreloadOvertakenByTheRoomsLoadIsReleasedWhileItsSocketStaysOpen(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	// The first admission of this room holds here, where it has read and rendered the document and
	// has not yet offered either to the room's load (admitDocument). Every admission after it goes
	// through, so the socket that loads the room is admitted while this one waits.
	rendered := make(chan struct{})
	release := make(chan struct{})
	var overtaken atomic.Bool
	var releasedOnce sync.Once
	releaseOvertaken := func() { releasedOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseOvertaken)
	service.admissionRendered = func(room string) {
		if room != artifactID || overtaken.Swap(true) {
			return
		}
		close(rendered)
		<-release
	}

	cold := dialSocketAsync(server.URL, artifactID)
	select {
	case <-rendered:
	case <-time.After(roomWait(t)):
		t.Fatal("the first cold socket never rendered the document it read, so nothing here is tested")
	}

	admitSocket(t, server.URL, artifactID, "the socket whose load makes the room resident")
	releaseOvertaken()
	overtakenSocket := <-cold
	if overtakenSocket.err != nil {
		t.Fatalf("the overtaken socket was refused (%v), want it admitted to the resident room", overtakenSocket.err)
	}
	t.Cleanup(func() { _ = overtakenSocket.connection.Close() })
	_ = overtakenSocket.connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	if _, message, err := overtakenSocket.connection.ReadMessage(); err != nil {
		t.Fatalf("the overtaken socket's first frame = %q (%v), want the room's sync", message, err)
	}

	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload is held for a room already loaded here, want none: no load of this room will ever read it")
	}

	// The socket joined the room it never loaded, so what the room commits reaches it: the
	// release above took its offer back, not its place in the room.
	if _, err := joinedReplaceText(service, artifactID, "Overtaken.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write the document the overtaken socket joined: %v", err)
	}
	_ = overtakenSocket.connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	if _, message, err := overtakenSocket.connection.ReadMessage(); err != nil {
		t.Fatalf("the overtaken socket's frame after a write = %q (%v), want the write's update", message, err)
	}
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
		service.issueOpenChanged(ctx, "DOC-1")
	}

	peer := connectPeer(t, server.URL, artifactID, "alice")
	if !closed.Load() {
		t.Fatal("the socket's admission never reached the hook that closes the issue, so nothing here is tested")
	}
	// The room's answer to the peer's sync step 1 says its load is done, as the first frame does
	// for any socket here: the load is what reads the issue the close left (admittedIssueRead),
	// and the scope the room grants is what it recorded (authorizeSchemaVersion).
	select {
	case <-peer.Answers:
	case <-time.After(roomWait(t)):
		t.Fatal("the socket never received the document from the room it loaded")
	}
	scope, err := peer.Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), roomWait(t))
	if err != nil || scope != "readonly" {
		t.Fatalf("the socket whose issue closed while it loaded is authenticated %q (%v), want readonly", scope, err)
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
// beyond the test's own sockets are registered as a socket's admission registers its browser
// (addConnection), since a thousand real sockets would test the operating system's limits more
// than the service's count.
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
}

// The caps DISPATCH_MAX_LIVE_ROOMS and DISPATCH_MAX_DOC_CONNECTIONS set hold for sockets that open
// at once, as every browser of a room does after an eviction or a deploy, and not only for sockets
// that open one after another: a burst of cold opens never leaves more rooms live, or more browsers
// connected, than the cap, and every socket a cap turns away is answered 503, as the settings say,
// never 401, which tells a browser it is not signed in. On a real Dispatch with the two settings at 2
// and 1,000, ten sockets opened at once on ten documents were all admitted and left ten rooms live;
// with 1,000 and 3, ten sockets on one document left three admitted and seven answered 401.
func TestConcurrentSocketOpensStayWithinTheCapsAndAreRefusedWith503(t *testing.T) {
	const burst = 10
	open := func(t *testing.T, serverURL string, ids []string) (admitted []*gws.Conn, refusals map[int]int) {
		t.Helper()
		var mu sync.Mutex
		refusals = map[int]int{}
		start := make(chan struct{})
		var dials sync.WaitGroup
		for i := range burst {
			dials.Add(1)
			go func() {
				defer dials.Done()
				<-start
				connection, response, err := gws.DefaultDialer.Dial(socketURL(serverURL, ids[i%len(ids)]), http.Header{"X-Dispatch-User": []string{"alice"}})
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					admitted = append(admitted, connection)
					return
				}
				status := 0
				if response != nil {
					status = response.StatusCode
				}
				refusals[status]++
			}()
		}
		close(start)
		dials.Wait()
		t.Cleanup(func() {
			for _, connection := range admitted {
				_ = connection.Close()
			}
		})
		return admitted, refusals
	}
	serve := func(t *testing.T, rooms, connections int) (*Service, string) {
		t.Helper()
		database := storetest.Open(t)
		service := New(Deps{
			Store:             database,
			Events:            events.NewBroker(),
			Identity:          headerIdentity(database),
			ServerURL:         "https://dispatch.example",
			Settle:            time.Hour,
			MaxLiveRooms:      rooms,
			MaxDocConnections: connections,
		})
		t.Cleanup(func() {
			if err := service.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		service.srv.RoomIdleTimeout = time.Hour
		server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
		t.Cleanup(server.Close)
		return service, server.URL
	}
	refusedOtherwise := func(refusals map[int]int) map[int]int {
		other := map[int]int{}
		for status, count := range refusals {
			if status != http.StatusServiceUnavailable {
				other[status] = count
			}
		}
		return other
	}

	t.Run("room cap", func(t *testing.T) {
		const roomCap = 2
		service, serverURL := serve(t, roomCap, 1_000)
		ids := createDocuments(t, service.store, burst)
		admitted, refusals := open(t, serverURL, ids)
		if live := len(service.srv.Rooms()); live > roomCap || len(admitted) > roomCap {
			t.Errorf("%d sockets opened at once on %d documents: %d admitted and %d rooms live, want at most the room cap of %d (refusals by status: %v)", burst, burst, len(admitted), live, roomCap, refusals)
		}
		if other := refusedOtherwise(refusals); len(other) > 0 {
			t.Errorf("sockets the room cap turned away were answered %v, want 503 for each", other)
		}
	})
	t.Run("connection cap", func(t *testing.T) {
		const connectionCap = 3
		service, serverURL := serve(t, 1_000, connectionCap)
		ids := createDocuments(t, service.store, 1)
		admitted, refusals := open(t, serverURL, ids)
		if len(admitted) > connectionCap {
			t.Errorf("%d sockets opened at once on one document: %d admitted, want at most the connection cap of %d", burst, len(admitted), connectionCap)
		}
		if other := refusedOtherwise(refusals); len(other) > 0 {
			t.Errorf("of %d sockets opened at once on one document with a connection cap of %d, %d were admitted and the rest answered %v: a socket a cap turns away is answered 503, never another status", burst, connectionCap, len(admitted), refusals)
		}
	})
}

// ygo fires a room's OnUnloadDocument only after it has taken the room out of its map and waited
// for the room's persistence to drain (provider/websocket: CloseRoom, handleDisconnect, the idle
// sweep), so the room can load afresh - a browser reconnecting, every browser at once after a
// forced close - before the room it replaced reports its unload. That late unload is of the old
// room, not of the one resident now: a cold socket whose read began while the room was out of
// ygo's map, and whose offer lands after both the new load and the old room's unload, still finds
// the room loaded here and keeps nothing of its read, neither the history nor its hold on the
// document's head-tracker entry, for as long as it stays open.
func TestAPreloadOfferedAfterAnEarlierRoomsLateUnloadIsNotKeptWhileTheRoomIsLoaded(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]
	admitSocket(t, server.URL, artifactID, "the socket that loads the first room").Close()

	// The first room's unload waits here, where ygo has already taken the room out of its map.
	unloading := make(chan struct{})
	unloadGo := make(chan struct{})
	var unloadOnce, unloadGoOnce sync.Once
	letUnload := func() { unloadGoOnce.Do(func() { close(unloadGo) }) }
	t.Cleanup(letUnload)
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		if room == artifactID {
			unloadOnce.Do(func() { close(unloading); <-unloadGo })
		}
		unload(ctx, room)
	}
	closed := make(chan error, 1)
	go func() { closed <- service.srv.CloseRoom(artifactID, true) }()
	select {
	case <-unloading:
	case <-time.After(roomWait(t)):
		t.Fatal("the first room never reached its unload, so nothing here is tested")
	}

	// A cold socket reads and renders the document while the room is out of ygo's map, and holds
	// there, before its offer.
	rendered := make(chan struct{})
	release := make(chan struct{})
	var held atomic.Bool
	var releaseOnce sync.Once
	letOffer := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letOffer)
	service.admissionRendered = func(room string) {
		if room != artifactID || held.Swap(true) {
			return
		}
		close(rendered)
		<-release
	}
	cold := dialSocketAsync(server.URL, artifactID)
	select {
	case <-rendered:
	case <-time.After(roomWait(t)):
		t.Fatal("the held cold socket never rendered the document, so nothing here is tested")
	}

	// Another socket loads the room afresh, then the first room's unload is let go.
	admitSocket(t, server.URL, artifactID, "the socket that loads the second room")
	letUnload()
	if err := <-closed; err != nil {
		t.Fatalf("close the first room: %v", err)
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("the second room is not resident after the first room's unload")
	}

	letOffer()
	socket := <-cold
	if socket.err != nil {
		t.Fatalf("the held cold socket was refused (%v), want it admitted to the resident room", socket.err)
	}
	t.Cleanup(func() { _ = socket.connection.Close() })
	_ = socket.connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	if _, message, err := socket.connection.ReadMessage(); err != nil {
		t.Fatalf("the held socket's first frame = %q (%v), want the room's sync", message, err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload is held for a room loaded here while its socket stays open, want none: no load of this room will ever read it")
	}
}

// The resident bytes the room figures report (RoomFigures, dispatch_rooms.resident_bytes) are the
// bytes the loads ygo holds took, and each unload of a name ends the name's oldest load. A room
// that loaded again before an earlier room of the same name reported its unload keeps the bytes it
// counted: that late unload is the older load's end, and ygo's OnUnloadDocument names the room
// alone, so bytes kept on the room's record would be taken back by it - leaving the figure short
// by the whole of the resident room's history, for as long as that room lives.
func TestAnEarlierRoomsLateUnloadLeavesTheResidentRoomsBytesResident(t *testing.T) {
	r := unloadAcrossAReload(t)
	r.letFirstUnload()
	r.awaitFirstClose()
	r.awaitUnload("the first room")
	// What is left is the second room's load and its write.
	if got := r.service.residentBytes.Load(); got != r.second {
		t.Errorf("the resident bytes after the first room's late unload = %d, want the %d the second room counted with its write: that unload takes back the bytes of the load that ended", got, r.second)
	}

	// The second room's unload takes back what that load counted, and nothing is left.
	_ = r.socket.Close()
	if err := r.service.srv.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")
	r.requireNothingCounted("once every room of the document has unloaded")
}

// Two rooms of one document that have both left ygo's map can report their unloads in either
// order, and an unload that has begun may wait while a later room of the document loads and
// unloads in full. Whatever the order, each unload ends one of the loads counted under the name,
// so once both have run no bytes of either are counted and the name counts no load.
func TestUnloadsOfTwoRoomsInEitherOrderLeaveNoResidentBytes(t *testing.T) {
	r := unloadAcrossAReload(t)
	// The first room's unload goes on past what releaseUnloadedRoom reads, then waits before it
	// ends a load (beforeUnloadEndsLoad); only that unload waits, and a sync.Once would make the
	// second wait for the first as well.
	waiting := make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	letProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(letProceed)
	var held atomic.Bool
	r.service.beforeUnloadEndsLoad = func(room string) {
		if room != r.room || held.Swap(true) {
			return
		}
		close(waiting)
		<-proceed
	}
	r.letFirstUnload()
	select {
	case <-waiting:
	case <-time.After(roomWait(t)):
		t.Fatal("the first room never began its unload, so nothing here is tested")
	}

	// The second room unloads in full while the first unload waits. The first close holds the
	// service's close gate for the room (roomServer.CloseRoom) until its unload returns, so the
	// second room is closed by ygo's own CloseRoom, as ygo's idle sweep and a last browser's
	// departure retire a room, taking no gate.
	_ = r.socket.Close()
	if err := r.service.srv.Server.CloseRoom(r.room, true); err != nil {
		t.Fatalf("close the second room: %v", err)
	}
	r.awaitUnload("the second room")

	letProceed()
	r.awaitFirstClose()
	r.requireNothingCounted("once both rooms have unloaded")
}

// reloadedRoom is a document whose first room has left ygo's map with its unload held, and whose
// second room has loaded and taken a write meanwhile (unloadAcrossAReload). first and second are
// the resident bytes each load counted, and second is larger, so an unload that took back the
// wrong load's bytes shows.
type reloadedRoom struct {
	t              *testing.T
	service        *Service
	room           string
	socket         *gws.Conn
	first, second  int64
	letFirstUnload func()
	closed         chan error
	unloaded       chan struct{}
}

// unloadAcrossAReload loads a document's room, closes it, holds ygo's call of the room's unload,
// and loads and writes to the room again while that unload is held.
func unloadAcrossAReload(t *testing.T) *reloadedRoom {
	t.Helper()
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	r := &reloadedRoom{t: t, service: service, room: createDocuments(t, database, 1)[0], closed: make(chan error, 1), unloaded: make(chan struct{}, 2)}
	admitSocket(t, server.URL, r.room, "the socket that loads the first room").Close()
	// The service holds this room alone, so the figure is the first room's load.
	if r.first = service.residentBytes.Load(); r.first <= 0 {
		t.Fatalf("the first room's load counted %d resident bytes, want the history it decoded; nothing here is tested", r.first)
	}

	// ygo calls the first room's unload, after taking the room out of its map, only once it is let
	// go. Every unload of the room reports itself once it has run.
	entering := make(chan struct{})
	enter := make(chan struct{})
	var enterOnce sync.Once
	r.letFirstUnload = func() { enterOnce.Do(func() { close(enter) }) }
	t.Cleanup(r.letFirstUnload)
	var entered atomic.Bool
	unload := service.srv.OnUnloadDocument
	service.srv.OnUnloadDocument = func(ctx context.Context, room string) {
		if room == r.room && !entered.Swap(true) {
			close(entering)
			<-enter
		}
		unload(ctx, room)
		if room == r.room {
			r.unloaded <- struct{}{}
		}
	}
	go func() { r.closed <- service.srv.CloseRoom(r.room, true) }()
	select {
	case <-entering:
	case <-time.After(roomWait(t)):
		t.Fatal("the first room never reached its unload, so nothing here is tested")
	}

	// The room loads afresh while the first room's unload is held, so the service counts both
	// loads, and a write the second room takes counts against it alone.
	r.socket = admitSocket(t, server.URL, r.room, "the socket that loads the second room")
	if _, err := joinedReplaceText(service, r.room, "A write the second room takes, longer than the seed.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write to the second room: %v", err)
	}
	if r.second = service.residentBytes.Load() - r.first; r.second <= r.first {
		t.Fatalf("the second room counted %d resident bytes against the first room's %d, want more; nothing here is tested", r.second, r.first)
	}
	return r
}

// awaitUnload waits for the next unload of the room to have run.
func (r *reloadedRoom) awaitUnload(what string) {
	r.t.Helper()
	select {
	case <-r.unloaded:
	case <-time.After(roomWait(r.t)):
		r.t.Fatalf("%s never reported its unload", what)
	}
}

// awaitFirstClose waits for the first room's close to return.
func (r *reloadedRoom) awaitFirstClose() {
	r.t.Helper()
	if err := <-r.closed; err != nil {
		r.t.Fatalf("close the first room: %v", err)
	}
}

// requireNothingCounted requires that no bytes and no load of the room are counted.
func (r *reloadedRoom) requireNothingCounted(when string) {
	r.t.Helper()
	if got := r.service.residentBytes.Load(); got != 0 {
		r.t.Errorf("the resident bytes %s = %d, want 0: a load whose bytes no unload took back stays counted for good", when, got)
	}
	r.service.roomSlots.mu.Lock()
	slot := r.service.roomSlots.rooms[r.room]
	r.service.roomSlots.mu.Unlock()
	if len(slot.loads) != 0 {
		r.t.Errorf("the document still counts %d loads %s, want none", len(slot.loads), when)
	}
}

// A cold socket that offers its read while the room's load is running - after the load has looked
// for a preload (LoadDoc's takePreload) and before it records the room as loaded here
// (onLoadDocument) - offers to a load that will never read it. The load takes that offer out as it
// records itself, so the socket keeps neither the history nor its hold on the document's
// head-tracker entry while it stays open. The load is held inside its own read of the store
// (insideFirstLoadStore), which LoadDoc makes once takePreload has found nothing.
func TestAPreloadOfferedWhileTheRoomLoadsIsReleasedAtThatLoad(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocuments(t, database, 1)[0]
	reading, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letLoad := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letLoad)
	// The load holds inside its own read of the store, once that read has returned and before
	// Load hands it back, which is where LoadDoc makes it: after takePreload has found nothing.
	store := &insideFirstLoadStore{VersionedStore: NewPgVersioned(database), room: artifactID, during: func() {
		close(reading)
		<-release
	}}
	store.armed.Store(true)
	service := New(Deps{
		Store:       database,
		Persistence: store,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		letLoad()
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	offered := make(chan struct{})
	var offeredOnce sync.Once
	service.afterSocketPreload = func(room string, _ *persistence.LoadResult) {
		if room == artifactID {
			offeredOnce.Do(func() { close(offered) })
		}
	}
	loaded := make(chan error, 1)
	go func() { loaded <- service.warmLiveDocument(context.Background(), artifactID) }()
	select {
	case <-reading:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's load never read the store, so nothing here is tested")
	}

	cold := dialSocketAsync(server.URL, artifactID)
	select {
	case <-offered:
	case <-time.After(roomWait(t)):
		t.Fatal("the cold socket never offered its read while the room loaded, so nothing here is tested")
	}
	if _, stored := service.preloads.Load(artifactID); !stored {
		t.Fatal("the cold socket's offer was not stored while the room loaded, so nothing here is tested")
	}

	letLoad()
	if err := <-loaded; err != nil {
		t.Fatalf("load the room: %v", err)
	}
	socket := <-cold
	if socket.err != nil {
		t.Fatalf("the cold socket was refused (%v), want it admitted to the resident room", socket.err)
	}
	t.Cleanup(func() { _ = socket.connection.Close() })
	_ = socket.connection.SetReadDeadline(time.Now().Add(roomWait(t)))
	if _, message, err := socket.connection.ReadMessage(); err != nil {
		t.Fatalf("the cold socket's first frame = %q (%v), want the room's sync", message, err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Error("a preload offered while the room loaded is still held after that load, want none: no load of this room will ever read it")
	}
}
