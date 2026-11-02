package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// idleRigURLVariable names the rig's own service in the environment of every process the rig's
// launchers start. It is the one variable a role process needs that the pod's own environment
// does not carry: everything else it reads — LEGION_ISSUE, LEGION_ROLE, LEGION_GENERATION,
// LEGION_DAEMON_URL and ENVOY_URL — is the environment the Sandbox runtime builds for a role
// (internal/runtime/sandbox's mainEnvironment).
const idleRigURLVariable = "LEGION_IDLE_RIG_URL"

// idleRigRole is one role process the rig has seen: the claim it registered, the session it
// became, and every message it was given — the daemon's prompts over the worker stream, and the
// Envoy listener's pushes to its serving port.
type idleRigRole struct {
	Claim       string
	Secret      string
	Session     string
	SessionFile string
	Registered  int
	Prompts     []idleRigMessage
	Messages    []idleRigMessage
}

// idleRigMessage is one message a role process recorded, with the moment it arrived.
type idleRigMessage struct {
	ID   string
	Text string
	At   time.Time
}

// idleRigPlan is what a role process does with the next prompt it is given: take a turn on it, or
// acknowledge it and take none, which is the shape of a resume that never reaches its first turn.
// Topics are the topics the process registers with the Envoy listener beside its agent subject,
// as `envoy_subscribe` adds one.
type idleRigPlan struct {
	TakeTurn bool     `json:"takeTurn"`
	Topics   []string `json:"topics"`
}

// idleRigAgents is the rig's own service: it hands each role process the boot token its pod's
// launcher wrote for the generation, records what each process registered and every message it
// was given, and answers what the next prompt should do. A role process reaches it at
// idleRigURLVariable.
//
// The boot token is handed over rather than read from the path the launch names, because the path
// a launch names is the pod's (`/var/run/legion/private/g<n>/LEGION_BOOT_TOKEN`) and this rig's
// launchers write their generations under a private directory of their own. The rig knows both.
type idleRigAgents struct {
	t    *testing.T
	url  string
	pool *pgxpool.Pool

	mu sync.Mutex
	// private is each role's launcher private directory, by "<pod>/<role>".
	private map[string]string
	roles   map[string]*idleRigRole
	plans   map[string]idleRigPlan
	// plan is the plan a role with none of its own gets.
	plan idleRigPlan
}

func newIdleRigAgents(t *testing.T, pool *pgxpool.Pool) *idleRigAgents {
	t.Helper()
	a := &idleRigAgents{t: t, pool: pool, private: map[string]string{}, roles: map[string]*idleRigRole{},
		plans: map[string]idleRigPlan{}, plan: idleRigPlan{TakeTurn: true}}
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	a.url = server.URL
	return a
}

func idleRigKey(pod, role string) string { return pod + "/" + role }

// setPrivate records where a pod's role launcher writes its generations, so the rig can read that
// generation's boot token for the role process.
func (a *idleRigAgents) setPrivate(pod, role, dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.private[idleRigKey(pod, role)] = dir
}

// setPlan is what the role's next prompts do.
func (a *idleRigAgents) setPlan(pod, role string, plan idleRigPlan) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.plans[idleRigKey(pod, role)] = plan
}

// role is what the rig has seen of a role process, empty when it has seen none.
func (a *idleRigAgents) role(pod, role string) idleRigRole {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen, ok := a.roles[idleRigKey(pod, role)]
	if !ok {
		return idleRigRole{}
	}
	copied := *seen
	copied.Prompts = append([]idleRigMessage(nil), seen.Prompts...)
	copied.Messages = append([]idleRigMessage(nil), seen.Messages...)
	return copied
}

// prompts is every task the daemon gave the role over the worker stream.
func (a *idleRigAgents) prompts(pod, role string) []idleRigMessage { return a.role(pod, role).Prompts }

// messages is every message the Envoy listener pushed to the role's session.
func (a *idleRigAgents) messages(pod, role string) []idleRigMessage {
	return a.role(pod, role).Messages
}

// counting is how many of the role's prompts and pushed messages hold want.
func idleRigCount(messages []idleRigMessage, want string) int {
	count := 0
	for _, message := range messages {
		if strings.Contains(message.Text, want) {
			count++
		}
	}
	return count
}

func (a *idleRigAgents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/agent/boot-token":
		a.bootToken(w, r)
	case "/agent/registered":
		a.registered(w, r)
	case "/agent/record":
		a.record(w, r)
	case "/agent/plan":
		a.servePlan(w, r)
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func (a *idleRigAgents) bootToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Pod, Role  string
		Generation uint64
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	dir := a.private[idleRigKey(request.Pod, request.Role)]
	a.mu.Unlock()
	if dir == "" {
		http.Error(w, "no launcher private directory for "+idleRigKey(request.Pod, request.Role), http.StatusNotFound)
		return
	}
	path := filepath.Join(dir, shimwire.GenerationDir(request.Generation), "LEGION_BOOT_TOKEN")
	token, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeIdleRigJSON(w, map[string]string{"token": strings.TrimSpace(string(token))})
}

// registered records a role process's registration and writes the session row its next resume is
// looked up by, as the agent's own Oh My Pi writes its session into the session table.
func (a *idleRigAgents) registered(w http.ResponseWriter, r *http.Request) {
	var request struct{ Pod, Role, Claim, Secret, Session, SessionFile string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := a.pool.Exec(r.Context(),
		`insert into omp_session_files (path, mtime_ms) values ($1, $2)
		 on conflict (path) do update set mtime_ms = excluded.mtime_ms`,
		request.SessionFile, time.Now().UnixMilli()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	key := idleRigKey(request.Pod, request.Role)
	seen, ok := a.roles[key]
	if !ok {
		seen = &idleRigRole{}
		a.roles[key] = seen
	}
	seen.Claim, seen.Secret, seen.Session, seen.SessionFile = request.Claim, request.Secret, request.Session, request.SessionFile
	seen.Registered++
	a.mu.Unlock()
	writeIdleRigJSON(w, map[string]string{})
}

func (a *idleRigAgents) record(w http.ResponseWriter, r *http.Request) {
	var request struct{ Pod, Role, Kind, ID, Text string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	key := idleRigKey(request.Pod, request.Role)
	seen, ok := a.roles[key]
	if !ok {
		seen = &idleRigRole{}
		a.roles[key] = seen
	}
	message := idleRigMessage{ID: request.ID, Text: request.Text, At: time.Now().UTC()}
	if request.Kind == "prompt" {
		seen.Prompts = append(seen.Prompts, message)
	} else {
		seen.Messages = append(seen.Messages, message)
	}
	a.mu.Unlock()
	writeIdleRigJSON(w, map[string]string{})
}

func (a *idleRigAgents) servePlan(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	plan, ok := a.plans[idleRigKey(r.URL.Query().Get("pod"), r.URL.Query().Get("role"))]
	if !ok {
		plan = a.plan
	}
	a.mu.Unlock()
	writeIdleRigJSON(w, plan)
}

func writeIdleRigJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// TestIdleRigRoleProcess is the role process of a pod of TestIdleReleaseAndWake: the whole of
// `legion worker-shim … -- <omp argv>` as a pod runs it, with Oh My Pi's own part scripted. The
// pod's real launcher starts it, through the `legion` the runtime was configured with
// (idleRigLegionTool), with the argv and environment a role is given.
//
// It is the stand-in of packages/daemon/internal/capabilities/image_test.go's fakeOmp grown the
// three things the rig's claims need: it registers with the daemon and reports ready over the
// worker stream the shim speaks (packages/daemon/internal/daemon/daemon_test.go's shim), claims
// its role on the Envoy listener and serves the port the listener delivers to, and records every
// message it is given — the daemon's prompts and the listener's pushes alike — with the rig.
//
// Run by anything but that launcher it does nothing: with no rig service named in its
// environment there is no rig to serve.
func TestIdleRigRoleProcess(t *testing.T) {
	rig := os.Getenv(idleRigURLVariable)
	if rig == "" {
		return
	}
	process, err := newIdleRigRoleProcess(rig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "idle rig role process:", err)
		os.Exit(1)
	}
	if err := process.run(); err != nil {
		fmt.Fprintln(os.Stderr, "idle rig role process:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// idleRigRoleProcess is one running role process: its worker stream connection, its claim, its
// session, and the port the Envoy listener delivers to it on.
type idleRigRoleProcess struct {
	rig        string
	pod        string
	role       string
	generation uint64
	daemon     string
	envoy      string
	connect    string
	tokenFile  string

	conn   net.Conn
	lines  *bufio.Reader
	writer *shimwire.Writer

	claimToken     claim.Token
	secret         string
	session        string
	port           int
	client         *http.Client
	unregisterOnce sync.Once
}

func newIdleRigRoleProcess(rig string) (*idleRigRoleProcess, error) {
	p := &idleRigRoleProcess{rig: rig, client: &http.Client{Timeout: 20 * time.Second}}
	p.pod = os.Getenv("LEGION_SANDBOX")
	if p.pod == "" {
		p.pod = os.Getenv("LEGION_ISSUE")
	}
	p.role = os.Getenv("LEGION_ROLE")
	p.daemon = os.Getenv("LEGION_DAEMON_URL")
	p.envoy = os.Getenv("ENVOY_URL")
	generation, err := strconv.ParseUint(os.Getenv("LEGION_GENERATION"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("LEGION_GENERATION %q: %w", os.Getenv("LEGION_GENERATION"), err)
	}
	p.generation = generation
	argv := os.Args
	for i, arg := range argv {
		if i+1 >= len(argv) {
			break
		}
		switch arg {
		case "--connect":
			p.connect = strings.TrimPrefix(argv[i+1], "tcp://")
		case "--boot-token-file":
			p.tokenFile = argv[i+1]
		}
	}
	switch {
	case p.pod == "" || p.role == "":
		return nil, fmt.Errorf("no pod (%q) or role (%q) in the environment", p.pod, p.role)
	case p.connect == "":
		return nil, fmt.Errorf("no --connect in %v", argv)
	case p.daemon == "" || p.envoy == "":
		return nil, fmt.Errorf("no daemon (%q) or Envoy (%q) URL in the environment", p.daemon, p.envoy)
	}
	return p, nil
}

// run connects, registers, claims the role, reports ready, and then serves the daemon's frames
// until it is shut down.
func (p *idleRigRoleProcess) run() error {
	bootToken, err := p.bootToken()
	if err != nil {
		return err
	}
	if err := p.dial(bootToken); err != nil {
		return err
	}
	defer p.conn.Close()
	if err := p.serveSessionPort(); err != nil {
		return err
	}
	if err := p.register(bootToken); err != nil {
		return err
	}
	plan, err := p.plan()
	if err != nil {
		return err
	}
	if err := p.subscribe(plan.Topics); err != nil {
		return err
	}
	if err := p.claimRole(); err != nil {
		return err
	}
	if err := p.ready(); err != nil {
		return err
	}
	if err := p.reportActivity(); err != nil {
		return err
	}
	// The registration goes when the process does, as a session's Envoy plugin drops it when its
	// process ends: a released pod's session must be gone from the listener's registry, or a
	// message published to its role topic would be forwarded to a port nothing answers instead
	// of being answered 404 and reported as the wake it is.
	defer p.unregister()
	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stopping
		p.unregister()
		os.Exit(0)
	}()
	return p.serve()
}

// unregister drops the session's role claim and its registration with the listener.
func (p *idleRigRoleProcess) unregister() {
	p.unregisterOnce.Do(func() {
		_ = p.delete(p.envoy + "/v1/sessions/" + p.session)
		_ = p.delete(p.envoy + "/v1/interests/" + p.session)
	})
}

func (p *idleRigRoleProcess) delete(url string) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	return p.do(request, nil)
}

func (p *idleRigRoleProcess) bootToken() (string, error) {
	var answer struct{ Token string }
	if err := p.post(p.rig+"/agent/boot-token",
		map[string]any{"pod": p.pod, "role": p.role, "generation": p.generation}, &answer); err != nil {
		return "", err
	}
	if answer.Token == "" {
		return "", fmt.Errorf("the rig handed no boot token for %s/%s generation %d (%s)", p.pod, p.role, p.generation, p.tokenFile)
	}
	return answer.Token, nil
}

func (p *idleRigRoleProcess) dial(bootToken string) error {
	conn, err := net.DialTimeout("tcp", p.connect, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial the worker stream %s: %w", p.connect, err)
	}
	p.conn, p.lines, p.writer = conn, bufio.NewReader(conn), shimwire.NewWriter(conn)
	if err := p.writer.WriteFrame(shimwire.Hello2{BootToken: bootToken}); err != nil {
		return err
	}
	frame, err := p.next(30 * time.Second)
	if err != nil {
		return err
	}
	if frame.FrameType() != shimwire.TypeHelloAck {
		return fmt.Errorf("the daemon answered the hello with %s", frame.FrameType())
	}
	return nil
}

// serveSessionPort serves the one route the Envoy listener delivers on (session.Deliverer's
// prompt_async push), recording every message with the rig.
func (p *idleRigRoleProcess) serveSessionPort() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.port = listener.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/prompt_async") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Parts []map[string]string `json:"parts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var text strings.Builder
		for _, part := range body.Parts {
			text.WriteString(part["text"])
		}
		_ = p.post(p.rig+"/agent/record",
			map[string]any{"pod": p.pod, "role": p.role, "kind": "message", "text": text.String()}, nil)
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return nil
}

func (p *idleRigRoleProcess) register(bootToken string) error {
	p.session = "ses-" + p.pod + "-" + p.role
	sessionFile := idleRigSessionFile(p.pod, p.role)
	var answer claim.RegisterResponse
	if err := p.post(p.daemon+"/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: p.session, OmpSessionFile: sessionFile,
		AgentID: "idle-rig-agent", PluginContract: api.DaemonAPIVersion,
	}, &answer); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	p.claimToken, p.secret = answer.ClaimToken, answer.Secret
	return p.post(p.rig+"/agent/registered", map[string]any{
		"pod": p.pod, "role": p.role, "claim": string(p.claimToken), "secret": p.secret,
		"session": p.session, "sessionFile": sessionFile,
	}, nil)
}

func (p *idleRigRoleProcess) plan() (idleRigPlan, error) {
	var plan idleRigPlan
	err := p.get(p.rig+"/agent/plan?pod="+p.pod+"&role="+p.role, &plan)
	return plan, err
}

// subscribe registers the session with the Envoy listener, as a session's Envoy plugin does: its
// serving port, the machine it runs on, and every topic it follows beside its own agent subject.
func (p *idleRigRoleProcess) subscribe(topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	return p.post(p.envoy+"/v1/interests/subscribe", map[string]any{
		"session_id": p.session, "dir": "/legion/workspaces/" + p.pod, "topics": topics,
		"port": p.port, "host": "127.0.0.1", "title": p.role, "driving": true,
		"capabilities": []string{"prompt_async"}, "plugin": "idle-rig", "plugin_version": "0",
	}, nil)
}

// claimRole takes the role topic of this claim, which is what makes a message published to it
// reach this session and what makes its absence the listener's wake report.
func (p *idleRigRoleProcess) claimRole() error {
	return p.post(p.envoy+"/v1/roles/set", map[string]any{
		"role": string(p.claimToken), "session_id": p.session,
	}, nil)
}

func (p *idleRigRoleProcess) ready() error {
	return p.post(p.daemon+"/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: p.claimToken, SessionID: p.session, Secret: p.secret, Generation: p.generation,
	}, nil)
}

// reportActivity is the session's report that it runs no background work and has no reminder due,
// which the idle rule reads before it releases the pod (R1).
func (p *idleRigRoleProcess) reportActivity() error {
	return p.post(p.daemon+"/legion/v1/claims/activity", claim.ActivityRequest{
		ClaimToken: p.claimToken, SessionID: p.session, Secret: p.secret,
		Activity: claim.SessionActivity{At: time.Now().UTC()},
	}, nil)
}

// serve answers the daemon's frames until the connection ends or a shutdown arrives.
func (p *idleRigRoleProcess) serve() error {
	for {
		frame, err := p.next(0)
		if err != nil {
			return nil
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			if err := p.writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true}); err != nil {
				return err
			}
		case shimwire.GetState:
			streaming := false
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if err := p.writer.WriteFrame(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data}); err != nil {
				return err
			}
		case shimwire.Prompt:
			if err := p.prompted(f); err != nil {
				return err
			}
		case shimwire.BundleWorkspace:
			// The release's save of the workspace's unpushed work. This workspace has none, which
			// is the empty bundle a clean workspace saves.
			if err := p.writer.WriteFrame(shimwire.BundleWorkspaceResult{
				ID: f.ID, OK: true, Head: "0000000000000000000000000000000000000000", At: time.Now().UTC(),
			}); err != nil {
				return err
			}
		case shimwire.AdoptWorkingCopy:
			if err := p.writer.WriteFrame(shimwire.AdoptWorkingCopyResult{ID: f.ID, OK: true}); err != nil {
				return err
			}
		case shimwire.Shutdown:
			return nil
		}
	}
}

// prompted records the task and, when the rig's plan says so, takes a turn on it. A plan that
// takes no turn acknowledges the prompt and nothing else, which is a resume whose woken role
// never reaches its first turn.
func (p *idleRigRoleProcess) prompted(prompt shimwire.Prompt) error {
	if err := p.post(p.rig+"/agent/record", map[string]any{
		"pod": p.pod, "role": p.role, "kind": "prompt", "id": prompt.ID, "text": prompt.Message,
	}, nil); err != nil {
		return err
	}
	if err := p.writer.WriteFrame(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true}); err != nil {
		return err
	}
	plan, err := p.plan()
	if err != nil {
		return err
	}
	if plan.TakeTurn {
		if err := p.writer.WriteFrame(shimwire.AgentStart{}); err != nil {
			return err
		}
		if err := p.writer.WriteFrame(shimwire.AgentEnd{}); err != nil {
			return err
		}
		if err := p.writer.WriteFrame(shimwire.PromptResult{ID: prompt.ID, AgentInvoked: true}); err != nil {
			return err
		}
	}
	// The report that follows every turn, and every task acknowledged without one: the idle rule
	// releases no pod whose session has said nothing since its last turn, and a task
	// acknowledged is a turn as far as that rule reads it (pods.IdleClaim.Reported).
	return p.reportActivity()
}

func (p *idleRigRoleProcess) next(within time.Duration) (shimwire.Frame, error) {
	deadline := time.Time{}
	if within > 0 {
		deadline = time.Now().Add(within)
	}
	if err := p.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	line, err := p.lines.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return shimwire.Decode(line)
}

func (p *idleRigRoleProcess) post(url string, body, answer any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return p.do(request, answer)
}

func (p *idleRigRoleProcess) get(url string, answer any) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return p.do(request, answer)
}

func (p *idleRigRoleProcess) do(request *http.Request, answer any) error {
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		return fmt.Errorf("%s %s = %d %s", request.Method, request.URL.Path, response.StatusCode, read)
	}
	if answer == nil || len(bytes.TrimSpace(read)) == 0 {
		return nil
	}
	return json.Unmarshal(read, answer)
}
