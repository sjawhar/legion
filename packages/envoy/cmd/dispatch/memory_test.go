package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// createdIssue is an issue a memory test created, and its primary document.
type createdIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// createIssue creates an issue of the memory harness's project whose spec is spec, past the
// near-duplicate check its title would meet beside the others a test creates.
func (p *dispatchProcess) createIssue(t *testing.T, title, spec string) createdIssue {
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
func (p *dispatchProcess) edit(t *testing.T, artifactID string, op map[string]any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ops": []map[string]any{op}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	return p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", "application/json", bytes.NewReader(body), http.Header{"X-Dispatch-User": {"alice"}})
}

// text is a document's markdown, as GET .../text answers it.
func (p *dispatchProcess) text(t *testing.T, artifactID string) string {
	t.Helper()
	answer := p.send(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", nil, http.Header{"X-Dispatch-User": {"alice"}})
	var text struct{ Markdown string }
	if answer.status != http.StatusOK || json.Unmarshal(answer.body, &text) != nil {
		t.Fatalf("read the document's text: status %d body %.300s", answer.status, answer.body)
	}
	return text.Markdown
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
	return p.tryUploadNamed(mode, issue, fmt.Sprintf("memory-%d.md", time.Now().UnixNano()), markdown)
}

// tryUploadNamed is tryUpload of a document named name: a new version of the issue's document of
// that name where it has one.
func (p *dispatchProcess) tryUploadNamed(mode, issue, name, markdown string) (uploadResult, error) {
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
