//go:build memory

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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
	// env is added to every server's environment.
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
	// database is the harness's own handle on the database this server serves, which a measured
	// window takes the block-index lease on (holdBlockIndexLease).
	database *store.Store
}

// start starts a Dispatch server on a loopback port the kernel chose and the test holds from that
// moment, which the server inherits as DISPATCH_LISTEN_FD and serves on, so no other program can
// take the port before it serves.
func (h *memoryHarness) start(t *testing.T) *memoryServer {
	t.Helper()
	held := cmdtest.HoldPort(t)
	port := strconv.Itoa(held.Port)
	h.drainBeforeStart(t, port)
	command := exec.Command(os.Args[0])
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		memoryTestServeEnv + "=1",
		"DATABASE_URL=" + h.database.Pool.Config().ConnString(),
		"DISPATCH_AGENT_TOKEN=" + memoryTestToken,
		"DISPATCH_SIGNING_KEY=memory-test-signing-key",
		"DISPATCH_IDENTITY=header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED=1",
		"DISPATCH_NATS_DISABLED=1",
		"DISPATCH_LISTEN_HOST=127.0.0.1",
		"DISPATCH_PORT=" + port,
		"DISPATCH_WEB_DIST=" + t.TempDir(),
		"DISPATCH_SERVER_URL=http://127.0.0.1:" + port,
		// Every measured server collects at GOGC=25, so a peak above idle reads the memory a
		// request holds plus the collector's margin at that setting, not the resident memory
		// production shows at Go's default GOGC=100, where the collector lets the heap grow to
		// twice the live set before it collects and an edit can raise resident memory by up to
		// about twice what it holds. A test that needs another value sets GOGC in
		// memoryHarness.env, which is appended after this and wins.
		"GOGC=25",
	}
	command.Env = append(command.Env, h.env...)
	held.Pass(command, "DISPATCH_LISTEN_FD")
	server := &memoryServer{command: command, base: "http://127.0.0.1:" + port, output: &lockedBuffer{}, exited: make(chan struct{}), database: h.database}
	command.Stdout, command.Stderr = server.output, server.output
	err := command.Start()
	// The server holds its own copy of the socket from here on, so it releases the port when it
	// exits.
	cmdtest.CloseExtraFiles(command)
	if err != nil {
		t.Fatalf("start a Dispatch server: %v", err)
	}
	go func() {
		_ = command.Wait()
		close(server.exited)
	}()
	t.Cleanup(func() { server.stop(t) })
	deadline := time.Now().Add(time.Minute)
	// The kernel queues a request on the held socket until the server serves it, so each one is
	// bounded by the start's own deadline.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.base+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := http.DefaultClient.Do(request); err == nil {
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

// owedBeforeStart is what the test's database owes when a server is about to start: the documents
// owed a block index, the items owed an index with the block grammar, the comments, messages and
// asks owed a block index of their own blocks, the rows of the re-index queue refs_grammar_owed -
// a comment, an ask or an issue message owed an index with this build's whole grammar, a document
// owed that index for the documents batch to write, or one document's whole scope - and the
// documents owed a settlement.
type owedBeforeStart struct {
	documents   int
	items       int
	itemBlocks  int
	texts       int
	settlements int
}

// readOwed reads in one statement everything a starting server would find owed.
func (h *memoryHarness) readOwed(t *testing.T) owedBeforeStart {
	t.Helper()
	var owed owedBeforeStart
	if err := h.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from document_block_versions where version is null),
		       (select count(*) from block_grammar_owed),
		       (select count(*) from comment_block_state where owed_at is not null)
		         + (select count(*) from message_block_state where owed_at is not null)
		         + (select count(*) from ask_block_state where owed_at is not null),
		       (select count(*) from refs_grammar_owed),
		       (select count(*) from doc_settlements_pending)
	`).Scan(&owed.documents, &owed.items, &owed.itemBlocks, &owed.texts, &owed.settlements); err != nil {
		t.Fatalf("read what a starting server would find owed: %v", err)
	}
	return owed
}

// drainBeforeStart leaves the database owing nothing the server about to start would do inside a
// measured window. A fresh server arms the settlement of every row older than a minute at start
// and every minute after (docs.SettlementResumeInterval and settlementResumeAge) and indexes the
// blocks of every document owed one, both of which read whole documents: left owed, either runs
// inside the next window, and the peak measured there is that work's memory, not the request's.
//
// It does that work itself rather than waiting for it. Between one server's stop and the next
// server's start no process in this test is settling anything, so a settlement a write left on a
// server that has since stopped would stand for ever: a guard that waited for such a row to
// settle would wait out its whole timeout and fail, which is how every memory test of this
// package went red on CI (INFRA-985, run 42013424869).
func (h *memoryHarness) drainBeforeStart(t *testing.T, port string) {
	t.Helper()
	owed := h.readOwed(t)
	if total := owed.settlements + owed.documents + owed.items + owed.itemBlocks + owed.texts; total == 0 {
		t.Logf("block index before start (%s): indexed %d documents, %d items, %d item blocks, %d texts; owed %d",
			t.Name(), 0, 0, 0, 0, total)
		return
	}
	// One service for both pieces of work, shut down before the server starts, so neither its
	// compaction queue nor a lease of its own outlives this step. ServerURL is the port the test
	// already holds, the one refs.ReindexOwed hands text.ExtractAt, so the index this writes
	// is the index the server would have written.
	service := docs.New(docs.Deps{Store: h.database, Events: events.NewBroker(), ServerURL: "http://127.0.0.1:" + port})
	defer service.Shutdown(context.Background())
	if owed.settlements > 0 {
		h.settleOwed(t, service)
	}
	var documents, items, itemBlocks, texts int
	if owed.documents+owed.items+owed.itemBlocks+owed.texts > 0 {
		documents, items, itemBlocks, texts = h.writeOwedBlockIndex(t, service)
	}
	// What the line reports is a reading taken after the work, not an assumption that the work
	// cleared everything: a row still owed here is one nothing in this process can clear, and it
	// would be measured inside the window as this guard exists to prevent.
	left := h.readOwed(t)
	remaining := left.settlements + left.documents + left.items + left.itemBlocks + left.texts
	if remaining != 0 {
		t.Fatalf("after the drain (%s) the database still owes %d documents a block index, %d items, %d item blocks, %d texts and %d settlements, which nothing in this process can clear",
			t.Name(), left.documents, left.items, left.itemBlocks, left.texts, left.settlements)
	}
	t.Logf("block index before start (%s): indexed %d documents, %d items, %d item blocks, %d texts; owed %d",
		t.Name(), documents, items, itemBlocks, texts, remaining)
}

// settleOwed settles, on this goroutine, every document owed a settlement, by the issue each
// belongs to: SettleIssueDocuments is the only exported synchronous settle, and it returns when
// the settlements it ran have committed. A row still owed after it is one nothing here can clear,
// and the failure names it.
func (h *memoryHarness) settleOwed(t *testing.T, service *docs.Service) {
	t.Helper()
	ctx := context.Background()
	rows, err := h.database.Pool.Query(ctx, `
		select distinct coalesce(artifacts.issue_key, '')
		from doc_settlements_pending pending join artifacts on artifacts.id = pending.artifact_id
	`)
	if err != nil {
		t.Fatalf("read the issues of the documents owed a settlement: %v", err)
	}
	var issues []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			t.Fatalf("read the issue of a document owed a settlement: %v", err)
		}
		if key != "" {
			issues = append(issues, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issues of the documents owed a settlement: %v", err)
	}
	for _, issue := range issues {
		if err := service.SettleIssueDocuments(ctx, issue); err != nil {
			t.Fatalf("settle the documents of issue %s before a measured server starts: %v", issue, err)
		}
	}
	if pending := h.pendingSettlements(t); len(pending) > 0 {
		t.Fatalf("a settlement was still owed after this harness settled every issue that owed one (%s); a document that belongs to no issue cannot be settled from here",
			strings.Join(pending, "; "))
	}
}

// pendingSettlements names each document owed a settlement, with the marked_at of its row.
func (h *memoryHarness) pendingSettlements(t *testing.T) []string {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(),
		`select artifact_id::text, marked_at from doc_settlements_pending order by marked_at`)
	if err != nil {
		t.Fatalf("read the documents owed a settlement: %v", err)
	}
	defer rows.Close()
	var pending []string
	for rows.Next() {
		var artifactID string
		var markedAt time.Time
		if err := rows.Scan(&artifactID, &markedAt); err != nil {
			t.Fatalf("read a document owed a settlement: %v", err)
		}
		pending = append(pending, fmt.Sprintf("%s marked at %s", artifactID, markedAt.Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents owed a settlement: %v", err)
	}
	return pending
}

// writeOwedBlockIndex writes, through service, the block index the server would have written, and
// reports what it indexed.
func (h *memoryHarness) writeOwedBlockIndex(t *testing.T, service *docs.Service) (documents, items, itemBlocks, texts int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(120 * time.Second)
	var cursors docs.BlockIndexCursors
	// liveAheadTicks counts the ticks in a row that indexed nothing and found a document live
	// ahead: a document whose stored text is ahead of its latest version is moved on by a
	// settlement, which settleOwed has already run for every issue that owed one, so two such
	// ticks say the drain cannot finish.
	liveAheadTicks := 0
	for {
		tick, err := service.ReconcileBlockIndex(ctx, cursors, nil)
		if err != nil {
			t.Fatalf("write the block index a starting server would write: %v", err)
		}
		cursors = tick.Cursor
		documents += tick.Documents
		items += tick.Items
		itemBlocks += tick.ItemBlocks
		texts += tick.Texts
		indexed := tick.Documents + tick.Items + tick.ItemBlocks + tick.Texts
		if tick.OwedDocuments+tick.OwedItems+tick.OwedItemBlocks+tick.OwedTexts == 0 {
			return documents, items, itemBlocks, texts
		}
		if indexed == 0 && tick.LiveAhead > 0 {
			liveAheadTicks++
			if liveAheadTicks > 1 {
				t.Fatalf("the block index is owed documents whose stored text is ahead of their latest version, which nothing in this test process settles: %s",
					strings.Join(h.owedDocumentNames(t), "; "))
			}
		} else {
			liveAheadTicks = 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("the block index was still owed %d documents, %d items, %d item blocks and %d texts two minutes into the drain: %s",
				tick.OwedDocuments, tick.OwedItems, tick.OwedItemBlocks, tick.OwedTexts, strings.Join(h.owedDocumentNames(t), "; "))
		}
		if indexed == 0 {
			// A document the tick could not index is logged and left owed, so a loop that indexes
			// nothing would otherwise read the whole owed set as fast as the database answers
			// until the deadline. Wait as the settlement poll does, so the drain costs the
			// database no more than ten reads a second while it gets nowhere.
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// owedDocumentNames names each document still owed a block index, with the marked_at of its
// pending settlement where it has one: a document live ahead owes one.
func (h *memoryHarness) owedDocumentNames(t *testing.T) []string {
	t.Helper()
	rows, err := h.database.Pool.Query(context.Background(), `
		select owed.artifact_id::text, pending.marked_at
		from document_block_versions owed
		left join doc_settlements_pending pending on pending.artifact_id = owed.artifact_id
		where owed.version is null
		order by owed.artifact_id
	`)
	if err != nil {
		t.Fatalf("read the documents owed a block index: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var artifactID string
		var markedAt *time.Time
		if err := rows.Scan(&artifactID, &markedAt); err != nil {
			t.Fatalf("read a document owed a block index: %v", err)
		}
		if markedAt == nil {
			names = append(names, artifactID+" owing no settlement")
			continue
		}
		names = append(names, fmt.Sprintf("%s marked at %s", artifactID, markedAt.Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents owed a block index: %v", err)
	}
	return names
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

// resident is the server's resident memory in bytes, VmRSS now or VmHWM its peak.
func (p *memoryServer) resident(t *testing.T, field string) int64 {
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
func (p *memoryServer) peakAboveIdle(t *testing.T, during func()) int64 {
	t.Helper()
	// Every measured window goes through here, so the lease is taken here: from before the window
	// begins to after the peak has been read, which is the whole of what the guard below watches.
	// The release is deferred, so a window whose own failure ends the test gives the lease back on
	// the way out and the rows after it measure their windows too.
	release := p.holdBlockIndexLease(t)
	defer release()
	// Writing 5 to clear_refs resets the peak (VmHWM) to the memory resident now.
	if err := os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", p.command.Process.Pid), []byte("5"), 0); err != nil {
		t.Fatalf("reset the server's peak memory: %v", err)
	}
	written := p.output.len()
	during()
	// A block-index pass reads whole documents of its own, so one inside the window would make the
	// peak that pass's memory rather than the request's. start leaves nothing owed, and a pass
	// that finds nothing owed logs nothing, so a clean window holds no such line. What a drain
	// taken before the server started cannot cover is a window that fills an owed queue itself -
	// any window that stores a document - since that row is owed while the measurement runs and
	// the server's next tick would index it there; the pass is held off for the window's length by
	// its own lease (holdBlockIndexLease) instead, and this guard stays as the proof that it was.
	// The handler is the slog.NewTextHandler main installs with slog.SetDefault in
	// cmd/dispatch/main.go, which writes the message mid-line as msg="dispatch: block index", so
	// the substring is the match and not a line's prefix.
	for _, line := range strings.Split(p.output.from(written), "\n") {
		if strings.Contains(line, "dispatch: block index") {
			t.Fatalf("the server indexed blocks inside the measured window, so the peak is that pass's memory: %s", line)
		}
	}
	return p.resident(t, "VmHWM") - p.idle
}

// blockIndexLeaseNamespace is the first key every loop lease is taken under - duty's
// leaseNamespace, which stays unexported - the four bytes of "LOOP". The second key is the
// lease's own name, which Postgres hashes with hashtext.
const blockIndexLeaseNamespace int32 = 0x4C4F4F50

// blockIndexLeaseWait bounds the wait for the lease. A pass reads one batch of each queue it
// serves and gives the lease back, seconds at most, so this is a hang guard on a wait nothing
// healthy reaches and not a time the wait is meant to spend: a wait that reaches it means the
// pass never gave its lease back, and the window cannot be measured with the pass held off.
const blockIndexLeaseWait = 2 * time.Minute

// holdBlockIndexLease takes the block-index pass's own lease (docs.BlockIndexLease) and answers
// the call that gives it back. duty.Exclusive takes that lease before every pass and skips the
// pass where another session holds it, so no block-index pass runs while this test holds it:
// a window that owes the index of what it just wrote leaves that work to a tick after the lease
// is given back. It does not replace drainBeforeStart, which clears what was owed before the
// server started, work the lease only postpones; the two together leave the window clean.
//
// The lock belongs to the session that took it, so one connection of the harness's pool takes it,
// holds it for the window and gives it back (duty.FreeSessionLock, which destroys rather than
// returns a connection whose unlock failed, since what such a session holds is unknown).
func (p *memoryServer) holdBlockIndexLease(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := p.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection for the block-index lease: %v", err)
	}
	deadline := time.Now().Add(blockIndexLeaseWait)
	for {
		var held bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease).Scan(&held); err != nil {
			conn.Release()
			t.Fatalf("take the block-index lease: %v", err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			conn.Release()
			t.Fatalf("the block-index pass held the %q lease for more than %s, so this window could not be measured with the pass held off:\n%s",
				docs.BlockIndexLease, blockIndexLeaseWait, p.output.tail())
		}
		// pg_try_advisory_lock answers false rather than queueing behind the holder, so a pass
		// running at this moment is waited out here: it gives the lease back when it ends.
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		if err := duty.FreeSessionLock(conn, `select pg_advisory_unlock($1, hashtext($2))`,
			blockIndexLeaseNamespace, docs.BlockIndexLease); err != nil {
			t.Errorf("give the block-index lease back: %v", err)
		}
	}
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
	logtest.Buffer
}

func (b *lockedBuffer) tail() string {
	output := b.String()
	if len(output) > 4000 {
		output = output[len(output)-4000:]
	}
	return output
}

// len is how much the server has written, and from everything it wrote past offset: what one
// measured window logged (peakAboveIdle), which tail's last 4,000 bytes do not answer for.
func (b *lockedBuffer) len() int {
	return b.Len()
}

func (b *lockedBuffer) from(offset int) string {
	output := b.String()
	if offset >= len(output) {
		return ""
	}
	return output[offset:]
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
