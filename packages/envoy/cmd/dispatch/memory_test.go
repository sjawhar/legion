package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
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

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// memoryTestServeEnv makes this test binary run main, so the memory tests drive a real Dispatch
	// process and read its own resident memory.
	memoryTestServeEnv = "DISPATCH_MEMORY_TEST_SERVE"
	memoryTestToken    = "memory-test-token"
	// requestMemoryBound is the most one request may raise a fresh server's resident memory above
	// what it held idle, and concurrentMemoryBound the most two at once may (LEGION-481). Production
	// runs one task of 1,024 MiB.
	requestMemoryBound    = 256 << 20
	concurrentMemoryBound = 512 << 20
	documentCap           = 1 << 20
)

// A markdown upload of any shape at the 1 MiB cap holds at most 256 MiB above the server's idle
// memory, through the real route on a real Dispatch process, multipart and JSON alike: stored, or
// refused with a 4xx that names why. Before LEGION-481 one 1 MiB `)_` upload took a gigabyte and
// the production task with it. Each shape runs on a fresh server, so what one leaves resident
// does not hide what the next costs, and the peak counts the settlement that follows a stored one.
func TestEveryUploadAtTheCapStaysWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range capShapes() {
		modes := []string{"json"}
		if shape.name == ")_" {
			modes = append(modes, "multipart")
		}
		t.Run(shape.name, func(t *testing.T) {
			markdown := shape.markdown()
			if len(markdown) != documentCap {
				t.Fatalf("shape is %d bytes, want the %d-byte cap", len(markdown), documentCap)
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					server := memory.start(t)
					var upload uploadResult
					peak := server.peakAboveIdle(t, func() {
						upload = server.upload(t, mode, memory.issue, markdown)
						memory.waitForSettlement(t, upload)
					})
					requireAnswered(t, upload)
					t.Logf("%s %s: %d, %d MiB above idle in %s", shape.name, mode, upload.status, peak>>20, upload.elapsed)
					if peak > requestMemoryBound {
						t.Errorf("upload of %s (%s) held %d MiB above idle, want at most %d MiB", shape.name, mode, peak>>20, requestMemoryBound>>20)
					}
				})
			}
		})
	}
}

// The heaviest documents the element limit admits are stored, and storing one - upload and
// settlement - and then reading it - its text, its blocks, and a websocket load of its room - each
// hold at most the bound, a read on a server that has not loaded the document.
func TestTheHeaviestStoredDocumentsStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	for _, shape := range heaviestAdmittedShapes(t) {
		t.Run(shape.name, func(t *testing.T) {
			writer := memory.start(t)
			var upload uploadResult
			peak := writer.peakAboveIdle(t, func() {
				upload = writer.upload(t, "json", memory.issue, shape.markdown)
				memory.waitForSettlement(t, upload)
			})
			if upload.status != http.StatusCreated {
				t.Fatalf("upload of the heaviest %s the limit admits: status %d body %.300s, want 201", shape.name, upload.status, upload.body)
			}
			t.Logf("%s: stored, %d MiB above idle in %s", shape.name, peak>>20, upload.elapsed)
			if peak > requestMemoryBound {
				t.Errorf("storing %s held %d MiB above idle, want at most %d MiB", shape.name, peak>>20, requestMemoryBound>>20)
			}
			reader := memory.start(t)
			for _, read := range []struct {
				name string
				run  func(t *testing.T)
			}{
				{"text", func(t *testing.T) { reader.get(t, "/api/v1/artifacts/"+upload.artifactID+"/text") }},
				{"blocks", func(t *testing.T) { reader.get(t, "/api/v1/artifacts/"+upload.artifactID+"/blocks") }},
				{"websocket load", func(t *testing.T) { reader.loadOverWebsocket(t, upload.artifactID) }},
			} {
				peak := reader.peakAboveIdle(t, func() { read.run(t) })
				t.Logf("%s: %s read, %d MiB above idle", shape.name, read.name, peak>>20)
				if peak > requestMemoryBound {
					t.Errorf("%s read of %s held %d MiB above idle, want at most %d MiB", read.name, shape.name, peak>>20, requestMemoryBound>>20)
				}
			}
		})
	}
}

// Two uploads at once hold at most twice the bound together: two of a cap-sized `)_`, which the
// element limit refuses, and two of the heaviest `)_` document it admits, which are stored.
func TestTwoUploadsAtOnceStayWithinTheMemoryBound(t *testing.T) {
	memory := newMemoryHarness(t)
	heaviest := heaviestAdmitted(t, func(units int) string { return strings.Repeat(")_", units) })
	for _, pair := range []struct{ name, markdown string }{
		{name: "cap-sized )_", markdown: fill(")_")},
		{name: "heaviest admitted )_", markdown: heaviest},
	} {
		t.Run(pair.name, func(t *testing.T) {
			server := memory.start(t)
			var uploads [2]uploadResult
			var failures [2]error
			peak := server.peakAboveIdle(t, func() {
				var group sync.WaitGroup
				for index := range uploads {
					group.Go(func() { uploads[index], failures[index] = server.tryUpload("json", memory.issue, pair.markdown) })
				}
				group.Wait()
				if err := errors.Join(failures[:]...); err != nil {
					t.Fatal(err)
				}
				for _, upload := range uploads {
					memory.waitForSettlement(t, upload)
				}
			})
			for _, upload := range uploads {
				requireAnswered(t, upload)
			}
			t.Logf("two uploads of %s: %d and %d, %d MiB above idle", pair.name, uploads[0].status, uploads[1].status, peak>>20)
			if peak > concurrentMemoryBound {
				t.Errorf("two uploads of %s at once held %d MiB above idle, want at most %d MiB", pair.name, peak>>20, concurrentMemoryBound>>20)
			}
		})
	}
}

// memoryShape is a markdown document a memory test uploads.
type memoryShape struct {
	name     string
	markdown func() string
}

// capShapes are the shapes LEGION-481 names, each exactly at the 1 MiB cap: the ones LEGION-465's
// pull requests (#1670, #1669) found quadratic or deep, a table of a mebibyte of cells, and the
// node-heavy tables and links #1669's review measured. 1 MiB of bare `>` nests a quote per byte:
// main's parse ran 14 minutes into a stack overflow that took the server down, and #1669's depth
// bound refuses it as soon as it nests past 100.
func capShapes() []memoryShape {
	repeated := func(unit string) func() string { return func() string { return fill(unit) } }
	return []memoryShape{
		{")_", repeated(")_")},
		{"a_", repeated("a_")},
		{"a_b*", repeated("a_b*")},
		{"a~b_", repeated("a~b_")},
		{"a_b&", repeated("a_b&")},
		{"<a", repeated("<a")},
		{"[a", repeated("[a")},
		{"[^a then ]", func() string { return fill("[^a")[:documentCap-1] + "]" }},
		{"a line feed per character", repeated("a\n")},
		{"one run of *", func() string { return "a" + fill("*")[1:] }},
		{"one run of _", func() string { return "a" + fill("_")[1:] }},
		{"one run of ~", func() string { return "a" + fill("~")[1:] }},
		{"262,140 nested marks", func() string {
			run := strings.Repeat("*", 2*262_140)
			return run + strings.Repeat("x", documentCap-2*len(run)) + run
		}},
		{"100-deep quotes repeated", func() string {
			line := strings.Repeat("> ", 100) + "a\n"
			text := strings.Repeat(line, documentCap/len(line))
			return text + strings.Repeat("a", documentCap-len(text))
		}},
		{"a table of 1 MiB of cells", func() string {
			head := "| a | b |\n| - | - |\n"
			text := head + strings.Repeat("| x | y |\n", (documentCap-len(head))/10)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an escaped-pipe table", func() string {
			head := "| a |\n| --- |\n"
			text := head + strings.Repeat("| `x\\|y` |\n", (documentCap-len(head))/12)
			return text + strings.Repeat("z", documentCap-len(text))
		}},
		{"an image-in-link chain", repeated("[![a](b)](c)")},
		{"lists nested a level a line", func() string {
			var text strings.Builder
			for depth := 0; text.Len()+2*depth+4 <= documentCap; depth++ {
				text.WriteString(strings.Repeat("  ", depth) + "- a\n")
			}
			return text.String() + strings.Repeat("a", documentCap-text.Len())
		}},
		{"1 MiB of bare >", repeated(">")},
	}
}

// fill is unit repeated to exactly the document cap.
func fill(unit string) string {
	return strings.Repeat(unit, documentCap/len(unit)+1)[:documentCap]
}

// admittedShape is a document the element limit admits.
type admittedShape struct {
	name     string
	markdown string
}

// heaviestAdmittedShapes are the heaviest documents of the shapes that cost the most memory per
// element - italic spans, delimiter runs, table cells, headings and list items - that the element
// limit admits, each found by the parser the server writes with.
func heaviestAdmittedShapes(t *testing.T) []admittedShape {
	t.Helper()
	repeated := func(unit string) func(int) string {
		return func(units int) string { return strings.Repeat(unit, units) }
	}
	table := func(header, delimiter, row string) func(int) string {
		return func(rows int) string { return header + delimiter + strings.Repeat(row, rows) }
	}
	var shapes []admittedShape
	for _, shape := range []struct {
		name  string
		build func(int) string
	}{
		{")_", repeated(")_")},
		{"a_b*", repeated("a_b*")},
		{"a two-column table", table("| a | b |\n", "| - | - |\n", "| x | y |\n")},
		{"a four-column table", table("| a | b | c | d |\n", "| - | - | - | - |\n", "| w | x | y | z |\n")},
		{"headings", repeated("# a\n")},
		{"list items", repeated("- a\n")},
	} {
		shapes = append(shapes, admittedShape{name: shape.name, markdown: heaviestAdmitted(t, shape.build)})
	}
	return shapes
}

// heaviestAdmitted is the document of the most units build makes that the element limit admits,
// padded with a paragraph of plain words to the cap, which weighs next to nothing.
func heaviestAdmitted(t *testing.T, build func(units int) string) string {
	t.Helper()
	document := func(units int) string {
		text := build(units) + "\n\n"
		return text + strings.Repeat("word ", documentCap/5+1)[:documentCap-len(text)]
	}
	admitted := func(units int) bool {
		_, err := pmdoc.Parse(document(units))
		if err != nil && !errors.Is(err, pmdoc.ErrTooManyElements) {
			t.Fatalf("parse %d units: %v", units, err)
		}
		return err == nil
	}
	low, high := 1, 1
	for admitted(high) {
		low, high = high, high*2
	}
	for high-low > 1 {
		if middle := (low + high) / 2; admitted(middle) {
			low = middle
		} else {
			high = middle
		}
	}
	return document(low)
}

// memoryHarness is the database and the issue the memory tests' servers share.
type memoryHarness struct {
	t        *testing.T
	database *store.Store
	issue    string
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

// waitForSettlement waits until the document a stored upload wrote has settled: the settlement
// that has read every update deletes the document's doc_settlements_pending row as it commits.
func (h *memoryHarness) waitForSettlement(t *testing.T, upload uploadResult) {
	t.Helper()
	if upload.status != http.StatusCreated {
		return
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var owed bool
		if err := h.database.Pool.QueryRow(context.Background(),
			`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, upload.artifactID,
		).Scan(&owed); err != nil {
			t.Fatalf("read the document's pending settlement: %v", err)
		}
		if !owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s never settled", upload.artifactID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dispatchProcess is a Dispatch server: this test binary running main.
type dispatchProcess struct {
	command *exec.Cmd
	base    string
	output  *lockedBuffer
	exited  chan struct{}
	idle    int64
}

func (h *memoryHarness) start(t *testing.T) *dispatchProcess {
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
		"DISPATCH_ALLOWED_LOGINS=alice",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + port,
		"DISPATCH_WEB_DIST=" + t.TempDir(),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + port,
	}
	server := &dispatchProcess{command: command, base: "http://127.0.0.1:" + port, output: &lockedBuffer{}, exited: make(chan struct{})}
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

func (p *dispatchProcess) stop(t *testing.T) {
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

// resident is the server's resident memory in bytes, VmRSS now or VmHWM its peak.
func (p *dispatchProcess) resident(t *testing.T, field string) int64 {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.command.Process.Pid))
	if err != nil {
		t.Fatalf("read the server's memory (has it died?): %v\n%s", err, p.output.tail())
	}
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(line, field+":"); ok {
			kibibytes, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
			if err != nil {
				t.Fatalf("parse %s %q: %v", field, value, err)
			}
			return kibibytes << 10
		}
	}
	t.Fatalf("no %s in the server's status", field)
	return 0
}

// peakAboveIdle runs during and reports the most the server's resident memory rose above what it
// held idle, freshly started, while it ran.
func (p *dispatchProcess) peakAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	// Writing 5 to clear_refs resets the peak (VmHWM) to the memory resident now.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", p.command.Process.Pid), []byte("5"), 0); err != nil {
		t.Fatalf("reset the server's peak memory: %v", err)
	}
	during()
	return p.resident(t, "VmHWM") - p.idle
}

type response struct {
	status int
	body   []byte
}

func (p *dispatchProcess) send(t *testing.T, method, path, contentType string, body io.Reader, header http.Header) response {
	t.Helper()
	answer, err := p.trySend(method, path, contentType, body, header)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// trySend is send for a goroutine other than the test's, which may not end the test.
func (p *dispatchProcess) trySend(method, path, contentType string, body io.Reader, header http.Header) (response, error) {
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

func (p *dispatchProcess) get(t *testing.T, path string) {
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
func (p *dispatchProcess) upload(t *testing.T, mode, issue, markdown string) uploadResult {
	t.Helper()
	result, err := p.tryUpload(mode, issue, markdown)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// tryUpload is upload for a goroutine other than the test's.
func (p *dispatchProcess) tryUpload(mode, issue, markdown string) (uploadResult, error) {
	name := fmt.Sprintf("memory-%d.md", time.Now().UnixNano())
	actor := map[string]string{"kind": "session", "id": "memory-test"}
	var body bytes.Buffer
	var contentType string
	switch mode {
	case "json":
		if err := json.NewEncoder(&body).Encode(map[string]any{"name": name, "content": markdown, "actor": actor}); err != nil {
			return uploadResult{}, fmt.Errorf("encode the upload: %w", err)
		}
		contentType = "application/json"
	case "multipart":
		writer := multipart.NewWriter(&body)
		encodedActor, err := json.Marshal(actor)
		if err != nil {
			return uploadResult{}, fmt.Errorf("encode the actor: %w", err)
		}
		for field, value := range map[string]string{"name": name, "actor": string(encodedActor)} {
			if err := writer.WriteField(field, value); err != nil {
				return uploadResult{}, fmt.Errorf("write %s: %w", field, err)
			}
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="file"; filename="`+name+`"`)
		header.Set("Content-Type", "text/markdown")
		part, err := writer.CreatePart(header)
		if err != nil {
			return uploadResult{}, fmt.Errorf("create the file part: %w", err)
		}
		if _, err := io.WriteString(part, markdown); err != nil {
			return uploadResult{}, fmt.Errorf("write the file part: %w", err)
		}
		if err := writer.Close(); err != nil {
			return uploadResult{}, fmt.Errorf("close the multipart body: %w", err)
		}
		contentType = writer.FormDataContentType()
	default:
		return uploadResult{}, fmt.Errorf("unknown upload mode %q", mode)
	}
	started := time.Now()
	answer, err := p.trySend(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", contentType, &body, http.Header{"Authorization": {"Bearer " + memoryTestToken}})
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
func (p *dispatchProcess) loadOverWebsocket(t *testing.T, artifactID string) {
	t.Helper()
	address := "ws" + strings.TrimPrefix(p.base, "http") + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, answer, err := gws.DefaultDialer.Dial(address, http.Header{"X-Dispatch-User": {"alice"}})
	if err != nil {
		t.Fatalf("connect to the document's room: %v (%#v)", err, answer)
	}
	defer connection.Close()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, append(frame, ygsync.EncodeSyncStep1(crdt.New())...)); err != nil {
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
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
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

// lockedBuffer is the server's output, written by the process's copying goroutine and read by the
// test when it fails.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
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
