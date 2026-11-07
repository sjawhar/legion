package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fileIssue creates the project and issue a file upload lands on and returns the issue key.
func fileIssue(t *testing.T, handler http.Handler) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "FILES", "name": "Files",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "FILES", "title": "Issue", "spec": "# Files",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

type uploaded struct {
	Artifact struct {
		ID string `json:"id"`
	} `json:"artifact"`
	Version struct {
		Number int    `json:"number"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	} `json:"version"`
}

// versionRow is what a version's row holds of its bytes: nil when they live in the store.
func versionRow(t *testing.T, database *store.Store, artifactID string, number int) []byte {
	t.Helper()
	var content []byte
	if err := database.Pool.QueryRow(context.Background(), `select content from artifact_versions where artifact_id = $1 and number = $2`, artifactID, number).Scan(&content); err != nil {
		t.Fatalf("read version row: %v", err)
	}
	return content
}

func TestUploadsGoToTheFileStoreAndAreServedFromIt(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	image := pngBytes(strings.Repeat("pixels", 4096))
	sha := files.SHA256(image)

	first := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", first.Code, first.Body.String())
	}
	version := decodeBody[uploaded](t, first)
	if version.Version.SHA256 != sha || version.Version.Size != len(image) {
		t.Fatalf("version = %+v, want sha %s and size %d", version.Version, sha, len(image))
	}
	if row := versionRow(t, database, version.Artifact.ID, version.Version.Number); row != nil {
		t.Fatalf("the row holds %d bytes; with a store it should hold none", len(row))
	}
	if memory.Len() != 1 || memory.MIME(sha) != "image/png" {
		t.Fatalf("store holds %d objects (mime %q), want the one image as image/png", memory.Len(), memory.MIME(sha))
	}

	// The same bytes again, as a second version of the same name: one object, two rows.
	second := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", image, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("second upload: status=%d body=%s", second.Code, second.Body.String())
	}
	duplicate := decodeBody[uploaded](t, second)
	if duplicate.Version.Number != 2 || duplicate.Version.SHA256 != sha {
		t.Fatalf("second version = %+v, want version 2 under sha %s", duplicate.Version, sha)
	}
	if row := versionRow(t, database, version.Artifact.ID, 2); row != nil {
		t.Fatalf("the second row holds %d bytes; with a store it should hold none", len(row))
	}

	// The version route answers from the store with the headers it always sent.
	served := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), image) {
		t.Fatalf("serve version: status=%d, %d body bytes, want 200 with the image", served.Code, served.Body.Len())
	}
	headers := served.Header()
	if headers.Get("Content-Type") != "image/png" || headers.Get("ETag") != sha || headers.Get("Content-Disposition") != "attachment" || headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("serve version headers = %v", headers)
	}
	bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/artifacts/shot-png/versions/2", nil, "alice")
	if bySlug.Code != http.StatusOK || !bytes.Equal(bySlug.Body.Bytes(), image) {
		t.Fatalf("serve version by slug: status=%d, %d body bytes", bySlug.Code, bySlug.Body.Len())
	}
}

// A file version's bytes never change, so its download says a browser may keep it for a year
// without asking again, privately since the route is authenticated; a document version is the
// document's JSON read and says nothing of the kind.
func TestAFileVersionIsCachedForeverAndADocumentVersionIsNot(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := fileIssue(t, handler)
	if upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.png"}, "shot.png", "image/png", []byte("png bytes"), "alice"); upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	for target, want := range map[string]string{
		"/api/v1/issues/" + issue + "/artifacts/shot-png/versions/1": "private, max-age=31536000, immutable",
		"/api/v1/issues/" + issue + "/artifacts/spec/versions/1":     "",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control %q, want %q", target, got, want)
		}
	}
}

func TestAnUploadTheStoreRefusesLeavesNoVersionBehind(t *testing.T) {
	memory := filestest.NewMemory()
	memory.SetFailure(errors.New("bucket unreachable"))
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)

	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "notes.bin"}, "notes.bin", "application/octet-stream", []byte("payload"), "alice")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upload against a failing store: status=%d body=%s", response.Code, response.Body.String())
	}
	if code := responseCode(t, response); code != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want FILE_STORE_UNAVAILABLE", code)
	}
	var artifacts int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifacts where issue_key = $1 and kind <> 'doc'`, issue).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("%d file artifacts exist after a refused upload, want none", artifacts)
	}
}

func TestARowStillHoldingItsBytesIsServedFromTheRow(t *testing.T) {
	// An upload made before the store existed keeps its bytes in the row; the store configured
	// later holds nothing for it, and the route serves the row.
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := fileIssue(t, handler)
	legacy := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "old.bin"}, "old.bin", "application/octet-stream", []byte("legacy bytes"), "alice")
	if legacy.Code != http.StatusCreated {
		t.Fatalf("legacy upload: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
	version := decodeBody[uploaded](t, legacy)
	if versionRow(t, database, version.Artifact.ID, 1) == nil {
		t.Fatal("an upload with no store left no bytes in the row")
	}

	memory := filestest.NewMemory()
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example", Files: memory,
	})
	if err != nil {
		t.Fatalf("new deps with a store: %v", err)
	}
	withStore := http.NewServeMux()
	t.Cleanup(Register(withStore, t.Context(), deps).Wait)
	served := dispatchRequest(t, withStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if served.Code != http.StatusOK || served.Body.String() != "legacy bytes" {
		t.Fatalf("serve a row-held version with a store configured: status=%d body=%q", served.Code, served.Body.String())
	}
	if memory.Len() != 0 {
		t.Fatalf("serving a row-held version wrote %d objects to the store", memory.Len())
	}
}

// A `HEAD` of a version's address is what a view block weighs against its 16 MiB before it sends
// the bytes to a page (dispatch://INFRA-682, R6), so it must answer the length and read none of
// them. The row records every version's size, whether its bytes sit in the row or in the store,
// so the answer needs no object at all: the bucket is made unreachable here, and the `HEAD` is
// expected to answer all the same, with the download headers its `GET` carries.
func TestAHeadOfAStoreBackedVersionAnswersItsLengthWithoutReadingTheObject(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	body := []byte(strings.Repeat("attempts", 4096))
	upload := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "loop.json"}, "loop.json", "application/json", body, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	version := decodeBody[uploaded](t, upload)
	if versionRow(t, database, version.Artifact.ID, 1) != nil {
		t.Fatal("the row kept its bytes, so this test would not reach the store path")
	}
	route := "/api/v1/artifacts/" + version.Artifact.ID + "/versions/1"

	memory.SetFailure(errors.New("the store must not be read to answer a HEAD"))
	head := dispatchRequest(t, handler, http.MethodHead, route, nil, "alice")
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD of a store-backed version: status=%d body=%s, want 200 answered from the row's size", head.Code, head.Body.String())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len(body))
	}
	if got := head.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("HEAD Content-Type = %q, want application/json", got)
	}

	// The control: with the bucket reachable the same address still serves the bytes, so what
	// the HEAD avoided is a read it did not need rather than one the route has lost.
	memory.SetFailure(nil)
	served := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
	if served.Code != http.StatusOK || !bytes.Equal(served.Body.Bytes(), body) {
		t.Fatalf("GET after the HEAD: status=%d, %d body bytes", served.Code, served.Body.Len())
	}
}

func TestAClearedRowIsAnsweredByWhatTheStoreSays(t *testing.T) {
	memory := filestest.NewMemory()
	handler, database, _ := newTestServer(t, testServerOptions{files: memory})
	issue := fileIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "gone.bin"}, "gone.bin", "application/octet-stream", []byte("gone bytes"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)
	route := "/api/v1/artifacts/" + version.Artifact.ID + "/versions/1"

	// The store cannot be reached: an outage a retry may mend, 502.
	memory.SetFailure(errors.New("bucket unreachable"))
	served := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
	if served.Code != http.StatusBadGateway || responseCode(t, served) != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("serve a version while the store is down: status=%d body=%s, want 502 FILE_STORE_UNAVAILABLE", served.Code, served.Body.String())
	}
	// Nor the version's year-long cache header, which would keep the failure in place of the bytes.
	if served.Header().Get("Content-Disposition") != "" || served.Header().Get("ETag") != "" || served.Header().Get("Cache-Control") != "" {
		t.Fatalf("a failed read sent the success headers: %v", served.Header())
	}

	// The store answers but holds no object under the row's hash: data a retry never brings
	// back, named as such, 500.
	memory.SetFailure(nil)
	memory.Delete(version.Version.SHA256)
	missing := dispatchRequest(t, handler, http.MethodGet, route, nil, "alice")
	if missing.Code != http.StatusInternalServerError || responseCode(t, missing) != "FILE_MISSING" {
		t.Fatalf("serve a version whose object is gone: status=%d body=%s, want 500 FILE_MISSING", missing.Code, missing.Body.String())
	}
	if got := missing.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("a version whose object is gone: Cache-Control %q, want none", got)
	}

	// Without a store at all, a cleared row is a configuration the server names: 503.
	noStore, err := NewDeps(DepsInput{
		Store: database, Identity: headerIdentity(database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new deps without a store: %v", err)
	}
	withoutStore := http.NewServeMux()
	t.Cleanup(Register(withoutStore, t.Context(), noStore).Wait)
	unconfigured := dispatchRequest(t, withoutStore, http.MethodGet, "/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil, "alice")
	if unconfigured.Code != http.StatusServiceUnavailable || responseCode(t, unconfigured) != "FILE_STORE_UNAVAILABLE" {
		t.Fatalf("serve a cleared row with no store: status=%d body=%s, want 503 FILE_STORE_UNAVAILABLE", unconfigured.Code, unconfigured.Body.String())
	}
}

// heldPut is a store whose Put waits until the test releases it, so a test can look at the
// database while an upload's bytes are on their way to the store.
type heldPut struct {
	*filestest.Memory
	entered chan struct{}
	release chan struct{}
}

func (h heldPut) Put(ctx context.Context, sha, mime string, body []byte) error {
	close(h.entered)
	<-h.release
	return h.Memory.Put(ctx, sha, mime, body)
}

// uploadLockProbeWindow bounds how long the probe below retries the issue's row lock. It is far
// longer than any write this test's own setup makes - the issue create and the settlement of the
// spec document it wrote - and shorter than the package's test timeout, so a holder the probe
// cannot name is reported as a failure rather than a hang.
const uploadLockProbeWindow = 20 * time.Second

// uploadIssueLock is the statement an upload's transaction takes the issue row with
// (storeArtifact's issue branch, through requireOpenOwnerStatus and issueOpenStatus under
// ownerRowLock), as pg_stat_activity shows it, and it is the only statement of this test's two
// writers that an upload runs: pg_stat_activity shows a holder at whatever statement it is on,
// and the settlement's is one of the document's own reads or lockEventOwner's of last_seq
// (events/broker.go), never this one. It is also the first statement the upload's transaction
// makes, so an upload that opened its transaction before writing its object would sit on it for
// the whole of Put. One that ran further statements between the row and Put would show one of
// those instead and be retried to the window's end, whose failure names what it read rather
// than timing out mutely.
const uploadIssueLock = "select status, closed_at is null from issues where key = $1 for no key update"

// issueRowLockHolder is what the probe reads of the backend whose transaction holds the issue's
// row, so a red run names the writer it lost the row to.
type issueRowLockHolder struct {
	pid       int32
	state     string
	xactStart *time.Time
	query     string
}

func (h issueRowLockHolder) String() string {
	start := "null"
	if h.xactStart != nil {
		start = h.xactStart.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("backend %d, state %q, xact_start %s, query %q", h.pid, h.state, start, h.query)
}

// readIssueRowLockHolder names the transaction holding issue's row: the row's xmax is that
// transaction's id, pg_locks holds that transaction's own lock on its id, and pg_stat_activity
// names the backend running it. It answers nil when the holder ended between the refusal and this
// read, and when several lockers left a multixact in xmax, which is no transaction id.
func readIssueRowLockHolder(t *testing.T, database *store.Store, issue string) *issueRowLockHolder {
	t.Helper()
	var holder issueRowLockHolder
	var state, query *string
	err := database.Pool.QueryRow(context.Background(), `
		select a.pid, a.state, a.xact_start, a.query
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.locktype = 'transactionid' and l.granted
			and l.transactionid = (select xmax from issues where key = $1)
	`, issue).Scan(&holder.pid, &state, &holder.xactStart, &query)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the holder of issue %s's row lock: %v", issue, err)
	}
	holder.state, holder.query = deref(state), deref(query)
	return &holder
}

// An upload writes its object before its transaction opens, so while the store takes the bytes
// the issue's row is free: the transaction locks it for no key update, and every other writer
// of the issue (a comment, an ask, a status change) would queue behind a store write nothing
// bounds.
//
// The upload stays in Put until this test releases it, so a lock the upload holds is held for
// every attempt of the probe below, while the issue's own setup holds that row only for a
// moment: the spec document fileIssue wrote settles a settle delay later, and that settlement's
// transaction takes the issue row as the owner of the event it appends (events/broker.go,
// lockEventOwner). The probe therefore retries, and takes the lock once that writer commits.
//
// Which of the two it met is measured rather than reasoned: the first refused attempt reads the
// holder of the row (readIssueRowLockHolder) and logs it, so a run that waited names the writer
// it waited for, and a holder running the upload's own statement (uploadIssueLock) fails the
// test there, where retrying could only spend the window. Most runs are refused nothing and log
// nothing: the settlement usually commits while fileIssue's own request is still answering,
// before there is an upload to hold anything.
func TestAnUploadHoldsNoIssueLockWhileTheStoreWrites(t *testing.T) {
	held := heldPut{Memory: filestest.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	handler, database, _ := newTestServer(t, testServerOptions{files: held})
	issue := fileIssue(t, handler)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "held.bin"); err != nil {
		t.Fatalf("write name field: %v", err)
	}
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="held.bin"`},
		"Content-Type":        {"application/octet-stream"},
	})
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write([]byte("held bytes")); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish multipart body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Dispatch-User", "alice")
	uploaded := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		uploaded <- response
	}()
	// Registered after the server's own cleanups, so it runs before them: a failure below must
	// not leave the upload parked in Put while the server's shutdown waits for it.
	t.Cleanup(func() {
		release()
		<-finished
	})

	select {
	case <-held.entered:
	case response := <-uploaded:
		t.Fatalf("the upload answered without writing to the store: status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(uploadLockProbeWindow)
	// Read once, at the first refusal: a reading per attempt would bury the one that matters,
	// and a holder parked in Put is still there to be named at any later attempt.
	var holder *issueRowLockHolder
	var looked bool
	heldBy := func() string {
		switch {
		case !looked:
			return "nothing: no attempt was refused"
		case holder == nil:
			return "nothing: the holding transaction had ended before the refusal could be followed up"
		}
		return holder.String()
	}
	for {
		_, err := database.Pool.Exec(context.Background(), `select key from issues where key = $1 for no key update nowait`, issue)
		if err == nil {
			break
		}
		var refused *pgconn.PgError
		if !looked && errors.As(err, &refused) && refused.Code == "55P03" {
			looked = true
			holder = readIssueRowLockHolder(t, database, issue)
			t.Logf("the issue's row refused the probe (55P03) while the store holds the upload's bytes; its lock is held by %s", heldBy())
			if holder != nil && strings.Contains(holder.query, uploadIssueLock) {
				t.Fatalf("the upload holds the issue's row while its bytes are in the store: %s", holder)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock the issue while its upload's bytes are in flight, over %s of attempts: %v; the first refusal read %s", uploadLockProbeWindow, err, heldBy())
		}
		time.Sleep(2 * time.Millisecond)
	}
	release()
	if response := <-uploaded; response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
}

// tampered is a store whose object under one hash reads back as other bytes of the same length,
// as a replaced or corrupted object in the bucket would, and passes every other call on.
type tampered struct {
	files.Store
	sha   string
	bytes []byte
}

func (s tampered) Get(ctx context.Context, sha string) (*files.Object, error) {
	if sha != s.sha {
		return s.Store.Get(ctx, sha)
	}
	size := int64(len(s.bytes))
	return &files.Object{Body: files.NewVerifyingReader(io.NopCloser(bytes.NewReader(s.bytes)), sha, size), Size: size}, nil
}

// A version whose object no longer holds the row's bytes is never delivered as a whole file: the
// response is cut short of its Content-Length, which a client reports. Over a real connection,
// since the cut is the connection's.
func TestATamperedObjectIsNotDeliveredWhole(t *testing.T) {
	original := bytes.Repeat([]byte("o"), 300_000)
	replaced := append(bytes.Repeat([]byte("o"), 299_999), 'x')
	store := tampered{Store: filestest.NewMemory(), sha: files.SHA256(original), bytes: replaced}
	handler, _, _ := newTestServer(t, testServerOptions{files: store})
	issue := fileIssue(t, handler)
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue+"/artifacts", map[string]string{"name": "shot.bin"}, "shot.bin", "application/octet-stream", original, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", response.Code, response.Body.String())
	}
	version := decodeBody[uploaded](t, response)

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/artifacts/"+version.Artifact.ID+"/versions/1", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("X-Dispatch-User", "alice")
	served, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer served.Body.Close()
	if served.StatusCode != http.StatusOK || served.ContentLength != int64(len(original)) {
		t.Fatalf("status %d, Content-Length %d, want 200 and %d: the object opened, so the headers went out", served.StatusCode, served.ContentLength, len(original))
	}
	got, err := io.ReadAll(served.Body)
	if err == nil || int64(len(got)) >= int64(len(original)) {
		t.Fatalf("the client read %d of %d bytes with error %v; want fewer bytes and a read error", len(got), len(original), err)
	}
}
