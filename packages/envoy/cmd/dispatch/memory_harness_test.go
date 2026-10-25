//go:build memory

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The real-process harness the memory tests (memory_test.go) drive Dispatch through: a test
// binary that runs main as a Dispatch server on the shared test database (memoryTestServeEnv), the
// server's resident memory read from /proc, and requests over HTTP and the document websocket.
// Both files build only under the `memory` tag, which the dispatch-memory CI job sets: their rows
// run past go test's default deadline, so the unit run of every Go package leaves them out.

const memoryTestToken = "memory-test-token"

// memoryHarness is the database and the issue the memory tests' servers share.
type memoryHarness struct {
	t        *testing.T
	database *store.Store
	issue    string
	// env is added to every server's environment, after the harness's own: a variable it repeats
	// wins, since exec.Cmd keeps the last value of each.
	env []string
}

func newMemoryHarness(t *testing.T) *memoryHarness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the memory tests read a server's resident memory from /proc")
	}
	harness := &memoryHarness{t: t, database: storetest.Open(t)}
	server := harness.start(t)
	for _, request := range []struct{ path, body string }{
		{"/api/v1/projects", `{"key":"MEM","name":"Memory"}`},
		{"/api/v1/issues", `{"project":"MEM","title":"Memory bound"}`},
	} {
		response := server.send(t, http.MethodPost, request.path, "application/json", strings.NewReader(request.body), http.Header{"X-Dispatch-User": {"alice"}})
		if response.status != http.StatusCreated {
			t.Fatalf("POST %s: status %d body %s", request.path, response.status, response.body)
		}
		var created struct{ Key string }
		if err := json.Unmarshal(response.body, &created); err != nil {
			t.Fatalf("decode %s: %v", request.path, err)
		}
		harness.issue = created.Key
	}
	server.stop(t)
	return harness
}

// waitForSettlement waits until the document a stored upload wrote has settled.
func (h *memoryHarness) waitForSettlement(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status == http.StatusCreated {
		h.waitForSettled(t, upload.artifactID)
	}
}

// waitForSettled waits until a document has settled every write it has had: the settlement that
// has read every update deletes the document's doc_settlements_pending row as it commits.
func (h *memoryHarness) waitForSettled(t *testing.T, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var owed bool
		if err := h.database.Pool.QueryRow(context.Background(),
			`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID,
		).Scan(&owed); err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s never settled", artifactID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// memoryServer is a Dispatch server: this test binary running main.
type memoryServer struct {
	command *exec.Cmd
	base    string
	output  *lockedBuffer
	exited  chan struct{}
	idle    int64
}

func (h *memoryHarness) start(t *testing.T) *memoryServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("free the port: %v", err)
	}
	command := exec.Command(os.Args[0])
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		memoryTestServeEnv + "=1",
		"DATABASE_URL=" + h.database.Pool.Config().ConnString(),
		"DISPATCH_AGENT_TOKEN=" + memoryTestToken,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + port,
		"DISPATCH_WEB_DIST=" + t.TempDir(),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + port,
		// main sets the runtime's memory limit from the control group it runs in
		// (setGoMemoryLimit), which here is the runner's, so the bounds above idle the memory
		// tests hold (memory_test.go) would be weighed under whatever limit the runner happens to
		// expose. Every server runs with none unless its test sets GOMEMLIMIT or AUTOMEMLIMIT in
		// env.
		"AUTOMEMLIMIT=off",
	}
	command.Env = append(command.Env, h.env...)
	server := &memoryServer{command: command, base: "http://127.0.0.1:" + port, output: &lockedBuffer{}, exited: make(chan struct{})}
	command.Stdout, command.Stderr = server.output, server.output
	if err := command.Start(); err != nil {
		t.Fatalf("start a Dispatch server: %v", err)
	}
	go func() {
		_ = command.Wait()
		close(server.exited)
	}()
	t.Cleanup(func() { server.stop(t) })
	deadline := time.Now().Add(time.Minute)
	for {
		if response, err := http.Get(server.base + "/healthz"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-server.exited:
			t.Fatalf("the Dispatch server exited while starting:\n%s", server.output.tail())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Dispatch server did not answer /healthz:\n%s", server.output.tail())
		}
		time.Sleep(50 * time.Millisecond)
	}
	server.idle = server.resident(t, "VmRSS")
	return server
}

func (p *memoryServer) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		_ = p.command.Process.Kill()
		<-p.exited
	}
}

// With AUTOMEMLIMIT=off a real server sets no memory limit, whatever control group the runner puts
// it in, and its boot line says so. The test pins the variable itself rather than trusting the
// harness's pin or the runner's control group. A container with no readable limit is the provider
// row of TestTheGoMemoryLimitIsNinetyPercentOfTheContainers (main_test.go).
func TestTheGoMemoryLimitIsNotSetWithAutomemlimitOff(t *testing.T) {
	harness := newMemoryHarness(t)
	harness.env = append(harness.env, "AUTOMEMLIMIT=off")
	server := harness.start(t)
	server.stop(t)
	output := server.output.String()
	const line = "dispatch: no go memory limit set: GOMEMLIMIT or AUTOMEMLIMIT=off in the environment, so the container's was not read"
	if count := strings.Count(output, line); count != 1 {
		t.Fatalf("the server logged %q %d times, want once:\n%s", line, count, output)
	}
	if strings.Contains(output, "dispatch: go memory limit") {
		t.Fatalf("the server set a memory limit with AUTOMEMLIMIT=off:\n%s", output)
	}
}

// resident is the server's resident memory in bytes, VmRSS now or VmHWM its peak.
func (p *memoryServer) resident(t *testing.T, field string) int64 {
	t.Helper()
	resident, found, err := p.readResident(field)
	if err != nil {
		t.Fatalf("%v\n%s", err, p.output.tail())
	}
	if !found {
		t.Fatalf("no %s in the server's status", field)
	}
	return resident
}

// readResident is resident for a goroutine other than the test's, and reports whether the
// server's status has the field at all.
func (p *memoryServer) readResident(field string) (int64, bool, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.command.Process.Pid))
	if err != nil {
		return 0, false, fmt.Errorf("read the server's memory (has it died?): %w", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, field+":"); ok {
			kibibytes, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				return 0, true, fmt.Errorf("parse %s %q: %w", field, value, err)
			}
			return kibibytes << 10, true, nil
		}
	}
	return 0, false, nil
}

// peakAboveIdle runs during and reports the most the server's resident memory rose above what it
// held idle, freshly started, while it ran.
func (p *memoryServer) peakAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	return p.peakResident(t, during) - p.idle
}

// residentSampleInterval is how often peakResident reads the server's resident memory where the
// kernel keeps no peak of it.
const residentSampleInterval = 5 * time.Millisecond

// peakResident runs during and reports the most memory the server held resident while it ran.
// Where /proc keeps a peak, as Linux does on CI's runner, that is VmHWM, reset first by writing 5
// to clear_refs, and exact. gVisor, which this repository's sandboxes run in, keeps neither: its
// status has no VmHWM line and its /proc no clear_refs, so there a goroutine reads VmRSS every
// residentSampleInterval while during runs, and a spike shorter than that interval can pass unseen.
func (p *memoryServer) peakResident(t *testing.T, during func()) int64 {
	t.Helper()
	if _, kept, err := p.readResident("VmHWM"); err != nil {
		t.Fatalf("%v\n%s", err, p.output.tail())
	} else if kept {
		if err := os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", p.command.Process.Pid), []byte("5"), 0); err != nil {
			t.Fatalf("reset the server's peak memory: %v", err)
		}
		during()
		return p.resident(t, "VmHWM")
	}
	stop, sampled := make(chan struct{}), make(chan int64, 1)
	go func() {
		ticker := time.NewTicker(residentSampleInterval)
		defer ticker.Stop()
		peak := int64(0)
		for {
			if resident, _, err := p.readResident("VmRSS"); err == nil {
				peak = max(peak, resident)
			}
			select {
			case <-stop:
				sampled <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	func() {
		defer close(stop)
		during()
	}()
	return max(<-sampled, p.resident(t, "VmRSS"))
}

type response struct {
	status int
	body   []byte
}

func (p *memoryServer) send(t *testing.T, method, path, contentType string, body io.Reader, header http.Header) response {
	t.Helper()
	answer, err := p.trySend(method, path, contentType, body, header)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// trySend is send for a goroutine other than the test's, which may not end the test.
func (p *memoryServer) trySend(method, path, contentType string, body io.Reader, header http.Header) (response, error) {
	request, err := http.NewRequest(method, p.base+path, body)
	if err != nil {
		return response{}, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	request.Header = header
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	answer, err := (&http.Client{Timeout: 20 * time.Minute}).Do(request)
	if err != nil {
		return response{}, fmt.Errorf("%s %s: %w\n%s", method, path, err, p.output.tail())
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		return response{}, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	return response{status: answer.StatusCode, body: read}, nil
}

func (p *memoryServer) get(t *testing.T, path string) {
	t.Helper()
	if answer := p.send(t, http.MethodGet, path, "", nil, http.Header{"X-Dispatch-User": {"alice"}}); answer.status != http.StatusOK {
		t.Fatalf("GET %s: status %d body %.300s", path, answer.status, answer.body)
	}
}

type uploadResult struct {
	status     int
	body       []byte
	artifactID string
	elapsed    time.Duration
}

// upload sends markdown as a new document of issue through POST /api/v1/issues/{key}/artifacts,
// as a JSON body or a multipart file, with an agent's bearer token as production's checks do.
func (p *memoryServer) upload(t *testing.T, mode, issue, markdown string) uploadResult {
	t.Helper()
	result, err := p.tryUpload(mode, issue, markdown)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// tryUpload is upload for a goroutine other than the test's.
func (p *memoryServer) tryUpload(mode, issue, markdown string) (uploadResult, error) {
	return p.tryUploadNamed(mode, issue, fmt.Sprintf("memory-%d.md", time.Now().UnixNano()), markdown)
}

// tryUploadNamed is tryUpload of a document named name: a new version of the issue's document of
// that name where it has one.
func (p *memoryServer) tryUploadNamed(mode, issue, name, markdown string) (uploadResult, error) {
	actor := map[string]string{"kind": "session", "id": "memory-test"}
	var body *bytes.Buffer
	var contentType string
	switch mode {
	case "json":
		body = &bytes.Buffer{}
		if err := json.NewEncoder(body).Encode(map[string]any{"name": name, "content": markdown, "actor": actor}); err != nil {
			return uploadResult{}, fmt.Errorf("encode the upload: %w", err)
		}
		contentType = "application/json"
	case "multipart":
		encodedActor, err := json.Marshal(actor)
		if err != nil {
			return uploadResult{}, fmt.Errorf("encode the actor: %w", err)
		}
		body, contentType, err = apitest.MultipartUpload(map[string]string{"name": name, "actor": string(encodedActor)}, name, "text/markdown", []byte(markdown))
		if err != nil {
			return uploadResult{}, err
		}
	default:
		return uploadResult{}, fmt.Errorf("unknown upload mode %q", mode)
	}
	started := time.Now()
	answer, err := p.trySend(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", contentType, body, http.Header{"Authorization": {"Bearer " + memoryTestToken}})
	if err != nil {
		return uploadResult{}, err
	}
	result := uploadResult{status: answer.status, body: answer.body, elapsed: time.Since(started).Round(time.Millisecond)}
	if answer.status == http.StatusCreated {
		var created struct {
			Artifact struct{ ID string } `json:"artifact"`
		}
		if err := json.Unmarshal(answer.body, &created); err != nil {
			return uploadResult{}, fmt.Errorf("decode the upload's answer: %w", err)
		}
		result.artifactID = created.Artifact.ID
	}
	return result, nil
}

// requireAnswered fails the test unless the upload was stored or refused with a 4xx naming its
// code and why.
func requireAnswered(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status == http.StatusCreated {
		return
	}
	var refusal struct{ Code, Error string }
	if upload.status < 400 || upload.status >= 500 || json.Unmarshal(upload.body, &refusal) != nil || refusal.Code == "" || refusal.Error == "" {
		t.Fatalf("upload answered %d %.300s, want 201 or a 4xx naming its code and reason", upload.status, upload.body)
	}
}

// loadOverWebsocket opens the document's room as a browser does - it connects and asks for the
// whole document - and closes once the room has sent it.
func (p *memoryServer) loadOverWebsocket(t *testing.T, artifactID string) {
	t.Helper()
	connection, err := p.dialDocument(artifactID)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(gws.BinaryMessage, documentFrame(artifactID, frameSync, ygsync.EncodeSyncStep1(crdt.New()))); err != nil {
		t.Fatalf("ask the room for the document: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the room's answer: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != frameSync {
			continue
		}
		if kind, _, err := ygsync.ReadSyncMessage(decoder.RemainingBytes()); err == nil && kind == ygsync.MsgSyncStep2 {
			break
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}
	// The room's last peer leaving runs its settlement; give it the time a settlement of the
	// heaviest admitted document takes here, so the peak counts it.
	time.Sleep(3 * time.Second)
}

// The tags of the document socket's frames this harness reads and writes: Hocuspocus's framing,
// which Dispatch's rooms speak (internal/dispatch/docs/service.go:559), puts the room's name before
// each (sjawhar/ygo v1.51.3-sami.2, provider/websocket/server.go:66-80).
const (
	frameSync       = 0
	frameSyncStatus = 8
	framePing       = 9
	framePong       = 10
)

// documentFrame is a frame of the document socket of room: its name, tag, then payload.
func documentFrame(room string, tag uint64, payload []byte) []byte {
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(tag)
	})
	return append(frame, payload...)
}

// dialDocument connects to the document's room as a person's browser does: signed in, read-write
// at the server's schema version (schemaReadOnly, internal/dispatch/docs/websocket.go:318-320).
// The server answers the upgrade only once it has admitted the browser and loaded the document
// (Service.ServeHTTP, websocket.go:214-283), and a burst of browsers to documents at the bound
// queues behind one count of a stored version at a time (storedCounts.counting) and loads sharing
// two processors, so a browser waits for that answer as long as a browser does rather than the
// 45 seconds gorilla's default dialer gives up after.
func (p *memoryServer) dialDocument(artifactID string) (*gws.Conn, error) {
	address := "ws" + strings.TrimPrefix(p.base, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	dialer := gws.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Minute}
	connection, answer, err := dialer.Dial(address, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		status := 0
		if answer != nil {
			status = answer.StatusCode
		}
		return nil, fmt.Errorf("connect to the document's room: %w (status %d)", err, status)
	}
	return connection, nil
}

// browserAnswerWithin is how long a browser waits for the room to answer what it sent: an update's
// apply on a document at the bound, with its rendering and its replica's catch-up, takes about a
// second here, and two of them share two processors with whatever else the drive has open.
const browserAnswerWithin = 2 * time.Minute

// browser is a document socket a test holds open as the dashboard's editor holds one. A goroutine
// of its own reads every frame the server sends, so a socket held open while its room broadcasts
// never fills the server's queue for it - ygo closes a peer whose queue fills
// (provider/websocket/peer.go:763-810) - and hands on only the answers to what the test sent: a
// SyncStatus, a Pong, a SyncStep2. The rest - the room's own step 1, awareness, other browsers'
// updates - is read and dropped, so the test process keeps no document for a socket it holds.
type browser struct {
	room       string
	connection *gws.Conn
	answers    chan browserAnswer
	// ended is closed once the read loop has stopped, and end is why: a *gws.CloseError where
	// the server closed the socket with a close frame.
	ended     chan struct{}
	end       error
	left      chan struct{}
	leaveOnce sync.Once
}

// browserAnswer is one frame the room sent in answer: a SyncStatus's flag, a Pong, or a SyncStep2's
// update, which is the room's whole state for a browser that asked with nothing.
type browserAnswer struct {
	tag    uint64
	status uint64
	state  []byte
}

// openBrowser is tryOpenBrowser for the test's own goroutine, failing it unless the room admits
// the browser.
func (p *memoryServer) openBrowser(t *testing.T, artifactID string) *browser {
	t.Helper()
	opened, err := p.tryOpenBrowser(artifactID)
	if err != nil {
		t.Fatalf("open document %s in a browser: %v", artifactID, err)
	}
	return opened
}

// tryOpenBrowser connects to the document's room and waits for the room's whole state, which ygo
// sends a peer once the room has loaded and the peer is registered in it
// (provider/websocket/server.go:2098-2104), then sends a Ping and waits for its Pong: a first frame
// lifts ygo's handshake deadline (server.go:2112-2135), so a socket held open without an edit is
// not closed thirty seconds in. A socket the server closes before any of that - the room budget's
// refusal, which completes the upgrade only to close it (refuseOverRoomBudget,
// internal/dispatch/docs/websocket.go:185-188) - returns the *gws.CloseError it was closed with.
func (p *memoryServer) tryOpenBrowser(artifactID string) (*browser, error) {
	connection, err := p.dialDocument(artifactID)
	if err != nil {
		return nil, err
	}
	opened := &browser{
		room:       artifactID,
		connection: connection,
		answers:    make(chan browserAnswer, 4),
		ended:      make(chan struct{}),
		left:       make(chan struct{}),
	}
	go opened.read()
	if _, err := opened.await(frameSync); err != nil {
		opened.leave()
		return nil, err
	}
	if err := opened.ping(); err != nil {
		opened.leave()
		return nil, err
	}
	return opened, nil
}

func (b *browser) read() {
	defer close(b.ended)
	for {
		_, message, err := b.connection.ReadMessage()
		if err != nil {
			b.end = err
			return
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		tag, err := decoder.ReadVarUint()
		if err != nil {
			continue
		}
		answer := browserAnswer{tag: tag}
		switch tag {
		case frameSyncStatus:
			if answer.status, err = decoder.ReadVarUint(); err != nil {
				continue
			}
		case framePong:
		case frameSync:
			kind, update, err := ygsync.ReadSyncMessage(decoder.RemainingBytes())
			if err != nil || kind != ygsync.MsgSyncStep2 {
				continue
			}
			answer.state = update
		default:
			continue
		}
		select {
		case b.answers <- answer:
		case <-b.left:
			return
		}
	}
}

// await is the room's next answer, which must carry tag, or the reason the socket ended first.
func (b *browser) await(tag uint64) (browserAnswer, error) {
	timer := time.NewTimer(browserAnswerWithin)
	defer timer.Stop()
	checked := func(answer browserAnswer) (browserAnswer, error) {
		if answer.tag != tag {
			return answer, fmt.Errorf("the room answered with a frame tagged %d, want %d", answer.tag, tag)
		}
		return answer, nil
	}
	select {
	case answer := <-b.answers:
		return checked(answer)
	case <-b.ended:
		select {
		case answer := <-b.answers:
			return checked(answer)
		default:
			return browserAnswer{}, b.end
		}
	case <-timer.C:
		return browserAnswer{}, fmt.Errorf("the room sent nothing tagged %d within %s", tag, browserAnswerWithin)
	}
}

func (b *browser) write(frame []byte) error {
	return b.connection.WriteMessage(gws.BinaryMessage, frame)
}

// ping sends a Ping and waits for the Pong: the socket is open and its peer reads in its room.
func (b *browser) ping() error {
	if err := b.write(documentFrame(b.room, framePing, nil)); err != nil {
		return err
	}
	_, err := b.await(framePong)
	return err
}

// state is the room's whole state, as a SyncStep2 answers a browser that asks with an empty state
// vector.
func (b *browser) state(t *testing.T) []byte {
	t.Helper()
	if err := b.write(documentFrame(b.room, frameSync, ygsync.EncodeSyncStep1(crdt.New()))); err != nil {
		t.Fatalf("ask the room for its state: %v", err)
	}
	answer, err := b.await(frameSync)
	if err != nil {
		t.Fatalf("read the room's state: %v", err)
	}
	return answer.state
}

// updateFrame is the frame the editor sends a change in: a sync Update.
func (b *browser) updateFrame(update []byte) []byte {
	return documentFrame(b.room, frameSync, ygsync.EncodeUpdate(update))
}

// stateFrame is the frame the editor hands its whole state over in: a SyncStep2.
func (b *browser) stateFrame(state []byte) []byte {
	return documentFrame(b.room, frameSync, encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(state)
	}))
}

// send sends frame, an Update or a SyncStep2, and reports whether the room applied what it carries,
// from the SyncStatus ygo answers each with on a Hocuspocus-framed socket (provider/websocket/
// peer.go:233-264). For one the room did not apply it returns too the close that followed: the
// update hook's refusal, sent at once after its SyncStatus(0) (peer.go:254-260), or the websocket
// library's, which closes a frame past the server's read limit before any room reads it, with no
// SyncStatus at all. Nil where the socket stays open, as it does for an update that fails to apply.
func (b *browser) send(frame []byte) (applied bool, closed *gws.CloseError, err error) {
	written := b.write(frame)
	if written == nil {
		answer, err := b.await(frameSyncStatus)
		switch {
		case err == nil && answer.status == 1:
			return true, nil, nil
		case err == nil:
			// Refused or failed: a refusal's close follows its SyncStatus(0) at once.
			select {
			case <-b.ended:
			case <-time.After(5 * time.Second):
				return false, nil, nil
			}
		case !errors.As(err, &closed):
			return false, nil, err
		}
	} else {
		// The server closed the socket while the frame was being written: what it closed with is
		// what the read loop read.
		select {
		case <-b.ended:
		case <-time.After(5 * time.Second):
			return false, nil, written
		}
	}
	if errors.As(b.end, &closed) {
		return false, closed, nil
	}
	if written != nil {
		return false, nil, written
	}
	return false, nil, b.end
}

// closed waits for the socket to end, and is the close frame it ended with.
func (b *browser) closed() (*gws.CloseError, error) {
	select {
	case <-b.ended:
	case <-time.After(browserAnswerWithin):
		return nil, fmt.Errorf("the socket was still open after %s", browserAnswerWithin)
	}
	var closed *gws.CloseError
	if !errors.As(b.end, &closed) {
		return nil, fmt.Errorf("the socket ended without a close frame: %w", b.end)
	}
	return closed, nil
}

// leave closes the socket as a browser leaving the page does.
func (b *browser) leave() {
	b.leaveOnce.Do(func() {
		close(b.left)
		_ = b.connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = b.connection.Close()
	})
}

// proofFragment is the Yjs fragment a document's Proof tree is kept in
// (internal/dispatch/docs/tree.go:19), which this package cannot read.
const proofFragment = "prosemirror"

// browserDocument is a browser's copy of a document, which it edits and sends the updates of. It
// is a Yjs document of the test's own, under a client id of its own, so an update it makes from a
// state copied into several documents (cloneDocuments) applies to each of them alike: a drive
// encodes one update once and sends the same bytes to every room, and holds one document for it.
type browserDocument struct {
	doc      *crdt.Doc
	fragment *crdt.YXmlFragment
}

func newBrowserDocument(t *testing.T, state []byte) *browserDocument {
	t.Helper()
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("decode the document's state: %v", err)
	}
	return &browserDocument{doc: doc, fragment: doc.GetXmlFragment(proofFragment)}
}

// browserEdit is the one update a browser holding state sends for change.
func browserEdit(t *testing.T, state []byte, change func(tree *pmdoc.Node)) []byte {
	t.Helper()
	return newBrowserDocument(t, state).edit(t, change)
}

func (d *browserDocument) tree(t *testing.T) *pmdoc.Node {
	t.Helper()
	tree, err := pmdoc.Read(d.fragment)
	if err != nil {
		t.Fatalf("read the browser's document: %v", err)
	}
	return tree
}

// edit makes change to the document's Proof tree, written by the tree diff the server's own writes
// make, which keeps every Yjs type the change leaves alone (pmdoc.Update), and returns the update
// that one transaction made, which is what the editor sends for it.
func (d *browserDocument) edit(t *testing.T, change func(tree *pmdoc.Node)) []byte {
	t.Helper()
	tree := d.tree(t)
	change(tree)
	var update []byte
	stop := d.doc.OnUpdate(func(made []byte, _ any) { update = made })
	defer stop()
	if err := d.doc.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, d.fragment, tree) }); err != nil {
		t.Fatalf("write the browser's edit: %v", err)
	}
	if update == nil {
		t.Fatal("the browser's edit changed nothing")
	}
	return update
}

// size is the document's rendering measured as the server measures a room's (pmdoc.MeasureDocument).
func (d *browserDocument) size(t *testing.T) pmdoc.DocumentSize {
	t.Helper()
	markdown, err := pmdoc.Render(d.tree(t))
	if err != nil {
		t.Fatalf("render the browser's document: %v", err)
	}
	return pmdoc.MeasureDocument(markdown)
}

// lockedBuffer is the server's output, written by the process's copying goroutine and read by the
// test when it fails, or whole by a test that asserts what the server logged.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *lockedBuffer) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	output := b.buffer.String()
	if len(output) > 4000 {
		output = output[len(output)-4000:]
	}
	return output
}

// coldPeaks reads and edits a document on servers that have not loaded it (coldReads, coldEdit)
// and fails the test where a read or the edit holds more than the bound above idle.
func (h *memoryHarness) coldPeaks(t *testing.T, name, artifactID string, edit map[string]any) {
	t.Helper()
	h.coldReads(t, name, artifactID)
	if peak := h.coldEdit(t, name, artifactID, edit); peak > requestMemoryBound {
		t.Errorf("%s: a cold one-word edit held %d MiB above idle, want at most %d MiB", name, peak>>20, requestMemoryBound>>20)
	}
}

// coldReads reads a document on servers that have not loaded it - its text and a websocket load of
// its room, each on a fresh server, then four reads of its text at once - and fails the test where
// one of the first two holds more than the bound above idle. Every peak is logged under name; the
// four reads at once are logged beside the production task's 4,096 MiB.
func (h *memoryHarness) coldReads(t *testing.T, name, artifactID string) {
	t.Helper()
	for _, cold := range []struct {
		name string
		run  func(server *memoryServer)
	}{
		{"text read", func(server *memoryServer) { server.get(t, "/api/v1/artifacts/"+artifactID+"/text") }},
		{"websocket load", func(server *memoryServer) { server.loadOverWebsocket(t, artifactID) }},
	} {
		server := h.start(t)
		peak := server.peakAboveIdle(t, func() { cold.run(server) })
		server.stop(t)
		t.Logf("%s: a cold %s, %d MiB above idle", name, cold.name, peak>>20)
		if peak > requestMemoryBound {
			t.Errorf("%s: a cold %s held %d MiB above idle, want at most %d MiB", name, cold.name, peak>>20, requestMemoryBound>>20)
		}
	}
	server := h.start(t)
	peak := server.peakAboveIdle(t, func() { server.coldTextReads(t, artifactID, 4) })
	server.stop(t)
	t.Logf("%s: four cold text reads at once, %d MiB above idle", name, peak>>20)
}

// coldTextReads reads a document's text readers times at once, failing unless every read answers
// 200. Its caller measures the fresh server's peak around it.
func (p *memoryServer) coldTextReads(t *testing.T, artifactID string, readers int) {
	t.Helper()
	failures := make([]error, readers)
	var group sync.WaitGroup
	for index := range readers {
		group.Go(func() {
			answer, err := p.trySend(http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
			if err == nil && answer.status != http.StatusOK {
				err = fmt.Errorf("read %d: status %d body %.300s", index, answer.status, answer.body)
			}
			failures[index] = err
		})
	}
	group.Wait()
	if err := errors.Join(failures...); err != nil {
		t.Fatal(err)
	}
}

// coldEdit sends edit, a one-word edit, to a document on a server that has not loaded it, fails the
// test unless it is taken, and logs and returns its peak above the server's idle memory.
func (h *memoryHarness) coldEdit(t *testing.T, name, artifactID string, edit map[string]any) int64 {
	t.Helper()
	server := h.start(t)
	var answer response
	peak := server.peakAboveIdle(t, func() { answer = server.edit(t, artifactID, edit) })
	server.stop(t)
	t.Logf("%s: a cold one-word edit answered %d, %d MiB above idle", name, answer.status, peak>>20)
	if answer.status != http.StatusOK {
		t.Errorf("%s: a cold one-word edit answered %d %.300s, want 200", name, answer.status, answer.body)
	}
	return peak
}

// blockAsks are the ids of the asks settlement indexes from the blocks ask-0 to ask-<count-1> of
// the issue's spec, in that order, once it has indexed every one.
func (p *memoryServer) blockAsks(t *testing.T, issueKey string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		answer := p.send(t, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks?state=all", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		if answer.status != http.StatusOK || json.Unmarshal(answer.body, &asks) != nil {
			t.Fatalf("list the issue's asks: status %d body %.300s", answer.status, answer.body)
		}
		byBlock := map[string]string{}
		for _, ask := range asks {
			if ask.BlockID != nil {
				byBlock[*ask.BlockID] = ask.ID
			}
		}
		ids := make([]string, 0, count)
		for index := range count {
			if id, found := byBlock[fmt.Sprintf("ask-%d", index)]; found {
				ids = append(ids, id)
			}
		}
		if len(ids) == count {
			return ids
		}
		if time.Now().After(deadline) {
			t.Fatalf("settlement indexed %d of the spec's %d asks", len(ids), count)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// createdIssue is an issue a memory test created, and its primary document.
type createdIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// createIssue creates an issue of the memory harness's project whose spec is spec, past the
// near-duplicate check its title would meet beside the others a test creates.
func (p *memoryServer) createIssue(t *testing.T, title, spec string) createdIssue {
	t.Helper()
	body, err := json.Marshal(map[string]any{"project": "MEM", "title": title, "spec": spec, "force": true})
	if err != nil {
		t.Fatalf("encode the issue: %v", err)
	}
	created := p.send(t, http.MethodPost, "/api/v1/issues", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
	var issue createdIssue
	if created.status != http.StatusCreated || json.Unmarshal(created.body, &issue) != nil || issue.PrimaryArtifactID == "" {
		t.Fatalf("create the issue %q: status %d body %.300s", title, created.status, created.body)
	}
	return issue
}

// edit sends one edit operation to a document as a person does.
func (p *memoryServer) edit(t *testing.T, artifactID string, op map[string]any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ops": []map[string]any{op}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
}

// text is a document's markdown, as GET .../text answers it.
func (p *memoryServer) text(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var text struct{ Markdown string }
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &text) != nil {
		t.Fatalf("read the document's text: status %d body %.300s", answer.status, answer.body)
	}
	return text.Markdown
}
