//go:build memory

package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The real-process drives of the per-peer write-queue budget (peerWriteQueueBytes,
// internal/dispatch/docs/peer_update.go, which the document service sets as ygo's
// Server.PeerWriteQueueBytes): a Dispatch server in its own process (memory_harness_test.go),
// driven by sockets that complete a room's handshake and then never read again, and by an honest
// browser on a link slow enough to lag behind another browser's typing.

const (
	// peerWriteQueueBytesForTest mirrors peerWriteQueueBytes
	// (internal/dispatch/docs/peer_update.go), the bytes one socket may hold queued and not yet
	// taken off its queue. No drive takes the mirror on trust: the line ygo logs for every socket
	// it closes for the budget names the budget it was closed against, and each drive fails
	// unless that is this number.
	peerWriteQueueBytesForTest = 60 << 20
	// writeQueueFrameCap mirrors ygo's default PeerWriteQueueSize, 512 frames whatever their size
	// (defaultPeerWriteQueueSize, sjawhar/ygo provider/websocket/server.go:208-212): the only
	// bound on a peer's queue before the budget. The single-socket drive asks for that many whole
	// states, so what it would hold with the budget off is what the frame cap alone allows.
	writeQueueFrameCap = 512
	// floodFramesPerSocket is how many whole states each of the twenty sockets asks for: half as
	// many again as the budget admits of the document those twenty rooms hold (runOfWords), whose
	// whole state is a mebibyte of text, so about sixty-three of them reach the budget and the
	// next is refused. Each socket therefore passes its budget, and none of the twenty asks for
	// the frame cap's 512 at once.
	floodFramesPerSocket = 96
)

// writeQueueCloseLine is what ygo logs for each peer its byte budget closes, with the figures
// behind it (peer.enqueueLocked, sjawhar/ygo provider/websocket/write_budget.go:6-30). It reaches
// the server's own log because ygo logs through slog.Default() while Server.Logger is unset
// (provider/websocket/server.go:168-174), which cmd/dispatch sets at boot (main.go:190).
var writeQueueCloseLine = regexp.MustCompile(`msg="peer queued bytes over budget; closing peer" room=(\S+) path=("[^"]*"|\S+) queuedBytes=(\d+) budgetBytes=(\d+) frameBytes=(\d+) queueSize=(\d+)`)

// writeQueueClose is one such close: the room, the enqueue path the frame was refused on, what the
// socket held queued, the budget it was weighed against, the frame that was refused, and the frame
// cap still in force beside the budget.
type writeQueueClose struct {
	room, path                    string
	queued, budget, frame, frames int64
}

// writeQueueCloses is every close the budget has logged, in the order the server logged them.
func writeQueueCloses(t *testing.T, server *memoryServer) []writeQueueClose {
	t.Helper()
	var closes []writeQueueClose
	for _, match := range writeQueueCloseLine.FindAllStringSubmatch(server.output.String(), -1) {
		closed := writeQueueClose{room: match[1], path: strings.Trim(match[2], `"`)}
		for index, field := range []*int64{&closed.queued, &closed.budget, &closed.frame, &closed.frames} {
			value, err := strconv.ParseInt(match[index+3], 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", match[0], err)
			}
			*field = value
		}
		closes = append(closes, closed)
	}
	return closes
}

// writeQueueClosesByRoom waits up to within for the budget to close a socket of every one of
// rooms, and is the first close the server logged for each: a room it closed none of has no entry.
// One wait covers the whole fleet, so a drive whose budget is off waits once rather than once a
// room.
func writeQueueClosesByRoom(t *testing.T, server *memoryServer, rooms []string, within time.Duration) map[string]writeQueueClose {
	t.Helper()
	deadline := time.Now().Add(within)
	found := make(map[string]writeQueueClose, len(rooms))
	for {
		for _, closed := range writeQueueCloses(t, server) {
			if _, seen := found[closed.room]; !seen {
				found[closed.room] = closed
			}
		}
		missing := false
		for _, room := range rooms {
			if _, seen := found[room]; !seen {
				missing = true
			}
		}
		if !missing || time.Now().After(deadline) {
			return found
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeQueueCloseWithin is the first close the budget logs of room's socket, waiting up to within
// for the server to log one, and reports whether it did.
func writeQueueCloseWithin(t *testing.T, server *memoryServer, room string, within time.Duration) (writeQueueClose, bool) {
	t.Helper()
	closed, found := writeQueueClosesByRoom(t, server, []string{room}, within)[room]
	return closed, found
}

// requireWriteQueueClose fails the drive unless the server closed room's socket for the budget
// within within, and unless the figures it logged hold the budget's own rule
// (requireBudgetFigures), where largest is the largest frame that room can send the socket.
func requireWriteQueueClose(t *testing.T, server *memoryServer, room, what string, largest int64, within time.Duration) writeQueueClose {
	t.Helper()
	closed, found := writeQueueCloseWithin(t, server, room, within)
	if !found {
		t.Fatalf("%s: the server logged no close of its write-queue budget for room %s within %s:\n%s", what, room, within, server.output.tail())
	}
	requireBudgetFigures(t, closed, what, largest)
	return closed
}

// requireBudgetFigures fails the drive unless the figures the server logged for a close hold the
// budget's own rule: the socket was weighed against this package's mirror of the budget, the bytes
// already waiting for it had reached that budget - a frame is refused only then, so a socket with
// an empty queue takes a frame of any size - and the queue they had reached it from stands no more
// than one frame past it, since the frame that crossed was queued from under the budget. largest
// is the largest frame the socket's room can send it.
func requireBudgetFigures(t *testing.T, closed writeQueueClose, what string, largest int64) {
	t.Helper()
	if closed.budget != peerWriteQueueBytesForTest {
		t.Errorf("%s: the server closed the socket against a budget of %d bytes, want %d", what, closed.budget, peerWriteQueueBytesForTest)
	}
	if closed.queued < closed.budget {
		t.Errorf("%s: the socket held %d bytes queued when it was closed, want at least the %d-byte budget: a frame is refused only once the bytes already waiting reach it", what, closed.queued, closed.budget)
	}
	if bound := closed.budget + max(largest, closed.frame); closed.queued >= bound {
		t.Errorf("%s: the socket held %d bytes queued when it was closed, want less than the %d the budget and one frame allow", what, closed.queued, bound)
	}
}

// silentSocket is a document socket that completes its room's handshake - it reads the room's
// SyncStep1 and the whole state the room hands it - and then never reads again while it asks for
// that whole state over and over. Each SyncStep1 it sends is answered with one whole-state
// SyncStep2 (sjawhar/ygo provider/websocket/peer.go:96-108), which the server queues for a socket
// that is not draining it: this is the shape that grew a server's heap without bound before the
// budget, a forty-one-byte ask for three megabytes held.
type silentSocket struct {
	room       string
	connection *gws.Conn
	// handoff is the whole-state frame the room answered the handshake with, in bytes: what one
	// SyncStep1 makes the server hold for a socket that never reads.
	handoff int
}

// openSilent opens such a socket on the document's room and reads the handshake through, as a
// browser does, leaving the socket open and unread.
func (p *memoryServer) openSilent(t *testing.T, artifactID string) *silentSocket {
	t.Helper()
	connection, err := p.dialDocument(artifactID)
	if err != nil {
		t.Fatalf("open document %s on a socket that never reads: %v", artifactID, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetReadDeadline(time.Now().Add(browserAnswerWithin)); err != nil {
		t.Fatalf("set the socket's read deadline: %v", err)
	}
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the room's handshake: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if tag, err := decoder.ReadVarUint(); err != nil || tag != frameSync {
			continue
		}
		if kind, _, err := ygsync.ReadSyncMessage(decoder.RemainingBytes()); err != nil || kind != ygsync.MsgSyncStep2 {
			continue
		}
		return &silentSocket{room: artifactID, connection: connection, handoff: len(message)}
	}
}

// askForState is the frame this socket floods the room with: a SyncStep1 carrying the state vector
// of an empty document, which the editor sends on every reconnect and which the room answers with
// its whole state.
func (s *silentSocket) askForState() []byte {
	return documentFrame(s.room, frameSync, ygsync.EncodeSyncStep1(crdt.New()))
}

// flood asks the room for its whole state frames times over, never reading an answer, and stops at
// the first ask the socket cannot take, which is how the server's close reaches this side. It
// reports how many asks were written and why the last was not. The server drops the asks it has
// not answered when the budget closes the socket (closeConn, sjawhar/ygo
// provider/websocket/peer.go:880-885), so the flood is never answered more times than it is closed
// for.
func (s *silentSocket) flood(frames int) (int, error) {
	ask := s.askForState()
	for sent := range frames {
		if err := s.connection.WriteMessage(gws.BinaryMessage, ask); err != nil {
			return sent, err
		}
	}
	return frames, nil
}

// ended waits for the socket to end, reading and dropping whatever the server had already written
// to it, and is the error it ended with. Its caller has already taken whatever memory reading was
// to measure: a read here lets a server that was not going to close the socket write more.
func (s *silentSocket) ended(within time.Duration) error {
	if err := s.connection.SetReadDeadline(time.Now().Add(within)); err != nil {
		return err
	}
	for {
		if _, _, err := s.connection.ReadMessage(); err != nil {
			return err
		}
	}
}

// writeQueueHeapMargin is how far above the budget and two whole-state frames the single-socket
// drive lets the server's memory rise over what it held idle with the same room open. The budget
// bounds the frames a socket holds queued, and the frame its writer is in the middle of is a
// second frame beside them (creditLocked, sjawhar/ygo provider/websocket/peer.go:905-909); neither
// bounds the garbage that making those frames leaves, and at the runtime's own collector ratio the
// whole-state encode behind each frame is garbage until the next cycle. Measured on 2026-10-31
// over three runs (the drive logs every figure): the server held 102,973,472 to 119,939,104 bytes
// above idle for a socket closed holding 63,117,516, which is 34,047,646 to 51,013,278 above the
// budget and two 3,005,633-byte frames. 64 MiB is a third as much again as the worst of the
// three, and the same drive with the budget off held 1,228,628,256 above idle, nine times the
// bound this margin makes.
const writeQueueHeapMargin = 64 << 20

// One socket that completes a mebibyte document's handshake and then asks for the room's whole
// state 512 times over - ygo's frame cap, the only bound before the budget - without ever reading
// an answer is closed by the server inside its write-queue budget, and what the server holds for
// it stays inside that budget.
//
// Measured on 2026-10-31 over three runs: each SyncStep1 makes the server queue a 3,005,633-byte
// whole-state frame, so the frame cap alone would let this one socket hold 1,538,884,096 bytes -
// 1.43 GiB, more than a third of the pod - for the 20,992 bytes it uploads. With the budget the
// server closed it on a direct send with 63,117,516 bytes queued and a 3,005,596-byte frame
// refused, every run alike, and its memory rose 102,973,472 to 119,939,104 bytes above what the
// same server held idle with the room open and an honest browser reading (the heaviest run held
// 269,736,264 bytes at the most against 149,797,160 idle, of what the Go memory limit bounds, 308
// MiB charged against 194), inside the budget, two frames and writeQueueHeapMargin, 136,034,690
// bytes. The honest browser still answers a ping afterwards.
//
// Run on 2026-10-31 with the budget at zero, the drive fails twice over: the one socket's 512
// asks took the server 1,228,628,256 bytes above idle, 1,379 MiB as a pod's kernel would charge
// it, and nothing closed the socket for a budget within the two minutes the drive waits.
func TestASocketThatNeverReadsIsClosedWithinItsWriteQueueBudget(t *testing.T) {
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	source, _ := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	// An honest browser holds the room open and reads everything the room sends it, so what the
	// drive weighs is the silent socket's own memory beside a loaded room, not the room's load.
	reader := server.openBrowser(t, source)
	defer reader.leave()
	idleFrom := time.Now()
	time.Sleep(3 * time.Second)
	idle := server.tracePeaks(t, idleFrom, time.Now())
	if idle.samples == 0 {
		t.Fatalf("the server's memory trace holds no sample of the idle server:\n%s", server.output.tail())
	}
	t.Logf("idle with the room open: a pod's kernel would charge %d MiB, the runtime held %d bytes of what its %d-byte limit bounds, over %d trace samples",
		idle.charged>>20, idle.held, idle.limit, idle.samples)

	silent := server.openSilent(t, source)
	var (
		sent   int
		asked  error
		closed writeQueueClose
		found  bool
	)
	peak := server.peakResident(t, func() {
		sent, asked = silent.flood(writeQueueFrameCap)
		closed, found = writeQueueCloseWithin(t, server, source, 2*time.Minute)
	})
	t.Logf("the socket asked for the room's whole state %d times of %d, %d bytes uploaded, and stopped at %v; the handshake's whole-state frame was %d bytes, so the 512-frame cap alone would have let it hold %d bytes",
		sent, writeQueueFrameCap, sent*len(silent.askForState()), asked, silent.handoff, int64(writeQueueFrameCap)*int64(silent.handoff))
	// The memory the socket cost is reported before its close is required, so a drive without the
	// budget says both what the server held and that nothing closed the socket.
	bound := int64(peerWriteQueueBytesForTest) + 2*int64(silent.handoff) + writeQueueHeapMargin
	t.Logf("the flood: a pod's kernel would charge %d MiB, the runtime held %d bytes, %d above idle, against the %d bytes the budget, two frames and the margin allow",
		peak.charged>>20, peak.held, peak.held-idle.held, bound)
	if peak.held-idle.held > bound {
		t.Errorf("the server held %d bytes more than it did idle while one socket that never reads asked for %d whole states, want no more than the %d-byte budget, two %d-byte frames and the %d-byte margin",
			peak.held-idle.held, sent, peerWriteQueueBytesForTest, silent.handoff, int64(writeQueueHeapMargin))
	}
	requireUnderThePod(t, peak, "one socket that never reads")
	if !found {
		t.Fatalf("the server logged no close of its write-queue budget for room %s while a socket that never reads asked it for %d whole states:\n%s", source, sent, server.output.tail())
	}
	requireBudgetFigures(t, closed, "the socket that never reads", int64(silent.handoff))
	t.Logf("the server closed it on the %s path with %d bytes queued and a %d-byte frame refused, against its %d-byte budget and %d-frame cap",
		closed.path, closed.queued, closed.frame, closed.budget, closed.frames)
	if err := silent.ended(2 * time.Minute); err == nil {
		t.Error("the socket that never reads is still open, want the server to have closed it")
	} else {
		t.Logf("the socket ended with %v", err)
	}
	// The room is still the honest browser's: its socket survived the other's close.
	if err := reader.ping(); err != nil {
		t.Errorf("the honest browser lost its room: %v", err)
	}
}

// runOfWords is a document of one paragraph: size bytes of plain words in a single run of text,
// which a room loads as a handful of structs, since the decode merges a run into one item
// (squashRuns, sjawhar/ygo crdt/transaction.go:242-397), and renders as one paragraph. Its whole
// state is that text, so a socket asking for it is sent a mebibyte a time for a copy rather than
// for a walk of the tens of thousands of structs a mebibyte of proseMarkdown's sections holds.
func runOfWords(t *testing.T, size int) string {
	t.Helper()
	markdown := strings.Repeat("word ", size/5)
	if measured := pmdoc.MeasureDocument(markdown); !measured.Counted || measured.Bytes > pmdoc.MaxDocumentBytes || measured.Elements > pmdoc.MaxDocumentElements {
		t.Fatalf("%d bytes in one run measure %+v, past a bound an upload obeys", size, measured)
	}
	return markdown
}

// Twenty sockets at once, each on a mebibyte document of its own, each completing the handshake
// and then asking for its room's whole state without ever reading an answer, keep the server's
// peak under the pod's memory by both of requireUnderThePod's readings, and every one of them is
// closed by its write-queue budget.
//
// The arithmetic the budget is sized by (peerWriteQueueBytes, TestTheWriteQueueBudgetsArithmetic):
// twenty sockets hold at most 20 x 62,914,560 bytes and the frame that crossed each budget, and
// the frames twenty rooms can send are bounded by the room budget they are charged in,
// 425,585,911 bytes in all, so the twenty come to 1,683,877,111, which beside a room budget
// charged to its 2 GiB limit is 3,831,360,759, inside the 3,865,470,566-byte Go memory limit a 4
// GiB pod gives the server. The twenty rooms here are a mebibyte each, far short of that limit,
// so what this drive weighs is the queues.
//
// Their mebibyte is one run of words (runOfWords) rather than proseMarkdown's sections, because
// what a socket's budget must fill against is ygo's ten-second per-write timeout (writeTimeout,
// sjawhar/ygo provider/websocket/server.go:50-53): the writer of a socket that never reads blocks
// once the connection's buffers are full, and ten seconds later that write fails and ygo closes
// the peer without weighing any budget. Twenty rooms of prose, whose whole state is 3,005,633
// bytes of 77,600 structs to walk, leave this two-processor pod answering far too slowly for
// twenty queues to reach 60 MiB inside that window - run that way the drive was closed by the
// write timeout on sixteen of its twenty sockets - and twenty rooms of a mebibyte each also park
// the server at its Go memory limit, where the collector takes half of what processors there are.
//
// Measured on 2026-10-31 over four runs, of 25 to 32 s, the last under sjawhar/ygo
// v1.54.1-sami.3: the twenty sockets asked for 1,920 whole states between them and held
// 1,258,425,600 bytes when the budget closed every one of them, 62,921,280 each - sixty
// 1,048,688-byte frames, the sixty-first refused - where the frame cap alone would have let them
// hold 10,738,944,000. A pod's kernel would have charged the server 1,552 to 2,093 MiB of the
// pod's 4,096 at its peak, and it held 1,570,798,216 to 2,141,035,272 bytes of what its
// 3,865,470,566-byte limit bounds, 1.6 GiB under it at the most, with VmRSS at 1,589 to 2,096
// MiB.
//
// Run on 2026-10-31 with the budget at zero, the drive fails on every socket: not one of the
// twenty was closed by a budget within the two minutes it waits.
func TestTwentySocketsThatNeverReadStayUnderThePodsMemory(t *testing.T) {
	const sockets = 20
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	rooms, _ := memory.documentCopies(t, runOfWords(t, pmdoc.MaxDocumentBytes), sockets)
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	silent := make([]*silentSocket, sockets)
	for index, room := range rooms {
		silent[index] = server.openSilent(t, room)
	}
	type outcome struct {
		sent  int
		asked error
		ended error
	}
	outcomes := make([]outcome, sockets)
	var closes map[string]writeQueueClose
	peak := server.peakResident(t, func() {
		var group sync.WaitGroup
		for index := range outcomes {
			group.Go(func() {
				outcomes[index].sent, outcomes[index].asked = silent[index].flood(floodFramesPerSocket)
			})
		}
		group.Wait()
		closes = writeQueueClosesByRoom(t, server, rooms, 2*time.Minute)
	})
	// Each socket's end is waited for beside the others: a server that closes none of them would
	// otherwise be waited out twenty times over.
	var ending sync.WaitGroup
	for index := range outcomes {
		ending.Go(func() { outcomes[index].ended = silent[index].ended(2 * time.Minute) })
	}
	ending.Wait()
	var queued, asked int64
	for index, result := range outcomes {
		what := fmt.Sprintf("socket %d", index)
		asked += int64(result.sent)
		closed, found := closes[rooms[index]]
		if !found {
			t.Errorf("%s: the server logged no close of its write-queue budget for room %s, having been asked for %d whole states (%v)", what, rooms[index], result.sent, result.asked)
			continue
		}
		requireBudgetFigures(t, closed, what, int64(silent[index].handoff))
		if result.ended == nil {
			t.Errorf("%s is still open, want the server to have closed it", what)
		}
		queued += closed.queued
	}
	t.Logf("%d sockets that never read asked for %d whole states in all and held %d bytes between them when they were closed; the frame cap alone would have let them hold %d",
		sockets, asked, queued, int64(sockets)*int64(writeQueueFrameCap)*int64(silent[0].handoff))
	if bound := int64(sockets) * (int64(peerWriteQueueBytesForTest) + int64(silent[0].handoff)); queued > bound {
		t.Errorf("the sockets held %d bytes, want no more than the %d the budget and one frame each allow", queued, bound)
	}
	requireUnderThePod(t, peak, "twenty sockets that never read")
}

// throttledReadInterval is how often the honest browser on a slow link reads one frame. The room
// here answers a keystroke in about 180 ms - a mebibyte document's apply, its rendering and its
// replica's apply, on two processors - so a reader slower than that falls behind a burst of
// them, and one faster than it never does.
const throttledReadInterval = 250 * time.Millisecond

// burstKeystrokes is how many one-character inserts the typing browser sends in the burst, each
// broadcast to every other socket of the room. The burst ends with that browser handing its whole
// state back, as a browser reconnecting to the room does, which ygo relays to the room's other
// sockets as the frame it arrived in (applySyncUpdate's broadcastSync, sjawhar/ygo
// provider/websocket/peer.go:278): the heaviest frame an honest room broadcasts, and the one the
// budget is sized to hold beside a reader's ordinary lag.
const burstKeystrokes = 200

// throttledReader is an honest browser whose link is slow: it reads one frame every
// throttledReadInterval and applies every sync message to a document of its own, so the room's
// broadcasts queue for it exactly as they do for a browser on a slow connection. Unlike
// silentSocket it does read, so the server's queue for it drains, only slowly.
type throttledReader struct {
	room       string
	connection *gws.Conn
	doc        *crdt.Doc
	stop       chan struct{}
	done       chan struct{}
	stopOnce   sync.Once

	mu sync.Mutex
	// read is how many bytes of frames this browser has taken off the socket, frames how many
	// frames, pongs how many Pongs, and ended why the reading stopped, which for a browser the
	// server never closes is only the drive stopping it.
	read   int64
	frames int
	pongs  int
	ended  error
}

// openThrottled opens such a browser on the document's room. It sends a Ping at once, which lifts
// ygo's handshake deadline as a browser's first frame does, and reads nothing but at its interval
// from then on, the room's own SyncStep1, its whole state and that Ping's Pong included.
func (p *memoryServer) openThrottled(t *testing.T, artifactID string) *throttledReader {
	t.Helper()
	connection, err := p.dialDocument(artifactID)
	if err != nil {
		t.Fatalf("open document %s on a slow link: %v", artifactID, err)
	}
	slow := &throttledReader{
		room:       artifactID,
		connection: connection,
		doc:        crdt.New(),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if err := connection.WriteMessage(gws.BinaryMessage, documentFrame(artifactID, framePing, nil)); err != nil {
		t.Fatalf("ping the room from a slow link: %v", err)
	}
	go slow.run()
	t.Cleanup(slow.leave)
	return slow
}

// run reads one frame every throttledReadInterval until the drive stops it or the socket ends.
func (s *throttledReader) run() {
	defer close(s.done)
	ticker := time.NewTicker(throttledReadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
		if err := s.connection.SetReadDeadline(time.Now().Add(browserAnswerWithin)); err != nil {
			s.finish(err)
			return
		}
		_, message, err := s.connection.ReadMessage()
		if err != nil {
			s.finish(err)
			return
		}
		if err := s.take(message); err != nil {
			s.finish(err)
			return
		}
	}
}

// take applies one frame of the document socket to this browser's own document.
func (s *throttledReader) take(message []byte) error {
	decoder := encoding.NewDecoder(message)
	if _, err := decoder.ReadVarString(); err != nil {
		return fmt.Errorf("read the frame's room: %w", err)
	}
	tag, err := decoder.ReadVarUint()
	if err != nil {
		return fmt.Errorf("read the frame's tag: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read += int64(len(message))
	s.frames++
	switch tag {
	case framePong:
		s.pongs++
	case frameSync:
		kind, update, err := ygsync.ReadSyncMessage(decoder.RemainingBytes())
		if err != nil || (kind != ygsync.MsgSyncStep2 && kind != ygsync.MsgUpdate) {
			// The room's own SyncStep1 carries its state vector and nothing to apply.
			return nil
		}
		if err := crdt.ApplyUpdateV1(s.doc, update, nil); err != nil {
			return fmt.Errorf("apply the room's %d-byte sync message: %w", len(update), err)
		}
	}
	return nil
}

func (s *throttledReader) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended == nil {
		s.ended = err
	}
}

// taken is what this browser has read so far: bytes, frames, Pongs, and why its reading stopped.
func (s *throttledReader) taken() (int64, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read, s.frames, s.pongs, s.ended
}

// state is this browser's document as it stands, encoded whole.
func (s *throttledReader) state() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return crdt.EncodeStateAsUpdateV1(s.doc, nil)
}

// ping asks the room for a Pong, which arrives in this browser's own slow stream: a socket the
// server has closed cannot answer one.
func (s *throttledReader) ping() error {
	return s.connection.WriteMessage(gws.BinaryMessage, documentFrame(s.room, framePing, nil))
}

func (s *throttledReader) leave() {
	s.stopOnce.Do(func() {
		close(s.stop)
		<-s.done
		_ = s.connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = s.connection.Close()
	})
}

// An honest browser on a slow link is never closed by the write-queue budget, however far a burst
// of another browser's typing leaves it behind, and ends with the whole document; a socket in the
// same room at the same moment that never reads at all is closed.
//
// The room holds three sockets in turn: a browser typing burstKeystrokes one-character inserts as
// fast as the room answers them and then handing its whole state back as a reconnecting browser
// does, an honest browser reading one frame every throttledReadInterval throughout, and, once the
// burst has ended, a socket that completes the handshake and asks for the room's whole state
// without reading. The slow browser's lag is read from its own stream: what it had still to
// receive when the burst ended is what the server had still to write to it, its queue and the
// socket's buffers together, so it is an upper bound on the queue the budget weighs - over by the
// tens of bytes of awareness the socket that never reads adds by joining and leaving the room
// while the slow browser is still draining.
//
// Measured on 2026-10-31 and 2026-11-01 over five runs, the last two under sjawhar/ygo
// v1.54.1-sami.3: 200
// keystrokes and a handoff of 3,008,463 to 3,008,663 bytes took 49.657 to 59.138 s and left the
// slow browser 3,008,506 to 3,009,102 bytes behind
// when the burst ended - nearly all of it the relayed handoff - 4.78 per cent of its
// 62,914,560-byte budget, and under half the 6,291,456 bytes the budget clears the largest frame
// a room can send by. It had read 198 to 204 of the 205 frames and 3,018,298 to 3,018,697 of the
// 6,026,804 to 6,027,403 bytes it ended with, drained the rest at its own rate, ended holding the
// room's whole document byte for byte, and answered a ping afterwards. The socket that never
// reads was closed on the same room with 63,177,849 to 63,182,049 bytes queued and a 3,008,469 to
// 3,008,669-byte frame refused.
//
// Run on 2026-10-31 with the budget at zero, the drive fails where the socket that never reads
// should have been closed and was not. The slow browser is closed by neither setting, which is
// why both sockets run in one room: what the budget must tell apart is which of them it ends.
func TestAThrottledReaderIsNotClosedByTheWriteQueueBudget(t *testing.T) {
	memory := newMemoryHarness(t)
	memory.env = append(memory.env, "GOMEMLIMIT="+strconv.Itoa(podGoMemoryLimit))
	source, _ := memory.storedDocument(t, proseMarkdown(t, pmdoc.MaxDocumentBytes))
	server := memory.start(t)
	requirePodGoMemoryLimit(t, server)
	typing := server.openBrowser(t, source)
	defer typing.leave()
	document := newBrowserDocument(t, typing.state(t))
	text := firstXMLText(document.fragment)
	if text == nil {
		t.Fatal("the typed room's state holds no text")
	}
	slow := server.openThrottled(t, source)

	burst := time.Now()
	for typed := range burstKeystrokes {
		var update []byte
		stop := document.doc.OnUpdate(func(made []byte, _ any) { update = made })
		document.doc.Transact(func(txn *crdt.Transaction) { text.Insert(txn, typed, "k", nil) })
		stop()
		applied, closed, err := typing.send(typing.updateFrame(update))
		if err != nil || !applied {
			t.Fatalf("keystroke %d was answered applied %v, closed %v, %v", typed, applied, closed, err)
		}
	}
	// The burst ends as a browser reconnecting to the room ends its own: the whole state handed
	// back, which the room relays to every other socket.
	handoff := crdt.EncodeStateAsUpdateV1(document.doc, nil)
	if applied, closed, err := typing.send(typing.stateFrame(handoff)); err != nil || !applied {
		t.Fatalf("the typing browser's handoff of %d bytes was answered applied %v, closed %v, %v", len(handoff), applied, closed, err)
	}
	typedFor := time.Since(burst)
	atBurstEnd, framesAtEnd, _, ended := slow.taken()
	if ended != nil {
		t.Fatalf("the browser on a slow link stopped reading during the burst: %v", ended)
	}

	// The socket that never reads runs after the burst, in the room the slow browser is still
	// draining: the two are weighed against one budget apiece, and this one's close is what the
	// drive loses if the budget is off.
	silent := server.openSilent(t, source)
	sent, asked := silent.flood(floodFramesPerSocket)
	closed := requireWriteQueueClose(t, server, source, "the socket that never reads", int64(silent.handoff), 2*time.Minute)
	t.Logf("the socket that never reads asked for %d whole states of %d (%v) and was closed on the %s path with %d bytes queued and a %d-byte frame refused, against its %d-byte budget",
		sent, floodFramesPerSocket, asked, closed.path, closed.queued, closed.frame, closed.budget)
	if err := silent.ended(2 * time.Minute); err == nil {
		t.Error("the socket that never reads is still open, want the server to have closed it")
	}

	// The slow browser drains at its own rate until it holds everything the room does.
	want := canonicalState(t, typing.state(t))
	deadline := time.Now().Add(2 * time.Minute)
	for !bytes.Equal(canonicalState(t, slow.state()), want) {
		if time.Now().After(deadline) {
			read, frames, _, ended := slow.taken()
			t.Fatalf("the browser on a slow link did not hold the room's whole document after 2m: it had read %d frames and %d bytes, and its reading ended with %v",
				frames, read, ended)
		}
		time.Sleep(throttledReadInterval)
	}
	// Then until its stream goes quiet. The handoff that ends the burst carries nothing the
	// keystrokes did not, so the document alone can match before that frame has been read, and
	// the lag would then count as never owed what the server still had to write.
	for quiet := 0; quiet < 4; {
		_, before, _, _ := slow.taken()
		time.Sleep(throttledReadInterval)
		_, after, _, ended := slow.taken()
		if ended != nil {
			t.Fatalf("the browser on a slow link stopped reading while its stream drained: %v", ended)
		}
		if after == before {
			quiet++
		} else {
			quiet = 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("the browser on a slow link was still being written to after 2m, having read %d frames", after)
		}
	}
	drained, frames, pongs, ended := slow.taken()
	if ended != nil {
		t.Fatalf("the browser on a slow link stopped reading: %v", ended)
	}
	if err := slow.ping(); err != nil {
		t.Fatalf("ping the room from the slow link: %v", err)
	}
	for {
		read, _, answered, ended := slow.taken()
		if answered > pongs {
			break
		}
		if ended != nil {
			t.Fatalf("the browser on a slow link was closed before it could answer a ping, having read %d bytes: %v", read, ended)
		}
		if time.Now().After(deadline) {
			t.Fatal("the browser on a slow link did not answer a ping, want its socket still open")
		}
		time.Sleep(throttledReadInterval)
	}
	t.Logf("%d keystrokes and a %d-byte handoff in %s left the slow reader %d bytes behind when the burst ended: it had read %d frames and %d bytes of the %d frames and %d bytes it ended with, and the lag is %.4f per cent of its %d-byte budget",
		burstKeystrokes, len(handoff), typedFor.Round(time.Millisecond), drained-atBurstEnd, framesAtEnd, atBurstEnd, frames, drained,
		100*float64(drained-atBurstEnd)/float64(peerWriteQueueBytesForTest), peerWriteQueueBytesForTest)
	for _, logged := range writeQueueCloses(t, server) {
		t.Logf("the server closed a socket of room %s for its budget on the %s path, %d bytes queued and a %d-byte frame refused", logged.room, logged.path, logged.queued, logged.frame)
	}
	if drained-atBurstEnd > peerWriteQueueBytesForTest {
		t.Errorf("the slow reader was %d bytes behind at the burst's end, past its %d-byte budget: the budget would have closed an honest browser", drained-atBurstEnd, peerWriteQueueBytesForTest)
	}
}
