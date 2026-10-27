package docs

import (
	"bytes"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What a room holds in memory beyond its document: the record of each update its persistence has
// yet to take, the state a socket's admission loaded for the room's load, and the cold reads'
// cache. The tests here are about each holding no more, and for no longer, than the service needs.

// A room's update observer records each update's class for the persistence that takes the same
// update next (recordUpdateClass, consumeUpdateClass). ygo hands both the very slice it encoded at
// the commit (sameUpdate), so the record keeps that slice rather than a copy of every update a
// lagging room has queued, and it finds an update's class by the slice itself. Equal bytes are not
// the same update - two commits that change nothing encode alike - so each of two such updates
// takes its own class, and only a slice no class holds falls back to the bytes.
func TestAnUpdateClassHoldsNoCopyAndMatchesByIdentity(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	// Durable classes, so the room's record holds them (pendingUpdates) until each is taken.
	recordClass := func(update []byte) *UpdateCredit {
		credit := &UpdateCredit{}
		service.recordUpdateClass(artifactID, update, documentUpdateClass{contentChanged: true, durable: true, credit: credit})
		return credit
	}
	takeClass := func(update []byte, want *UpdateCredit, what string) {
		t.Helper()
		class, counted, found := service.consumeUpdateClass(artifactID, update)
		if found && class.durable {
			service.finishDurableAppend(artifactID, counted)
		}
		switch {
		case !found:
			t.Errorf("%s found no class", what)
		case class.credit != want:
			t.Errorf("%s took another update's class", what)
		}
	}

	observed := []byte("the update a room's observer was handed")
	observedCredit := recordClass(observed)
	state := service.room(artifactID)
	state.mu.Lock()
	var recorded []byte
	if count := len(state.updateClasses); count > 0 {
		recorded = state.updateClasses[count-1].update
	}
	state.mu.Unlock()
	if len(recorded) == 0 || &recorded[0] != &observed[0] {
		t.Errorf("the recorded class holds a copy of the update (%d bytes), want the observer's own slice", len(recorded))
	}
	takeClass(observed, observedCredit, "the observed update")

	// What a commit that changes nothing encodes to, twice: equal bytes, two slices.
	first := crdt.EncodeStateAsUpdateV1(crdt.New(), nil)
	second := crdt.EncodeStateAsUpdateV1(crdt.New(), nil)
	if !bytes.Equal(first, second) || &first[0] == &second[0] {
		t.Fatalf("two empty commits encode to %v and %v, want equal bytes in two slices", first, second)
	}
	firstCredit, secondCredit := recordClass(first), recordClass(second)
	takeClass(second, secondCredit, "the second of two empty commits, taken first")
	takeClass(first, firstCredit, "the first of two empty commits")

	// A slice no class holds, with the bytes of one that does, still finds that class.
	fallback := []byte("an update whose persistence holds a copy")
	fallbackCredit := recordClass(fallback)
	takeClass(bytes.Clone(fallback), fallbackCredit, "a copy of a recorded update")
}

// Two browsers opening a cold document at once each load it for their socket's admission, and the
// room's load takes one of the two states (takePreload). The other socket's state is then dead
// weight - a whole document's history - and nothing may keep it reachable: not the socket, which
// can stay open for hours, and not the preload map. A weak pointer to the losing state's update
// bytes is cleared by a collection while both sockets are still open.
func TestALosingPreloadIsReleasedAtOnce(t *testing.T) {
	database := storetest.Open(t)
	service, server := newTestServiceInstance(t, database)
	artifactID := createDocuments(t, database, 1)[0]

	var mu sync.Mutex
	var offered []weak.Pointer[byte]
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var released sync.Once
	releaseSockets := func() { released.Do(func() { close(release) }) }
	t.Cleanup(releaseSockets)
	service.afterSocketPreload = func(room string, loaded *persistence.LoadResult) {
		if room != artifactID {
			return
		}
		if len(loaded.Update) == 0 {
			t.Errorf("a cold socket offered an empty history")
		} else {
			mu.Lock()
			offered = append(offered, weak.Make(&loaded.Update[0]))
			mu.Unlock()
		}
		arrived <- struct{}{}
		<-release
	}

	type opened struct {
		connection *gws.Conn
		err        error
	}
	sockets := make(chan opened, 2)
	for range 2 {
		go func() {
			connection, err := dialDocument(server.URL, artifactID)
			sockets <- opened{connection, err}
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(roomWait(t)):
			t.Fatal("two cold sockets of one document never both offered what they loaded")
		}
	}
	mu.Lock()
	loser := losingPreload(t, service, artifactID, offered)
	mu.Unlock()
	releaseSockets()
	var connections []*gws.Conn
	for range 2 {
		socket := <-sockets
		if socket.err != nil {
			t.Fatalf("open a cold socket: %v", socket.err)
		}
		connections = append(connections, socket.connection)
		t.Cleanup(func() { _ = socket.connection.Close() })
	}
	waitFor(t, roomWait(t), "both sockets to join the loaded room", func() bool {
		return service.srv.GetDoc(artifactID) != nil && registeredConnections(service) == 2
	})

	runtime.GC()
	runtime.GC()
	if loser.Value() != nil {
		t.Errorf("the losing socket's loaded history is still reachable while its socket is open, want it released once the room loaded")
	}

	// Once both sockets have left, whatever still holds the losing history is something other than
	// the sockets, and the check above proves nothing.
	for _, connection := range connections {
		leaveDocument(connection)
	}
	waitFor(t, roomWait(t), "both sockets to be forgotten", func() bool {
		return registeredConnections(service) == 0
	})
	runtime.GC()
	runtime.GC()
	if loser.Value() != nil {
		t.Fatal("the losing socket's loaded history is still reachable after both sockets left: something besides the sockets holds it")
	}
}

// losingPreload is the weak pointer, of the two cold sockets' offered histories, to the one the
// room's load will not take: the one the preload map does not hold once both have offered theirs.
func losingPreload(t *testing.T, service *Service, room string, offered []weak.Pointer[byte]) weak.Pointer[byte] {
	t.Helper()
	if len(offered) != 2 {
		t.Fatalf("%d cold sockets offered a history, want 2", len(offered))
	}
	value, ok := service.preloads.Load(room)
	if !ok {
		t.Fatal("no preload is held for the room's load after two cold sockets offered theirs")
	}
	held := value.(*preloadedDocument).loaded.Update
	if len(held) == 0 {
		t.Fatal("the preload held for the room's load has no history")
	}
	switch &held[0] {
	case offered[0].Value():
		return offered[1]
	case offered[1].Value():
		return offered[0]
	}
	t.Fatal("the preload held for the room's load is neither socket's history")
	return weak.Pointer[byte]{}
}

// The cold reads' cache holds rendered documents up to a budget. A fixed 256 MiB is a sixteenth of
// one task size and an eighth of another, and the container's size is configurable, so the budget
// follows the Go memory limit the process runs under: a sixteenth of it, at least 64 MiB and at
// most 1 GiB, and 256 MiB when no limit is set (debug.SetMemoryLimit reports math.MaxInt64).
func TestTheReadBudgetFollowsTheGoMemoryLimit(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int64
		want  int64
	}{
		{"no limit", math.MaxInt64, 256 << 20},
		{"4 GiB", 4 << 30, 256 << 20},
		{"32 GiB", 32 << 30, 1 << 30},
		{"512 MiB", 512 << 20, 64 << 20},
	} {
		if got := readBudgetFor(test.limit); got != test.want {
			t.Errorf("the read budget under %s = %d MiB, want %d MiB", test.name, got>>20, test.want>>20)
		}
	}
}
