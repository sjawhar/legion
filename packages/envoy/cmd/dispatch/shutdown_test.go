package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// promptShutdown bounds a SIGTERM's whole ordered shutdown with a dashboard open. Ending the
// streams, finishing the requests in flight and settling one small document take milliseconds; a
// stream that holds http.Server.Shutdown open costs five seconds, the HTTP budget, alone.
const promptShutdown = 3 * time.Second

// streamedSessionID is the session whose conversation the test's agent view watches; the agent
// stream route takes a session uuid.
const streamedSessionID = "01a0e090-4848-7473-acc5-fc96e6a646d3"

// A deploy stops Dispatch while people have the dashboard open: every tab holds the event stream,
// and one on an agent's page holds that conversation's stream too. SIGTERM ends each stream, so
// http.Server.Shutdown returns once the requests in flight have, and the document service settles
// the document an edit has just left owing a settlement. An edit's settlement runs two seconds
// after it (docs.Deps.Settle's default), so a signal sent as soon as the edit answers reaches the
// document service with that settlement still owed, and Shutdown is the one that runs it.
func TestSIGTERMWithOpenStreamsExitsPromptlyAndSettlesTheOwedDocument(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcess(t, buildDispatch(t), database.Pool.Config().ConnString())
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)

	streams := map[string]<-chan streamEnd{
		"event stream":              process.openStream(t, "/api/v1/events"),
		"agent conversation stream": process.openStream(t, "/api/v1/agents/"+streamedSessionID+"/stream"),
	}
	process.editOwingSettlement(t, database, artifactID)

	signalled := time.Now()
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	process.waitExit(t, "SIGTERM")
	exited := time.Since(signalled)
	t.Logf("exited %v after SIGTERM", exited)
	for name, streamEnded := range streams {
		ended := <-streamEnded
		t.Logf("the %s ended %v after SIGTERM (%v)", name, ended.at.Sub(signalled), ended.err)
		// A stream the server ended reads to its end; one the exiting process cut reads as a
		// broken response, which the dashboard's reader also reconnects from, but which says the
		// server held the stream for as long as the process lived.
		if ended.err != io.EOF {
			t.Errorf("the %s ended with %v, want the server to end it (EOF)", name, ended.err)
		}
	}
	if exited >= promptShutdown {
		t.Errorf("Dispatch exited %v after SIGTERM, want under %v", exited, promptShutdown)
	}
	if strings.Contains(process.output.String(), `msg="dispatch: shutdown"`) {
		t.Errorf("HTTP shutdown did not finish inside its budget")
	}
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A request in flight can still hold http.Server.Shutdown for its whole budget, here a title change
// queued behind another writer's lock on the issue. The document service's budget starts when HTTP
// shutdown returns, so the settlement owed by the edit before the signal still runs and commits.
// Settlement waits on the same lock: the edit's own timer starts one two seconds in, during the HTTP
// budget, and Shutdown starts another. The lock is released once Shutdown's is queued behind it too,
// or two seconds after HTTP shutdown gave up, which is ample for the document service to read what
// is owed; a release before that read could let the timer's settlement commit first.
func TestSIGTERMWhileARequestHoldsHTTPShutdownStillSettlesTheOwedDocument(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcess(t, buildDispatch(t), database.Pool.Config().ConnString())
	process.waitHealthy(t)
	key, artifactID := process.createIssue(t)
	process.editOwingSettlement(t, database, artifactID)

	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, key); err != nil {
		t.Fatalf("lock the issue: %v", err)
	}
	go func() {
		request, err := http.NewRequest(http.MethodPatch, process.url("/api/v1/issues/"+key), strings.NewReader(`{"title":"Renamed"}`))
		if err != nil {
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", dispatchTestLogin)
		// Its answer is not this test's: the process may exit before it is written.
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	if waiting := waitForLockWaiters(t, holder, 1, 10*time.Second); waiting < 1 {
		t.Fatalf("the title change never queued behind the issue lock:\n%s", process.output.String())
	}

	signalled := time.Now()
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	process.waitForOutput(t, `msg="dispatch: shutdown"`)
	t.Logf("HTTP shutdown gave up %v after SIGTERM", time.Since(signalled))
	waiting := waitForLockWaiters(t, holder, 3, 2*time.Second)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	process.waitExit(t, "SIGTERM")
	t.Logf("released the issue lock with %d waiting on it; exited %v after SIGTERM", waiting, time.Since(signalled))
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A deploy usually finds someone with a document open: the spec tab holds the document's websocket,
// so its room is loaded with an editor connected, and what that editor last typed is owed a
// settlement for two seconds (docs.Deps.Settle's default). The document service settles the room
// while it is still loaded, and only then closes the editor's connection, so the edit is versioned,
// credited to the editor, before the process exits rather than when someone next opens the spec.
func TestSIGTERMWithAnEditorConnectedSettlesItsDocumentBeforeExit(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcess(t, buildDispatch(t), database.Pool.Config().ConnString())
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)
	editor := process.openEditor(t, artifactID, "before\n")
	// The spec the issue was created with is owed a settlement of its own; once that has run, the
	// pending-settlement row the editor's keystrokes write is theirs alone.
	waitForSettlementOwed(t, database, artifactID, false)
	editor.replaceText(t, "before", "after")
	waitForSettlementOwed(t, database, artifactID, true)

	signalled := time.Now()
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	process.waitExit(t, "SIGTERM")
	exited := time.Since(signalled)
	t.Logf("exited %v after SIGTERM", exited)
	select {
	case closed := <-editor.ended:
		t.Logf("the editor's connection ended %v after SIGTERM", closed.Sub(signalled))
	case <-time.After(5 * time.Second):
		t.Errorf("the editor's connection was still open 5s after Dispatch exited")
	}
	if exited >= promptShutdown {
		t.Errorf("Dispatch exited %v after SIGTERM, want under %v", exited, promptShutdown)
	}
	process.checkSettledAtShutdown(t, database, artifactID)
	number, markdown, authors := latestVersion(t, database, artifactID)
	if number != 2 || markdown != "after\n" || !strings.Contains(authors, `"`+dispatchTestLogin+`"`) {
		t.Errorf("the document's latest version is %d holding %q by %s, want version 2 holding the editor's edit, %q, credited to %s",
			number, markdown, authors, "after\n", dispatchTestLogin)
	}
	for _, line := range strings.Split(process.output.String(), "\n") {
		if strings.Contains(line, "room="+artifactID) {
			t.Logf("Dispatch logged: %s", line)
		}
	}
}

// checkSettledAtShutdown requires the exited process to have stopped in order with the document's
// owed settlement run and confirmed.
func (p *dispatchProcess) checkSettledAtShutdown(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	output := p.output.String()
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}
	if strings.Contains(output, "settlement unconfirmed") {
		t.Errorf("the document service could not confirm the owed settlement")
	}
	if !strings.Contains(output, `msg="dispatch: document settled before shutdown" room=`+artifactID) {
		t.Errorf("the document service did not settle the owed document before exiting")
	}
	if settlementOwed(t, database, artifactID) {
		t.Errorf("the document still owes its settlement after the shutdown that ran it")
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", output)
	}
}

// dispatchTestLogin is the allowlisted login the test calls the server as, through header identity.
const dispatchTestLogin = "alice"

// dispatchProcess is a Dispatch binary a test started, with its combined output.
type dispatchProcess struct {
	cmd    *exec.Cmd
	port   int
	output *lockedBuffer
	exited chan struct{}
}

func buildDispatch(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "envoy-dispatch")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build Dispatch: %v\n%s", err, out)
	}
	return binary
}

// startDispatchProcess starts binary on a free loopback port against databaseURL, with no NATS and
// nothing from the caller's environment or home, and kills it when the test ends if it still runs.
// DISPATCH_TEST_HOOKS serves the agent conversation relay in-process, as the browser harness does,
// since there is no NATS to relay it from.
func startDispatchProcess(t *testing.T, binary, databaseURL string) *dispatchProcess {
	t.Helper()
	home := t.TempDir()
	process := &dispatchProcess{port: freeTCPPort(t), output: &lockedBuffer{}, exited: make(chan struct{})}
	process.cmd = exec.Command(binary)
	process.cmd.Dir = home
	process.cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=shutdown-test-token",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS=" + dispatchTestLogin,
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(process.port),
		"DISPATCH_WEB_DIST=" + home,
		"DISPATCH_TEST_HOOKS=1",
	}
	process.cmd.Stdout, process.cmd.Stderr = process.output, process.output
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start Dispatch: %v", err)
	}
	go func() {
		_ = process.cmd.Wait()
		close(process.exited)
	}()
	t.Cleanup(func() {
		_ = process.cmd.Process.Kill()
		<-process.exited
	})
	return process
}

func (p *dispatchProcess) url(path string) string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port) + path
}

// waitHealthy waits up to 30 s for /healthz to answer 200. The port is bound after the SIGTERM
// handler is installed, so a server that answers is one a signal stops in order.
func (p *dispatchProcess) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(p.url("/healthz"))
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("Dispatch exited before it became healthy:\n%s", p.output.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("Dispatch never became healthy:\n%s", p.output.String())
}

// call sends body as the test's login and requires status, returning the response body.
func (p *dispatchProcess) call(t *testing.T, method, path, body string, status int) []byte {
	t.Helper()
	request, err := http.NewRequest(method, p.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, answer)
	}
	return answer
}

// createIssue creates a project and an issue whose spec is one word, returning the issue's key and
// its spec document's id.
func (p *dispatchProcess) createIssue(t *testing.T) (key, artifactID string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/projects", `{"key":"STOP","name":"Shutdown"}`, http.StatusCreated)
	var issue struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	created := p.call(t, http.MethodPost, "/api/v1/issues", `{"project":"STOP","title":"Settle at shutdown","spec":"before"}`, http.StatusCreated)
	if err := json.Unmarshal(created, &issue); err != nil || issue.Key == "" || issue.PrimaryArtifactID == "" {
		t.Fatalf("create issue: %v: %s", err, created)
	}
	return issue.Key, issue.PrimaryArtifactID
}

// editOwingSettlement edits the document and requires the edit to have left it owing a settlement.
func (p *dispatchProcess) editOwingSettlement(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	p.call(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits",
		`{"ops":[{"op":"replace","find":"before","with":"after"}]}`, http.StatusOK)
	if !settlementOwed(t, database, artifactID) {
		t.Fatalf("the edit left no pending-settlement row, so the signal would find nothing owed:\n%s", p.output.String())
	}
}

// streamEnd is when a stream stopped and the read error that stopped it.
type streamEnd struct {
	at  time.Time
	err error
}

// openStream opens the server-sent stream at path as a signed-in tab holds it and reads it until it
// ends, reporting when and how on the returned channel.
func (p *dispatchProcess) openStream(t *testing.T, path string) <-chan streamEnd {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, p.url(path), nil)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("open %s: status %d, content type %q: %s", path, response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	ended := make(chan streamEnd, 1)
	go func() {
		defer response.Body.Close()
		buffer := make([]byte, 4096)
		for {
			if _, err := response.Body.Read(buffer); err != nil {
				ended <- streamEnd{at: time.Now(), err: err}
				return
			}
		}
	}()
	return ended
}

// editor is a signed-in spec tab: a writable connection to a document's room that applies every
// update the room sends, as the dashboard's provider does.
type editor struct {
	artifactID string
	doc        *crdt.Doc
	writes     sync.Mutex
	connection *gws.Conn
	// answers carries the content of each sync step 2 the room sends, once it is applied.
	answers chan []byte
	// ended receives when the connection ended.
	ended chan time.Time
}

// openEditor connects an editor to the document at the schema version the dashboard presents, so
// the room takes its edits, and returns once the editor's copy of the document renders want.
func (p *dispatchProcess) openEditor(t *testing.T, artifactID, want string) *editor {
	t.Helper()
	url := "ws://127.0.0.1:" + strconv.Itoa(p.port) + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{dispatchTestLogin}})
	if err != nil {
		t.Fatalf("connect an editor to the document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	e := &editor{
		artifactID: artifactID, doc: crdt.New(), connection: connection,
		answers: make(chan []byte, 16), ended: make(chan time.Time, 1),
	}
	go func() {
		docstest.Drain(connection, e.doc, e.write, func(content []byte) {
			select {
			case e.answers <- content:
			default:
			}
		})
		e.ended <- time.Now()
	}()
	deadline := time.After(10 * time.Second)
	for e.markdown() != want {
		if err := e.write(ygsync.EncodeSyncStep1(crdt.New())); err != nil {
			t.Fatalf("ask the room for the document: %v", err)
		}
		select {
		case <-e.answers:
		case <-e.ended:
			t.Fatalf("the editor's connection closed before the room sent the document:\n%s", p.output.String())
		case <-deadline:
			t.Fatalf("the room never sent the editor the document:\n%s", p.output.String())
		}
	}
	return e
}

func (e *editor) write(syncMessage []byte) error {
	return docstest.WriteFrame(&e.writes, e.connection, e.artifactID, syncMessage)
}

// markdown renders the editor's copy of the document, read under its lock while the room's updates
// apply, or "" while the copy holds no document it can render.
func (e *editor) markdown() string {
	fragment := e.doc.GetXmlFragment("prosemirror")
	var rendered string
	e.doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return
		}
		rendered, _ = pmdoc.Render(tree)
	}, nil)
	return rendered
}

// replaceText types replacement over the first paragraph's text, old, in one keystroke's
// transaction, and sends the room the update it made.
func (e *editor) replaceText(t *testing.T, old, replacement string) {
	t.Helper()
	fragment := e.doc.GetXmlFragment("prosemirror")
	var changeErr error
	update := docstest.Transact(e.doc, func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != old {
			changeErr = fmt.Errorf("the document's first paragraph does not hold only %q", old)
			return
		}
		tree.Children[0].Children[0].Text = replacement
		changeErr = pmdoc.Update(txn, fragment, tree)
	})
	if changeErr != nil || update == nil {
		t.Fatalf("the editor's edit: update=%d bytes err=%v", len(update), changeErr)
	}
	if err := e.write(ygsync.EncodeUpdate(update)); err != nil {
		t.Fatalf("send the editor's edit: %v", err)
	}
}

// waitExit waits up to 30 s for Dispatch to exit, and fails the test with its output when it is
// still running; after names what it was waiting on.
func (p *dispatchProcess) waitExit(t *testing.T, after string) {
	t.Helper()
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("Dispatch was still running 30s after %s:\n%s", after, p.output.String())
	}
}

// waitForOutput waits up to 30 s for line to appear in Dispatch's output, and fails the test when
// Dispatch exits first or the line never appears.
func (p *dispatchProcess) waitForOutput(t *testing.T, line string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(p.output.String(), line) {
		select {
		case <-p.exited:
			t.Fatalf("Dispatch exited before it logged %q:\n%s", line, p.output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch never logged %q:\n%s", line, p.output.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForLockWaiters waits up to within for want backends to queue behind a lock holder's
// transaction owns, directly or behind an earlier waiter, and returns how many it last saw. It polls
// through holder's own connection and reads pg_locks, as api's waitForDatabaseLocks does.
func waitForLockWaiters(t *testing.T, holder pgx.Tx, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var count int
		if err := holder.QueryRow(context.Background(), `
			with recursive waiting(pid) as (
				select pid from pg_locks
				where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))
				union
				select blocked.pid from pg_locks blocked, waiting
				where not blocked.granted and waiting.pid = any(pg_blocking_pids(blocked.pid))
			)
			select count(*) from waiting
		`).Scan(&count); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if count >= want || time.Now().After(deadline) {
			return count
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settlementOwed reports whether the document's pending-settlement row is there.
func settlementOwed(t *testing.T, database *store.Store, artifactID string) bool {
	t.Helper()
	var owed bool
	if err := database.Pool.QueryRow(context.Background(),
		`select exists (select 1 from doc_settlements_pending where artifact_id = $1)`, artifactID,
	).Scan(&owed); err != nil {
		t.Fatalf("read the pending settlement: %v", err)
	}
	return owed
}

// waitForSettlementOwed waits up to 10 s for the document's pending-settlement row to be there, or
// gone.
func waitForSettlementOwed(t *testing.T, database *store.Store, artifactID string, owed bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for settlementOwed(t, database, artifactID) != owed {
		if time.Now().After(deadline) {
			t.Fatalf("the document's pending-settlement row was not %s within 10s", map[bool]string{true: "there", false: "gone"}[owed])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// latestVersion is the number, markdown and authors (JSON) of the document's latest version.
func latestVersion(t *testing.T, database *store.Store, artifactID string) (int, string, string) {
	t.Helper()
	var number int
	var markdown, authors string
	if err := database.Pool.QueryRow(context.Background(), `
		select number, coalesce(markdown, ''), authors::text
		from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&number, &markdown, &authors); err != nil {
		t.Fatalf("read the document's latest version: %v", err)
	}
	return number, markdown, authors
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
