package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// promptShutdown bounds a SIGTERM's whole ordered shutdown with a dashboard open. Ending the
// streams, finishing the requests in flight and settling one small document take milliseconds; a
// stream that held the HTTP drain open would cost httpDrainBeforeDocuments, fifteen seconds, alone.
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
	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)

	streams := map[string]<-chan streamEnd{
		"event stream":              process.openStream(t, "/api/v1/events"),
		"agent conversation stream": process.openStream(t, "/api/v1/agents/"+streamedSessionID+"/stream"),
	}
	process.editOwingSettlement(t, database, artifactID)

	signalled := process.Terminate(t)
	process.waitPromptExit(t, signalled)
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
	if output := process.Output.String(); strings.Contains(output, `msg="dispatch: shutdown"`) ||
		strings.Contains(output, "requests still in flight") {
		t.Errorf("HTTP shutdown did not drain before the document service started")
	}
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A request in flight can hold the HTTP drain for its whole share of the budget, here a title change
// queued behind another writer's lock on the issue. The document service starts once that share
// ends, so the settlement owed by the edit before the signal still runs and commits. Settlement waits
// on the same lock: the edit's own timer starts one two seconds in, during the drain, and Shutdown
// starts another. The lock is released once Shutdown's is queued behind it too, or two seconds after
// the document service started, which is ample for it to read what is owed; a release before that
// read could let the timer's settlement commit first.
func TestSIGTERMWhileARequestHoldsHTTPShutdownStillSettlesTheOwedDocument(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	process.waitHealthy(t)
	key, artifactID := process.createIssue(t)
	process.editOwingSettlement(t, database, artifactID)

	holder := lockIssue(t, database, key)
	go func() {
		// Its answer is not this test's.
		_, _ = process.renameIssue(key, "Renamed")
	}()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, 10*time.Second); waiting < 1 {
		t.Fatalf("the title change never queued behind the issue lock:\n%s", process.Output.String())
	}

	signalled := process.Terminate(t)
	process.WaitForOutput(t, `msg="dispatch: settle documents with requests still in flight"`)
	t.Logf("the document service started with the request in flight %v after SIGTERM", time.Since(signalled))
	waiting := storetest.WaitForLockWaiters(t, holder, 3, 2*time.Second)
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	process.WaitExit(t, "SIGTERM")
	t.Logf("released the issue lock with %d waiting on it; exited %v after SIGTERM", waiting, time.Since(signalled))
	process.checkSettledAtShutdown(t, database, artifactID)
}

// A write in flight at SIGTERM can wait on a lock past the HTTP drain's share of the budget and the
// document service's run, here a title change behind another writer that holds the issue for
// seventeen seconds after the signal. The database is answering, so Dispatch keeps the request's
// connection open until the write commits and its answer is sent, and only then closes the pool.
func TestSIGTERMWhileAWriteWaitsOnALockCommitsItAndAnswersBeforeExit(t *testing.T) {
	const lockHeldAfterSIGTERM = 17 * time.Second
	database := storetest.Open(t)
	process := startDispatchProcess(t, database.Pool.Config().ConnString())
	process.waitHealthy(t)
	key, artifactID := process.createIssue(t)
	// The issue's spec settles on its own timer, which would queue behind the lock too.
	waitForSettlementOwed(t, database, artifactID, false)

	holder := lockIssue(t, database, key)
	type answer struct {
		status int
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		status, err := process.renameIssue(key, "Committed after the lock")
		answered <- answer{status, err}
	}()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, 10*time.Second); waiting < 1 {
		t.Fatalf("the title change never queued behind the issue lock:\n%s", process.Output.String())
	}

	signalled := process.Terminate(t)
	select {
	case <-process.Exited:
		t.Errorf("Dispatch exited %v after SIGTERM, with the write still waiting on the lock", time.Since(signalled))
	case <-time.After(time.Until(signalled.Add(lockHeldAfterSIGTERM))):
	}
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the issue lock: %v", err)
	}
	process.WaitExit(t, "SIGTERM")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	select {
	case got := <-answered:
		if got.err != nil || got.status != http.StatusOK {
			t.Errorf("the title change answered %d (%v), want 200", got.status, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("the title change had no answer 5s after Dispatch exited")
	}
	var title string
	if err := database.Pool.QueryRow(context.Background(), `select title from issues where key = $1`, key).Scan(&title); err != nil {
		t.Fatalf("read the issue's title: %v", err)
	}
	if title != "Committed after the lock" {
		t.Errorf("the issue's title is %q, want the write that waited on the lock, %q", title, "Committed after the lock")
	}
	if code := process.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("Dispatch exited %d after SIGTERM, want 0", code)
	}
	if t.Failed() {
		t.Logf("Dispatch's output:\n%s", process.Output.String())
	}
}

// lockIssue holds the issue's row in a transaction of the test's own, as another writer would,
// and rolls it back when the test ends if the test has not.
func lockIssue(t *testing.T, database *store.Store, key string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lock holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, key); err != nil {
		t.Fatalf("lock the issue: %v", err)
	}
	return holder
}

// renameIssue changes the issue's title as alice and returns the status Dispatch answered.
func (p *dispatchProcess) renameIssue(key, title string) (int, error) {
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequest(http.MethodPatch, p.url("/api/v1/issues/"+key), strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dispatchTestLogin)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

// A deploy usually finds someone with a document open: the spec tab holds the document's websocket,
// so its room is loaded with an editor connected. The test gives its test-only process a longer
// settle delay than its whole shutdown budget, so only Shutdown can version the edit.
func TestSIGTERMWithAnEditorConnectedSettlesItsDocumentBeforeExit(t *testing.T) {
	database := storetest.Open(t)
	process := startDispatchProcessWithSettleDelay(t, database.Pool.Config().ConnString(), 5*time.Minute)
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)
	editor := process.openEditor(t, artifactID, "before\n")
	// The initial spec is already owed. Its row makes the Shutdown settlement load and settle this
	// room, so wait for the editor's durable update to advance that same row before signalling.
	waitForSettlementOwed(t, database, artifactID, true)
	marked := settlementMarkedAt(t, database, artifactID)
	replaceText(t, editor, "before", "after")
	waitForSettlementMarkedAfter(t, database, artifactID, marked)

	signalled := process.Terminate(t)
	process.WaitExit(t, "SIGTERM")
	t.Logf("exited %v after SIGTERM", time.Since(signalled))
	select {
	case <-editor.Ended:
		t.Logf("the editor's connection ended %v after SIGTERM", editor.EndedAt().Sub(signalled))
	case <-time.After(5 * time.Second):
		t.Errorf("the editor's connection was still open 5s after Dispatch exited")
	}
	process.checkSettledAtShutdown(t, database, artifactID)
	number, markdown, authors := latestVersion(t, database, artifactID)
	if number != 2 || markdown != "after\n" || !strings.Contains(authors, `"`+dispatchTestLogin+`"`) {
		t.Errorf("the document's latest version is %d holding %q by %s, want version 2 holding the editor's edit, %q, credited to %s",
			number, markdown, authors, "after\n", dispatchTestLogin)
	}
	for _, line := range strings.Split(process.Output.String(), "\n") {
		if strings.Contains(line, "room="+artifactID) {
			t.Logf("Dispatch logged: %s", line)
		}
	}
}

// A database that stops answering - a failover, a partition - holds every connection waiting on it,
// and closing the pool waits for every connection in use. Once the document service has stopped,
// Dispatch gives up on them as soon as its health probe finds the database silent, so it exits well
// inside a runtime's grace period instead of spending the rest of its budget or being killed.
func TestSIGTERMWithTheDatabaseUnansweringExitsWithinTheShutdownBudget(t *testing.T) {
	database := storetest.Open(t)
	relay := startDatabaseRelay(t, database.Pool.Config().ConnString())
	process := startDispatchProcess(t, relay.url)
	process.waitHealthy(t)
	_, artifactID := process.createIssue(t)
	editor := process.openEditor(t, artifactID, "before\n")
	replaceText(t, editor, "before", "after")
	waitForRoom(t, editor)

	relay.freeze()
	signalled := process.Terminate(t)
	process.WaitExit(t, "SIGTERM with the database unanswering")
	exited := time.Since(signalled)
	t.Logf("exited %v after SIGTERM", exited)
	// Nothing is in flight over HTTP, so the document service starts at once; then one health
	// probe, bounded at two seconds, finds the database silent.
	if bound := documentShutdownTimeout + 5*time.Second; exited > bound {
		t.Errorf("Dispatch exited %v after SIGTERM with the database unanswering, want within %v", exited, bound)
	}
	if !strings.Contains(process.Output.String(), "once the database stopped answering") {
		t.Errorf("Dispatch did not say it exited because the database stopped answering")
	}
	for _, line := range strings.Split(process.Output.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			t.Logf("Dispatch logged: %s", line)
		}
	}
}

// waitForRoom returns once the room has answered a request the editor sent after its edit: the
// room handles one connection's messages in order, so it has applied the edit by then.
func waitForRoom(t *testing.T, editor *docstest.Peer) {
	t.Helper()
	for len(editor.Answers) > 0 {
		<-editor.Answers
	}
	if err := editor.AskForDocument(); err != nil {
		t.Fatalf("ask the room for the document: %v", err)
	}
	select {
	case <-editor.Answers:
	case <-editor.Ended:
		t.Fatal("the editor's connection closed before the room answered")
	case <-time.After(10 * time.Second):
		t.Fatal("the room never answered the editor")
	}
}

// databaseRelay forwards Dispatch's database connections to the test's Postgres until frozen, and
// then forwards nothing more in either direction while holding every connection open, as a
// database that has stopped answering does.
type databaseRelay struct {
	url    string
	frozen chan struct{}
	once   sync.Once
}

func (r *databaseRelay) freeze() { r.once.Do(func() { close(r.frozen) }) }

// startDatabaseRelay starts a relay to the database databaseURL names and returns it with url, the
// same URL through the relay.
func startDatabaseRelay(t *testing.T, databaseURL string) *databaseRelay {
	t.Helper()
	target, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the database relay: %v", err)
	}
	relay := &databaseRelay{frozen: make(chan struct{})}
	done := make(chan struct{})
	var conns sync.WaitGroup
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		conns.Wait()
	})
	upstream := target.Host
	through := *target
	through.Host = listener.Addr().String()
	relay.url = through.String()
	forward := func(to, from net.Conn) {
		buffer := make([]byte, 32<<10)
		for {
			n, err := from.Read(buffer)
			select {
			case <-relay.frozen:
				<-done
				return
			default:
			}
			if n > 0 {
				if _, writeErr := to.Write(buffer[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = client.Close()
				continue
			}
			conns.Add(2)
			go func() {
				defer conns.Done()
				forward(server, client)
			}()
			go func() {
				defer conns.Done()
				forward(client, server)
			}()
			go func() {
				<-done
				_ = client.Close()
				_ = server.Close()
			}()
		}
	}()
	return relay
}

// checkSettledAtShutdown requires the exited process to have stopped in order with the document's
// owed settlement run and confirmed.
func (p *dispatchProcess) checkSettledAtShutdown(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	output := p.Output.String()
	if code := p.Cmd.ProcessState.ExitCode(); code != 0 {
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

// dispatchProcess is a Dispatch binary a test started, serving on port.
type dispatchProcess struct {
	*cmdtest.Process
	port int
}

// startDispatchProcess builds Dispatch and starts it on a free loopback port against databaseURL,
// with no NATS and nothing from the caller's environment or home, and kills it when the test ends if
// it still runs. DISPATCH_TEST_HOOKS serves the agent conversation relay in-process, as the browser
// harness does, since there is no NATS to relay it from.
func startDispatchProcess(t *testing.T, databaseURL string) *dispatchProcess {
	return startDispatchProcessWithSettleDelay(t, databaseURL, 0)
}

func startDispatchProcessWithSettleDelay(t *testing.T, databaseURL string, settleDelay time.Duration) *dispatchProcess {
	t.Helper()
	home := t.TempDir()
	port := cmdtest.FreeTCPPort(t)
	cmd := exec.Command(cmdtest.Build(t, "envoy-dispatch"))
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_AGENT_TOKEN=shutdown-test-token",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_ALLOWED_LOGINS=" + dispatchTestLogin,
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + strconv.Itoa(port),
		"DISPATCH_WEB_DIST=" + home,
		"DISPATCH_TEST_HOOKS=1",
	}
	if settleDelay > 0 {
		cmd.Env = append(cmd.Env, "DISPATCH_TEST_SETTLE_DELAY="+settleDelay.String())
	}
	return &dispatchProcess{Process: cmdtest.Start(t, "Dispatch", cmd), port: port}
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
		case <-p.Exited:
			t.Fatalf("Dispatch exited before it became healthy:\n%s", p.Output.String())
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("Dispatch never became healthy:\n%s", p.Output.String())
}

// waitPromptExit waits for Dispatch to exit after the SIGTERM sent at signalled, and requires it to
// have exited within promptShutdown.
func (p *dispatchProcess) waitPromptExit(t *testing.T, signalled time.Time) {
	t.Helper()
	p.WaitExit(t, "SIGTERM")
	exited := time.Since(signalled)
	t.Logf("exited %v after SIGTERM", exited)
	if exited >= promptShutdown {
		t.Errorf("Dispatch exited %v after SIGTERM, want under %v", exited, promptShutdown)
	}
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
		t.Fatalf("the edit left no pending-settlement row, so the signal would find nothing owed:\n%s", p.Output.String())
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

// openEditor connects a signed-in spec tab to the document at the schema version the dashboard
// presents, so the room takes its edits, and returns once the editor's copy renders want.
func (p *dispatchProcess) openEditor(t *testing.T, artifactID, want string) *docstest.Peer {
	t.Helper()
	url := "ws://127.0.0.1:" + strconv.Itoa(p.port) + "/ws/doc/" + artifactID + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	editor := docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{dispatchTestLogin}}, artifactID, crdt.New())
	deadline := time.After(10 * time.Second)
	for markdown(editor) != want {
		if err := editor.AskForDocument(); err != nil {
			t.Fatalf("ask the room for the document: %v", err)
		}
		select {
		case <-editor.Answers:
		case <-editor.Ended:
			t.Fatalf("the editor's connection closed before the room sent the document:\n%s", p.Output.String())
		case <-deadline:
			t.Fatalf("the room never sent the editor the document:\n%s", p.Output.String())
		}
	}
	return editor
}

// markdown renders the editor's copy of the document, read under its lock while the room's updates
// apply, or "" while the copy holds no document it can render.
func markdown(editor *docstest.Peer) string {
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var rendered string
	editor.Doc.Transact(func(txn *crdt.Transaction) {
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
func replaceText(t *testing.T, editor *docstest.Peer, old, replacement string) {
	t.Helper()
	fragment := editor.Doc.GetXmlFragment("prosemirror")
	var changeErr error
	if _, err := editor.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != old {
			changeErr = errors.New("the document's first paragraph does not hold only " + old)
			return
		}
		tree.Children[0].Children[0].Text = replacement
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil || changeErr != nil {
		t.Fatalf("the editor's edit: %v %v", err, changeErr)
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

func settlementMarkedAt(t *testing.T, database *store.Store, artifactID string) time.Time {
	t.Helper()
	var marked time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select marked_at from doc_settlements_pending where artifact_id = $1`, artifactID,
	).Scan(&marked); err != nil {
		t.Fatalf("read the pending settlement marker: %v", err)
	}
	return marked
}

func waitForSettlementMarkedAfter(t *testing.T, database *store.Store, artifactID string, before time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !settlementMarkedAt(t, database, artifactID).After(before) {
		if time.Now().After(deadline) {
			t.Fatalf("the editor's durable update did not advance the pending settlement marker within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
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
