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
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
		PassOn:    controllerPassOn{pool: st.Pool(), records: record.NewStore(), now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Decisions: routedDecisions{},
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

// A question passed to the controller is one agent-question notice, however often PassOn is
// called for it (a restart that finds it not yet marked passed on calls it again, also after the
// notice went): one controller notice row, published once to the controller topic, whose body
// names the question's id, issue, role, session, asker, ref, text and reason.
func TestOutboxAgentQuestionIsQueuedAndDeliveredOncePerQuestion(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	passOn := controllerPassOn{pool: pool, records: records, now: func() time.Time { return now }, log: quietLogger()}
	q := store.AnswerQuestion{ID: "5f0c2a9e7b14d3866a1e0c4b9d2f7a35", Issue: "LEGION-9", Role: claim.RoleTester, Session: "ses_tester"}
	origin := answerloop.Origin{Text: "Is PR 7 green?", Asker: "user:person@example.com", Ref: "dispatch://LEGION-9/message/m-1"}

	for range 2 {
		if err := passOn.PassOn(ctx, q, answerloop.ReasonNotAnswered, origin); err != nil {
			t.Fatal(err)
		}
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows after passing the question twice = %d, want one", rows)
	}
	publisher := &outboxPublisher{}
	runner := &outbox{pool: pool, project: "legion", dispatchProject: "LEGION", records: records, notices: publisher, now: func() time.Time { return now }, log: quietLogger()}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := passOn.PassOn(ctx, q, answerloop.ReasonNotAnswered, origin); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	published := publisher.publishes()
	if len(published) != 1 || outboxRows(t, pool) != 0 {
		t.Fatalf("publishes = %+v with %d rows left; want one and none", published, outboxRows(t, pool))
	}
	if published[0].topic != "notifications.legion.legion.controller" || published[0].message != "agent-question on LEGION-9" {
		t.Fatalf("published to %q as %q; want the controller topic, as an agent-question on the agent's issue", published[0].topic, published[0].message)
	}
	body, err := json.Marshal(published[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"agent-question","role":"tester","reason":"not_answered","question":{"id":"5f0c2a9e7b14d3866a1e0c4b9d2f7a35",` +
		`"issue":"LEGION-9","session":"ses_tester","asker":"user:person@example.com","ref":"dispatch://LEGION-9/message/m-1","text":"Is PR 7 green?"}}`
	if string(body) != want {
		t.Fatalf("notice body = %s, want %s", body, want)
	}
}

// A person's decision on an ask a stopped tester of a parked child opened goes on as one
// agent-decision notice: to the tree's architect while it holds its role, otherwise (its claim
// suspended, none the daemon supervises, or its tree lingering) to the controller. Handed off again,
// again after a restart, and again after the notice went, it queues nothing more, and the notice
// names the decision's id, the agent's issue, role and session, the person, where and what they
// decided.
func TestADecisionGoesOnceToTheRunningArchitectElseToTheController(t *testing.T) {
	const controller = "notifications.legion.legion.controller"
	for _, tc := range []struct {
		name      string
		architect supervise.ClaimState
		lingers   bool
		toArch    bool
	}{
		{name: "the tree's architect holds its role", architect: supervise.StateWorking, toArch: true},
		{name: "the tree's architect is suspended", architect: supervise.StateSuspended},
		{name: "no architect of the tree is supervised"},
		{name: "the tree lingers", architect: supervise.StateWorking, lingers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			root := "LEGION-1"
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-4", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: root, Parent: &root, Title: "Parked child", Phase: phase.Done, Generation: 1, Status: "backlog"})
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			if tc.architect != "" {
				architectClaim(t, sup, root, tc.architect)
			}
			publisher := &holderPublisher{}
			now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
			// A restart builds the outbox and the decisions anew over the same database.
			boot := func() routedDecisions {
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
					project: "legion", now: func() time.Time { return now }}
				return routedDecisions{outbox: runner, now: func() time.Time { return now }, log: quietLogger()}
			}
			decision := answerloop.Decision{ID: "dispatch:34", Asker: "user:person@example.com", Ref: "dispatch://LEGION-4/ask/a-1", Text: "Selected: B\n\nUse B.",
				Conversation: store.AgentConversation{Session: "ses_tester", Project: "legion", Issue: "LEGION-4", Role: claim.RoleTester}}
			decide := func(d routedDecisions) {
				t.Helper()
				if err := d.Decide(ctx, decision); err != nil {
					t.Fatal(err)
				}
			}
			first := boot()
			decide(first)
			decide(first)
			restarted := boot()
			decide(restarted)
			if rows := outboxRows(t, pool); rows != 1 {
				t.Fatalf("outbox rows after the decision was handed off three times = %d, want one", rows)
			}
			if err := restarted.outbox.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			decide(restarted)
			if err := restarted.outbox.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			_, delivered := publisher.snapshot()
			if len(delivered) != 1 || outboxRows(t, pool) != 0 {
				t.Fatalf("publishes = %+v with %d rows left; want one and none", delivered, outboxRows(t, pool))
			}
			want := controller
			if tc.toArch {
				want = architectTopic(t, root)
			}
			if delivered[0].topic != want || delivered[0].message != "agent-decision on LEGION-4" {
				t.Fatalf("published to %q as %q; want %q, as an agent-decision on the agent's issue", delivered[0].topic, delivered[0].message, want)
			}
			body, err := json.Marshal(delivered[0].payload)
			if err != nil {
				t.Fatal(err)
			}
			wantBody := `{"kind":"agent-decision","role":"tester","agent_decision":{"id":"dispatch:34","issue":"LEGION-4","session":"ses_tester",` +
				`"asker":"user:person@example.com","ref":"dispatch://LEGION-4/ask/a-1","text":"Selected: B\n\nUse B."}}`
			if string(body) != wantBody {
				t.Fatalf("notice body = %s, want %s", body, wantBody)
			}
		})
	}
}

// `legion state` lists a question passed to the controller for seven days after it was passed,
// and not after, newest first, with who asked and where as its hand-off says; a question never
// passed on is not listed.
func TestQuestionsForControllerListsAPassedQuestionForSevenDays(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testpg.Database(t, "legion_daemon_passed_questions"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	passOn := controllerPassOn{pool: st.Pool(), records: record.NewStore(), now: func() time.Time { return now }, log: quietLogger()}
	question := func(id, event string) store.AnswerQuestion {
		t.Helper()
		handoff := intake.Handoff{
			OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester", RoleToken: string(gateToken), Project: "legion",
			Envelope: intake.HandoffEnvelope{EventID: event, Source: "agent", SourceEventID: "s", SourceSession: "ses_asker",
				Topic: "notifications.agent.ses_tester", DedupeKey: "agent.ses_tester." + id, PayloadSummary: "Did it pass?", TraceID: "t",
				Sender: &intake.HandoffSender{SessionID: "ses_asker", Title: "LEGION-9 implementer"}},
		}
		raw, err := json.Marshal(handoff)
		if err != nil {
			t.Fatal(err)
		}
		q, _, err := st.ReceiveQuestion(ctx, store.AnswerQuestion{
			ID: id, Project: "legion", Identity: handoff.Envelope.DedupeKey, Session: "ses_tester", Token: gateToken, Issue: "LEGION-9",
			Role: claim.RoleTester, Kind: "envoy", MayAskNothing: true, Handoff: raw, ReceivedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	pass := func(q store.AnswerQuestion, reason answerloop.Reason, at time.Time) {
		t.Helper()
		q.PassReason = string(reason)
		q.State, q.UpdatedAt = store.QuestionAppended, at
		if err := st.UpdateQuestion(ctx, q, store.QuestionReceived); err != nil {
			t.Fatal(err)
		}
		if err := passOn.PassOn(ctx, q, reason, answerloop.Origin{Asker: "session:ses_asker (LEGION-9 implementer)", Ref: "envoy:evt"}); err != nil {
			t.Fatal(err)
		}
		q.State = store.QuestionPassedOn
		if err := st.UpdateQuestion(ctx, q, store.QuestionAppended); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, "update processed_events set processed_at = $3 where source = $1 and event_id = $2",
			store.QuestionPassedSource, q.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	older, newer := now.Add(-7*24*time.Hour+time.Minute), now.Add(-time.Hour)
	pass(question("q-older", "evt-older"), answerloop.ReasonNeedsWork, older)
	pass(question("q-newer", "evt-newer"), answerloop.Reason("answer_seconds"), newer)
	question("q-answered", "evt-answered")

	listed := func(at time.Time) []api.QuestionForController {
		t.Helper()
		var questions []api.QuestionForController
		if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
			var err error
			questions, err = questionsForController(ctx, tx, st, "legion", at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return questions
	}
	entry := func(id, event, reason string, at time.Time) api.QuestionForController {
		return api.QuestionForController{ID: id, Issue: "LEGION-9", Role: claim.RoleTester, Session: "ses_tester",
			Asker: "session:ses_asker (LEGION-9 implementer)", Ref: "envoy:" + event, Reason: reason, PassedAt: at}
	}
	newerEntry, olderEntry := entry("q-newer", "evt-newer", "answer_seconds", newer), entry("q-older", "evt-older", "needs_work", older)
	if got := listed(now); !reflect.DeepEqual(got, []api.QuestionForController{newerEntry, olderEntry}) {
		t.Fatalf("questions for the controller = %+v, want the newer and then the older", got)
	}
	// Seven days and a second after the older was passed it is no longer listed.
	later := older.Add(api.QuestionsForControllerWindow + time.Second)
	if got := listed(later); !reflect.DeepEqual(got, []api.QuestionForController{newerEntry}) {
		t.Fatalf("questions for the controller seven days after the older was passed = %+v, want the newer alone", got)
	}
}
