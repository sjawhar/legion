package answerloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

const (
	project     = "legion"
	testerToken = claim.Token("legion-legion-legion-9-tester")
	askID       = "a0000000-0000-4000-8000-000000000001"
)

var testNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// fakeGate is the daemon's gate as the loop sees it: a session whose claim runs it, and one whose
// claim started (Append refused), each set by the test; started may be set while an answer is
// generated, as a claim's start would. onAppend, when set, sees each append's outcome.
type fakeGate struct {
	mu       sync.Mutex
	running  map[string]bool
	started  map[string]bool
	onAppend func(error)
}

func (g *fakeGate) Running(_ claim.Token, session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[session] || g.started[session]
}

func (g *fakeGate) Append(_ claim.Token, session string, write func() error) (bool, error) {
	g.mu.Lock()
	refused := g.running[session] || g.started[session]
	g.mu.Unlock()
	if refused {
		return false, nil
	}
	err := write()
	if g.onAppend != nil {
		g.onAppend(err)
	}
	return true, err
}

func (g *fakeGate) set(field map[string]bool, session string, on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	field[session] = on
}

// request is one request a stand-in took: its method, path and decoded body.
type request struct {
	Method string
	Path   string
	Body   map[string]any
}

// standIn is an httptest server recording every request; answer writes its response.
type standIn struct {
	*httptest.Server
	mu       sync.Mutex
	requests []request
}

func newStandIn(t *testing.T, answer func(w http.ResponseWriter, r request)) *standIn {
	s := &standIn{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{Method: r.Method, Path: r.URL.Path}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.Body); err != nil {
				t.Errorf("%s %s body: %v", r.Method, r.URL.Path, err)
			}
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		answer(w, req)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) posts() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var posts []request
	for _, r := range s.requests {
		if r.Method == http.MethodPost {
			posts = append(posts, r)
		}
	}
	return posts
}

// fakeTool is the gh tool: no call runs (the stub answer makes none), and Lookup answers what the
// test says GitHub holds for each seq.
type fakeTool struct {
	found map[int]ghtool.Found
	err   map[int]error
}

func (f *fakeTool) Run(context.Context, int, string) ghtool.Result { return ghtool.Result{} }
func (f *fakeTool) Lookup(_ context.Context, seq int) (ghtool.Found, error) {
	if err := f.err[seq]; err != nil {
		return ghtool.Found{}, err
	}
	return f.found[seq], nil
}
func (f *fakeTool) Close() error { return nil }

type toolsFunc func() Tool

func (f toolsFunc) Open(context.Context, ghtool.Agent, string) (Tool, error) { return f(), nil }

type issuesFunc func(string) IssueFacts

func (f issuesFunc) Facts(_ context.Context, issue string) (IssueFacts, error) { return f(issue), nil }

type passed struct {
	ID, Reason, Asker, Ref string
}

type recorder struct {
	mu        sync.Mutex
	passed    []passed
	decisions []Decision
}

func (r *recorder) PassOn(_ context.Context, q store.AnswerQuestion, reason Reason, asker, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.passed = append(r.passed, passed{q.ID, string(reason), asker, ref})
	return nil
}

func (r *recorder) Decide(_ context.Context, d Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = append(r.decisions, d)
	return nil
}

// rig is one loop over a real store, real OMP session files, a Dispatch stand-in and a listener
// stand-in, with a stub in internal/answer's place.
type rig struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	gate     *fakeGate
	tool     *fakeTool
	rec      *recorder
	dispatch *standIn
	listener *standIn
	dir      string
	cfg      Config
	loop     *Loop

	mu       sync.Mutex
	answer   func(req answer.Request) (answer.Reply, error)
	requests []answer.Request
	// ask is what GET /api/v1/asks/{id} answers; held makes a role publish 404 unclaimed.
	ask  map[string]any
	held bool
	// tick moves the clock a millisecond each time it is read, so questions are received in order.
	tick atomic.Int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testpg.Database(t, "legion_answerloop"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		t: t, ctx: ctx, st: st, gate: &fakeGate{running: map[string]bool{}, started: map[string]bool{}},
		tool: &fakeTool{found: map[int]ghtool.Found{}, err: map[int]error{}}, rec: &recorder{}, dir: t.TempDir(),
	}
	r.answer = func(req answer.Request) (answer.Reply, error) {
		return answered(req, "Plum, from "+req.Question.Text), nil
	}
	r.ask = map[string]any{"ask": map[string]any{"id": askID, "issue_key": "LEGION-9", "author": map[string]any{"kind": "session", "id": "ses_tester"}}, "replies": []any{}}
	r.dispatch = newStandIn(t, func(w http.ResponseWriter, req request) {
		if req.Method == http.MethodGet && strings.HasPrefix(req.Path, "/api/v1/asks/") {
			r.mu.Lock()
			body, _ := json.Marshal(r.ask)
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"reply"}`))
	})
	r.listener = newStandIn(t, func(w http.ResponseWriter, req request) {
		r.mu.Lock()
		held := r.held
		r.mu.Unlock()
		if req.Path == "/v1/messages/publish" && held {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder","reason":"unclaimed"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	r.cfg = Config{
		Project: project,
		Answers: config.Answers{Bounds: config.AnswerBounds{
			Answer: 180 * time.Second, ModelCalls: 8, OutputTokens: 4000, GHCall: 30 * time.Second, GHOutputChars: 60000, GHCalls: 20, Concurrent: 4, DailyUSD: 50,
		}},
		Store: st, Dispatch: dispatch.New(r.dispatch.URL, "dispatch-token"), Envoy: notify.New(r.listener.URL, "envoy-token"),
		Gate: r.gate, Tools: toolsFunc(func() Tool { return r.tool }), Conversations: Harnesses{},
		Issues: issuesFunc(func(issue string) IssueFacts {
			return IssueFacts{Recorded: true, DispatchStatus: "done", Phase: "done", PullRequest: &PullRequestFacts{Number: 7, State: "merged", Head: "abc123"}}
		}),
		PassOn: r.rec, Decisions: r.rec,
		Answer: func(_ context.Context, req answer.Request) (answer.Reply, error) {
			r.mu.Lock()
			r.requests = append(r.requests, req)
			answerFn := r.answer
			r.mu.Unlock()
			return answerFn(req)
		},
		Now: func() time.Time { return testNow.Add(time.Duration(r.tick.Add(1)) * time.Millisecond) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	r.loop = New(r.cfg)
	return r
}

// restart is a new loop over the same store, as a daemon restart builds it.
func (r *rig) restart() { r.loop = New(r.cfg) }

// answered is a reply in the form, with the exchange to append.
func answered(req answer.Request, text string) answer.Reply {
	return answer.Reply{Kind: answer.Answered, Text: text, CostUSD: 0.25, Append: conversation.Append{
		QuestionID: req.Question.ID, Question: req.Question.Text, Provider: "anthropic", API: "anthropic-messages", Model: "claude-opus-5-5",
		Turns: []conversation.Turn{{Content: []conversation.Block{{Type: conversation.Text, Text: text}}}},
	}}
}

func (r *rig) answerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// conversation records an OMP conversation of session for token, started at minute, in a session
// file of its own, and returns its path.
func (r *rig) conversation(session string, token claim.Token, minute int) string {
	r.t.Helper()
	path := filepath.Join(r.dir, session+".jsonl")
	content := `{"type":"session","version":3,"id":"` + session + `","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-opus-5-5"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Test the fruit."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Tested: plum."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	started := testNow.Add(time.Duration(minute) * time.Minute)
	if _, err := r.st.RegisterConversation(r.ctx, store.AgentConversation{
		Session: session, Project: project, Issue: "LEGION-9", Role: claim.RoleTester, Token: token,
		Harness: runtime.BackendOmp, Location: path, StartedAt: started, RecordedAt: started,
	}); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// receive hands h to the loop as the consumer would.
func (r *rig) receive(h intake.Handoff) {
	r.t.Helper()
	raw, err := json.Marshal(h)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.loop.Receive(r.ctx, h, raw); err != nil {
		r.t.Fatalf("Receive: %v", err)
	}
}

// drain works every question the loop can work now, each conversation's in order.
func (r *rig) drain() {
	for range 3 {
		r.loop.schedule(r.ctx)
		r.loop.workers.Wait()
	}
}

// questions is every question the store holds, by id.
func (r *rig) questions() map[string]store.AnswerQuestion {
	r.t.Helper()
	rows, err := r.st.Pool().Query(r.ctx, `select id from answer_questions`)
	if err != nil {
		r.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			r.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	found := map[string]store.AnswerQuestion{}
	for _, id := range ids {
		q, _, err := r.st.Question(r.ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		found[id] = q
	}
	return found
}

// only is the one question the store holds.
func (r *rig) only() store.AnswerQuestion {
	r.t.Helper()
	all := r.questions()
	if len(all) != 1 {
		r.t.Fatalf("questions = %+v, want one", all)
	}
	for _, q := range all {
		return q
	}
	return store.AnswerQuestion{}
}

// frame is a Dispatch targeted send's envelope payload.
func frame(eventType string, actor intake.DispatchActor, issue *string, payload map[string]any, delivery map[string]any) string {
	encoded, _ := json.Marshal(map[string]any{
		"event":    map[string]any{"type": eventType, "actor": actor, "issue_key": issue, "project": "LEGION", "payload": payload},
		"delivery": delivery,
	})
	return string(encoded)
}

// bare is a Dispatch event as a follower or author route carries it.
func bare(eventType string, actor intake.DispatchActor, issue *string, payload map[string]any) string {
	encoded, _ := json.Marshal(map[string]any{"type": eventType, "actor": actor, "issue_key": issue, "project": "LEGION", "payload": payload})
	return string(encoded)
}

var person = intake.DispatchActor{Kind: "user", ID: "person@example.com"}

func dispatchHandoff(session, payload string) intake.Handoff {
	return intake.Handoff{
		OriginalTopic: "notifications.agent." + session, TargetSession: session, RoleToken: string(testerToken), Project: project,
		Envelope: intake.HandoffEnvelope{EventID: "dispatch-1", Source: "dispatch", SourceEventID: "1", Topic: "notifications.agent." + session,
			DedupeKey: "dispatch-1", PayloadSummary: "LEGION-9", Payload: payload, TraceID: "t"},
	}
}

func messageHandoff(session, messageID string, attempt int, body string) intake.Handoff {
	return dispatchHandoff(session, frame("message.created", person, new("LEGION-9"),
		map[string]any{"id": messageID, "issue_key": "LEGION-9", "body": body, "target": "session:" + session},
		map[string]any{"attempt": attempt, "mode": "btw"}))
}

func envoyHandoff(topic, target, eventID, dedupeKey, text string) intake.Handoff {
	return intake.Handoff{
		OriginalTopic: topic, TargetSession: target, RoleToken: string(testerToken), Project: project,
		Envelope: intake.HandoffEnvelope{EventID: eventID, Source: "agent", SourceEventID: "s", SourceSession: "ses_asker", Topic: topic,
			DedupeKey: dedupeKey, PayloadSummary: text, TraceID: "t"},
	}
}

// fileLines is the session file's content.
func fileContent(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func describe(posts []request) string {
	var parts []string
	for _, p := range posts {
		parts = append(parts, fmt.Sprintf("%s %s %v", p.Method, p.Path, p.Body))
	}
	return strings.Join(parts, "; ")
}
