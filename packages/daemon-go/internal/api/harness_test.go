package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	testProject       = "legion"
	testOperatorToken = "operator-bearer-for-tests"
)

// testStore is a migrated database of the test's own on the devbox or CI Postgres: the routes
// are held to what a real store records, and tests never see each other's rows.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test the claim routes against. Start one and set it:\n" +
			"  docker run -d --name legion-pg -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16\n" +
			"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./internal/api/")
	}
	ctx := context.Background()
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	name := "legion_api_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	testURL := *base
	testURL.Path = "/" + name
	st, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}
	return st
}

// stillClock is time that never moves: a machine's timers are armed and never fire, so a route
// test sees only what its own requests did.
type stillClock struct{}

func (stillClock) Now() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }

func (stillClock) AfterFunc(time.Duration, func()) supervise.Cancel { return stopped{} }

type stopped struct{}

func (stopped) Stop() bool { return true }

// fixedSpecs is a launch the fake runtime accepts: the machine fills in who it is for.
type fixedSpecs struct{}

func (fixedSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// testSupervisor is the daemon's claim directory as the routes see it, over real machines and a
// real store.
type testSupervisor struct {
	ctx      context.Context
	deps     supervise.Deps
	store    *store.Store
	mu       sync.Mutex
	machines map[claim.Token]*supervise.Machine
	prompts  map[claim.Token]string
}

func (s *testSupervisor) Machine(token claim.Token) (*supervise.Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[token]
	return m, ok
}

func (s *testSupervisor) Create(ctx context.Context, c supervise.Claim, rolePrompt string) (*supervise.Machine, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.machines[c.Token]; ok {
		return m, false, nil
	}
	if err := s.deps.Store.PutClaim(ctx, c); err != nil {
		return nil, false, err
	}
	m, err := supervise.NewMachine(s.ctx, s.deps, c)
	if err != nil {
		return nil, false, err
	}
	s.machines[c.Token] = m
	s.prompts[c.Token] = rolePrompt
	return m, true, nil
}

func (s *testSupervisor) Claims(ctx context.Context) ([]supervise.Claim, error) {
	return s.store.Claims(ctx)
}

type harness struct {
	t          *testing.T
	ctx        context.Context
	store      *store.Store
	runtime    *fake.Runtime
	conns      *fake.Conns
	tokens     *BootTokens
	supervisor *testSupervisor
	handler    http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := testStore(t)
	tokens := NewBootTokens(st)
	rt, conns := fake.NewRuntime(), fake.NewConns()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := &testSupervisor{
		ctx: ctx,
		deps: supervise.Deps{
			Runtime: rt, Conns: conns, Store: tokens.Recording(st), Specs: fixedSpecs{}, Clock: stillClock{}, Log: quiet,
			Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
			Timeouts: supervise.Timeouts{
				Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 30 * time.Second,
			},
		},
		store:    st,
		machines: map[claim.Token]*supervise.Machine{},
		prompts:  map[claim.Token]string{},
	}
	server := NewServer("127.0.0.1", 8437, Options{
		Supervisor: sup, BootTokens: tokens, Project: testProject, OperatorToken: testOperatorToken, Log: quiet,
	})
	return &harness{t: t, ctx: ctx, store: st, runtime: rt, conns: conns, tokens: tokens, supervisor: sup, handler: server.Handler}
}

// request sends one request through the server's own handler. A nil body sends none; a string is
// sent as it is, anything else as its JSON.
func (h *harness) request(method, target string, body any, header http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	switch body := body.(type) {
	case nil:
	case string:
		reader = bytes.NewBufferString(body)
	default:
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, target, reader)
	for name, values := range header {
		req.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

func (h *harness) operator(method, target string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(method, target, body, http.Header{"Authorization": {"Bearer " + testOperatorToken}})
}

// launch creates a claim the way the spawn route does and launches it, returning its token and
// the boot token its launch minted.
func (h *harness) launch(issue string, role claim.Role) (claim.Token, string) {
	h.t.Helper()
	token, err := claim.NewToken(testProject, issue, role)
	if err != nil {
		h.t.Fatalf("token: %v", err)
	}
	m, _, err := h.supervisor.Create(h.ctx, supervise.Claim{
		Token: token, Project: testProject, Tree: issue, Issue: issue, Role: role, State: supervise.StateQueued,
	}, "role prompt")
	if err != nil {
		h.t.Fatalf("create %s: %v", token, err)
	}
	if err := m.Handle(h.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		h.t.Fatalf("spawn %s: %v", token, err)
	}
	return token, h.bootToken(token)
}

// bootToken is the boot token the claim's latest launch carried.
func (h *harness) bootToken(token claim.Token) string {
	h.t.Helper()
	calls := h.runtime.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if (calls[i].Method == "Spawn" || calls[i].Method == "Resume") && calls[i].Spec.Claim == token {
			return calls[i].Spec.BootToken
		}
	}
	h.t.Fatalf("no launch of %s was made", token)
	return ""
}

// relaunch has the claim's process found gone, so the machine launches it again at the next
// generation, and returns the new launch's boot token.
func (h *harness) relaunch(token claim.Token) string {
	h.t.Helper()
	m, _ := h.supervisor.Machine(token)
	loc := m.Claim().Locator
	if loc == nil {
		h.t.Fatalf("%s has no process to lose", token)
	}
	if err := m.Handle(h.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *loc, Kind: runtime.Gone}}); err != nil {
		h.t.Fatalf("lose %s's process: %v", token, err)
	}
	return h.bootToken(token)
}

func (h *harness) stored(token claim.Token) supervise.Claim {
	h.t.Helper()
	claims, err := h.store.Claims(h.ctx)
	if err != nil {
		h.t.Fatalf("read the claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	h.t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

func (h *harness) register(bootToken, session string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: "agent-" + session, PluginContract: 1,
	}, nil)
}

// registered registers session on the launch bootToken names and returns what the agent learned.
func (h *harness) registered(bootToken, session string) claim.RegisterResponse {
	h.t.Helper()
	recorder := h.register(bootToken, session)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("register %s = %d, want 200; body %s", session, recorder.Code, recorder.Body)
	}
	var response claim.RegisterResponse
	decodeInto(h.t, recorder, &response)
	return response
}

func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode the body: %v", err)
	}
}

// wantRefusal holds a response to the status and the sentence of one refusal.
func wantRefusal(t *testing.T, recorder *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body, err)
	}
	if body["error"] != message {
		t.Fatalf("refusal = %q, want %q", body["error"], message)
	}
	if len(body) != 1 {
		t.Fatalf("refusal body %s carries more than its sentence", recorder.Body)
	}
}

func sortedTokens(claims []OperatorClaim) []string {
	tokens := make([]string, len(claims))
	for i, c := range claims {
		tokens[i] = string(c.Token)
	}
	sort.Strings(tokens)
	return tokens
}
