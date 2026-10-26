package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

type noTool struct{}

func (noTool) Run(context.Context, int, string) ghtool.Result { return ghtool.Result{} }
func (noTool) Lookup(context.Context, int) (ghtool.Found, error) {
	return ghtool.Found{}, ghtool.ErrNotAWrite
}
func (noTool) Close() error { return nil }
func (noTool) Open(context.Context, ghtool.Agent, string) (answerloop.Tool, error) {
	return noTool{}, nil
}

type noFacts struct{}

func (noFacts) Facts(context.Context, string) (answerloop.IssueFacts, error) {
	return answerloop.IssueFacts{}, nil
}

// The race the gate exists for, through the real gate and the loop: the stopped tester's claim is
// started while its answer is being generated. The start passes its check, the answer is dropped
// with nothing added to the conversation, and the question goes to the running agent on its role
// topic (A7).
func TestAClaimStartedDuringAnAnswerGetsTheQuestionAndItsConversationNothing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testpg.Database(t, "legion_daemon_answers"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ses_tester.jsonl")
	session := `{"type":"session","version":3,"id":"s","timestamp":"2026-10-26T00:00:00.000Z","cwd":"/work"}` + "\n" +
		`{"id":"e0000001","parentId":null,"timestamp":"2026-10-26T00:00:00.000Z","type":"model_change","model":"anthropic/claude-opus-5-5"}` + "\n" +
		`{"id":"e0000002","parentId":"e0000001","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"Test it."}],"attribution":"user","timestamp":1}}` + "\n" +
		`{"id":"e0000003","parentId":"e0000002","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"Tested."}],"api":"anthropic-messages","provider":"anthropic","model":"claude-opus-5-5","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}` + "\n"
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if _, err := st.RegisterConversation(ctx, store.AgentConversation{
		Session: "ses_tester", Project: "legion", Issue: "LEGION-9", Role: claim.RoleTester, Token: gateToken,
		Harness: runtime.BackendOmp, Location: path, StartedAt: now, RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var published []map[string]any
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		published = append(published, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer listener.Close()
	posted := false
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer dispatchServer.Close()

	gate, _, views := newTestGate(t)
	loop := answerloop.New(answerloop.Config{
		Project: "legion",
		Answers: config.Answers{Bounds: config.AnswerBounds{Answer: time.Minute, ModelCalls: 1, OutputTokens: 100, Concurrent: 1, DailyUSD: 50}},
		Store:   st, Dispatch: dispatch.New(dispatchServer.URL, "t"), Envoy: notify.New(listener.URL, "t"), Gate: gate,
		Issues: noFacts{}, Tools: noTool{}, Conversations: answerloop.Harnesses{},
		PassOn: loggedPassOn{slog.New(slog.NewTextHandler(io.Discard, nil))}, Decisions: loggedDecisions{slog.New(slog.NewTextHandler(io.Discard, nil))},
		Answer: func(ctx context.Context, req answer.Request) (answer.Reply, error) {
			// The operator resumes the tester while its answer is generated.
			if err := gate.CheckLaunch(ctx, views.view.Claim); err != nil {
				t.Errorf("the start's check: %v", err)
			}
			views.set(supervise.StateLaunching, 4)
			return answer.Reply{Kind: answer.Answered, Text: "Tested.", Append: conversation.Append{
				QuestionID: req.Question.ID, Question: req.Question.Text, Provider: "anthropic", API: "anthropic-messages", Model: "claude-opus-5-5",
				Turns: []conversation.Turn{{Content: []conversation.Block{{Type: conversation.Text, Text: "Tested."}}}},
			}}, nil
		},
		Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	handoff := intake.Handoff{
		OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: string(gateToken), Project: "legion",
		Envelope: intake.HandoffEnvelope{EventID: "evt-1", Source: "agent", SourceEventID: "s", SourceSession: "ses_asker",
			Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester.0123456789abcdef0123456789abcdef", PayloadSummary: "Did it pass?", TraceID: "t"},
	}
	raw, _ := json.Marshal(handoff)
	if err := loop.Receive(ctx, handoff, raw); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { loop.Run(running); close(done) }()
	testwait.Eventually(t, "the question forwarded to the running tester", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(published) == 1
	})
	stop()
	<-done
	if content, _ := os.ReadFile(path); string(content) != session {
		t.Fatalf("the conversation changed:\n%s", content[len(session):])
	}
	if published[0]["topic"] != "notifications.role."+string(gateToken) || published[0]["handoff"] != false || posted {
		t.Fatalf("listener got %v, Dispatch posted %t; want the question on the tester's role topic alone", published, posted)
	}
}
