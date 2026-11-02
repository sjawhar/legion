package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A read's answer is bounded on the silence between its bytes and the rate they keep, never on
// how long the whole of it takes, and a file version download is bounded that way from its first
// byte: its own deadline (downloadDeadline) bounds the work before the answer - the row's read
// and the store's open, which the store bounds at files.OpenTimeout of its own - and stops there
// (servedAsRead, admission.WithDeadlineUntilAnswer, boundedAnswer.beginsAnswer). These two tests
// drive the whole route over a real TCP connection, against the real AWS SDK and the loopback
// fake bucket (filestest.ServeS3), because what the bound is about - a write blocking until the
// kernel drains its send buffer, and an object read at the caller's pace - neither a recorder nor
// the in-memory store has.

// downloadTestBucket is the fake bucket these downloads are served from.
const downloadTestBucket = "example-download-bucket"

// downloadCutMessage is the line serveFileVersion logs for a download its reader cut.
const downloadCutMessage = "a file version's download was cut: its reader stopped or fell below the floor"

// A file version whose bytes live in the store is answered in full to a reader that keeps to the
// answer's bounds, however many times its download's deadline that takes. Here the deadline is a
// second and a half - enough for the row's read and the store's open on a loaded pod, and no
// more - and the reader takes five of them, never silent and far above the floor, and it gets
// every byte: the download hashes to what the version's row records.
//
// Bounded by that deadline for its whole length, the object's next read failed the second its
// context did, after the headers and the Content-Length had gone out; io.Copy failed, the
// request's context was ended, and the handler returned as if the client had left, leaving the
// client a 200 short of its Content-Length with nothing logged. On the deployment's five minutes
// that cut every reader of a 25 MiB file below about 85 KiB/s, 2.7 times the floor.
func TestAStoreBackedDownloadOutlastsItsDeadlineWhileItsReaderKeepsToTheFloor(t *testing.T) {
	const deadline, idle, grace, floor = 1500 * time.Millisecond, time.Second, 2 * time.Second, 32 << 10
	const chunk, every = 64 << 10, 60 * time.Millisecond
	// Larger than the kernel's send and receive buffers together, so the handler's writes wait
	// on the reader rather than handing the whole file over at once.
	size := 2 * int(maxSendBuffer())

	handler, database := downloadTestServer(t, deadline, transferBounds{idle: idle, grace: grace, floor: floor})
	artifact, file := storeBackedVersion(t, handler, database, size)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	started := time.Now()
	response := downloadOverARealConnection(t, httpServer.Listener.Addr().String(), "/api/v1/artifacts/"+artifact+"/versions/1", chunk)
	if response.ContentLength != int64(size) {
		t.Fatalf("the download's Content-Length = %d, want the file's %d bytes", response.ContentLength, size)
	}

	// 64 KiB every 60 ms is about 1.1 MB/s, thirty times the floor, and the reader is never
	// silent for more than 60 ms; the whole download takes about eight seconds, five deadlines.
	hash := sha256.New()
	block := make([]byte, chunk)
	for read := 0; read < size; {
		time.Sleep(every)
		n, err := io.ReadFull(response.Body, block[:min(chunk, size-read)])
		read += n
		hash.Write(block[:n])
		if err != nil {
			t.Fatalf("the download was cut %v in, after %d of %d bytes: %v",
				time.Since(started).Round(time.Millisecond), read, size, err)
		}
	}
	elapsed := time.Since(started)
	if got := hex.EncodeToString(hash.Sum(nil)); got != files.SHA256(file) {
		t.Fatalf("the download hashed to %s, want the file's %s", got, files.SHA256(file))
	}
	if elapsed < 3*deadline {
		t.Fatalf("the download took %v, within three times its %v deadline: it outlasts nothing", elapsed, deadline)
	}
}

// The other side: a reader that takes a store-backed download's first bytes and then stops is
// still cut, within the idle bound and what the kernel's send buffer takes to drain at the floor
// (movingDeadline.drain), since the deadline that stopped at the answer bounds none of it. The
// cut is logged once at INFO and the connection aborted, so the client sees a cut connection
// rather than a clean 200 short of its Content-Length and an operator reading the logs can tell
// a reader that gave up from a store that failed.
func TestAStoreBackedDownloadToAReaderThatStopsIsCutAndLogged(t *testing.T) {
	const deadline, idle, floor, chunk = 1500 * time.Millisecond, time.Second, 2 << 20, 16 << 10
	// The bound the download is cut within, with a margin for a loaded pod's scheduling.
	const margin = 3 * time.Second
	size := 2 * int(maxSendBuffer())
	bound := idle + time.Duration(min(int64(size), maxSendBuffer()))*time.Second/floor

	logged := captureLog(t)
	// The grace outlasts the test, so what cuts the answer is the write's own deadline rather
	// than the floor judgment on the bytes already handed over (movingDeadline.tooSlow).
	handler, database := downloadTestServer(t, deadline, transferBounds{idle: idle, grace: time.Minute, floor: floor})
	artifact, _ := storeBackedVersion(t, handler, database, size)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	started := time.Now()
	response := downloadOverARealConnection(t, httpServer.Listener.Addr().String(), "/api/v1/artifacts/"+artifact+"/versions/1", chunk)
	// The reader takes the download's first bytes and then stops, leaving the rest to fill the
	// kernel's buffers; nothing but the answer's own bound can end the handler's writes.
	if _, err := io.ReadFull(response.Body, make([]byte, chunk)); err != nil {
		t.Fatalf("read the download's first %d bytes: %v", chunk, err)
	}

	line := awaitLineWith(t, logged, downloadCutMessage, bound+margin)
	elapsed := time.Since(started)
	if line == "" {
		t.Fatalf("a reader that stopped reading was not reported cut within %v: the %v idle bound and the %d-byte send buffer draining at %d bytes a second; the log holds:\n%s",
			bound+margin, idle, maxSendBuffer(), int64(floor), logged.String())
	}
	if elapsed < idle {
		t.Fatalf("the download was cut %v in, want it given its %v idle bound at the least", elapsed, idle)
	}
	if !strings.Contains(line, "level=INFO") || !strings.Contains(line, "artifact="+artifact) ||
		logField(line, "version") != "1" || logField(line, "size") != strconv.Itoa(size) {
		t.Fatalf("the cut line names the wrong download, or is not the INFO line a reader's doing is: %s", line)
	}
	written, err := strconv.ParseInt(logField(line, "written"), 10, 64)
	if err != nil || written <= 0 || written >= int64(size) {
		t.Fatalf("the cut line says written=%q, want some but not all of the %d bytes: %s", logField(line, "written"), size, line)
	}
	// The client reads what the kernel still held and then the cut connection, never the clean
	// end of a body its Content-Length promised.
	if _, err := io.Copy(io.Discard, response.Body); err == nil {
		t.Fatalf("the cut download ended as if whole after %d of %d bytes", written, size)
	}
}

// downloadTestServer is a server whose uploads go to a fake bucket on loopback, reached through
// the real SDK (files.NewS3, as a deployment opens its store), whose file version downloads are
// bounded at deadline before their answers begin and whose transfers are bounded by bounds.
// filestest.ServeS3 points this process's SDK at the fake with t.Setenv, so no caller of this
// may be parallel.
func downloadTestServer(t *testing.T, deadline time.Duration, bounds transferBounds) (http.Handler, *store.Store) {
	t.Helper()
	filestest.ServeS3(t, downloadTestBucket)
	fileStore, err := files.NewS3(context.Background(), downloadTestBucket)
	if err != nil {
		t.Fatalf("open the file store: %v", err)
	}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		files: fileStore, downloadDeadline: deadline,
	})
	served.transfer = bounds
	return handler, database
}

// storeBackedVersion uploads size bytes as a file version and answers the artifact that holds it
// and the bytes it uploaded, having checked that the bytes went to the store and the row holds
// none of them: what the download then reads is the object, at the pace its reader sets.
func storeBackedVersion(t *testing.T, handler http.Handler, database *store.Store, size int) (string, []byte) {
	t.Helper()
	issue := fileIssue(t, handler)
	file := make([]byte, size)
	for i := range file {
		file[i] = byte(i * 31)
	}
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts",
		map[string]string{"name": "download.bin"}, "download.bin", "application/octet-stream", file, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload the file: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)
	if version.Version.SHA256 != files.SHA256(file) || version.Version.Size != size {
		t.Fatalf("the version = %+v, want %d bytes under %s", version.Version, size, files.SHA256(file))
	}
	if row := versionRow(t, database, version.Artifact.ID, version.Version.Number); row != nil {
		t.Fatalf("the version's row holds %d bytes; with a store it holds none", len(row))
	}
	return version.Artifact.ID, file
}

// downloadOverARealConnection sends the download over a TCP connection of its own and answers
// its head, the body left to the caller to read at whatever pace it is proving. The reader's
// receive buffer is sized to chunk so that the handler's writes wait on the reader rather than
// on a buffer large enough to swallow the file, and the connection's own deadline outlasts
// every bound the tests here measure.
func downloadOverARealConnection(t *testing.T, address, target string, chunk int) *http.Response {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.(*net.TCPConn).SetReadBuffer(chunk); err != nil {
		t.Fatalf("size the reader's receive buffer: %v", err)
	}
	if _, err := fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\nX-Dispatch-User: alice\r\n\r\n", target, address); err != nil {
		t.Fatalf("send the download: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read the download's head: %v", err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the download answered %d, want 200", response.StatusCode)
	}
	return response
}

// awaitLineWith is the first line logged holds that names message, within within, and "" if the
// time passes with none: the handler writes it on its own goroutine, after the client has
// stopped reading.
func awaitLineWith(t *testing.T, logged *logBuffer, message string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, line := range strings.Split(logged.String(), "\n") {
			if strings.Contains(line, message) {
				return line
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logField is the value of key in a slog text line, and "" where the line carries no such field.
func logField(line, key string) string {
	for _, field := range strings.Fields(line) {
		if name, value, ok := strings.Cut(field, "="); ok && name == key {
			return value
		}
	}
	return ""
}
