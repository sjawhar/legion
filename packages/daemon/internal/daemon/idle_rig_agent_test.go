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
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// idleRigURLVariable names the rig's own service in the environment of every process the rig's
// launchers start. It is the one variable a role process needs that the pod's own environment
// does not carry: everything else it reads — LEGION_ISSUE, LEGION_ROLE, LEGION_GENERATION,
// LEGION_DAEMON_URL, ENVOY_URL and ENVOY_NATS_URL — is the environment the Sandbox runtime
// builds for a role (internal/runtime/sandbox's mainEnvironment).
const idleRigURLVariable = "LEGION_IDLE_RIG_URL"

// idleRigRole is one role process the rig has seen: the claim it registered, the session it
// became, and every message it was given — the daemon's prompts over the worker stream, and the
// messages the session's own consumer was relayed.
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

// messages is every message the role's session was given, relayed from its own consumer.
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
// its role on the Envoy listener and takes the session's messages from the session's own
// consumer over NATS, and records every message it is given — the daemon's prompts and the
// consumer's frames alike — with the rig.
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

// The subjects, header and replies of a session's own delivery, which the role process reads
// and answers as a plugin in consumer mode does. They are packages/envoy's: bus.DeliverSubject
// and contracts.AgentSubject build the two subjects, cmd/listener/session_relay.go names the
// header and the replies, and cmd/listener/session_delivery.go the mode the registration
// answers. The rig writes them out because they are another module's, and an Envoy that renamed
// one would be a contract change this rig has to be told about.
const (
	// idleRigDeliverSubjectPrefix and the session id are the session's relay subject, where the
	// listener that owns the session publishes each message of its durable consumer.
	idleRigDeliverSubjectPrefix = "notifications.envoy.deliver."
	// idleRigAgentSubjectPrefix and the session id are the session's agent subject: its inbox,
	// which a role-lane forward reaches as a core request waiting for a receipt.
	idleRigAgentSubjectPrefix = "notifications.agent."
	// idleRigStreamSeqHeader is a relay frame's stream sequence, in decimal.
	idleRigStreamSeqHeader = "Envoy-Stream-Seq"
	// idleRigRelayReceived is the answer that the frame decoded, before any work of the
	// session's; idleRigRelayDone is the answer that it was written, which acknowledges the
	// message to the consumer.
	idleRigRelayReceived = "received"
	idleRigRelayDone     = "done"
	// idleRigDeliveryConsumer is the delivery mode a registration answers for a session whose
	// own consumer the listener holds and relays.
	idleRigDeliveryConsumer = "consumer"
)

// idleRigEnvelope is what the rig reads of an Envoy envelope, the frame's data unchanged: the
// event it carries, the topic it was sent on (which tells a role-lane forward from a message
// stored on the session's own agent subject), and the message itself.
type idleRigEnvelope struct {
	EventID        string `json:"event_id"`
	Topic          string `json:"topic"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
}

// text is the message a sender sent: its payload, which the listener fills only for a message
// longer than the one-line summary it also carries (cmd/listener's messageEnvelope).
func (e idleRigEnvelope) text() string {
	if e.Payload != "" {
		return e.Payload
	}
	return e.PayloadSummary
}

// idleRigRoleProcess is one running role process: its worker stream connection, its claim, its
// session, and the NATS connection its session's own consumer is relayed to it on.
type idleRigRoleProcess struct {
	rig        string
	pod        string
	role       string
	generation uint64
	daemon     string
	envoy      string
	natsURLs   string
	connect    string
	tokenFile  string

	conn   net.Conn
	lines  *bufio.Reader
	writer *shimwire.Writer

	claimToken claim.Token
	secret     string
	session    string
	// epoch is this process's one NATS connection, as a plugin's registration names it: a new
	// one tells the listener to restart the session's relay at its consumer's ack floor.
	epoch          string
	bus            *nats.Conn
	client         *http.Client
	unregisterOnce sync.Once

	// recorded is every stream sequence this process has already recorded with the rig. The
	// relay is at-least-once — a frame whose reply it did not see within relayReceiptWait comes
	// again after it restarts at the ack floor — so a session keeps a ledger of what it has
	// written, as a plugin's does, and writes each stored message once. It is keyed on the
	// stream sequence and not on the event id, so a second stored copy of one event is still
	// recorded twice: that would be a delivery the pipeline made twice, which is what the test
	// counts.
	recordedMu sync.Mutex
	recorded   map[uint64]bool
}

func newIdleRigRoleProcess(rig string) (*idleRigRoleProcess, error) {
	p := &idleRigRoleProcess{rig: rig, client: &http.Client{Timeout: 20 * time.Second}, recorded: map[uint64]bool{}}
	p.pod = os.Getenv("LEGION_SANDBOX")
	if p.pod == "" {
		p.pod = os.Getenv("LEGION_ISSUE")
	}
	p.role = os.Getenv("LEGION_ROLE")
	p.daemon = os.Getenv("LEGION_DAEMON_URL")
	p.envoy = os.Getenv("ENVOY_URL")
	p.natsURLs = os.Getenv("ENVOY_NATS_URL")
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
	case p.natsURLs == "":
		return nil, fmt.Errorf("no ENVOY_NATS_URL in the environment: the session's messages come from its own consumer, over NATS")
	}
	// The session id is this pod's role, as its registration with the daemon reports it: the
	// subjects the session is reached on are built from it before anything is registered, so
	// nothing the listener relays can arrive before this process is listening for it.
	p.session = "ses-" + p.pod + "-" + p.role
	p.epoch = fmt.Sprintf("idle-rig-%d-%d", os.Getpid(), time.Now().UnixNano())
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
	if err := p.openDelivery(); err != nil {
		return err
	}
	defer p.bus.Close()
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
	// message published to its role topic would be forwarded to a session nothing is left of
	// instead of being answered 404 and reported as the wake it is.
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

// openDelivery opens the session's own NATS connection and takes the two subjects a session is
// reached on, as a plugin in consumer mode does (packages/envoy's "Session consumers", and the
// Go stand-ins of packages/envoy/cmd/listener's session_relay_test.go and
// current_registration_test.go):
//
//   - its relay subject, notifications.envoy.deliver.<session id>, where the listener that owns
//     the session publishes each message of the session's durable consumer. Every message the
//     session is given arrives here, and this is where the rig records it;
//   - its agent subject, notifications.agent.<session id>, where a role-lane forward and an
//     ad-hoc topic's forward arrive as core requests waiting for the holder's empty receipt.
//     That receipt is what the publish route answers its sender 200 on. Nothing is recorded
//     from it: the listener's forward is stored on the agent subject too, so the same message
//     comes back as a frame of the session's own consumer, which is the one copy the session
//     is given.
//
// Both are taken, and flushed to the server, before the session registers, so nothing the
// listener relays or forwards once it knows the session can arrive before this process listens.
func (p *idleRigRoleProcess) openDelivery() error {
	bus, err := nats.Connect(p.natsURLs, nats.Name("idle-rig-"+p.session), nats.MaxReconnects(-1))
	if err != nil {
		return fmt.Errorf("connect %s: %w", p.natsURLs, err)
	}
	p.bus = bus
	if _, err := bus.Subscribe(idleRigDeliverSubjectPrefix+p.session, p.relayed); err != nil {
		return fmt.Errorf("subscribe to the relay subject of %s: %w", p.session, err)
	}
	if _, err := bus.Subscribe(idleRigAgentSubjectPrefix+p.session, p.forwarded); err != nil {
		return fmt.Errorf("subscribe to the agent subject of %s: %w", p.session, err)
	}
	if err := bus.Flush(); err != nil {
		return fmt.Errorf("flush the subscriptions of %s: %w", p.session, err)
	}
	return nil
}

// relayed takes one frame of the session's own consumer: it answers `received` as soon as the
// frame decodes, records the message with the rig, and only then answers `done`, which is what
// acknowledges the message to the consumer. A frame whose stream sequence this process has
// already recorded — the relay sends again what it saw no reply to — is acknowledged and not
// recorded twice.
func (p *idleRigRoleProcess) relayed(msg *nats.Msg) {
	var item idleRigEnvelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: decode a frame for %s: %v\n", p.session, err)
		return
	}
	sequence, err := strconv.ParseUint(msg.Header.Get(idleRigStreamSeqHeader), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: a frame for %s carries stream sequence %q: %v\n",
			p.session, msg.Header.Get(idleRigStreamSeqHeader), err)
		return
	}
	if msg.Reply == "" {
		// There is nothing to answer it on, so the consumer could never be told the session has
		// it: the frame is malformed, as a plugin reads one with no reply (envoy-client's
		// decodeRelayFrame).
		fmt.Fprintf(os.Stderr, "idle rig role process: a frame for %s carries no reply subject\n", p.session)
		return
	}
	p.reply(msg, idleRigRelayReceived)
	if p.firstSighting(sequence) {
		if err := p.post(p.rig+"/agent/record", map[string]any{
			"pod": p.pod, "role": p.role, "kind": "message", "id": item.EventID, "text": item.text(),
		}, nil); err != nil {
			// The message is not recorded, so it is not acknowledged either: the frame's ack
			// wait runs out and the consumer delivers it again.
			p.forget(sequence)
			fmt.Fprintf(os.Stderr, "idle rig role process: record a message for %s: %v\n", p.session, err)
			return
		}
	}
	p.reply(msg, idleRigRelayDone)
}

// forwarded answers the empty receipt a role-lane forward and an ad-hoc topic's forward wait for
// on the session's agent subject, as every plugin answers one in either delivery mode. A frame
// carrying no reply, and one whose envelope's topic is the agent subject itself (a send stored
// through JetStream, whose reply subject is the server's own acknowledgement inbox), is left
// alone: an empty answer there fails the publisher.
func (p *idleRigRoleProcess) forwarded(msg *nats.Msg) {
	var item idleRigEnvelope
	if err := json.Unmarshal(msg.Data, &item); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: decode a forward to %s: %v\n", p.session, err)
		return
	}
	if msg.Reply == "" || item.Topic == idleRigAgentSubjectPrefix+p.session {
		return
	}
	if err := msg.Respond(nil); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: answer the receipt of a forward to %s: %v\n", p.session, err)
	}
}

// firstSighting reports whether this process has not recorded the stream sequence yet, taking it
// for recorded when it has not.
func (p *idleRigRoleProcess) firstSighting(sequence uint64) bool {
	p.recordedMu.Lock()
	defer p.recordedMu.Unlock()
	if p.recorded[sequence] {
		return false
	}
	p.recorded[sequence] = true
	return true
}

// forget gives a sequence back to the ledger, so a frame whose record did not reach the rig is
// recorded when the consumer delivers it again.
func (p *idleRigRoleProcess) forget(sequence uint64) {
	p.recordedMu.Lock()
	defer p.recordedMu.Unlock()
	delete(p.recorded, sequence)
}

func (p *idleRigRoleProcess) reply(msg *nats.Msg, word string) {
	if err := p.bus.Publish(msg.Reply, []byte(word)); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: reply %s to a frame for %s: %v\n", word, p.session, err)
		return
	}
	if err := p.bus.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "idle rig role process: flush the reply %s for %s: %v\n", word, p.session, err)
	}
}

func (p *idleRigRoleProcess) register(bootToken string) error {
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

// subscribe registers the session with the Envoy listener, as a session's Envoy plugin does: no
// port — this listener delivers nothing to a session's port, and refuses a registration naming
// one — the machine it runs on, every topic it follows beside its own agent subject, and the
// delivery it asks for: its own durable consumer, relayed to the NATS connection openDelivery
// opened, under this process's epoch. The answer says which delivery the session got, and a
// session left on the core path would be given nothing here, so anything but the consumer's is
// this process's failure to start (`core` with the listener's reason).
func (p *idleRigRoleProcess) subscribe(topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	var answer struct {
		Delivery *struct {
			Mode    string `json:"mode"`
			Subject string `json:"subject"`
			Reason  string `json:"reason"`
		} `json:"delivery"`
	}
	if err := p.post(p.envoy+"/v1/interests/subscribe", map[string]any{
		"session_id": p.session, "dir": "/legion/workspaces/" + p.pod, "topics": topics,
		"host": "127.0.0.1", "title": p.role, "self_subscribed": true,
		"capabilities": []string{"prompt_async"}, "plugin": "idle-rig", "plugin_version": "0",
		"delivery": map[string]any{"consumer": true, "epoch": p.epoch},
	}, &answer); err != nil {
		return err
	}
	switch {
	case answer.Delivery == nil || answer.Delivery.Mode != idleRigDeliveryConsumer:
		return fmt.Errorf("the listener registered %s on delivery %+v, want its own consumer's", p.session, answer.Delivery)
	case answer.Delivery.Subject != idleRigDeliverSubjectPrefix+p.session:
		return fmt.Errorf("the listener relays %s on %q, not the %q this process reads",
			p.session, answer.Delivery.Subject, idleRigDeliverSubjectPrefix+p.session)
	}
	return nil
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
