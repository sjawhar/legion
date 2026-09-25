package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	claimsProject       = "legion"
	claimsOperatorToken = "operator-bearer-for-claims-tests"
	architectClaim      = legionclaim.Token("legion-legion-legion-208-architect")
	operatorRoutes      = "/legion/v1/operator/"
)

// stillTime never moves: a machine's timers are armed and never fire, so a test sees only what
// its own requests did.
type stillTime struct{}

func (stillTime) Now() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }

func (stillTime) AfterFunc(time.Duration, func()) supervise.Cancel { return neverFires{} }

type neverFires struct{}

func (neverFires) Stop() bool { return true }

// anyLaunch is a launch the fake runtime accepts: the machine fills in who it is for.
type anyLaunch struct{}

func (anyLaunch) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/dev/null"}}}, nil
}

// memoryClaims is the daemon's claims as the routes reach them: real machines, over a store kept
// in memory — persistence is not what `legion claims` is held to. It is the routes' Supervisor,
// the machines' Store, and the boot tokens' store at once.
type memoryClaims struct {
	ctx  context.Context
	deps supervise.Deps

	mu       sync.Mutex
	machines map[legionclaim.Token]*supervise.Machine

	rowsMu sync.Mutex
	rows   map[legionclaim.Token]supervise.Claim
}

func (s *memoryClaims) Machine(token legionclaim.Token) (*supervise.Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[token]
	return m, ok
}

func (s *memoryClaims) Create(ctx context.Context, c supervise.Claim, _ string) (*supervise.Machine, bool, error) {
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
	return m, true, nil
}

func (s *memoryClaims) Claims(context.Context) ([]supervise.Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claims := make([]supervise.Claim, 0, len(s.machines))
	for _, m := range s.machines {
		claims = append(claims, m.Claim())
	}
	return claims, nil
}

func (s *memoryClaims) PutClaim(_ context.Context, c supervise.Claim) error {
	s.rowsMu.Lock()
	defer s.rowsMu.Unlock()
	c.BootTokenHash = slices.Clone(c.BootTokenHash)
	s.rows[c.Token] = c
	return nil
}

func (s *memoryClaims) PutDelivery(context.Context, legionclaim.Token, supervise.Delivery) error {
	return nil
}

func (s *memoryClaims) RetireDelivery(context.Context, legionclaim.Token, string) error { return nil }

func (s *memoryClaims) ClaimByBootTokenHash(_ context.Context, hash []byte) (supervise.Claim, bool, error) {
	s.rowsMu.Lock()
	defer s.rowsMu.Unlock()
	for _, row := range s.rows {
		if bytes.Equal(row.BootTokenHash, hash) {
			return row, true, nil
		}
	}
	return supervise.Claim{}, false, nil
}

// seenRequest is one request that reached the daemon, and what the daemon answered it.
type seenRequest struct {
	method, path, authorization, contentType string
	body                                     []byte
	status                                   int
	answer                                   []byte
}

// operatorDaemon is the daemon's HTTP surface as `legion claims` reaches it: the real
// api.NewServer over real claim machines and a fake runtime, on a loopback port, with every
// request that reached it recorded.
type operatorDaemon struct {
	t         *testing.T
	runtime   *fake.Runtime
	url       string
	port      int
	tokenFile string

	mu   sync.Mutex
	seen []seenRequest
}

func newOperatorDaemon(t *testing.T) *operatorDaemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := fake.NewRuntime()
	claims := &memoryClaims{ctx: ctx, machines: map[legionclaim.Token]*supervise.Machine{}, rows: map[legionclaim.Token]supervise.Claim{}}
	tokens := api.NewBootTokens(claims)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	claims.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: tokens.Recording(claims), Specs: anyLaunch{}, Clock: stillTime{}, Log: quiet,
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
		Timeouts: supervise.Timeouts{
			Boot: time.Minute, RegistrationIntervals: 3, RPC: 5 * time.Second, Probe: 30 * time.Second,
		},
	}
	handler := api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: claims, BootTokens: tokens, Project: claimsProject, OperatorToken: claimsOperatorToken, Log: quiet,
	}).Handler

	d := &operatorDaemon{t: t, runtime: rt, tokenFile: writeFile(t, "operator-token", claimsOperatorToken+"\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the request body: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		d.mu.Lock()
		d.seen = append(d.seen, seenRequest{
			method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: body, status: recorder.Code, answer: recorder.Body.Bytes(),
		})
		d.mu.Unlock()
		for name, values := range recorder.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	d.port = server.Listener.Addr().(*net.TCPAddr).Port
	return d
}

// writeFile is a 0600 file of the test's own holding contents.
func writeFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// reach is where the daemon is and the bearer's file: the flags every subcommand takes.
func (d *operatorDaemon) reach() []string {
	return []string{"--port", strconv.Itoa(d.port), "--operator-token-file", d.tokenFile}
}

// claims runs `legion claims <args>` and holds its output to the one rule every run keeps: the
// operator token is in neither stream, whatever happened.
func (d *operatorDaemon) claims(args ...string) (int, string, string) {
	d.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "claims"}, args...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, claimsOperatorToken) {
			d.t.Fatalf("legion claims %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// succeed runs `legion claims <args>` and requires it to exit 0 having said nothing on stderr.
func (d *operatorDaemon) succeed(args ...string) string {
	d.t.Helper()
	code, out, errb := d.claims(args...)
	if code != 0 || errb != "" {
		d.t.Fatalf("legion claims %v = %d, stderr %q; want 0 and no stderr", args, code, errb)
	}
	return out
}

// operatorRequests is every operator request that reached the daemon, in order.
func (d *operatorDaemon) operatorRequests() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	var requests []seenRequest
	for _, seen := range d.seen {
		if strings.HasPrefix(seen.path, operatorRoutes) {
			requests = append(requests, seen)
		}
	}
	return requests
}

// lastOperatorRequest is the one operator request the latest command made.
func (d *operatorDaemon) lastOperatorRequest() seenRequest {
	d.t.Helper()
	requests := d.operatorRequests()
	if len(requests) == 0 {
		d.t.Fatal("no operator request reached the daemon")
	}
	return requests[len(requests)-1]
}

// wantNothingReached holds the daemon to having answered nothing at all.
func (d *operatorDaemon) wantNothingReached() {
	d.t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.seen) != 0 {
		d.t.Fatalf("%d requests reached the daemon, want none: first %s %s", len(d.seen), d.seen[0].method, d.seen[0].path)
	}
}

// post sends body to one of the agent's claim routes, as the plugin would.
func (d *operatorDaemon) post(path string, body any, into any) {
	d.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		d.t.Fatalf("encode %s: %v", path, err)
	}
	response, err := http.Post(d.url+path, "application/json", bytes.NewReader(encoded))
	if err != nil {
		d.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		d.t.Fatalf("read %s's answer: %v", path, err)
	}
	if response.StatusCode/100 != 2 {
		d.t.Fatalf("POST %s = %d %s", path, response.StatusCode, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			d.t.Fatalf("decode %s's answer %s: %v", path, answer, err)
		}
	}
}

// ready has the claim's agent register and declare itself ready, as the plugin does at boot.
func (d *operatorDaemon) ready(token legionclaim.Token) {
	d.t.Helper()
	var bootToken string
	for _, call := range d.runtime.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			bootToken = call.Spec.BootToken
		}
	}
	if bootToken == "" {
		d.t.Fatalf("no launch of %s was made", token)
	}
	var registered legionclaim.RegisterResponse
	d.post("/legion/v1/claims/register", legionclaim.RegisterRequest{
		BootToken: bootToken, SessionID: "ses_" + string(token), OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: "agent", PluginContract: 1,
	}, &registered)
	d.post("/legion/v1/claims/ready", legionclaim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_" + string(token), Secret: registered.Secret, Generation: registered.Generation,
	}, nil)
}

// spawnArgs is a valid spawn of the root architect of LEGION-208.
func (d *operatorDaemon) spawnArgs(extra ...string) []string {
	args := append([]string{"spawn"}, d.reach()...)
	args = append(args, "--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect",
		"--prompt-file", writeFile(d.t, "architect.md", "Reply ready and wait.\n"))
	return append(args, extra...)
}

// decodeStrict decodes body into into, refusing a member into does not have.
func decodeStrict(t *testing.T, body []byte, into any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// wantOperatorRequest holds a request to the route, the operator's bearer, and — when it carries
// one — a JSON body.
func wantOperatorRequest(t *testing.T, seen seenRequest, method, path string) {
	t.Helper()
	if seen.method != method || seen.path != path {
		t.Fatalf("request = %s %s, want %s %s", seen.method, seen.path, method, path)
	}
	if seen.authorization != "Bearer "+claimsOperatorToken {
		t.Fatalf("request %s carried Authorization %q, want the operator's bearer", path, seen.authorization)
	}
	if len(seen.body) > 0 && seen.contentType != "application/json" {
		t.Fatalf("request %s carried a body as %q, want application/json", path, seen.contentType)
	}
}

// The spawn: the claim named by its tree, issue, and role, the role prompt read from its file and
// sent as it is, the first task with it, the operator's bearer on the request — and the claim the
// daemon answered, printed on one line.
func TestClaimsSpawnPostsTheClaimWithTheOperatorBearer(t *testing.T) {
	d := newOperatorDaemon(t)

	out := d.succeed(d.spawnArgs("--task", "Say hello.")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims")
	var sent api.SpawnRequest
	decodeStrict(t, seen.body, &sent)
	want := api.SpawnRequest{
		Tree: "LEGION-208", Issue: "LEGION-208", Role: legionclaim.RoleArchitect, Prompt: "Reply ready and wait.\n", Task: "Say hello.",
	}
	if sent != want {
		t.Fatalf("spawn sent %+v, want %+v", sent, want)
	}
	if seen.status != http.StatusCreated {
		t.Fatalf("the daemon answered the spawn %d %s, want 201", seen.status, seen.answer)
	}
	if spawns := d.runtime.CallsOf("Spawn"); len(spawns) != 1 || spawns[0].Spec.Claim != architectClaim {
		t.Fatalf("spawns = %+v, want one launch of %s", spawns, architectClaim)
	}
	if want := "legion-legion-legion-208-architect architect LEGION-208 launching 1\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

// Omitting --prompt-file selects the daemon-composed role prompt. A caller must not send an empty
// prompt override, because an absent override is what lets a resumed claim use the same composed
// role parts.
func TestClaimsSpawnWithoutPromptFileUsesTheDaemonRolePrompt(t *testing.T) {
	d := newOperatorDaemon(t)

	args := append([]string{"spawn"}, d.reach()...)
	args = append(args, "--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect")
	d.succeed(args...)

	var sent map[string]any
	decodeStrict(t, d.lastOperatorRequest().body, &sent)
	if _, ok := sent["prompt"]; ok {
		t.Fatalf("spawn sent prompt override %q, want no prompt member", sent["prompt"])
	}
}

// A spawn without --task sends no task: the claim launches with nothing queued.
func TestClaimsSpawnWithoutATaskSendsNone(t *testing.T) {
	d := newOperatorDaemon(t)

	d.succeed(d.spawnArgs()...)

	var sent map[string]any
	decodeStrict(t, d.lastOperatorRequest().body, &sent)
	if _, ok := sent["task"]; ok {
		t.Fatalf("spawn sent %v, want no task member", sent)
	}
}

func TestClaimsDeliverPostsTheTask(t *testing.T) {
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)

	out := d.succeed(append(append([]string{"deliver"}, d.reach()...), "--claim", string(architectClaim), "--task", "Review the plan.")...)

	seen := d.lastOperatorRequest()
	wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/deliver")
	var sent api.DeliverRequest
	decodeStrict(t, seen.body, &sent)
	if sent != (api.DeliverRequest{Task: "Review the plan."}) {
		t.Fatalf("deliver sent %+v, want the task", sent)
	}
	var answered api.OperatorClaim
	decodeStrict(t, seen.answer, &answered)
	if answered.Pending == nil || answered.Pending.Task != "Review the plan." {
		t.Fatalf("the daemon answered pending %+v, want the task queued", answered.Pending)
	}
	if want := "legion-legion-legion-208-architect architect LEGION-208 launching 1\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

// suspend, resume, and stop each post their request, with no body, to the claim's route, and
// print the claim as the request left it.
func TestClaimsSuspendResumeAndStopDriveTheClaim(t *testing.T) {
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)
	d.ready(architectClaim)

	for _, step := range []struct {
		action, method, want string
	}{
		{"suspend", "Suspend", "legion-legion-legion-208-architect architect LEGION-208 suspended 1\n"},
		{"resume", "Resume", "legion-legion-legion-208-architect architect LEGION-208 launching 2\n"},
		{"stop", "Release", "legion-legion-legion-208-architect architect LEGION-208 retired 2\n"},
	} {
		out := d.succeed(append(append([]string{step.action}, d.reach()...), "--claim", string(architectClaim))...)

		seen := d.lastOperatorRequest()
		wantOperatorRequest(t, seen, http.MethodPost, "/legion/v1/operator/claims/"+string(architectClaim)+"/"+step.action)
		if len(seen.body) != 0 {
			t.Fatalf("%s sent a body %q, want none", step.action, seen.body)
		}
		if out != step.want {
			t.Fatalf("%s stdout = %q, want %q", step.action, out, step.want)
		}
		if calls := d.runtime.CallsOf(step.method); len(calls) != 1 {
			t.Fatalf("after %s the runtime saw %v, want one %s", step.action, d.runtime.Methods(), step.method)
		}
	}
}

// list prints one line per claim — token, role, issue, state, generation — in the daemon's token
// order, and --json prints the daemon's answer byte for byte.
func TestClaimsListPrintsOneLinePerClaim(t *testing.T) {
	d := newOperatorDaemon(t)
	d.succeed(d.spawnArgs()...)
	worker := append(append([]string{"spawn"}, d.reach()...), "--tree", "LEGION-208", "--issue", "LEGION-209",
		"--role", "implementer", "--prompt-file", writeFile(t, "implementer.md", "Implement."))
	d.succeed(worker...)
	d.ready(architectClaim)

	out := d.succeed(append([]string{"list"}, d.reach()...)...)

	wantOperatorRequest(t, d.lastOperatorRequest(), http.MethodGet, "/legion/v1/operator/claims")
	want := "legion-legion-legion-208-architect architect LEGION-208 ready 1\n" +
		"legion-legion-legion-209-implementer implementer LEGION-209 launching 1\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}

	out = d.succeed(append([]string{"list", "--json"}, d.reach()...)...)
	if answer := d.lastOperatorRequest().answer; out != string(answer) {
		t.Fatalf("list --json printed %q, want the daemon's answer %q verbatim", out, answer)
	}
}

func TestClaimsListOfNoClaimsPrintsNothing(t *testing.T) {
	d := newOperatorDaemon(t)

	if out := d.succeed(append([]string{"list"}, d.reach()...)...); out != "" {
		t.Fatalf("stdout = %q, want nothing", out)
	}
	if out := d.succeed(append([]string{"list", "--json"}, d.reach()...)...); out != "{\"claims\":[]}\n" {
		t.Fatalf("list --json = %q, want the daemon's empty list", out)
	}
}

// Every answer that is not a 2xx is the daemon's refusal: printed with its status and sentence on
// stderr, nothing on stdout, and the command fails — under --json too.
func TestClaimsPrintsTheDaemonsRefusalAndFails(t *testing.T) {
	for _, testCase := range []struct {
		name string
		// args builds the command against a daemon that holds a launched, not yet ready architect.
		args func(d *operatorDaemon) []string
		want string
	}{
		{
			name: "a request the claim's state refuses",
			args: func(d *operatorDaemon) []string {
				return append(append([]string{"suspend"}, d.reach()...), "--claim", string(architectClaim))
			},
			want: "the daemon answered 409 Conflict: suspend refused: the claim is launching (the agent is not ready, so there is nothing to suspend yet)",
		},
		{
			name: "the same refusal under --json",
			args: func(d *operatorDaemon) []string {
				return append(append([]string{"suspend", "--json"}, d.reach()...), "--claim", string(architectClaim))
			},
			want: "the daemon answered 409 Conflict: suspend refused: the claim is launching",
		},
		{
			name: "a claim the daemon does not hold",
			args: func(d *operatorDaemon) []string {
				return append(append([]string{"stop"}, d.reach()...), "--claim", "legion-legion-legion-1-tester")
			},
			want: "the daemon answered 404 Not Found: no claim legion-legion-legion-1-tester",
		},
		{
			name: "a spawn the daemon cannot name",
			args: func(d *operatorDaemon) []string {
				args := append([]string{"spawn"}, d.reach()...)
				return append(args, "--tree", "LEGION-208", "--issue", "LEGION-210", "--role", "controller",
					"--prompt-file", writeFile(d.t, "controller.md", "Control."))
			},
			want: `the daemon answered 400 Bad Request: role "controller" is not a role`,
		},
		{
			name: "a bearer the daemon does not hold",
			args: func(d *operatorDaemon) []string {
				return []string{"list", "--port", strconv.Itoa(d.port), "--operator-token-file", writeFile(d.t, "wrong", "not-the-operator-bearer\n")}
			},
			want: "the daemon answered 403 Forbidden: Invalid operator token",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			d := newOperatorDaemon(t)
			d.succeed(d.spawnArgs()...)
			args := testCase.args(d)

			code, out, errb := d.claims(args...)

			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr %q", code, errb)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want nothing", out)
			}
			if !strings.HasPrefix(errb, "legion claims "+args[0]+": ") || !strings.Contains(errb, testCase.want) {
				t.Fatalf("stderr = %q, want it to name the command and carry %q", errb, testCase.want)
			}
			if strings.Contains(errb, "not-the-operator-bearer") {
				t.Fatalf("stderr = %q names the bearer the command sent", errb)
			}
		})
	}
}

// A daemon that does not answer at all fails the command, naming where it looked.
func TestClaimsFailsWhenNoDaemonAnswers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	d := &operatorDaemon{t: t, tokenFile: writeFile(t, "operator-token", claimsOperatorToken)}

	code, out, errb := d.claims("list", "--port", strconv.Itoa(port), "--operator-token-file", d.tokenFile)

	if code != 1 || out != "" || !strings.Contains(errb, "127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 naming 127.0.0.1:%d", code, out, errb, port)
	}
}

// Without --port, the daemon is found the way `legion state` finds it: the configuration's bind
// and port.
func TestClaimsFindsTheDaemonFromTheConfiguration(t *testing.T) {
	d := newOperatorDaemon(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf("project: demo\nbind: 127.0.0.1\nport: %d\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nstate_dir: %s\n", d.port, dir)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	d.succeed("list", "--config", config, "--operator-token-file", d.tokenFile)

	wantOperatorRequest(t, d.lastOperatorRequest(), http.MethodGet, "/legion/v1/operator/claims")
}

// A required flag left out — or given empty — is a usage error named by the flag, and nothing is
// sent.
func TestClaimsRefusesAMissingFlagBeforeReachingTheDaemon(t *testing.T) {
	token := writeFile(t, "operator-token", claimsOperatorToken)
	for _, sub := range []struct {
		name  string
		flags [][2]string // every required flag, with a valid value
	}{
		{"spawn", [][2]string{{"operator-token-file", token}, {"tree", "LEGION-208"}, {"issue", "LEGION-208"}, {"role", "architect"}}},
		{"deliver", [][2]string{{"operator-token-file", token}, {"claim", string(architectClaim)}, {"task", "Review the plan."}}},
		{"suspend", [][2]string{{"operator-token-file", token}, {"claim", string(architectClaim)}}},
		{"resume", [][2]string{{"operator-token-file", token}, {"claim", string(architectClaim)}}},
		{"stop", [][2]string{{"operator-token-file", token}, {"claim", string(architectClaim)}}},
		{"list", [][2]string{{"operator-token-file", token}}},
	} {
		for _, missing := range sub.flags {
			for _, form := range []string{"absent", "empty"} {
				t.Run(sub.name+"/"+missing[0]+"/"+form, func(t *testing.T) {
					d := newOperatorDaemon(t)
					args := []string{sub.name, "--port", strconv.Itoa(d.port)}
					for _, flag := range sub.flags {
						switch {
						case flag[0] != missing[0]:
							args = append(args, "--"+flag[0], flag[1])
						case form == "empty":
							args = append(args, "--"+flag[0], "")
						}
					}

					code, out, errb := d.claims(args...)

					if code != 2 || out != "" {
						t.Fatalf("exit %d, stdout %q; want 2 and nothing on stdout", code, out)
					}
					if want := "legion claims " + sub.name + ": --" + missing[0] + " is required"; !strings.Contains(errb, want) {
						t.Fatalf("stderr = %q, want %q", errb, want)
					}
					d.wantNothingReached()
				})
			}
		}
	}
}

// The bearer is read by ReadSecretPointer's rules: a token file that cannot be read, or holds
// nothing but whitespace, fails the command naming the path, and nothing is sent.
func TestClaimsRefusesAnOperatorTokenFileItCannotRead(t *testing.T) {
	files := map[string]func(t *testing.T) string{
		"absent": func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		"empty":  func(t *testing.T) string { return writeFile(t, "operator-token", "") },
		"blank":  func(t *testing.T) string { return writeFile(t, "operator-token", " \n\t\n") },
		"a directory": func(t *testing.T) string {
			return t.TempDir()
		},
		"unreadable": func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a 0000 file")
			}
			path := writeFile(t, "operator-token", claimsOperatorToken)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatalf("chmod %s: %v", path, err)
			}
			return path
		},
	}
	prompt := writeFile(t, "prompt.md", "Reply ready and wait.")
	for _, sub := range [][]string{
		{"spawn", "--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect", "--prompt-file", prompt},
		{"deliver", "--claim", string(architectClaim), "--task", "Review the plan."},
		{"suspend", "--claim", string(architectClaim)},
		{"resume", "--claim", string(architectClaim)},
		{"stop", "--claim", string(architectClaim)},
		{"list"},
	} {
		for name, file := range files {
			t.Run(sub[0]+"/"+name, func(t *testing.T) {
				d := newOperatorDaemon(t)
				path := file(t)
				args := append(slices.Clone(sub), "--port", strconv.Itoa(d.port), "--operator-token-file", path)

				code, out, errb := d.claims(args...)

				if code != 1 || out != "" {
					t.Fatalf("exit %d, stdout %q; want 1 and nothing on stdout", code, out)
				}
				if want := "legion claims " + sub[0] + ": --operator-token-file names " + path + ", which "; !strings.HasPrefix(errb, want) {
					t.Fatalf("stderr = %q, want it to start %q", errb, want)
				}
				d.wantNothingReached()
			})
		}
	}
}

// The role prompt is a file too: one that cannot be read fails the spawn naming the path.
func TestClaimsSpawnRefusesAPromptFileItCannotRead(t *testing.T) {
	d := newOperatorDaemon(t)
	absent := filepath.Join(t.TempDir(), "absent.md")
	args := append(append([]string{"spawn"}, d.reach()...),
		"--tree", "LEGION-208", "--issue", "LEGION-208", "--role", "architect", "--prompt-file", absent)

	code, out, errb := d.claims(args...)

	if code != 1 || out != "" || !strings.HasPrefix(errb, "legion claims spawn: ") || !strings.Contains(errb, absent) {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 naming %s", code, out, errb, absent)
	}
	d.wantNothingReached()
}

func TestClaimsWithoutAKnownSubcommandIsAUsageError(t *testing.T) {
	for _, testCase := range []struct {
		args []string
		want string
	}{
		{nil, "usage: legion claims spawn|deliver|suspend|resume|stop|list [flags]"},
		{[]string{"frobnicate"}, `legion claims: unknown subcommand "frobnicate"`},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "claims"}, testCase.args...), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), testCase.want) {
			t.Fatalf("claims %v = %d, stderr %q; want 2 and %q", testCase.args, code, errb.String(), testCase.want)
		}
	}
}

// A word after the flags is not a flag the command reads: refused, so `claims stop <token>` is
// never read as a stop of no claim.
func TestClaimsRefusesAPositionalArgument(t *testing.T) {
	d := newOperatorDaemon(t)

	code, _, errb := d.claims(append(append([]string{"stop"}, d.reach()...), string(architectClaim))...)

	if code != 2 || !strings.Contains(errb, `legion claims stop: unexpected argument "`+string(architectClaim)+`"`) {
		t.Fatalf("exit %d, stderr %q; want 2 naming the argument", code, errb)
	}
	d.wantNothingReached()
}
