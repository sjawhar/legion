package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The controller is sent every hold — a phase worker whose budget ran out, and the architect's
// escalation, which the spec addresses to the controller — and the tree architect's own death,
// since the architect is who every other notice of its tree reaches. Each goes on the controller's
// role topic under the notice's own dedupe key, after the issue's topic, which is published exactly
// as before. The worker-died a phase worker's hold comes with stays the architect's.
func TestTheControllerIsSentEveryHoldAndTheTreeArchitectsDeath(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-2", Title: "Root", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	const issueTopic, controllerTopic = "notifications.legion.legion.LEGION-2", "notifications.role.legion-legion-controller"
	for i, tc := range []struct {
		name   string
		notice record.Notice
		want   []string
	}{
		{"a phase worker's budget ran out", record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, []string{issueTopic, controllerTopic}},
		{"the architect escalated", record.Notice{Kind: "held", Phase: phase.Planning, Reason: "escalated"}, []string{issueTopic, controllerTopic}},
		{"the tree architect's claim failed", record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Planning}, []string{issueTopic, controllerTopic}},
		{"a phase worker's claim failed", record.Notice{Kind: "worker-died", Role: claim.RolePlanner, Phase: phase.Planning}, []string{issueTopic}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := mustOutboxRow(t, "LEGION-2", tc.notice, time.Now())
			row.ID = int64(700 + i)
			publisher := &outboxPublisher{}
			if err := (&outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, log: quietLogger()}).execute(context.Background(), row); err != nil {
				t.Fatalf("execute %s: %v", tc.notice.Kind, err)
			}
			if got := publisher.topics(); !slices.Equal(got, tc.want) {
				t.Fatalf("topics = %v, want %v", got, tc.want)
			}
			for _, key := range publisher.keys() {
				if want := fmt.Sprintf("legion-outbox:%d", row.ID); key != want {
					t.Fatalf("dedupe key = %q, want the notice's own %q on every topic", key, want)
				}
			}
		})
	}
}

// A planner whose launches run out holds its issue, and the held notice reaches the controller —
// the Stage 4b `controller` checkpoint. Here no session holds the controller role when the notice
// is published: the listener refuses it, the outbox finishes its row rather than retrying it, and
// the notice is kept in Postgres. The daemon restarts before any controller holds the role. The
// notice then reaches the controller exactly once, under the key the refused publish carried, and
// the architect's issue topic has only the held and worker-died notices of the first publish.
//
// Each case isolates one trigger. A controller that registers after the restart and then claims
// its role, as `legion controller start`'s Oh My Pi does, is delivered to by its registration: the
// controller watch is an hour away. A controller that registered before the notice, and claimed
// its role while the daemon was down, is delivered to by the restarted daemon's controller watch:
// no registration or hold follows the restart.
func TestAHeldNoticeReachesTheControllerOnceAcrossARestart(t *testing.T) {
	for _, tc := range []struct {
		name          string
		registerFirst bool
		sweep         time.Duration
	}{
		{"the controller registers after the restart", false, time.Hour},
		{"the restarted daemon finds the registered controller holding its role", true, 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := newControllerListener()
			server := httptest.NewServer(listener)
			t.Cleanup(server.Close)
			cfg := workflowConfig(t, workflowNATS(t))
			cfg.EnvoyURL = server.URL
			cfg.LaunchFailureLimit = 1
			rt := fake.NewRuntime()
			rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")})
			o := fakeRuntime(rt, &built{})
			o.orphanSweep = 20 * time.Millisecond
			o.workflowTokens = &workflowTokenRecorder{}
			d := startDaemon(t, cfg, o)
			register := func() {
				t.Helper()
				status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", struct{}{}, true)
				if status != http.StatusOK {
					t.Fatalf("controller secret = %d; body %s", status, body)
				}
				var capability api.ControllerSecretResponse
				if err := json.Unmarshal(body, &capability); err != nil {
					t.Fatalf("decode %s: %v", body, err)
				}
				status, body = d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
					BootToken: capability.Secret, SessionID: "ses_controller", OmpSessionFile: "/sessions/ses_controller.jsonl",
					AgentID: "ses_controller", PluginContract: api.GoDaemonAPIVersion,
				}, false)
				if status != http.StatusOK {
					t.Fatalf("register the controller = %d; body %s", status, body)
				}
			}
			if tc.registerFirst {
				register()
			}

			token, err := claim.ProjectToken(cfg.Project)
			if err != nil {
				t.Fatal(err)
			}
			issue := cfg.Project + "-1"
			issueTopic := "notifications.legion." + token + "." + issue
			controllerTopic := "notifications.role." + string(claim.ControllerToken(token))
			pool, err := pgxpool.New(context.Background(), cfg.PostgresDSN)
			if err != nil {
				t.Fatalf("open the daemon's database: %v", err)
			}
			t.Cleanup(pool.Close)
			count := func(query string) int {
				t.Helper()
				var rows int
				if err := pool.QueryRow(context.Background(), query, issue).Scan(&rows); err != nil {
					t.Fatalf("%s: %v", query, err)
				}
				return rows
			}
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return record.NewStore().PutIssue(context.Background(), tx, record.Issue{
					Key: issue, Tree: issue, Project: cfg.Project, Title: "held", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U",
				})
			}); err != nil {
				t.Fatalf("record the issue in planning: %v", err)
			}
			// The one launch the limit allows fails, and the operator is answered with it; the claim
			// is failed, and the workflow holds the issue.
			if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{Tree: issue, Issue: issue, Role: claim.RolePlanner, Prompt: "Plan it."}, true); status != http.StatusInternalServerError || !strings.Contains(string(body), "pane launch failed") {
				t.Fatalf("spawn the planner = %d %s, want its one launch refused", status, body)
			}

			// The outbox publishes the held and worker-died notices in turn and finishes both rows,
			// the held one's by keeping its controller notice rather than retrying it.
			eventually(t, "both notices published, their outbox rows finished, and the held one kept", func() bool {
				return count("select count(*) from outbox where issue = $1") == 0 && len(listener.acceptedOn(issueTopic)) == 2 &&
					len(listener.refusedOn(controllerTopic)) > 0 && count("select count(*) from controller_notices where issue = $1") == 1
			})
			refused := listener.refusedOn(controllerTopic)[0]
			if refused.Message != "held on "+issue {
				t.Fatalf("the controller's notice = %+v, want held on %s", refused, issue)
			}
			onIssue := listener.acceptedOn(issueTopic)
			if onIssue[0].Message != "held on "+issue || onIssue[1].Message != "worker-died on "+issue || onIssue[0].DedupeKey != refused.DedupeKey {
				t.Fatalf("the issue topic took %+v, want held then worker-died, the held one under %s", onIssue, refused.DedupeKey)
			}

			d.stop()
			if tc.registerFirst {
				listener.claim(string(claim.ControllerToken(token)), "ses_controller")
			}
			restarted := o
			restarted.orphanSweep = tc.sweep
			d = startDaemon(t, cfg, restarted)
			if !tc.registerFirst {
				register()
				listener.claim(string(claim.ControllerToken(token)), "ses_controller")
			}

			// Delivered, and no longer kept: one goroutine delivers, removing each notice once the
			// listener takes it, so nothing is left to publish again.
			eventually(t, "the held notice delivered on the controller's role and no longer kept", func() bool {
				return len(listener.acceptedOn(controllerTopic)) > 0 && count("select count(*) from controller_notices where issue = $1") == 0
			})
			if tc.sweep < time.Minute {
				lookups := listener.lookups()
				eventually(t, "the controller watch to find the controller alive five more times", func() bool {
					return listener.lookups() >= lookups+5
				})
			}
			delivered := listener.acceptedOn(controllerTopic)
			if len(delivered) != 1 || delivered[0].Message != "held on "+issue || delivered[0].DedupeKey != refused.DedupeKey {
				t.Fatalf("the controller took %+v, want the held notice once under %s", delivered, refused.DedupeKey)
			}
			var payload record.Notice
			if err := json.Unmarshal([]byte(delivered[0].Payload), &payload); err != nil {
				t.Fatalf("decode the delivered payload %q: %v", delivered[0].Payload, err)
			}
			if want := (record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}); payload != want {
				t.Fatalf("the controller's payload = %+v, want %+v", payload, want)
			}
			if onIssue := listener.acceptedOn(issueTopic); len(onIssue) != 2 {
				t.Fatalf("the issue topic took %+v after the controller held its role, want its two notices only", onIssue)
			}
		})
	}
}

// controllerListener is the Envoy listener as the daemon uses it: the publish route, which refuses
// a role topic no session holds 404 with the listener's reason (packages/envoy/cmd/listener/api.go
// writeRoleHolderError), and the role lookup the controller watch reads.
type controllerListener struct {
	mu       sync.Mutex
	holders  map[string]string
	accepted []listenerPublish
	refused  []listenerPublish
	lookedUp int
}

type listenerPublish struct {
	Topic     string `json:"topic"`
	Message   string `json:"message"`
	Payload   string `json:"payload"`
	DedupeKey string `json:"dedupe_key"`
}

func newControllerListener() *controllerListener {
	return &controllerListener{holders: map[string]string{}}
}

func (l *controllerListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/publish":
		var published listenerPublish
		if err := json.NewDecoder(r.Body).Decode(&published); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if role, ok := strings.CutPrefix(published.Topic, "notifications.role."); ok && l.holders[role] == "" {
			l.refused = append(l.refused, published)
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no holder for role " + role, "reason": "unclaimed"})
			return
		}
		l.accepted = append(l.accepted, published)
		_ = json.NewEncoder(w).Encode(map[string]string{})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/roles/"):
		l.lookedUp++
		role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		if l.holders[role] == "" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no holder for role " + role, "reason": "unclaimed"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"holder": l.holders[role], "last_seen": time.Now().UnixMilli()})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

// claim is a session claiming role, as the plugin's claimEnvoyRole does after its registration.
func (l *controllerListener) claim(role, session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holders[role] = session
}

func (l *controllerListener) acceptedOn(topic string) []listenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return onTopic(l.accepted, topic)
}

func (l *controllerListener) refusedOn(topic string) []listenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return onTopic(l.refused, topic)
}

func (l *controllerListener) lookups() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lookedUp
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
