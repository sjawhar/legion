package deliveryaudit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/deliveryaudit"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// commentColumns is comments.sql's output contract, in order.
var commentColumns = []string{
	"comment_id", "event_id", "owner", "created_at", "published_at", "author", "shape", "reopened",
	"target", "target_kind", "owed_reasons", "addressed", "mention_state", "mention_error",
	"route_used", "claim_used", "route_source", "mention_envelope_id", "mention_delivery",
}

// recorder is the outbox's NATS boundary: it accepts every envelope and records each topic under
// the event it carries, so a test can hold what the outbox sent against what it recorded.
type recorder struct {
	mu     sync.Mutex
	topics map[string][]string
}

func (r *recorder) Window() bus.Sender { return r }

func (r *recorder) PublishAsync(item contracts.Envelope) <-chan error {
	r.mu.Lock()
	r.topics[item.SourceEventID] = append(r.topics[item.SourceEventID], item.Topic)
	r.mu.Unlock()
	accepted := make(chan error, 1)
	accepted <- nil
	return accepted
}

func (r *recorder) sent(eventID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.topics[eventID])
}

// heardEnvelope is the envelope id the stand-in listener mints for every send to s-heard.
const heardEnvelope = "envelope-heard-1"

// rig is a Dispatch API server on its own database, with a stand-in Envoy listener that knows two
// live sessions: s-mention, every send to which it refuses, and s-heard, every send to which it
// accepts under heardEnvelope.
type rig struct {
	handler   http.Handler
	database  *store.Store
	broker    *events.Broker
	documents docs.API
	sent      *recorder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	const mentioned = `{"session_id":"s-mention","title":"mentioned","capabilities":["btw","steer"]}`
	const heard = `{"session_id":"s-heard","title":"heard","capabilities":["btw","steer"]}`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s-mention":
			_, _ = w.Write([]byte(mentioned))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s-heard":
			_, _ = w.Write([]byte(heard))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + mentioned + "," + heard + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var send struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&send); err == nil && send.TargetSession == "s-heard" {
				_, _ = w.Write([]byte(`{"event_id":"` + heardEnvelope + `","recipient":"s-heard","duplicate":false}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"the session's pane is gone"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session"}`))
		}
	}))
	t.Cleanup(listener.Close)

	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	people := store.NewPgPeopleStore(database.Pool, "signing-key", nil)
	if err := people.Record(context.Background(), "alice"); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse agent token: %v", err)
	}
	deps, err := api.NewDeps(api.DepsInput{
		Store:       database,
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		AgentTokens: tokens,
		ServerURL:   "https://dispatch.example",
		EnvoyURL:    listener.URL,
		Docs:        documents,
		Events:      broker,
		Lifetime:    t.Context(),
	})
	if err != nil {
		t.Fatalf("API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(api.Register(mux, deps))
	return &rig{handler: mux, database: database, broker: broker, documents: documents, sent: &recorder{topics: map[string][]string{}}}
}

// call sends one request: as the person login names when it is not empty, else as the session
// actor names (an agent bearer, the actor in the body). It fails the test unless the answer is want,
// and decodes a JSON answer into a map.
func (r *rig) call(t *testing.T, method, target, login string, actor string, body map[string]any, want int) map[string]any {
	t.Helper()
	var reader *bytes.Reader
	if actor != "" {
		if body == nil {
			body = map[string]any{}
		}
		body["actor"] = map[string]any{"kind": "session", "id": actor}
	}
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, target, err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d body=%s, want %d", method, target, response.Code, response.Body.String(), want)
	}
	decoded := map[string]any{}
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %s %s: %v (%s)", method, target, err, response.Body.String())
		}
	}
	return decoded
}

// comment posts body to target (an owner's comments route) and returns the comment's id.
func (r *rig) comment(t *testing.T, target, login, actor string, body map[string]any) string {
	t.Helper()
	return r.call(t, http.MethodPost, target, login, actor, body, http.StatusCreated)["id"].(string)
}

// publishAll runs the real outbox until every event is published.
func (r *rig) publishAll(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: r.database, Publisher: r.sent, Broker: r.broker, Docs: r.documents})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var unpublished int
		if err := r.database.Pool.QueryRow(context.Background(), `
			select count(*) from events
			where published_at is null and coalesce(payload ->> 'pending', 'false') <> 'true'
		`).Scan(&unpublished); err != nil {
			t.Fatalf("count unpublished events: %v", err)
		}
		if unpublished == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d events unpublished", unpublished)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// query runs one statement and returns its column names and its rows, every column read as text.
func (r *rig) query(t *testing.T, statement string, args ...any) ([]string, []map[string]string) {
	t.Helper()
	rows, err := r.database.Pool.Query(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("run statement: %v\n%s", err, statement)
	}
	defer rows.Close()
	var columns []string
	for _, field := range rows.FieldDescriptions() {
		columns = append(columns, field.Name)
	}
	var result []map[string]string
	for rows.Next() {
		values := make([]string, len(columns))
		targets := make([]any, len(columns))
		for index := range values {
			targets[index] = &values[index]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		row := map[string]string{}
		for index, column := range columns {
			row[column] = values[index]
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return columns, result
}

func (r *rig) audit(t *testing.T, from, to time.Time) []map[string]string {
	t.Helper()
	statement, err := deliveryaudit.Bind(deliveryaudit.CommentsSQL, "from", "to")
	if err != nil {
		t.Fatalf("bind comments.sql: %v", err)
	}
	columns, rows := r.query(t, statement, from, to)
	if !slices.Equal(columns, commentColumns) {
		t.Fatalf("comments.sql columns = %v, want %v", columns, commentColumns)
	}
	return rows
}

func (r *rig) eventSeq(t *testing.T, issue, eventType string) int {
	t.Helper()
	var seq int
	if err := r.database.Pool.QueryRow(context.Background(),
		`select max(seq) from events where issue_key = $1 and type = $2`, issue, eventType).Scan(&seq); err != nil {
		t.Fatalf("read %s's %s sequence: %v", issue, eventType, err)
	}
	return seq
}

func createIssue(t *testing.T, r *rig, project, title, spec string) (string, string) {
	t.Helper()
	created := r.call(t, http.MethodPost, "/api/v1/issues", "alice", "", map[string]any{
		"project": project, "title": title, "spec": spec,
	}, http.StatusCreated)
	return created["key"].(string), created["primary_artifact_id"].(string)
}

func claim(t *testing.T, r *rig, key, session string) {
	t.Helper()
	r.call(t, http.MethodPost, "/api/v1/issues/"+key+"/claim", "", session, nil, http.StatusOK)
}

// rowsFor returns the audit rows of one comment.
func rowsFor(rows []map[string]string, commentID string) []map[string]string {
	var found []map[string]string
	for _, row := range rows {
		if row["comment_id"] == commentID {
			found = append(found, row)
		}
	}
	return found
}

// targetRow returns the single row of commentID for target, failing when there is not exactly one.
func targetRow(t *testing.T, rows []map[string]string, commentID, target string) map[string]string {
	t.Helper()
	var found []map[string]string
	for _, row := range rowsFor(rows, commentID) {
		if row["target"] == target {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rows of comment %s for target %q = %v, want one (all its rows: %v)", commentID, target, found, rowsFor(rows, commentID))
	}
	return found[0]
}

// expect checks the named columns of row.
func expect(t *testing.T, row map[string]string, want map[string]string) {
	t.Helper()
	for _, column := range slices.Sorted(maps.Keys(want)) {
		if row[column] != want[column] {
			t.Errorf("comment %s target %q: %s = %q, want %q (row %v)", row["comment_id"], row["target"], column, row[column], want[column], row)
		}
	}
}

// expectAddressedAsRecorded checks that the row's addressed column says whether its target is a
// destination the outbox recorded on the comment's event, and that those recorded destinations are
// exactly the topics the outbox sent the event to.
func (r *rig) expectAddressedAsRecorded(t *testing.T, row map[string]string) {
	t.Helper()
	var recorded []string
	if err := r.database.Pool.QueryRow(context.Background(),
		`select published_destinations from events where id = $1::bigint`, row["event_id"]).Scan(&recorded); err != nil {
		t.Fatalf("read event %s's destinations: %v", row["event_id"], err)
	}
	sent := r.sent.sent(row["event_id"])
	if !slices.Equal(slices.Sorted(slices.Values(recorded)), slices.Sorted(slices.Values(sent))) {
		t.Errorf("event %s recorded destinations %v, but the outbox sent it to %v", row["event_id"], recorded, sent)
	}
	want := fmt.Sprint(row["target"] != "" && slices.Contains(recorded, row["target"]))
	if row["addressed"] != want {
		t.Errorf("comment %s target %q: addressed = %q, want %q (destinations %v)", row["comment_id"], row["target"], row["addressed"], want, recorded)
	}
}

func agent(session string) string { return contracts.AgentSubject(session) }

func TestCommentsSQLReadsWhomEachPersonsCommentWasOwedAndWhomItReached(t *testing.T) {
	r := newRig(t)
	from := time.Now().Add(-time.Minute)
	r.call(t, http.MethodPost, "/api/v1/projects", "alice", "", map[string]any{"key": "BET", "name": "Bets"}, http.StatusCreated)

	// (a) BET-15's shape: a session claims an unrouted issue; a person comments on a passage of its
	// spec; the session replies and resolves the thread; the person replies, which reopens it.
	threadIssue, spec := createIssue(t, r, "BET", "Spec thread", "We ship the design on Friday.\n")
	claim(t, r, threadIssue, "s-claimant")
	root := r.comment(t, "/api/v1/issues/"+threadIssue+"/comments", "alice", "", map[string]any{
		"body": "Why Friday?", "anchor": map[string]any{"artifact": spec, "quote": "on Friday"},
	})
	sessionReply := r.comment(t, "/api/v1/issues/"+threadIssue+"/comments", "", "s-claimant", map[string]any{
		"body": "The review lands on Thursday.", "reply_to": root,
	})
	r.call(t, http.MethodPost, "/api/v1/comments/"+root+"/resolve", "", "s-claimant", nil, http.StatusOK)
	personReply := r.comment(t, "/api/v1/issues/"+threadIssue+"/comments", "alice", "", map[string]any{
		"body": "Not convinced; which review?", "reply_to": sessionReply,
	})

	// (b) A mention of a live session whose delivery the listener refuses.
	mentionIssue, _ := createIssue(t, r, "BET", "Mention", "A mention.\n")
	mention := r.comment(t, "/api/v1/issues/"+mentionIssue+"/comments", "alice", "", map[string]any{
		"body": "Can you look?", "mentions": []map[string]any{{"target": "session:s-mention"}},
	})

	// (c) A person's reply on an ask's thread: the asker, a session that followed and stayed, and a
	// session that followed and left before the reply.
	askIssue, _ := createIssue(t, r, "BET", "Ask", "An ask.\n")
	ask := r.call(t, http.MethodPost, "/api/v1/issues/"+askIssue+"/asks", "", "s-asker", map[string]any{
		"question": "Which region?",
	}, http.StatusCreated)["id"].(string)
	r.call(t, http.MethodPut, "/api/v1/asks/"+ask+"/followers/s-stay", "alice", "", nil, http.StatusNoContent)
	r.call(t, http.MethodPut, "/api/v1/asks/"+ask+"/followers/s-gone", "alice", "", nil, http.StatusNoContent)
	r.call(t, http.MethodDelete, "/api/v1/asks/"+ask+"/followers/s-gone", "alice", "", nil, http.StatusNoContent)
	askReply := r.comment(t, "/api/v1/issues/"+askIssue+"/comments", "alice", "", map[string]any{
		"body": "Which regions are on the table?", "ask_id": ask,
	})

	// (d) A routed issue whose claimant is another session.
	routedIssue, _ := createIssue(t, r, "BET", "Routed", "A routed issue.\n")
	claim(t, r, routedIssue, "s-worker")
	r.call(t, http.MethodPatch, "/api/v1/issues/"+routedIssue, "alice", "", map[string]any{"route": "role:reviewer"}, http.StatusOK)
	routed := r.comment(t, "/api/v1/issues/"+routedIssue+"/comments", "alice", "", map[string]any{"body": "Ready for review?"})

	// (e) An unrouted issue whose claim was released before the comment; it is closed later.
	releasedIssue, _ := createIssue(t, r, "BET", "Released", "A released issue.\n")
	claim(t, r, releasedIssue, "s-old")
	r.call(t, http.MethodDelete, "/api/v1/issues/"+releasedIssue+"/claim", "alice", "", nil, http.StatusOK)
	released := r.comment(t, "/api/v1/issues/"+releasedIssue+"/comments", "alice", "", map[string]any{"body": "Anyone?"})

	// (f) A comment on a project document a session wrote.
	document := r.call(t, http.MethodPost, "/api/v1/projects/BET/artifacts", "", "s-writer", map[string]any{
		"name": "plan.md", "content": "The plan ships on Friday.\n",
	}, http.StatusCreated)["artifact"].(map[string]any)["id"].(string)
	documentComment := r.comment(t, "/api/v1/artifacts/"+document+"/comments", "alice", "", map[string]any{
		"body": "Which Friday?", "anchor": map[string]any{"artifact": document, "quote": "on Friday"},
	})

	// (h) A person's reply on an ask's thread that mentions a live session by BTW, which the
	// listener accepts.
	heardIssue, _ := createIssue(t, r, "BET", "Heard", "A heard mention.\n")
	heardAsk := r.call(t, http.MethodPost, "/api/v1/issues/"+heardIssue+"/asks", "", "s-heard-asker", map[string]any{
		"question": "Which zone?",
	}, http.StatusCreated)["id"].(string)
	heardReply := r.comment(t, "/api/v1/issues/"+heardIssue+"/comments", "alice", "", map[string]any{
		"body": "Ask s-heard.", "ask_id": heardAsk,
		"mentions": []map[string]any{{"target": "session:s-heard"}}, "delivery": "btw",
	})

	r.publishAll(t)

	// After the publish: a follower who joins the ask now was not following when the reply went
	// out, and closing the released issue changes nothing about a comment published before.
	r.call(t, http.MethodPut, "/api/v1/asks/"+ask+"/followers/s-late", "alice", "", nil, http.StatusNoContent)
	r.call(t, http.MethodPatch, "/api/v1/issues/"+releasedIssue, "alice", "", map[string]any{"status": "done"}, http.StatusOK)

	// (g) A comment the outbox has not published.
	unpublished := r.comment(t, "/api/v1/issues/"+threadIssue+"/comments", "alice", "", map[string]any{"body": "Still waiting."})

	rows := r.audit(t, from, time.Now().Add(time.Minute))
	ordered := slices.IsSortedFunc(rows, func(a, b map[string]string) int {
		return strings.Compare(a["created_at"]+a["comment_id"]+"\x00"+a["target"], b["created_at"]+b["comment_id"]+"\x00"+b["target"])
	})
	if !ordered {
		t.Errorf("rows are not ordered by created_at, comment_id, target: %v", rows)
	}
	for _, row := range rows {
		if row["author"] != "alice" {
			t.Errorf("row of a comment by %q: %v; only a person's comments are listed", row["author"], row)
		}
	}
	threadClaim := fmt.Sprintf("%d;claim=%d", r.eventSeq(t, threadIssue, "issue.created"), r.eventSeq(t, threadIssue, "issue.claimed"))

	t.Run("a person's reply that reopens a session's resolved thread is owed to the claimant", func(t *testing.T) {
		if got := rowsFor(rows, personReply); len(got) != 1 {
			t.Fatalf("rows of the person's reply = %v, want one: the claimant", got)
		}
		row := targetRow(t, rows, personReply, agent("s-claimant"))
		expect(t, row, map[string]string{
			"owner": threadIssue, "shape": "reply", "reopened": "true", "target_kind": "session",
			"owed_reasons": "claimant", "route_used": "", "claim_used": "session:s-claimant",
			"route_source": threadClaim, "mention_state": "", "mention_error": "",
			"mention_envelope_id": "", "mention_delivery": "",
		})
		if row["event_id"] == "" || row["published_at"] == "" {
			t.Errorf("the reply's row names no published event: %v", row)
		}
		// Today's fan-out sends a person's reply to the thread's root and parent authors only, both
		// the person here, so this reads false until the outbox also reaches the claimant.
		r.expectAddressedAsRecorded(t, row)
		rootRow := targetRow(t, rows, root, agent("s-claimant"))
		expect(t, rootRow, map[string]string{"shape": "anchored", "reopened": "false", "owed_reasons": "claimant"})
		if rows := rowsFor(rows, sessionReply); len(rows) != 0 {
			t.Errorf("the session's reply is listed: %v", rows)
		}
	})

	t.Run("a mention whose delivery failed carries the attempt's state, error and mode", func(t *testing.T) {
		row := targetRow(t, rows, mention, agent("s-mention"))
		expect(t, row, map[string]string{
			"shape": "conversation", "owed_reasons": "mention", "target_kind": "session",
			"mention_state": "failed", "mention_delivery": "steer", "mention_envelope_id": "",
			"claim_used": "", "route_used": "",
		})
		if row["mention_error"] == "" {
			t.Errorf("the failed mention's row carries no error: %v", row)
		}
		r.expectAddressedAsRecorded(t, row)
	})

	t.Run("a sent mention carries the envelope the listener minted and the attempt's mode", func(t *testing.T) {
		row := targetRow(t, rows, heardReply, agent("s-heard"))
		expect(t, row, map[string]string{
			"shape": "ask_thread", "owed_reasons": "mention", "target_kind": "session",
			"mention_state": "sent", "mention_error": "", "mention_delivery": "btw",
			"mention_envelope_id": heardEnvelope,
		})
		_, recorded := r.query(t, `
			select envelope_id, delivery from comment_deliveries
			where comment_id = $1 and target = 'session:s-heard' order by attempt desc limit 1
		`, heardReply)
		if len(recorded) != 1 || recorded[0]["envelope_id"] != row["mention_envelope_id"] || recorded[0]["delivery"] != row["mention_delivery"] {
			t.Errorf("comment_deliveries = %v, want the row's envelope id and mode: %v", recorded, row)
		}
		asker := targetRow(t, rows, heardReply, agent("s-heard-asker"))
		expect(t, asker, map[string]string{"owed_reasons": "asker;follower", "mention_state": "", "mention_envelope_id": "", "mention_delivery": ""})
	})

	t.Run("an ask-thread reply is owed to the asker and the followers as they stood when it was published", func(t *testing.T) {
		got := rowsFor(rows, askReply)
		targets := []string{}
		for _, row := range got {
			targets = append(targets, row["target"])
		}
		sort.Strings(targets)
		if want := []string{agent("s-asker"), agent("s-stay")}; !slices.Equal(targets, want) {
			t.Fatalf("targets of the ask reply = %v, want %v: not s-gone, who left before it, nor s-late, who joined after", targets, want)
		}
		asker := targetRow(t, rows, askReply, agent("s-asker"))
		expect(t, asker, map[string]string{"shape": "ask_thread", "owed_reasons": "asker;follower", "addressed": "true"})
		r.expectAddressedAsRecorded(t, asker)
		stay := targetRow(t, rows, askReply, agent("s-stay"))
		expect(t, stay, map[string]string{"owed_reasons": "follower", "addressed": "true"})
		r.expectAddressedAsRecorded(t, stay)
	})

	t.Run("a routed issue's comment is owed to the route and not the claimant", func(t *testing.T) {
		got := rowsFor(rows, routed)
		if len(got) != 1 {
			t.Fatalf("rows of the routed comment = %v, want one: the route", got)
		}
		row := targetRow(t, rows, routed, "notifications.role.reviewer")
		expect(t, row, map[string]string{
			"target_kind": "role", "owed_reasons": "route", "addressed": "true",
			"route_used": "role:reviewer", "claim_used": "session:s-worker",
			"route_source": fmt.Sprint(r.eventSeq(t, routedIssue, "issue.updated")),
		})
		r.expectAddressedAsRecorded(t, row)
	})

	t.Run("a comment on an unrouted issue with its claim released has one row and no target", func(t *testing.T) {
		got := rowsFor(rows, released)
		if len(got) != 1 {
			t.Fatalf("rows of the comment on the released issue = %v, want one", got)
		}
		expect(t, got[0], map[string]string{
			"target": "", "target_kind": "", "owed_reasons": "", "addressed": "false",
			"route_used": "", "claim_used": "",
			"route_source": fmt.Sprintf("%d;claim=%d", r.eventSeq(t, releasedIssue, "issue.created"), r.eventSeq(t, releasedIssue, "issue.released")),
		})
	})

	t.Run("a comment on a project document is addressed to the session that wrote it, owed to nobody", func(t *testing.T) {
		var slug string
		if err := r.database.Pool.QueryRow(context.Background(), `select slug from artifacts where id = $1`, document).Scan(&slug); err != nil {
			t.Fatalf("read the document's slug: %v", err)
		}
		row := targetRow(t, rows, documentComment, agent("s-writer"))
		expect(t, row, map[string]string{
			"owner": "BET/" + slug, "shape": "anchored", "owed_reasons": "addressed-only", "addressed": "true",
			"route_used": "", "claim_used": "", "route_source": "",
		})
		r.expectAddressedAsRecorded(t, row)
	})

	t.Run("an unpublished comment has no published_at and is read as at its event's creation", func(t *testing.T) {
		row := targetRow(t, rows, unpublished, agent("s-claimant"))
		expect(t, row, map[string]string{
			"published_at": "", "owed_reasons": "claimant", "addressed": "false", "route_source": threadClaim,
		})
		if row["event_id"] == "" {
			t.Errorf("the unpublished comment's row names no event: %v", row)
		}
	})

	t.Run("a window that ends before the comments lists none of them", func(t *testing.T) {
		if early := r.audit(t, from.Add(-time.Hour), from); len(early) != 0 {
			t.Errorf("rows before any comment = %v", early)
		}
	})

	t.Run("trace.sql shows the reply's transaction, mentions, issue state and thread", func(t *testing.T) {
		script, err := deliveryaudit.Bind(deliveryaudit.TraceSQL, "comment")
		if err != nil {
			t.Fatalf("bind trace.sql: %v", err)
		}
		sections := map[string][]map[string]string{}
		statements := deliveryaudit.Statements(script)
		if len(statements) != 5 {
			t.Fatalf("trace.sql has %d statements, want 5", len(statements))
		}
		for _, statement := range statements {
			columns, result := r.query(t, statement, personReply)
			if len(columns) == 0 || columns[0] != "section" {
				t.Fatalf("a trace.sql result does not begin with its section: %v", columns)
			}
			for _, row := range result {
				sections[row["section"]] = append(sections[row["section"]], row)
			}
		}
		transaction := []string{}
		for _, row := range sections["transaction_events"] {
			transaction = append(transaction, row["type"])
		}
		if want := []string{"comment.reopened", "comment.created"}; !slices.Equal(transaction, want) {
			t.Errorf("the reply's transaction = %v, want %v", transaction, want)
		}
		if state := sections["issue_state"]; len(state) != 1 || state[0]["claim_used"] != "session:s-claimant" || state[0]["route_used"] != "" {
			t.Errorf("issue_state = %v, want the claimant and no route", state)
		}
		thread := []string{}
		for _, row := range sections["thread"] {
			thread = append(thread, row["kind"]+" "+row["author"])
		}
		if want := []string{"root user:alice", "reply session:s-claimant", "reply user:alice"}; !slices.Equal(thread, want) {
			t.Errorf("thread = %v, want %v", thread, want)
		}
		if mentions := sections["comment_mentions"]; len(mentions) != 0 {
			t.Errorf("the reply mentioned nobody, but comment_mentions = %v", mentions)
		}
	})
}

func TestBindNamesEachVariableOnce(t *testing.T) {
	bound, err := deliveryaudit.Bind(`select :'from'::timestamptz, 'session:' || x, :'to', :'from'`, "from", "to")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if want := `select $1::timestamptz, 'session:' || x, $2, $1`; bound != want {
		t.Errorf("Bind = %q, want %q", bound, want)
	}
	if _, err := deliveryaudit.Bind(`select :'from'`, "from", "to"); err == nil {
		t.Error("Bind accepted a name the script never uses")
	}
	if _, err := deliveryaudit.Bind(`select :'from', :'other'`, "from"); err == nil {
		t.Error("Bind accepted a variable it was not given")
	}
}
