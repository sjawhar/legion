package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// A test hook acts on every envoy-dispatch process serving the database, not only on the one its
// call reached. A browser suite against two processes holds its page's stream, socket, document
// and conversation viewer on whichever process the page connected to, and calls a hook through an
// API request that may land on the other; a hook that acted where it landed would disconnect,
// quiesce or feed frames to the wrong process.
//
// Each process with the hooks on (Deps.TestHooksEnabled; production never mounts them) holds one
// listening connection of its own on testHooksChannel and testHookAcksChannel, and on that
// connection a session-level advisory lock, testHookLockNamespace and the connection's backend
// pid. The lock dies with the connection, so the granted locks in that namespace are exactly the
// processes that will hear a hook, and a process that dies leaves none behind. A call sends the
// hook and its arguments on testHooksChannel; every process, the caller included, runs the hook's
// local action when it hears it and acknowledges on testHookAcksChannel under its lock key; the
// caller answers once every process it is waiting for has acknowledged, with the first error any
// of them reported, or 504 TEST_HOOK_TIMEOUT after testHookTimeout naming those that did not.
const (
	testHooksChannel    = "dispatch_test_hooks"
	testHookAcksChannel = "dispatch_test_hook_acks"
	// testHookLockNamespace is the first key of every test-hook lock ("HOOK"), in the two-key form
	// so it can never collide with the single-key locks internal/dispatch/docs takes, and apart
	// from the two-key namespaces of the loop leases (duty.leaseNamespace, "LOOP") and the copy
	// locks (docs.copiedAsksLockNamespace, "COPY"). The second key is the listening connection's
	// backend pid, which no other live connection has, so two processes' keys never meet.
	testHookLockNamespace int32 = 0x484F4F4B
	// testHookTimeout bounds a call: the wait for this process's own listener, and then for every
	// other process's acknowledgement. A hook's own work - closing streams, or quiesce's flush of
	// each live document - takes milliseconds in a test; ten seconds is a process that is not
	// going to answer.
	testHookTimeout = 10 * time.Second
	// testHookPoll is how often a waiting call reads the held locks again, which is how it stops
	// waiting for a process that died after the call was sent.
	testHookPoll = 100 * time.Millisecond
	// testHookPartBytes bounds the slice of a call one notification carries. Postgres refuses a
	// NOTIFY payload of 8000 bytes or more, and a published frame - a long transcript's replay,
	// which the browser suite sets - is often larger, so a call is split into parts sent in one
	// transaction; the remaining hundred bytes are each part's header (testHookPart).
	testHookPartBytes = 7900
)

// The hooks a call names.
const (
	hookDisconnectStreams = "events.disconnect"
	hookDisconnectSockets = "delivery.disconnect"
	hookQuiesceDocuments  = "artifacts.quiesce"
	hookPublishFrame      = "agents.publish"
)

// testHookCall is one hook and its own arguments: for hookPublishFrame the session, the `as` form
// ("replay", "drop", "responder", or empty for a live frame) and the frame, or for the responder
// form whether the session answers.
type testHookCall struct {
	Hook       string `json:"hook"`
	Session    string `json:"session,omitempty"`
	As         string `json:"as,omitempty"`
	Frame      string `json:"frame,omitempty"`
	Responding bool   `json:"responding,omitempty"`
}

// testHookAck is one process's acknowledgement of a call: its lock key, and the status and body
// its local action answered. Only a failure carries a body; 200 is the hook's {"ok": true}.
type testHookAck struct {
	ID     string          `json:"id"`
	Key    int32           `json:"key"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// testHookFanout is this process's side of the fan-out: its listening connection and lock, the
// calls it is reassembling, and the acknowledgements its own callers wait for.
type testHookFanout struct {
	server   *server
	lifetime context.Context
	running  sync.WaitGroup

	mu sync.Mutex
	// key is the backend pid of the listening connection holding this process's lock, zero
	// between connections.
	key int32
	// partial is every call whose parts have not all arrived, by call id.
	partial map[string]*testHookParts
	// waiting is every call this process sent and has not answered, by call id.
	waiting map[string]*testHookWait
}

type testHookParts struct {
	pieces []string
	have   int
}

type testHookWait struct {
	acks map[int32]testHookAck
	// arrived holds at most one signal: the waiter reads every ack under mu when it wakes.
	arrived chan struct{}
}

// startTestHookFanout starts s's listening connection on lifetime. It runs from register rather
// than on a first use, as the event relay's does, because a process must be listening before any
// other process calls a hook.
func startTestHookFanout(s *server, lifetime context.Context) *testHookFanout {
	fanout := &testHookFanout{
		server:   s,
		lifetime: lifetime,
		partial:  map[string]*testHookParts{},
		waiting:  map[string]*testHookWait{},
	}
	listener := &pglisten.Listener{
		Owner:    "dispatch",
		Dial:     fanout.dial,
		Channels: []string{testHooksChannel, testHookAcksChannel},
		OnNotify: fanout.onNotify,
	}
	fanout.running.Add(1)
	go func() { defer fanout.running.Done(); listener.Run(lifetime) }()
	return fanout
}

// wait returns once the listener has, after its lifetime ended.
func (f *testHookFanout) wait() { f.running.Wait() }

// dial opens the listening connection, from the pool's listener pool and so outside the shared
// pool, listens, and then takes this process's lock on it. Listening first is what makes the lock
// mean something to a caller: a lock it reads before it sends a call belongs to a connection that
// was already listening, so that process hears the call. pglisten.Listener issues the same LISTEN
// again once this returns, which changes nothing. A reconnect forgets every partial call, whose
// remaining parts may have been sent while there was no connection to hear them.
func (f *testHookFanout) dial(ctx context.Context) (*pgx.Conn, error) {
	f.mu.Lock()
	f.key = 0
	clear(f.partial)
	f.mu.Unlock()
	conn, err := f.server.deps.Store.Pool.Listener(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, pglisten.DefaultPingTimeout)
	defer cancel()
	key := int32(conn.PgConn().PID())
	var held bool
	if _, err = conn.Exec(ctx, "listen "+testHooksChannel+"; listen "+testHookAcksChannel); err == nil {
		err = conn.QueryRow(ctx, `select pg_try_advisory_lock($1, $2)`, testHookLockNamespace, key).Scan(&held)
	}
	if err == nil && !held {
		err = fmt.Errorf("test-hook lock %d is held by another connection", key)
	}
	if err != nil {
		// Bounded on its own, as pglisten bounds every close: the dial's context may be the one
		// that ended, and a half-open connection's Terminate would otherwise hold the redial.
		closeCtx, cancelClose := context.WithTimeout(context.Background(), pglisten.DefaultPingTimeout)
		defer cancelClose()
		_ = conn.Close(closeCtx)
		return nil, fmt.Errorf("take the test-hook lock: %w", err)
	}
	f.mu.Lock()
	f.key = key
	f.mu.Unlock()
	return conn, nil
}

// onNotify runs on the listener's goroutine. A hook's action runs there too, so the hooks this
// process hears run one at a time in the order they were sent, as they did when each ran inside
// its own request.
func (f *testHookFanout) onNotify(channel, payload string) {
	switch channel {
	case testHooksChannel:
		f.heard(payload)
	case testHookAcksChannel:
		f.acknowledged(payload)
	}
}

// heard takes one part of a call, and once the call is whole runs it and acknowledges it.
func (f *testHookFanout) heard(payload string) {
	id, part, parts, piece, ok := parseTestHookPart(payload)
	if !ok {
		slog.Warn("dispatch: a test-hook notification is malformed; ignored", "payload_bytes", len(payload))
		return
	}
	f.mu.Lock()
	partial := f.partial[id]
	if partial == nil {
		partial = &testHookParts{pieces: make([]string, parts)}
		f.partial[id] = partial
	}
	if len(partial.pieces) != parts || partial.pieces[part-1] != "" {
		delete(f.partial, id)
		f.mu.Unlock()
		slog.Warn("dispatch: a test-hook call's parts disagree; ignored", "call", id)
		return
	}
	partial.pieces[part-1] = piece
	partial.have++
	whole := partial.have == parts
	if whole {
		delete(f.partial, id)
	}
	key := f.key
	f.mu.Unlock()
	if !whole {
		return
	}
	ack := testHookAck{ID: id, Key: key, Status: http.StatusOK}
	var call testHookCall
	if err := json.Unmarshal([]byte(strings.Join(partial.pieces, "")), &call); err != nil {
		ack.Status, ack.Body = testHookFailure(http.StatusInternalServerError, "TEST_HOOK_FAILED", "process "+strconv.Itoa(int(key))+" could not read the call: "+err.Error())
	} else {
		ctx, cancel := context.WithTimeout(f.lifetime, testHookTimeout)
		ack.Status, ack.Body = f.server.runTestHook(ctx, call)
		cancel()
	}
	f.acknowledge(ack)
}

// acknowledge sends ack through the shared pool: the listening connection is busy waiting.
func (f *testHookFanout) acknowledge(ack testHookAck) {
	payload, err := json.Marshal(ack)
	if err == nil && len(payload) > testHookPartBytes {
		ack.Status, ack.Body = testHookFailure(http.StatusInternalServerError, "TEST_HOOK_FAILED",
			fmt.Sprintf("process %d answered the hook %d with a body too long for a notification", ack.Key, ack.Status))
		payload, err = json.Marshal(ack)
	}
	if err != nil {
		slog.Warn("dispatch: encode a test-hook acknowledgement", "call", ack.ID, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(f.lifetime, testHookTimeout)
	defer cancel()
	if _, err := f.server.deps.Store.Pool.Exec(ctx, `select pg_notify($1, $2)`, testHookAcksChannel, string(payload)); err != nil {
		slog.Warn("dispatch: send a test-hook acknowledgement", "call", ack.ID, "error", err)
	}
}

// acknowledged records one process's acknowledgement for the caller waiting on it, if this
// process sent that call.
func (f *testHookFanout) acknowledged(payload string) {
	var ack testHookAck
	if err := json.Unmarshal([]byte(payload), &ack); err != nil {
		slog.Warn("dispatch: a test-hook acknowledgement is malformed; ignored", "error", err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if wait := f.waiting[ack.ID]; wait != nil {
		wait.acks[ack.Key] = ack
		signal(wait.arrived)
	}
}

// call sends one hook to every process and answers the status and body the caller writes.
//
// The processes it waits for are those holding a lock when it sends: each took its lock after it
// started listening (dial), so each hears the call. It waits first for its own process to be
// among them, so a call made while this process's listener is still connecting acts here too. A
// process whose lock is gone while the call waits is no longer waited for: it died, and nothing
// it held is left to act on.
func (f *testHookFanout) call(ctx context.Context, call testHookCall) (int, []byte) {
	encoded, err := json.Marshal(call)
	if err != nil {
		return testHookFailure(http.StatusInternalServerError, "TEST_HOOK_FAILED", "encode the call: "+err.Error())
	}
	id := rand.Text()
	parts := splitTestHookCall(id, string(encoded))
	ctx, cancel := context.WithTimeout(ctx, testHookTimeout)
	defer cancel()

	wait := &testHookWait{acks: map[int32]testHookAck{}, arrived: make(chan struct{}, 1)}
	f.mu.Lock()
	f.waiting[id] = wait
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.waiting, id)
		f.mu.Unlock()
	}()

	awaited, err := f.awaitOwnLock(ctx)
	if err != nil {
		return f.unanswered(ctx, err, "this process's test-hook listener did not take its lock")
	}
	if err := pgx.BeginFunc(ctx, f.server.deps.Store.Pool, func(tx pgx.Tx) error {
		for _, part := range parts {
			if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, testHooksChannel, part); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return f.unanswered(ctx, err, "the call could not be sent")
	}

	timedOut := func(missing []int32) (int, []byte) {
		return testHookFailure(http.StatusGatewayTimeout, "TEST_HOOK_TIMEOUT",
			fmt.Sprintf("no acknowledgement within %v from the test-hook process(es) holding lock key %s (each key is the backend pid of that process's listening connection)",
				testHookTimeout, joinKeys(missing)))
	}
	poll := time.NewTicker(testHookPoll)
	defer poll.Stop()
	for {
		f.mu.Lock()
		var missing []int32
		var failed *testHookAck
		for _, key := range awaited {
			ack, acked := wait.acks[key]
			switch {
			case !acked:
				missing = append(missing, key)
			case ack.Status != http.StatusOK && failed == nil:
				failed = &ack
			}
		}
		f.mu.Unlock()
		if len(missing) == 0 {
			if failed != nil {
				return failed.Status, failed.Body
			}
			return http.StatusOK, nil
		}
		select {
		case <-wait.arrived:
		case <-poll.C:
			held, err := f.heldKeys(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return timedOut(missing)
				}
				return f.unanswered(ctx, err, "the processes holding a test-hook lock could not be read")
			}
			awaited = slices.DeleteFunc(awaited, func(key int32) bool { return !slices.Contains(held, key) })
		case <-ctx.Done():
			return timedOut(missing)
		}
	}
}

// awaitOwnLock waits until this process's own lock is among the held ones, and answers them.
func (f *testHookFanout) awaitOwnLock(ctx context.Context) ([]int32, error) {
	for {
		held, err := f.heldKeys(ctx)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		own := f.key
		f.mu.Unlock()
		if own != 0 && slices.Contains(held, own) {
			return held, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(testHookPoll):
		}
	}
}

// heldKeys is every granted test-hook lock on this database: one per live process with the hooks
// on. Advisory locks are per database, and other databases on the server are other deployments.
func (f *testHookFanout) heldKeys(ctx context.Context) ([]int32, error) {
	rows, err := f.server.deps.Store.Pool.Query(ctx, `
		select objid::bigint
		from pg_locks
		where locktype = 'advisory' and objsubid = 2 and granted
		  and classid::bigint = $1
		  and database = (select oid from pg_database where datname = current_database())`,
		int64(testHookLockNamespace))
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (int32, error) {
		var key int64
		err := row.Scan(&key)
		return int32(key), err
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// unanswered is a call that could not wait for its acknowledgements: 504 TEST_HOOK_TIMEOUT when
// its time ran out, 503 TEST_HOOK_UNAVAILABLE when the database refused it.
func (f *testHookFanout) unanswered(ctx context.Context, err error, what string) (int, []byte) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		f.mu.Lock()
		own := f.key
		f.mu.Unlock()
		return testHookFailure(http.StatusGatewayTimeout, "TEST_HOOK_TIMEOUT",
			fmt.Sprintf("%s within %v (lock key %d)", what, testHookTimeout, own))
	}
	return testHookFailure(http.StatusServiceUnavailable, "TEST_HOOK_UNAVAILABLE", what+": "+err.Error())
}

// runTestHookEverywhere runs call on every process and writes the answer. A server with no
// database shares nothing with any other process, so it runs the hook on itself.
func (s *server) runTestHookEverywhere(w http.ResponseWriter, r *http.Request, call testHookCall) {
	var status int
	var body []byte
	if s.testHooks == nil {
		status, body = s.runTestHook(r.Context(), call)
	} else {
		status, body = s.testHooks.call(r.Context(), call)
	}
	if status == http.StatusOK {
		WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// runTestHook is a hook's local action on this process, answering 200 or the status and body a
// failure writes.
func (s *server) runTestHook(ctx context.Context, call testHookCall) (int, []byte) {
	switch call.Hook {
	case hookDisconnectStreams:
		s.eventRelay.stream.CloseAll()
	case hookDisconnectSockets:
		for _, socket := range s.deliverySockets.all() {
			socket.drop()
		}
	case hookQuiesceDocuments:
		if err := s.deps.Docs.Quiesce(ctx); err != nil {
			answer := &testHookAnswer{header: http.Header{}}
			s.writeHandlerError(answer, err)
			return answer.status, answer.body.Bytes()
		}
	case hookPublishFrame:
		memory, ok := s.deps.AgentStream.(*agentstream.Memory)
		if !ok {
			return testHookFailure(http.StatusServiceUnavailable, "STREAM_UNAVAILABLE",
				"a process's relay takes frames from sessions, not from a test hook")
		}
		switch call.As {
		case "replay":
			memory.SetReplay(call.Session, agentstream.Frame(call.Frame))
		case "drop":
			memory.Drop(call.Session)
		case "responder":
			memory.SetResponder(call.Session, call.Responding)
		default:
			memory.Publish(call.Session, agentstream.Frame(call.Frame))
		}
	default:
		return testHookFailure(http.StatusInternalServerError, "TEST_HOOK_FAILED", "a process does not know the hook "+call.Hook)
	}
	return http.StatusOK, nil
}

// testHookFailure is the status and body writeError writes.
func testHookFailure(status int, code, message string) (int, []byte) {
	body, _ := json.Marshal(map[string]string{"error": message, "code": code})
	return status, body
}

// testHookAnswer records what a handler's error writer writes, so a process can send it to the
// caller as its acknowledgement.
type testHookAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *testHookAnswer) Header() http.Header         { return a.header }
func (a *testHookAnswer) Write(p []byte) (int, error) { return a.body.Write(p) }
func (a *testHookAnswer) WriteHeader(status int)      { a.status = status }

// splitTestHookCall splits encoded into the payloads of its notifications, each
// "<id> <part> <parts> <piece>" with part counted from 1. A piece ends on a character boundary,
// since Postgres refuses a payload that is not valid text in the database's encoding.
func splitTestHookCall(id, encoded string) []string {
	var pieces []string
	for len(encoded) > testHookPartBytes {
		cut := testHookPartBytes
		for !utf8.RuneStart(encoded[cut]) {
			cut--
		}
		pieces = append(pieces, encoded[:cut])
		encoded = encoded[cut:]
	}
	pieces = append(pieces, encoded)
	parts := make([]string, len(pieces))
	for index, piece := range pieces {
		parts[index] = fmt.Sprintf("%s %d %d %s", id, index+1, len(pieces), piece)
	}
	return parts
}

// parseTestHookPart reads one payload splitTestHookCall wrote.
func parseTestHookPart(payload string) (id string, part, parts int, piece string, ok bool) {
	fields := strings.SplitN(payload, " ", 4)
	if len(fields) != 4 {
		return "", 0, 0, "", false
	}
	part, partErr := strconv.Atoi(fields[1])
	parts, partsErr := strconv.Atoi(fields[2])
	if partErr != nil || partsErr != nil || parts < 1 || part < 1 || part > parts {
		return "", 0, 0, "", false
	}
	return fields[0], part, parts, fields[3], true
}

func joinKeys(keys []int32) string {
	named := make([]string, len(keys))
	for index, key := range keys {
		named[index] = strconv.Itoa(int(key))
	}
	return strings.Join(named, ", ")
}
