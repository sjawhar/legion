package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// sendWholeBodyThenRead posts body to target over a connection of its own, sending all of it
// before it reads a byte of the answer, as WebKit and Firefox send an upload: an answer the server
// writes while body is still unread, and closes the connection after, never reaches such a client.
func sendWholeBodyThenRead(t *testing.T, address, target string, header http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var head bytes.Buffer
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", target, address, len(body))
	if err := header.Write(&head); err != nil {
		t.Fatal(err)
	}
	head.WriteString("\r\n")
	if _, err := connection.Write(head.Bytes()); err != nil {
		t.Fatalf("send the request's head: %v", err)
	}
	if _, err := connection.Write(body); err != nil {
		t.Fatalf("send the whole %d-byte body before reading the answer: %v", len(body), err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read the answer once the body was sent: %v", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer's body: %v", err)
	}
	return response, answer
}

// createClosedIssue creates an issue in the project createArtifactIssue made, and closes it.
func createClosedIssue(t *testing.T, handler http.Handler) model.Issue {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Closed"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the closed issue: status=%d body=%s", created.Code, created.Body.String())
	}
	closed := decodeBody[model.Issue](t, created)
	closeArtifactIssue(t, handler, closed.Key)
	return closed
}

// INFRA-258 (FinalReview4 L2, gate-fixes S20). An answer given before an upload's body is read -
// 409 KEY_IN_FLIGHT to a repeat whose first upload still runs, 409 ISSUE_CLOSED to an upload to a
// closed issue - reaches the client as that answer over a real connection: a client that sends its
// whole multi-megabyte body before it reads, as WebKit and Firefox do, and net/http's own client,
// which reads while it sends. Without the drain net/http writes the answer with the body unread and
// closes the connection under the client still sending, which resets it.
func TestAnEarlyAnswerToAnUploadReachesAClientThatSendsItsWholeBodyFirst(t *testing.T) {
	held := &heldFileStore{Memory: filestest.NewMemory(), reached: make(chan struct{})}
	handler, _, _ := newTestServer(t, testServerOptions{files: held})
	release, releaseOnce := heldUntilReleased(t)
	held.release = release
	served := httptest.NewServer(handler)
	t.Cleanup(served.Close)
	running := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)

	// The first upload under the key runs in this process and stops at the file store, so the key
	// stays in flight while the repeats below arrive.
	first := make(chan sentUpload, 1)
	go func() { first <- sendBigUpload(t, handler, running.Key, "running-upload") }()
	select {
	case <-held.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the first upload never reached the file store")
	}
	t.Cleanup(func() {
		releaseOnce()
		select {
		case <-first:
		case <-time.After(10 * time.Second):
			t.Error("the first upload never answered once released")
		}
	})

	body, bodyType, err := apitest.MultipartUpload(map[string]string{"name": "big.bin"}, "big.bin",
		"application/octet-stream", bytes.Repeat([]byte("a"), 24<<20))
	if err != nil {
		t.Fatal(err)
	}
	upload := body.Bytes()
	for _, early := range []struct {
		name  string
		issue string
		key   string
		code  string
	}{
		{name: "a repeat while its first upload runs", issue: running.Key, key: "running-upload", code: contracts.IdempotencyKeyInFlight},
		{name: "a keyed upload to a closed issue", issue: closed.Key, key: "closed-upload", code: "ISSUE_CLOSED"},
		{name: "an unkeyed upload to a closed issue", issue: closed.Key, code: "ISSUE_CLOSED"},
	} {
		header := http.Header{}
		header.Set("Content-Type", bodyType)
		header.Set("X-Dispatch-User", "alice")
		if early.key != "" {
			header.Set(contracts.IdempotencyKeyHeader, early.key)
		}
		target := "/api/v1/issues/" + early.issue + "/artifacts"
		assertAnswer := func(t *testing.T, status int, answer []byte) {
			t.Helper()
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(answer, &refusal); status != http.StatusConflict || err != nil || refusal.Code != early.code {
				t.Fatalf("status=%d body=%q (%v), want 409 %s", status, answer, err, early.code)
			}
		}
		t.Run(early.name+", to a client that sends its whole body first", func(t *testing.T) {
			response, answer := sendWholeBodyThenRead(t, served.Listener.Addr().String(), target, header, upload)
			assertAnswer(t, response.StatusCode, answer)
		})
		t.Run(early.name+", to net/http's client", func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, served.URL+target, bytes.NewReader(upload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header = header.Clone()
			response, err := served.Client().Do(request)
			if err != nil {
				t.Fatalf("post the upload: %v", err)
			}
			defer response.Body.Close()
			answer, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read the answer's body: %v", err)
			}
			assertAnswer(t, response.StatusCode, answer)
		})
	}
}

// endlessBody is a request body of n bytes that holds none of them in memory.
func endlessBody(n int64) *countingBody {
	return &countingBody{Reader: io.LimitReader(repeatedByte('a'), n)}
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = byte(b)
	}
	return len(p), nil
}

// INFRA-258 (gate-fixes S20). An early answer drains at most the route's own limit and one byte
// more (routeBodyLimit): a body that runs past it is answered as the limit answers it, 413 with the
// route's refusal and none of the early answer's headers, and the rest is never read. A body inside
// the limit is read to its end and the early answer stands, whichever layer gave it: the keyed
// layer's KEY_IN_FLIGHT, the closed-issue refusal of an upload, LOGIN_CHANGED on a JSON route. A
// 401 stands past the limit too: a caller that resolves to nobody is refused before any limit is
// judged. A client that asked to be told to continue before sending its body is answered with none
// of it read.
func TestAnEarlyAnswerDrainsTheBodyUpToTheRoutesLimit(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	open := createArtifactIssue(t, handler)
	closed := createClosedIssue(t, handler)
	// A first upload under the key that is still running: a row in flight, inside its lease.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into request_keys (login, idempotency_key, request_digest, method, path)
		values ('alice', 'held', 'digest', 'POST', $1)
	`, "/api/v1/issues/"+open.Key+"/artifacts"); err != nil {
		t.Fatalf("hold a key in flight: %v", err)
	}

	multipartBody := map[string]string{"Content-Type": "multipart/form-data; boundary=x"}
	keyed := func(key string, header map[string]string) map[string]string {
		with := map[string]string{contracts.IdempotencyKeyHeader: key}
		for name, value := range header {
			with[name] = value
		}
		return with
	}
	toOpen := "/api/v1/issues/" + open.Key + "/artifacts"
	toClosed := "/api/v1/issues/" + closed.Key + "/artifacts"
	message := "/api/v1/issues/" + open.Key + "/messages"
	otherLogin := map[string]string{"Content-Type": "application/json", contracts.DispatchLoginHeader: "bob"}
	jsonBody := map[string]string{"Content-Type": "application/json"}
	for _, early := range []struct {
		name      string
		target    string
		header    map[string]string
		anonymous bool
		size      int64
		status    int
		code      string
		wantRead  int64
	}{
		{
			name: "a repeat while its first upload runs, past the upload limit", target: toOpen, header: keyed("held", multipartBody),
			size: keyedBodyMax + 1<<20, status: http.StatusRequestEntityTooLarge, code: "CAP_EXCEEDED", wantRead: keyedBodyMax + 1,
		},
		{
			name: "a repeat while its first upload runs, inside the upload limit", target: toOpen, header: keyed("held", multipartBody),
			size: keyedBodyMax, status: http.StatusConflict, code: contracts.IdempotencyKeyInFlight, wantRead: keyedBodyMax,
		},
		{
			name: "an upload to a closed issue past the upload limit", target: toClosed, header: multipartBody,
			size: keyedBodyMax + 1<<20, status: http.StatusRequestEntityTooLarge, code: "CAP_EXCEEDED", wantRead: keyedBodyMax + 1,
		},
		{
			name: "an upload to a closed issue inside the upload limit", target: toClosed, header: multipartBody,
			size: keyedBodyMax, status: http.StatusConflict, code: "ISSUE_CLOSED", wantRead: keyedBodyMax,
		},
		{
			name: "a message pressed by another login, past the JSON limit", target: message, header: otherLogin,
			size: 2 << 20, status: http.StatusRequestEntityTooLarge, code: "REQUEST_TOO_LARGE", wantRead: maxJSONRequestBytes + 1,
		},
		{
			name: "a message pressed by another login, inside the JSON limit", target: message, header: otherLogin,
			size: 512 << 10, status: http.StatusConflict, code: contracts.DispatchLoginChanged, wantRead: 512 << 10,
		},
		{
			name: "an issue from a caller that resolves to nobody, past the JSON limit", target: "/api/v1/issues", header: jsonBody, anonymous: true,
			size: 8 << 20, status: http.StatusUnauthorized, wantRead: maxJSONRequestBytes + 1,
		},
		{
			name: "an issue from a caller that resolves to nobody, inside the JSON limit", target: "/api/v1/issues", header: jsonBody, anonymous: true,
			size: 512 << 10, status: http.StatusUnauthorized, wantRead: 512 << 10,
		},
		{
			name: "an upload to a closed issue that waits to be told to continue", target: toClosed,
			header: map[string]string{"Content-Type": "multipart/form-data; boundary=x", "Expect": "100-continue"},
			size:   4 << 20, status: http.StatusConflict, code: "ISSUE_CLOSED", wantRead: 0,
		},
	} {
		t.Run(early.name, func(t *testing.T) {
			sent := endlessBody(early.size)
			request := httptest.NewRequest(http.MethodPost, early.target, sent)
			if !early.anonymous {
				request.Header.Set("X-Dispatch-User", "alice")
			}
			for name, value := range early.header {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if code, _ := refusalOf(t, response); response.Code != early.status || (early.code != "" && code != early.code) {
				t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), early.status, early.code)
			}
			if read := sent.read.Load(); read != early.wantRead {
				t.Fatalf("read %d bytes of the %d-byte body, want %d", read, early.size, early.wantRead)
			}
			if early.status == http.StatusRequestEntityTooLarge {
				for _, name := range []string{contracts.IdempotencyKeyHeader, "Retry-After"} {
					if value := response.Header().Get(name); value != "" {
						t.Fatalf("the 413 carries the replaced answer's %s: %q", name, value)
					}
				}
				if !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("the 413's Content-Type = %q, want JSON", response.Header().Get("Content-Type"))
				}
			}
		})
	}
}
