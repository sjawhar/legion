package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A planner whose launches run out holds its issue: the architect's issue topic takes the held and
// worker-died notices once each, and the controller's topic takes the held one from an outbox row
// of its own. Here the listener refuses the controller's topic twice first, as a stream outage
// answers: only the controller's row retries, so the architect is sent the held notice once however
// long the controller's topic keeps failing, and every try carries the one key the controller's
// subscriber dedupes on. The controller reads the notice the issue topic carries, without the row's
// routing.
func TestAHeldNoticeReachesTheControllerTopicAndItsRetriesNeverResendTheArchitects(t *testing.T) {
	listener := newNoticeListener()
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.EnvoyURL = server.URL
	cfg.LaunchFailureLimit = 1
	rt := fake.NewRuntime()
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")})
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	d := startDaemon(t, cfg, o)

	token, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	issue := cfg.Project + "-1"
	issueTopic, controllerTopic := notify.Topic(token, issue), notify.ControllerTopic(token)
	listener.refuseFirst(controllerTopic, 2)
	pool, err := pgxpool.New(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
			Key: issue, Tree: issue, Project: cfg.Project, Title: "held", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U",
		})
	}); err != nil {
		t.Fatalf("record the issue in planning: %v", err)
	}
	// The one launch the limit allows fails, and the operator is answered with it; the claim is
	// failed, and the workflow holds the issue.
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RolePlanner, Prompt: "Plan it."}, true); status != http.StatusInternalServerError || !strings.Contains(string(body), "pane launch failed") {
		t.Fatalf("spawn the planner = %d %s, want its one launch refused", status, body)
	}

	eventually(t, "every notice row finished and the controller's held notice taken", func() bool {
		var rows int
		if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1", issue).Scan(&rows); err != nil {
			t.Fatalf("count the issue's outbox rows: %v", err)
		}
		return rows == 0 && len(listener.acceptedOn(controllerTopic)) == 1
	})
	onIssue := listener.acceptedOn(issueTopic)
	if len(onIssue) != 2 || onIssue[0].Message != "held on "+issue || onIssue[1].Message != "worker-died on "+issue {
		t.Fatalf("the issue topic took %+v, want held then worker-died, once each", onIssue)
	}
	refused, taken := listener.refusedOn(controllerTopic), listener.acceptedOn(controllerTopic)[0]
	if len(refused) != 2 {
		t.Fatalf("the controller's topic refused %d publishes, want the 2 it was set to refuse", len(refused))
	}
	for _, try := range refused {
		if try.DedupeKey != taken.DedupeKey {
			t.Fatalf("a refused try carried key %q and the taken one %q, want one key across the row's tries", try.DedupeKey, taken.DedupeKey)
		}
	}
	if taken.Message != "held on "+issue {
		t.Fatalf("the controller took %+v, want held on %s", taken, issue)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(taken.Payload), &payload); err != nil {
		t.Fatalf("decode the controller's payload %q: %v", taken.Payload, err)
	}
	if want := map[string]any{"kind": "held", "role": "planner", "phase": "planning"}; len(payload) != len(want) ||
		payload["kind"] != want["kind"] || payload["role"] != want["role"] || payload["phase"] != want["phase"] {
		t.Fatalf("the controller's payload = %v, want %v", payload, want)
	}
}

// noticeListener is the Envoy listener's publish route as the outbox uses it: it takes every
// publish, but for the first refusals on a topic, which it answers 500 as a stream outage does
// (packages/envoy/cmd/listener/api.go publishHandler).
type noticeListener struct {
	mu       sync.Mutex
	refuse   map[string]int
	accepted []listenerPublish
	refused  []listenerPublish
}

type listenerPublish struct {
	Topic     string `json:"topic"`
	Message   string `json:"message"`
	Payload   string `json:"payload"`
	DedupeKey string `json:"dedupe_key"`
}

func newNoticeListener() *noticeListener {
	return &noticeListener{refuse: map[string]int{}}
}

func (l *noticeListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		return
	}
	var published listenerPublish
	if err := json.NewDecoder(r.Body).Decode(&published); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if l.refuse[published.Topic] > 0 {
		l.refuse[published.Topic]--
		l.refused = append(l.refused, published)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "nats: no response from stream"})
		return
	}
	l.accepted = append(l.accepted, published)
	_ = json.NewEncoder(w).Encode(map[string]string{})
}

// refuseFirst makes the listener refuse the next n publishes to topic.
func (l *noticeListener) refuseFirst(topic string, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refuse[topic] = n
}

func (l *noticeListener) acceptedOn(topic string) []listenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return onTopic(l.accepted, topic)
}

func (l *noticeListener) refusedOn(topic string) []listenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return onTopic(l.refused, topic)
}

func onTopic(published []listenerPublish, topic string) []listenerPublish {
	var matched []listenerPublish
	for _, p := range published {
		if p.Topic == topic {
			matched = append(matched, p)
		}
	}
	return matched
}
