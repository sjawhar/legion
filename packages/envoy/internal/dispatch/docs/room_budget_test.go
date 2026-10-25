package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room is charged its latest count at its copies, plus the bytes of the updates it has taken
// since that count: one copy until its first update and two from then on, and each count replaces
// the last rather than adding to it, so the charge falls when the document does. A socket's
// reservation is replaced by its room's charge when the room loads, and the charge goes when ygo
// retires the room.
func TestARoomsChargeIsItsLatestCountAtItsCopiesPlusTheUpdatesSince(t *testing.T) {
	budget := &roomBudget{limit: roomBudgetBytes}
	charge := budget.load("room", 1_000)
	requireCharged(t, budget, charge, 1_000*bytesOfHeapAnElement, "a room that has taken no update holds one copy")

	charge.took(100, true)
	requireCharged(t, budget, charge, 2*1_000*bytesOfHeapAnElement+100, "the first update makes the second copy and adds its bytes")
	charge.took(50, true)
	requireCharged(t, budget, charge, 2*1_000*bytesOfHeapAnElement+150, "a later update adds its bytes")

	// The count of the rendering published once the room had taken 100 bytes re-bases the charge
	// there: the 50 bytes taken since stay beside it.
	charge.measured(800, 100)
	requireCharged(t, budget, charge, 2*800*bytesOfHeapAnElement+50, "a count of a smaller document lowers the charge")
	charge.measured(800, 150)
	requireCharged(t, budget, charge, 2*800*bytesOfHeapAnElement, "a count of the latest rendering leaves no update bytes beside it")
	charge.measured(elementsPastCounting, 150)
	requireCharged(t, budget, charge, 2*pmdoc.MaxDocumentElements*bytesOfHeapAnElement, "a rendering goldmark stopped counting is charged at the element bound")
	charge.measured(sizeUnmeasured, 150)
	requireCharged(t, budget, charge, 0, "a room that renders no markdown is charged its update bytes alone")

	budget.retire("room")
	charge.took(1_000, true)
	if budget.used != 0 || len(budget.rooms) != 0 {
		t.Fatalf("the budget holds %d bytes over %d rooms once the room retired, want nothing", budget.used, len(budget.rooms))
	}

	reservation, err := budget.reserve("other", 3_000)
	if err != nil || reservation == nil || budget.used != 3_000 {
		t.Fatalf("a reservation held %d bytes (%v), want 3000", budget.used, err)
	}
	other := budget.load("other", 10)
	if budget.used != 10*bytesOfHeapAnElement || len(budget.rooms["other"].reservations) != 0 {
		t.Fatalf("the loaded room is charged %d bytes beside %d reservations, want its own %d alone",
			budget.used, len(budget.rooms["other"].reservations), 10*bytesOfHeapAnElement)
	}
	budget.release(reservation)
	if joined, err := budget.reserve("other", 3_000); joined != nil || err != nil || budget.used != other.weight {
		t.Fatalf("a socket joining a charged room reserved %v (%v), and the budget holds %d bytes, want nothing reserved", joined, err, budget.used)
	}

	budget.limit = other.weight + 100
	if refused, err := budget.reserve("third", 101); refused != nil || !errors.Is(err, errRoomBudgetFull) || budget.used != other.weight {
		t.Fatalf("a reservation past the limit was %v (%v), and the budget holds %d bytes, want a refusal holding nothing", refused, err, budget.used)
	}
	other.took(1_000, true)
	budget.swept()
	if !budget.full.Load() {
		t.Fatal("a sweep that left the budget over did not raise full")
	}
	budget.retire("other")
	if budget.full.Load() || budget.used != 0 {
		t.Fatalf("the budget holds %d bytes and full is %t once its room retired, want nothing and full lowered", budget.used, budget.full.Load())
	}
}

// The budget's arithmetic at the shapes its design is stated in: a room at the element bound,
// edited, is the most a room's count is charged, and the budget holds thirty-four of them; a room
// of a mebibyte of prose (57,600 elements, TestAResidentRoomsHeapPerByteOfItsMarkdown's shape),
// edited, is charged 54 MB, and the budget refuses the fortieth.
func TestTheRoomBudgetsArithmeticAtTheBound(t *testing.T) {
	for _, shape := range []struct {
		name           string
		elements       int64
		weight, admits int64
	}{
		{"a room at the element bound", pmdoc.MaxDocumentElements, 61_603_840, 34},
		{"a room of a mebibyte of prose", 57_600, 54_144_000, 39},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if weight := roomWeight(shape.elements, 2, 0); weight != shape.weight {
				t.Fatalf("an edited room of %d elements is charged %d bytes, want %d", shape.elements, weight, shape.weight)
			}
			budget := &roomBudget{limit: roomBudgetBytes}
			admitted := int64(0)
			for {
				reservation, err := budget.reserve(fmt.Sprintf("room-%d", admitted), shape.weight)
				if errors.Is(err, errRoomBudgetFull) {
					break
				}
				if err != nil || reservation == nil {
					t.Fatalf("reserve room %d: %v", admitted+1, err)
				}
				admitted++
			}
			if admitted != shape.admits {
				t.Fatalf("the budget admitted %d rooms of %d bytes, want %d", admitted, shape.weight, shape.admits)
			}
		})
	}
}

func requireCharged(t *testing.T, budget *roomBudget, charge *roomCharge, want int64, what string) {
	t.Helper()
	if charge.weight != want || budget.used != want {
		t.Fatalf("%s: the room is charged %d bytes and the budget holds %d, want %d", what, charge.weight, budget.used, want)
	}
}

// Every way out of the document socket's handler returns the reservation its admission made, and
// the one way that keeps the socket hands it to the room's own charge when the room loads: a socket
// refused outside the schema, one whose admission load fails, one the budget refuses, and one that
// joins its room. The reservation is the stored document's count at both copies, held while the
// handler loads the document.
func TestTheDocumentSocketReturnsItsReservationOnEveryWayOut(t *testing.T) {
	database := storetest.Open(t)
	watch := &admissionWatchStore{VersionedStore: NewPgVersioned(database), held: make(map[string][]int64)}
	service := newRoomBudgetService(t, database, watch)
	watch.service.Store(service)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 4)
	outside, failing, refused, joined := ids[0], ids[1], ids[2], ids[3]
	reserved := roomWeight(countElements(latestVersionMarkdown(t, database, joined)), 2, 0)
	if reserved <= 0 {
		t.Fatalf("the stored document reserves %d bytes, want some", reserved)
	}

	t.Run("refused outside the schema", func(t *testing.T) {
		if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, outside); err != nil {
			t.Fatal(err)
		}
		requireClosedAtUpgrade(t, httpServer.URL, outside, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
		watch.requireHeldAtLoad(t, outside, reserved)
		requireNothingReserved(t, service)
	})
	t.Run("its admission load fails", func(t *testing.T) {
		watch.failing.Store(failing)
		if connection, err := dialDocument(httpServer.URL, failing); err == nil {
			_ = connection.Close()
			t.Fatal("the socket opened, want the failed load answered 503")
		} else if !strings.Contains(err.Error(), "503") {
			t.Fatalf("the socket was refused with %v, want 503", err)
		}
		watch.requireHeldAtLoad(t, failing, reserved)
		requireNothingReserved(t, service)
	})
	t.Run("the budget has no room for it", func(t *testing.T) {
		service.SetRoomBudgetForTest(reserved - 1)
		t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
		requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
		if service.srv.GetDoc(refused) != nil || len(watch.loads(refused)) != 0 {
			t.Fatal("the refused socket's document was loaded, want it refused before any load")
		}
		requireNothingReserved(t, service)
	})
	t.Run("it joins its room", func(t *testing.T) {
		connection, err := dialDocument(httpServer.URL, joined)
		if err != nil {
			t.Fatalf("open the document: %v", err)
		}
		waitFor(t, 10*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(joined) != nil })
		watch.requireHeldAtLoad(t, joined, reserved)
		loaded := roomWeight(countElements(renderedLive(t, service, joined)), 1, 0)
		requireNothingReserved(t, service)
		if used := budgetUsed(service); used != loaded {
			t.Fatalf("the budget holds %d bytes for the open room, want its own charge at one copy, %d", used, loaded)
		}
		leaveDocument(connection)
		if err := service.Evict(context.Background(), joined); err != nil {
			t.Fatal(err)
		}
		if used := budgetUsed(service); used != 0 {
			t.Fatalf("the budget holds %d bytes once the room retired, want none", used)
		}
	})
}

// Over the budget, the sweep closes the rooms nothing is still using and leaves the ones something
// is, then raises the signal the hook reads; the next browser is refused with the transient close
// before any room is made for it, while the server's own write still lands and its room is closed
// once it has; and once the last room in use is left, the budget closes it and lowers the signal.
func TestTheRoomBudgetClosesUnusedRoomsAndRefusesWhenNoneIsLeft(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 3)
	used, written, refused := ids[0], ids[1], ids[2]

	connection, err := dialDocument(httpServer.URL, used)
	if err != nil {
		t.Fatalf("open the document in use: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the room in use to go live", func() bool { return service.srv.GetDoc(used) != nil })
	appendThroughAPI(t, service, written)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, written) })

	service.SetRoomBudgetForTest(1)
	waitFor(t, 10*time.Second, "the sweep to close the unused room", func() bool { return service.srv.GetDoc(written) == nil })
	waitFor(t, 10*time.Second, "the budget to report itself full", service.budget.full.Load)
	if service.srv.GetDoc(used) == nil {
		t.Fatal("the sweep closed the room a browser holds open, want it kept")
	}

	requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	if service.srv.GetDoc(refused) != nil {
		t.Fatal("the refused browser's room was made, want none")
	}
	requireNothingReserved(t, service)

	// The server's own write is never refused: it opens its room over the budget, lands, and the
	// room goes once nothing holds it, the signal staying up for the room still in use.
	appendThroughAPI(t, service, refused)
	if text, err := service.Text(context.Background(), refused); err != nil || strings.Count(text, "Room cap probe.") != 2 {
		t.Fatalf("the document the server wrote over a full budget reads %q (%v), want the write", text, err)
	}
	waitFor(t, 10*time.Second, "the server's room to close once its write lands", func() bool { return service.srv.GetDoc(refused) == nil })
	if !service.budget.full.Load() {
		t.Fatal("the budget lowered its signal with the room in use still over it")
	}

	leaveDocument(connection)
	waitFor(t, 10*time.Second, "the left room to close", func() bool { return service.srv.GetDoc(used) == nil })
	waitFor(t, 10*time.Second, "the budget to lower its signal", func() bool { return !service.budget.full.Load() })
	if used := budgetUsed(service); used != 0 {
		t.Fatalf("the budget holds %d bytes with no room open, want none", used)
	}
}

// newRoomBudgetService is a document service over persist whose rooms stay resident a minute after
// their last use, as production's do, so what closes one in these tests is the budget's sweep.
func newRoomBudgetService(t *testing.T, database *store.Store, persist VersionedStore) *Service {
	t.Helper()
	service := New(Deps{
		Store:       database,
		Persistence: persist,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service
}

// admissionWatchStore records, at each load of a document's stored state (the socket's admission
// check, loadTree, and the room's own), the reservations the room budget holds for that document,
// and fails the loads of the document failing names.
type admissionWatchStore struct {
	VersionedStore
	service atomic.Pointer[Service]
	failing atomic.Value
	mu      sync.Mutex
	held    map[string][]int64
}

func (s *admissionWatchStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	budget := &s.service.Load().budget
	budget.mu.Lock()
	reserved := int64(0)
	if entry := budget.rooms[room]; entry != nil {
		for _, reservation := range entry.reservations {
			reserved += reservation.weight
		}
	}
	budget.mu.Unlock()
	s.mu.Lock()
	s.held[room] = append(s.held[room], reserved)
	s.mu.Unlock()
	if failing, _ := s.failing.Load().(string); failing == room {
		return persistence.LoadResult{}, errors.New("injected load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *admissionWatchStore) loads(room string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.held[room]...)
}

// requireHeldAtLoad requires the admission check's load of room, its first, to have found the
// socket's reservation of want bytes held.
func (s *admissionWatchStore) requireHeldAtLoad(t *testing.T, room string, want int64) {
	t.Helper()
	loads := s.loads(room)
	if len(loads) == 0 || loads[0] != want {
		t.Fatalf("the admission's load found %v bytes reserved, want %d first", loads, want)
	}
}

// requireClosedAtUpgrade opens room's socket and requires the server to complete the upgrade and
// close it at once with code and reason, before any sync.
func requireClosedAtUpgrade(t *testing.T, serverURL, room string, code int, reason string) {
	t.Helper()
	connection, err := dialDocument(serverURL, room)
	if err != nil {
		t.Fatalf("open the document: %v, want an upgrade the server closes", err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, message, err := connection.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != code || closed.Text != reason {
		t.Fatalf("first frame = %q (%v), want close %d %q before any sync", message, err, code, reason)
	}
}

// requireNothingReserved requires the budget to hold no reservation once the handlers the test
// drove have returned: a refusing handler returns once the client answers its close, a moment
// after the client has read it.
func requireNothingReserved(t *testing.T, service *Service) {
	t.Helper()
	waitFor(t, 5*time.Second, "the budget to hold no reservation", func() bool {
		service.budget.mu.Lock()
		defer service.budget.mu.Unlock()
		for _, entry := range service.budget.rooms {
			if len(entry.reservations) > 0 {
				return false
			}
		}
		return true
	})
}

func budgetUsed(service *Service) int64 {
	service.budget.mu.Lock()
	defer service.budget.mu.Unlock()
	return service.budget.used
}

// roomUnused is whether room's state holds nothing that outlasts the room (unusedLocked).
func roomUnused(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return service.unusedLocked(state)
}

func latestVersionMarkdown(t *testing.T, database *store.Store, room string) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, room).Scan(&markdown); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	return markdown
}

// renderedLive is the rendering of room's resident document, which its load counted.
func renderedLive(t *testing.T, service *Service, room string) string {
	t.Helper()
	markdown, err := renderDocument(service.srv.GetDoc(room))
	if err != nil {
		t.Fatal(err)
	}
	return markdown
}
