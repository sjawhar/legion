package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestOutboxMessageCrashRecoveryPostsOnceAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, record.MessagePost{Body: "The durable message."}, now))

	first := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	if remaining := outboxRows(t, pool); remaining != 1 {
		t.Fatalf("outbox rows after crash = %d, want leased unfinished row", remaining)
	}

	restarted := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want finished row", remaining)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	markers := 0
	for _, body := range bodies {
		if strings.Contains(body, "<!-- legion-outbox:") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("outbox-marked messages = %d in %#v, want one post across crash recovery", markers, bodies)
	}
}

// A long READY packet posts whole and reads back byte for byte. Dispatch bounds a message body
// block by block, so a 40,000-unit packet of ordinary paragraphs is taken; its message.created
// event carries only the first part of it and says so (body_truncated), which leaves the outbox's
// marker off the end of the event's body. The runner reads such a body from the message itself,
// so the row run again after a crash before its finish finds its own marker and posts nothing,
// and the issue holds exactly one message.
func TestOutboxPostsALongPacketWholeAndOnceAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	// Six paragraphs of 6,665 characters, 40,000 units in all: every block is far under
	// Dispatch's own block bound and the body is far over the units one event carries.
	packet := record.MessagePost{Body: strings.TrimSuffix(strings.Repeat(strings.Repeat("x", 6665)+"\n\n", 6), "\n\n")}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, packet, now))
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox").Scan(&id); err != nil {
		t.Fatalf("read the queued row: %v", err)
	}

	first := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: crashAfterPostStore{Store: records}, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := first.RunOnce(context.Background()); err != nil {
		t.Fatalf("first delivery = %v; a failed finish leaves the row leased and is not the tick's error", err)
	}
	restarted := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now.Add(outboxLease + time.Second) },
	}
	if err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows after recovery = %d, want the row posted and finished", remaining)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if want := packet.Posted(id); len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("Dispatch messages = %d of %d characters, want the 40,000-character packet posted once, whole, under row %d's marker",
			len(bodies), len(strings.Join(bodies, "")), id)
	}
	cut, truncated := messageCreatedEvent(t, baseURL, token, issue)
	if !truncated || strings.Contains(cut, record.MessageMarker(id)) {
		t.Fatalf("the message.created event carries %d characters (truncated=%v), want a cut body with no marker in it", len(cut), truncated)
	}
}

// A post Dispatch refuses the same way on every attempt — one block of the body over Dispatch's
// own block bound — is no retry's to repair. The row finishes and its issue's architect is told
// Dispatch's own refusal, rather than the row climbing its backoff for ever and telling nobody.
func TestOutboxFinishesAPostDispatchRefusesAndTellsTheArchitectAgainstScratchDispatch(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := createScratchDispatchIssue(t, baseURL)
	now := time.Now().UTC()
	over := record.MessagePost{Body: "READY #42 at head for " + issue + "\n\n" + strings.Repeat("x", 32001)}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue, over, now))
	runner := &outbox{
		dispatchProject: "LEGION", log: quietLogger(),
		pool: pool, records: records, dispatch: dispatch.New(baseURL, token),
		now: func() time.Time { return now },
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	bodies, err := dispatch.New(baseURL, token).MessageBodiesSince(context.Background(), issue, now.Add(-time.Second))
	if err != nil {
		t.Fatalf("read real Dispatch messages: %v", err)
	}
	if len(bodies) != 0 {
		t.Fatalf("Dispatch messages = %#v, want the refused post on no issue", bodies)
	}
	var kind string
	var payload []byte
	if err := pool.QueryRow(context.Background(), "select kind, payload from outbox").Scan(&kind, &payload); err != nil {
		t.Fatalf("read the row left behind: %v", err)
	}
	if kind != string(record.OutboxKindNotice) {
		t.Fatalf("the one row left is a %s row, want the refused post finished and its architect's notice in its place", kind)
	}
	var notice record.Notice
	if err := json.Unmarshal(payload, &notice); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if notice.Kind != "message-refused" || notice.Role != claim.RoleArchitect ||
		!strings.Contains(notice.Reason, issue) || !strings.Contains(notice.Reason, "CAP_EXCEEDED") ||
		!strings.Contains(notice.Reason, "READY #42 at head for "+issue) {
		t.Fatalf("the notice = %+v, want message-refused to the architect naming %s, the message's first line and Dispatch's refusal", notice, issue)
	}
}

// messageCreatedEvent is the issue's message.created event as Dispatch serves it: the body the
// event carries, and whether Dispatch says that body is only the first part of the message's.
func messageCreatedEvent(t *testing.T, baseURL, token, issue string) (string, bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/issues/"+url.PathEscape(issue)+"/events?limit=200", nil)
	if err != nil {
		t.Fatalf("create the events request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET the issue's events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET the issue's events = %s", response.Status)
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			Body          string `json:"body"`
			BodyTruncated bool   `json:"body_truncated"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		t.Fatalf("decode the issue's events: %v", err)
	}
	for _, event := range events {
		if event.Type == "message.created" {
			return event.Payload.Body, event.Payload.BodyTruncated
		}
	}
	t.Fatal("the issue has no message.created event")
	return "", false
}

type crashAfterPostStore struct{ record.Store }

func (crashAfterPostStore) FinishOutbox(context.Context, pgx.Tx, int64, string) error {
	return errors.New("simulated crash before finish")
}

// startScratchDispatch starts the real Dispatch server against a scratch Postgres database of its
// own with no NATS at all, and returns its base URL and agent token. Nothing it does reaches a
// bus: a test that only drives the outbox against it wants no events.
func startScratchDispatch(t *testing.T) (string, string) {
	t.Helper()
	d := startScratchDispatchOn(t, scratchDispatchOptions{})
	return d.base, d.token
}

// startScratchDispatchWithNATS is startScratchDispatch with its events turned on: the same server,
// publishing every issue event on natsURL, which is the NATS server the test holds
// (testnats.URL). Dispatch reconciles the ENVOY_NOTIFICATIONS stream there on its first
// connection, which it makes in the background once it starts, so the caller creates none — and
// must take the NATS server before this call, since testnats.URL empties it of every stream.
func startScratchDispatchWithNATS(t *testing.T, natsURL string) (string, string) {
	t.Helper()
	if natsURL == "" {
		t.Fatal("startScratchDispatchWithNATS was given no NATS server; use startScratchDispatch for a server with no events")
	}
	d := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL})
	return d.base, d.token
}

// scratchDispatchOptions are what a scratch Dispatch server's callers vary.
type scratchDispatchOptions struct {
	// natsURL is the NATS server the server publishes its events on; empty runs it with NATS off.
	natsURL string
	// root is the checkout whose packages/envoy the server is built from; empty is this one, four
	// directories above the package's.
	root string
	// log, when set, receives the server's output as well.
	log io.Writer
}

// scratchDispatch is a real Dispatch server a test builds and runs: its binary, its own database,
// the port it serves on (kept across a restart, since whatever the test pointed at the server
// names it), and the running process. stop and start take it down and bring it back on the same
// port and database; the test's end stops it and drops the database.
type scratchDispatch struct {
	t        *testing.T
	binary   string
	dir      string
	webDist  string
	database string
	natsURL  string
	token    string
	home     string
	log      io.Writer
	port     int
	base     string
	command  *exec.Cmd
	exited   chan error
	output   *syncBuffer
}

// startScratchDispatchOn creates the server's scratch database on the test DSN's Postgres, builds
// cmd/dispatch, and starts it.
func startScratchDispatchOn(t *testing.T, options scratchDispatchOptions) *scratchDispatch {
	t.Helper()
	ctx := context.Background()
	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("open Postgres admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	database := "dispatch_scratch_test_" + outboxSuffix(t)
	// A file copy of template1 (docs/testing.md, "A database a Go test creates").
	if _, err := admin.Exec(ctx, "create database "+database+" strategy file_copy"); err != nil {
		t.Fatalf("create scratch Dispatch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+database+" with (force)"); err != nil {
			t.Errorf("drop scratch Dispatch database: %v", err)
		}
	})
	serverURL := *base
	serverURL.Path = "/" + database
	root := options.root
	if root == "" {
		root = filepath.Join("..", "..", "..", "..")
	}
	if root, err = filepath.Abs(root); err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	d := &scratchDispatch{
		t: t, binary: filepath.Join(t.TempDir(), "dispatch"), dir: filepath.Join(root, "packages", "envoy"),
		webDist: filepath.Join(root, "packages", "dispatch", "web", "dist"), database: serverURL.String(),
		natsURL: options.natsURL, token: scratchAgentToken, home: filepath.Join(t.TempDir(), "home"),
		log: options.log,
	}
	build := exec.Command("go", "build", "-o", d.binary, "./cmd/dispatch")
	build.Dir = d.dir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scratch Dispatch: %v\n%s", err, output)
	}
	if err := os.MkdirAll(d.home, 0o700); err != nil {
		t.Fatalf("create scratch Dispatch home: %v", err)
	}
	t.Cleanup(d.stop)
	d.start()
	return d
}

// start runs the server and returns once GET /api/v1 answers 200. The first start serves on a port
// the kernel chooses; a restart serves on the same one again.
func (d *scratchDispatch) start() {
	t := d.t
	t.Helper()
	// Readiness is a server process starting and migrating a fresh database, whose time is the
	// machine's load: bounded by the test binary's own deadline rather than a fixed span, so a
	// slow start is not read as a server that will never answer.
	deadline := time.Now().Add(2 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-10*time.Second).Before(deadline) {
		deadline = testDeadline.Add(-10 * time.Second)
	}
	// Dispatch serves on a socket held here (DISPATCH_LISTEN_FD), so no other process can take the
	// port before Dispatch serves it: the first start on a port the kernel chose (heldSocket), a
	// restart on the port it served before (holdPortAgain).
	var held *os.File
	if d.port == 0 {
		held, d.port = heldSocket(t)
		d.base = fmt.Sprintf("http://127.0.0.1:%d", d.port)
	} else {
		held = d.holdPortAgain(deadline)
	}
	command := exec.Command(d.binary)
	command.Dir = d.dir
	// Nothing of the caller's Dispatch, Envoy or NATS settings reaches the scratch server, as
	// run-server.sh drops them: a Legion pane exports DISPATCH_URL and friends.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DISPATCH_") && !strings.HasPrefix(entry, "ENVOY_") && !strings.HasPrefix(entry, "NATS_") && !strings.HasPrefix(entry, "HOME=") {
			command.Env = append(command.Env, entry)
		}
	}
	// The bus is the one difference between the two forms: NATS off, or the test's own server,
	// whose ENVOY_NOTIFICATIONS stream this process reconciles as it starts.
	bus := "DISPATCH_NATS_DISABLED=1"
	if d.natsURL != "" {
		bus = "NATS_URLS=" + d.natsURL
	}
	command.Env = append(command.Env,
		"DATABASE_URL="+d.database,
		"DISPATCH_AGENT_TOKEN="+d.token,
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		bus,
		// ExtraFiles' first entry is descriptor 3 in Dispatch.
		"DISPATCH_LISTEN_FD=3",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		fmt.Sprintf("DISPATCH_PORT=%d", d.port),
		"DISPATCH_SERVER_URL="+d.base,
		"DISPATCH_WEB_DIST="+d.webDist,
		"HOME="+d.home,
	)
	command.ExtraFiles = []*os.File{held}
	output := &syncBuffer{}
	command.Stdout = output
	if d.log != nil {
		command.Stdout = io.MultiWriter(output, d.log)
	}
	command.Stderr = command.Stdout
	// From here Dispatch's is the only copy of the socket, so a Dispatch that exits releases the
	// port and nothing of the test's answers on it.
	err := command.Start()
	_ = held.Close()
	if err != nil {
		t.Fatalf("start scratch Dispatch: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	d.command, d.exited, d.output = command, exited, output
	for {
		select {
		case err := <-exited:
			d.command = nil
			t.Fatalf("scratch Dispatch exited before it was ready (%v):\n%s", err, output.String())
		default:
		}
		// Each poll is bounded (pollClient): until Dispatch serves, the held socket queues the
		// connection unanswered.
		if response, err := pollClient.Get(d.base + "/api/v1"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("scratch Dispatch did not become ready:\n%s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holdPortAgain takes the socket of the port the server served on before its stop (takeSocket), as
// heldSocket takes one of a port the kernel chooses. From the stop until here the port is any
// process's, so a run that finds it still held by another process (EADDRINUSE) binds it again
// until deadline. Every other failure of takeSocket, such as the descriptor the listener could not
// give, is this run's own and fails the test at once: binding again would never clear it.
func (d *scratchDispatch) holdPortAgain(deadline time.Time) *os.File {
	d.t.Helper()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(d.port))
	for {
		file, _, err := takeSocket(address)
		switch {
		case err == nil:
			return file
		case !errors.Is(err, syscall.EADDRINUSE):
			d.t.Fatalf("hold scratch Dispatch's port %d again: %v", d.port, err)
		case time.Now().After(deadline):
			d.t.Fatalf("scratch Dispatch's port %d was still taken at the deadline: %v", d.port, err)
		}
		d.t.Logf("scratch Dispatch's port %d is still taken (%v); binding it again", d.port, err)
		time.Sleep(500 * time.Millisecond)
	}
}

// stop interrupts the running server and waits for it to exit, killing it after ten seconds. A
// server that is not running is left as it is.
func (d *scratchDispatch) stop() {
	if d.command == nil {
		return
	}
	command := d.command
	d.command = nil
	_ = command.Process.Signal(os.Interrupt)
	select {
	case err := <-d.exited:
		if err != nil {
			d.t.Errorf("scratch Dispatch stopped: %v\n%s", err, d.output.String())
		}
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		<-d.exited
		d.t.Error("scratch Dispatch did not stop after SIGINT")
	}
}

func createScratchDispatchIssue(t *testing.T, baseURL string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	request := func(path string, body any) *http.Response {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s: %v", path, err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("create %s request: %v", path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dispatch-User", "smoke")
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return response
	}
	project := request("/api/v1/projects", map[string]string{"key": "LEGION", "name": "Scratch Legion"})
	if project.StatusCode != http.StatusCreated {
		project.Body.Close()
		t.Fatalf("create scratch project = %s", project.Status)
	}
	project.Body.Close()
	issue := request("/api/v1/issues", map[string]string{"project": "LEGION", "title": "Crash-safe outbox message"})
	defer issue.Body.Close()
	if issue.StatusCode != http.StatusCreated {
		t.Fatalf("create scratch issue = %s", issue.Status)
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(issue.Body).Decode(&created); err != nil {
		t.Fatalf("decode scratch issue: %v", err)
	}
	if created.Key == "" {
		t.Fatal("scratch Dispatch created an issue without a key")
	}
	return created.Key
}
