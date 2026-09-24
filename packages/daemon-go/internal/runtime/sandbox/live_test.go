//go:build e2e

// The Stage 4a live proof (LEGION-208 Stage 4 plan, Task 4.7): this package's runtime driven on a
// real cluster through the Legion daemon's own restricted identity, with the devbox's admin
// identity only for what an operator does beside it — exec, PVC reads, a Secret's hash, the
// namespace list. scripts/e2e/stage4a-sandbox-runtime.sh builds it with `-tags e2e`, runs it, and
// owns the run's inputs, its teardown, and the namespace comparison; see scripts/e2e/README.md.
//
// Each check prints what it observed, each line naming the identity that observed it, and then
// `CHECK <name>: PASS`. The first check that does not hold prints `CHECK <name>: FAIL: <why>` and
// ends the run. The agent every pod runs is a stub (decision 7): the Go shim bridges it as it would
// Oh My Pi, and it appends its pod's uid to a marker file on the tree volume — the session file a
// resume names — and sleeps.
//
// This file is the rig: the run's inputs, the boot-token registry, the observation and log
// records, the runtime's lifecycle, and the launches and reads every check shares. The checks
// live beside it: live_install_test.go (identity and install), live_lifecycle_test.go (the claims'
// lifecycle), and live_app_test.go (the implement App the provisioning token comes from).
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

// liveCheck is one check of the harness: the name it prints, and what it runs.
type liveCheck struct {
	name string
	run  func(*liveRig) error
}

// liveChecks are the harness's checks, in the order they run: the one list of them, which a
// development run's entry point is checked against. namespace-clean, the last check of the proof,
// is the script's: it compares the namespace after the teardown, outside the harness.
var liveChecks = []liveCheck{
	{"identity", (*liveRig).checkIdentity},
	{"installed", (*liveRig).checkInstalled},
	{"boot-refusal-negative", (*liveRig).checkBootRefusal},
	{"image-probe", (*liveRig).checkImageProbe},
	{"root-ready", (*liveRig).checkRootReady},
	{"gvisor", (*liveRig).checkGVisor},
	{"gateway-token", (*liveRig).checkGatewayToken},
	{"adopt-working-copy", (*liveRig).checkAdoptWorkingCopy},
	{"worker-colocated", (*liveRig).checkWorkerColocated},
	{"suspend", (*liveRig).checkSuspend},
	{"no-affinity", (*liveRig).checkNoAffinity},
	{"resume", (*liveRig).checkResume},
	{"same-agent-negative", (*liveRig).checkSameAgentNegative},
	{"kill-pod", (*liveRig).checkKillPod},
	{"stale-incarnation", (*liveRig).checkStaleIncarnation},
	{"respawn-before-register", (*liveRig).checkRespawnBeforeRegister},
	{"concurrent-provision", (*liveRig).checkConcurrentProvision},
	{"re-adopt", (*liveRig).checkReAdopt},
	{"orphan-sweep", (*liveRig).checkOrphanSweep},
	{"release-tree", (*liveRig).checkReleaseTree},
}

// The runtime's settings for the run: the boot timeout covers a Karpenter node coming up and the
// worker image's pull, which is what a first pod on an empty pool waits for.
const (
	liveBootTimeout   = 5 * time.Minute
	liveBootIntervals = 3
	liveGrace         = 15 * time.Second
	liveProbeInterval = 10 * time.Second
	liveAdoptTimeout  = time.Minute
	// liveRunningLimit bounds a launch's wait for its pod to run and its Sandbox to be Ready.
	liveRunningLimit = 15 * time.Minute
	// liveGoneLimit bounds a wait for a pod or an object to be deleted.
	liveGoneLimit = 5 * time.Minute
	// liveSettle is how long a check keeps listening for observations it must not see.
	liveSettle = 2*liveProbeInterval + 5*time.Second
)

// liveScheduling is the run's scheduling, as the 4b daemon's configuration sets it
// (runtime.kubernetes.scheduling.node_selector). Every pod of a tree requires the node of the tree's
// first scheduled pod (the tree volume attaches to one node), so that node must hold the tree. On
// production's `legion` NodePool a pod with no resource request lands on a c7a.medium, whose 8 pod
// slots its 7 daemonsets all but fill, so no second pod of the tree could ever join it. A CPU request
// on the root does not fix it: a child placed first pins the root to a node its request may not fit.
// Karpenter labels every node with its vCPU count, so selecting 4 keeps every Legion pod, the image
// probe's included, on 58-slot nodes and requests nothing. The lasting fix is the same floor on the
// NodePool itself (agent-c, components/legion).
var liveScheduling = Scheduling{NodeSelector: map[string]string{"karpenter.k8s.aws/instance-cpu": "4"}}

// liveGateway is the model gateway the run's pods are pointed at, as the 4b daemon's configuration
// sets it (runtime.kubernetes.gateway): production's middleman, the legion-worker ServiceAccount
// agent-c creates for Legion's pods, the audience middleman trusts the cluster's tokens for, and
// the shortest token lifetime. The stub agent calls no model; the pods carry the token so that the
// proof runs the pod shape real agents run, which `legion-sandbox-pods` must admit.
var liveGateway = Gateway{
	URL: "https://middleman.hawk.internal.trajectorylabs.com", Audience: "middleman-legion", ServiceAccount: "legion-worker",
	TokenExpiry: minTokenExpiry,
}

// The stub agent (decision 7): the shim runs it after its hello is acknowledged, with the Oh My Pi
// arguments the runtime appends as the shell's positional parameters, which it ignores.
var stubAgent = []string{"/bin/sh", "-c", `printf '%s\n' "$POD_UID" >>"$LEGION_E2E_MARKER" && exec sleep infinity`, "stage4a-stub"}

// liveEnv is what the script hands the harness.
type liveEnv struct {
	runtimeKubeconfig, runtimeContext, operatorContext string
	namespace, project, claimProject, image, repo      string
	streamHost, streamPort                             string
	appID, appKeyName                                  string
	record, work, from                                 string
}

func readLiveEnv(t *testing.T) liveEnv {
	t.Helper()
	get := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			fmt.Printf("CHECK identity: FAIL: %s is unset; the harness refuses to start (scripts/e2e/stage4a-sandbox-runtime.sh sets it)\n", name)
			t.FailNow()
		}
		return value
	}
	env := liveEnv{
		runtimeKubeconfig: get("LEGION_E2E_RUNTIME_KUBECONFIG"),
		runtimeContext:    get("LEGION_E2E_RUNTIME_CONTEXT"),
		operatorContext:   get("LEGION_E2E_OPERATOR_CONTEXT"),
		namespace:         get("LEGION_E2E_NAMESPACE"),
		project:           get("LEGION_E2E_PROJECT"),
		image:             get("LEGION_E2E_IMAGE"),
		repo:              get("LEGION_E2E_REPO"),
		streamHost:        get("LEGION_E2E_STREAM_HOST"),
		streamPort:        get("LEGION_E2E_STREAM_PORT"),
		appID:             get("LEGION_E2E_IMPLEMENT_APP_ID"),
		appKeyName:        get("LEGION_E2E_IMPLEMENT_APP_KEY"),
		record:            get("LEGION_E2E_RECORD"),
		work:              get("LEGION_E2E_WORK"),
		from:              os.Getenv("LEGION_E2E_FROM"),
	}
	if !strings.HasPrefix(env.project, "s4a-") {
		fmt.Printf("CHECK identity: FAIL: the run project %q lacks the reserved prefix s4a-\n", env.project)
		t.FailNow()
	}
	// The claim token's project is a project token, [a-z0-9]+; the label keeps the dashes.
	env.claimProject = strings.ReplaceAll(env.project, "-", "")
	entry := func(check liveCheck) bool { return check.name == env.from }
	if env.from != "" && (env.from == "identity" || env.from == "stale-incarnation" || !slices.ContainsFunc(liveChecks, entry)) {
		fmt.Printf("CHECK identity: FAIL: STAGE4A_FROM=%s is not an entry point (any check but identity and stale-incarnation, which rides kill-pod)\n", env.from)
		t.FailNow()
	}
	return env
}

// claimState is where a claim stands, as the daemon's machine would record it.
type claimState int

const (
	stateNone claimState = iota
	stateRunning
	stateSuspended
	stateDead
	stateReleased
)

// liveClaim is one claim of the run and what the harness recorded for it.
type liveClaim struct {
	token             claim.Token
	tree, issue, name string
	role              claim.Role
	marker            string
	state             claimState
	gen               uint64
	bootToken         string
	// loc is the running process; last is the latest locator, kept as Resume's hint once the
	// process stopped.
	loc, last *runtime.Locator
}

// minted is one boot token the harness minted, for one generation of one claim.
type minted struct {
	claim claim.Token
	gen   uint64
	hash  string
	armed bool
}

// registration is a hello the listener registered: the claim and generation of the Hello event,
// and the hash of the token the resolver accepted for it.
type registration struct {
	claim claim.Token
	gen   uint64
	hash  string
	at    time.Time
}

// registry is the harness's store of boot tokens, which outlives every listener and runtime of
// the run as the daemon's database does. Its resolver accepts only each claim's current,
// armed generation, and records every hello it judges.
type registry struct {
	mu       sync.Mutex
	byToken  map[string]minted
	current  map[claim.Token]uint64
	pending  map[claim.Token]string
	accepted []registration
	refused  map[claim.Token]int
	changed  chan struct{}
}

func newRegistry() *registry {
	return &registry{
		byToken: map[string]minted{}, current: map[claim.Token]uint64{}, pending: map[claim.Token]string{},
		refused: map[claim.Token]int{}, changed: make(chan struct{}),
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mint makes the claim's next generation current. An unarmed token is refused as unknown, so a
// pod on it can never register.
func (g *registry) mint(c claim.Token, armed bool) (string, uint64) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(raw)
	g.mu.Lock()
	defer g.mu.Unlock()
	gen := g.current[c] + 1
	g.current[c] = gen
	g.byToken[token] = minted{claim: c, gen: gen, hash: tokenHash(token), armed: armed}
	return token, gen
}

func (g *registry) resolve(bootToken string) (claim.Token, uint64, bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.byToken[bootToken]
	switch {
	case !ok:
		return "", 0, false, false
	case !m.armed:
		g.refused[m.claim]++
		return "", 0, false, false
	case m.gen != g.current[m.claim]:
		g.refused[m.claim]++
		return m.claim, m.gen, true, true
	}
	g.pending[m.claim] = m.hash
	return m.claim, m.gen, false, true
}

func (g *registry) hello(event stream.Hello) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accepted = append(g.accepted, registration{claim: event.Claim, gen: event.Generation, hash: g.pending[event.Claim], at: time.Now()})
	close(g.changed)
	g.changed = make(chan struct{})
}

// await is the claim's registration at gen after since, within limit.
func (g *registry) await(c claim.Token, gen uint64, since time.Time, limit time.Duration) (registration, bool) {
	deadline := time.After(limit)
	for {
		g.mu.Lock()
		for _, reg := range g.accepted {
			if reg.claim == c && reg.gen == gen && !reg.at.Before(since) {
				g.mu.Unlock()
				return reg, true
			}
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return registration{}, false
		}
	}
}

func (g *registry) registrations(c claim.Token) []registration {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []registration
	for _, reg := range g.accepted {
		if reg.claim == c {
			out = append(out, reg)
		}
	}
	return out
}

func (g *registry) refusals(c claim.Token) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused[c]
}

// observed is one observation the runtime delivered, numbered in delivery order across every
// runtime instance of the run.
type observed struct {
	runtime.Observation
	seq int
}

type observations struct {
	mu      sync.Mutex
	list    []observed
	changed chan struct{}
}

func (o *observations) add(obs runtime.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, observed{Observation: obs, seq: len(o.list) + 1})
	close(o.changed)
	o.changed = make(chan struct{})
}

// mark is the sequence number of the latest observation: what came after it has a larger one.
func (o *observations) mark() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.list)
}

func (o *observations) since(after int) []observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.list[after:])
}

// await is the first observation after `after` that match accepts, within limit.
func (o *observations) await(after int, limit time.Duration, match func(runtime.Observation) bool) (observed, bool) {
	deadline := time.After(limit)
	for {
		o.mu.Lock()
		for _, obs := range o.list[after:] {
			if match(obs.Observation) {
				o.mu.Unlock()
				return obs, true
			}
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return observed{}, false
		}
	}
}

// logRecorder keeps every record the runtime and the listener log, in memory for the checks
// that read them and as JSON lines in the run's evidence.
type logRecorder struct {
	mu      sync.Mutex
	json    slog.Handler
	records []map[string]string
}

type recordingHandler struct {
	rec   *logRecorder
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) WithGroup(string) slog.Handler            { return h }
func (h recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordingHandler{rec: h.rec, attrs: append(slices.Clone(h.attrs), attrs...)}
}

func (h recordingHandler) Handle(ctx context.Context, record slog.Record) error {
	fields := map[string]string{"msg": record.Message, "level": record.Level.String()}
	add := func(a slog.Attr) bool { fields[a.Key] = a.Value.String(); return true }
	for _, a := range h.attrs {
		add(a)
	}
	record.Attrs(add)
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	h.rec.records = append(h.rec.records, fields)
	return h.rec.json.WithAttrs(h.attrs).Handle(ctx, record)
}

func (l *logRecorder) find(msg string) (map[string]string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i]["msg"] == msg {
			return l.records[i], true
		}
	}
	return nil, false
}

// liveRig is the run: the two identities, the registry, and the current runtime instance.
type liveRig struct {
	t      *testing.T
	env    liveEnv
	ctx    context.Context
	cancel context.CancelFunc

	rc   *rest.Config // the runtime identity, as the runtime gets it
	kube kubernetes.Interface
	dyn  dynamic.Interface

	tokens   implementTokens
	identity runtime.GitIdentity
	prompt   string
	reg      *registry
	obs      *observations
	logs     *logRecorder
	log      *slog.Logger

	// The current runtime instance and its listener; stop ends both.
	rt      *Runtime
	ln      *stream.Listener
	stop    context.CancelFunc
	stopped sync.WaitGroup

	// claims are the run's claims in newLiveRig's order, each tree's root before its other claims.
	claims []*liveClaim
	// What kill-pod recorded for stale-incarnation.
	killed struct {
		old, fresh runtime.Locator
		gone       observed
	}
}

func TestStage4aSandboxRuntimeLive(t *testing.T) {
	env := readLiveEnv(t)
	r := newLiveRig(t, env)
	defer r.close()

	skipping := env.from != ""
	for _, check := range liveChecks {
		if skipping && check.name == env.from {
			skipping = false
		}
		if skipping && check.name != "identity" {
			fmt.Printf("CHECK %s: SKIPPED (development run from %s)\n", check.name, env.from)
			continue
		}
		r.run(check)
	}
	fmt.Println("stage 4a harness: every check passed")
}

func (r *liveRig) run(check liveCheck) {
	fmt.Printf("== %s\n", check.name)
	started := time.Now()
	if err := check.run(r); err != nil {
		fmt.Printf("CHECK %s: FAIL: %v\n", check.name, err)
		r.t.FailNow()
	}
	fmt.Printf("CHECK %s: PASS (%s)\n", check.name, time.Since(started).Round(time.Second))
}

// note prints one piece of evidence, naming the identity that observed it: runtime (the Legion
// daemon's restricted identity, through the runtime or the harness's own client under it),
// operator (the admin context, through kubectl), or harness (the listener and its resolver).
func note(who, format string, args ...any) {
	fmt.Printf("   [%s] %s\n", who, fmt.Sprintf(format, args...))
}

func newLiveRig(t *testing.T, env liveEnv) *liveRig {
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRig{t: t, env: env, ctx: ctx, cancel: cancel, reg: newRegistry(), obs: &observations{changed: make(chan struct{})}}
	fail := func(format string, args ...any) {
		fmt.Printf("CHECK identity: FAIL: "+format+"\n", args...)
		t.FailNow()
	}
	logFile, err := os.OpenFile(filepath.Join(env.work, "runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail("%v", err)
	}
	t.Cleanup(func() { logFile.Close() })
	r.logs = &logRecorder{json: slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})}
	r.log = slog.New(recordingHandler{rec: r.logs})

	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: env.runtimeKubeconfig}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: env.runtimeContext}).ClientConfig()
	if err != nil {
		fail("the runtime context %s in %s: %v", env.runtimeContext, env.runtimeKubeconfig, err)
	}
	r.rc = rc
	// The harness's own reads under the runtime identity ask the rules of every namespace, so
	// they run faster than client-go's default rate; the runtime keeps the default.
	own := rest.CopyConfig(rc)
	own.QPS, own.Burst = 50, 100
	if r.kube, err = kubernetes.NewForConfig(own); err != nil {
		fail("%v", err)
	}
	if r.dyn, err = dynamic.NewForConfig(own); err != nil {
		fail("%v", err)
	}

	prompt := filepath.Join(env.work, "role-prompt.md")
	if err := os.WriteFile(prompt, []byte("The Stage 4a stub agent; no model reads this.\n"), 0o600); err != nil {
		fail("%v", err)
	}
	r.prompt = prompt

	for _, c := range []struct {
		name, tree, issue string
		role              claim.Role
	}{
		{"root", "S4A-1", "S4A-1", claim.RoleArchitect},
		{"worker", "S4A-1", "S4A-1", claim.RoleImplementer},
		{"second", "S4A-1", "S4A-1", claim.RoleTester},
		{"fresh", "S4A-1", "S4A-1", claim.RoleReviewer},
		{"orphan", "S4A-1", "S4A-1", claim.RoleMerger},
		{"root2", "S4A-2", "S4A-2", claim.RoleArchitect},
		{"child2", "S4A-2", "S4A-3", claim.RolePlanner},
	} {
		token, err := claim.NewToken(env.claimProject, c.issue, c.role)
		if err != nil {
			fail("%v", err)
		}
		r.claims = append(r.claims, &liveClaim{
			token: token, tree: c.tree, issue: c.issue, role: c.role, name: c.name,
			marker: ompSessionsDir + "/" + SandboxName(token) + ".marker",
		})
	}
	return r
}

func (r *liveRig) claim(name string) *liveClaim {
	i := slices.IndexFunc(r.claims, func(c *liveClaim) bool { return c.name == name })
	return r.claims[i]
}

func (r *liveRig) close() {
	r.stopRuntime()
	r.cancel()
}

// sameLocator compares two locators by what they name; the struct holds pointers.
func sameLocator(a, b runtime.Locator) bool {
	return a.Runtime == b.Runtime && a.Claim == b.Claim && a.Incarnation == b.Incarnation &&
		(a.Sandbox == nil) == (b.Sandbox == nil) && (a.Sandbox == nil || *a.Sandbox == *b.Sandbox)
}

// startRuntime binds a fresh listener on the devbox's private address and builds a fresh runtime
// on it, as a daemon boot does, with an Observe feeding the run's observation record.
func (r *liveRig) startRuntime() error {
	if r.tokens.apps == nil {
		if err := r.resolveApp(); err != nil {
			return err
		}
	}
	ctx, stop := context.WithCancel(r.ctx)
	address := "tcp://" + r.env.streamHost + ":" + r.env.streamPort
	ln, err := stream.Listen(ctx, address, r.reg.resolve, stream.Options{RPCTimeout: 30 * time.Second, Log: r.log})
	if err != nil {
		stop()
		holder, _ := exec.Command("ss", "-Hltnp", "sport = :"+r.env.streamPort).CombinedOutput()
		return fmt.Errorf("the worker stream cannot bind %s: %v; the port's holder: %s", address, err, strings.TrimSpace(string(holder)))
	}
	rt, err := New(ctx, r.rc, Options{
		Namespace: r.env.namespace, Project: r.env.project, Image: r.env.image, StorageClass: "gp2", Scheduling: liveScheduling,
		StreamURL: address,
		Tools:     Tools{GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/go/bin/legion"},
		Gateway:   liveGateway,
		Agent:     stubAgent, BootTimeout: liveBootTimeout, BootIntervals: liveBootIntervals,
		TerminationGrace: liveGrace, ProbeInterval: liveProbeInterval, AdoptTimeout: liveAdoptTimeout,
		Tokens: r.tokens, Conns: ln, Log: r.log,
	})
	if err != nil {
		stop()
		return err
	}
	observe, err := rt.Observe(ctx)
	if err != nil {
		stop()
		return err
	}
	r.rt, r.ln, r.stop = rt, ln, stop
	r.stopped.Add(2)
	go func() {
		defer r.stopped.Done()
		for obs := range observe {
			r.obs.add(obs)
		}
	}()
	go func() {
		defer r.stopped.Done()
		for event := range ln.Events() {
			if hello, ok := event.(stream.Hello); ok {
				r.reg.hello(hello)
			}
		}
	}()
	return nil
}

// startRuntimeOnce starts the runtime unless one is running.
func (r *liveRig) startRuntimeOnce() error {
	if r.rt != nil {
		return nil
	}
	return r.startRuntime()
}

// stopRuntime ends the current runtime and its listener, as a daemon exit does: the informers,
// the Observe, every connection.
func (r *liveRig) stopRuntime() {
	if r.stop == nil {
		return
	}
	r.stop()
	r.stopped.Wait()
	r.rt, r.ln, r.stop = nil, nil, nil
}

// recordSandbox appends a Sandbox name to the script's record before the Sandbox can exist, so
// the teardown deletes it by its exact name whatever happens next.
func (r *liveRig) recordSandbox(name string) error {
	f, err := os.OpenFile(r.env.record, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, name)
	return err
}

// ---- operator steps ----------------------------------------------------------------------------

// kubectl runs one operator step in the namespace through the admin context.
func (r *liveRig) kubectl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"--context", r.env.operatorContext, "-n", r.env.namespace}, args...)
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// exec runs a command in a claim's main container, through the admin context.
func (r *liveRig) exec(c *liveClaim, command ...string) (string, error) {
	out, err := r.kubectl(append([]string{"exec", SandboxName(c.token), "-c", mainContainer, "--"}, command...)...)
	return strings.TrimSpace(out), err
}

func (r *liveRig) markerLines(c *liveClaim) ([]string, error) {
	out, err := r.exec(c, "cat", c.marker)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// ---- reads under the runtime identity ----------------------------------------------------------

func (r *liveRig) getSandbox(name string) (*sandbox, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	u, err := r.dyn.Resource(sandboxGVR).Namespace(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}

func (r *liveRig) getPod(name string) (*corev1.Pod, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	return r.kube.CoreV1().Pods(r.env.namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r *liveRig) initLog(name string) (string, error) {
	ctx, cancel := call(r.ctx)
	defer cancel()
	raw, err := r.kube.CoreV1().Pods(r.env.namespace).GetLogs(name, &corev1.PodLogOptions{Container: initContainer}).DoRaw(ctx)
	return string(raw), err
}

// poll re-checks cond every two seconds until it holds, fails, or limit passes.
func (r *liveRig) poll(limit time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := cond()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", limit, what)
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ---- launching, as the machine does -----------------------------------------------------------

func (r *liveRig) spec(c *liveClaim, resume string) runtime.SpawnSpec {
	return runtime.SpawnSpec{
		Claim: c.token, Project: r.env.claimProject, Tree: c.tree, Issue: c.issue, Role: c.role,
		Generation: c.gen, BootToken: c.bootToken, Env: map[string]string{"LEGION_E2E_MARKER": c.marker},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{r.prompt}}, Repository: r.env.repo, ResumeSessionFile: resume,
	}
}

// spawn is a Spawn of the claim's next generation; armed false mints a token no hello registers.
func (r *liveRig) spawn(c *liveClaim, armed bool) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	if err := r.recordSandbox(SandboxName(c.token)); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, armed)
	loc, err := r.rt.Spawn(r.ctx, r.spec(c, ""))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// resume is a Resume of the claim's next generation from file, with its last locator as prev.
func (r *liveRig) resume(c *liveClaim, file string) (runtime.Locator, error) {
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	loc, err := r.rt.Resume(r.ctx, c.last, r.spec(c, file))
	if err != nil {
		return runtime.Locator{}, err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	return loc, nil
}

// awaitRunning waits for the claim's current pod to run with its Sandbox Ready, then for its
// shim's hello at the current generation. A pod that runs and never says hello within one boot
// interval is a network-path failure, named as one.
func (r *liveRig) awaitRunning(c *liveClaim, since time.Time) (registration, error) {
	name := SandboxName(c.token)
	var pod *corev1.Pod
	err := r.poll(liveRunningLimit, "pod "+name+" to run with its Sandbox Ready", func() (bool, error) {
		p, err := r.getPod(name)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if string(p.UID) != c.loc.Incarnation {
			return false, fmt.Errorf("pod %s is uid %s, not the incarnation %s the launch returned", name, p.UID, c.loc.Incarnation)
		}
		if terminal(p) {
			log, _ := r.initLog(name)
			return false, fmt.Errorf("pod %s (uid %s) ended %s before it ran: %s; init log: %s", name, p.UID, p.Status.Phase, containerStates(p), strings.TrimSpace(log))
		}
		s, err := r.getSandbox(name)
		if err != nil {
			return false, err
		}
		ready := s.condition(conditionReady)
		pod = p
		return p.Status.Phase == corev1.PodRunning && ready != nil && ready.Status == metav1.ConditionTrue, nil
	})
	if err != nil {
		return registration{}, err
	}
	reg, ok := r.reg.await(c.token, c.gen, since, liveBootTimeout)
	if !ok {
		return registration{}, r.networkPathFailure(pod)
	}
	return reg, nil
}

// networkPathFailure names what a pod that runs but never says hello points at: the devbox
// address and port it dials, and the security groups of the node it runs on (from the node's
// EC2NodeClass, read as the operator).
func (r *liveRig) networkPathFailure(pod *corev1.Pod) error {
	nodeClass, err := r.kubectl("get", "node", pod.Spec.NodeName, "-o", `jsonpath={.metadata.labels.karpenter\.k8s\.aws/ec2nodeclass}`)
	groups := "(unknown: " + fmt.Sprint(err) + ")"
	if err == nil {
		out, err := r.kubectl("get", "ec2nodeclass", strings.TrimSpace(nodeClass), "-o", "jsonpath={.status.securityGroups[*].id}")
		groups = strings.TrimSpace(out)
		if err != nil {
			groups = "(unknown: " + err.Error() + ")"
		}
	}
	return fmt.Errorf("network path: pod %s (uid %s) runs on node %s but no hello reached tcp://%s:%s within %s; the node's security groups are [%s], and the devbox's security group must admit %s from them",
		pod.Name, pod.UID, pod.Spec.NodeName, r.env.streamHost, r.env.streamPort, liveBootTimeout, groups, r.env.streamPort)
}

// suspend is a Suspend of the claim's running process, then the wait for its pod to be gone.
func (r *liveRig) suspend(c *liveClaim) error {
	if err := r.rt.Suspend(r.ctx, *c.loc); err != nil {
		return err
	}
	if err := r.awaitPodGone(c); err != nil {
		return err
	}
	c.loc, c.state = nil, stateSuspended
	return nil
}

func (r *liveRig) awaitPodGone(c *liveClaim) error {
	name := SandboxName(c.token)
	return r.poll(liveGoneLimit, "pod "+name+" to be gone", func() (bool, error) {
		_, err := r.getPod(name)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// ensureRunning and ensureSuspended put a claim where a check starts from: nothing to do in a full
// run, where the earlier checks left it there, and the same runtime calls in a development run.
func (r *liveRig) ensureRunning(c *liveClaim) error {
	if c.state == stateRunning {
		return nil
	}
	since := time.Now()
	var err error
	if c.state == stateNone {
		_, err = r.spawn(c, true)
	} else {
		_, err = r.resume(c, c.marker)
	}
	if err == nil {
		_, err = r.awaitRunning(c, since)
	}
	if err != nil {
		return fmt.Errorf("setup: %s running: %w", c.name, err)
	}
	note("runtime", "setup: %s running at generation %d", c.name, c.gen)
	return nil
}

func (r *liveRig) ensureSuspended(c *liveClaim) error {
	if c.state == stateSuspended {
		return nil
	}
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.suspend(c); err != nil {
		return fmt.Errorf("setup: suspend %s: %w", c.name, err)
	}
	note("runtime", "setup: %s suspended", c.name)
	return nil
}

func containerStates(p *corev1.Pod) string {
	var parts []string
	for _, statuses := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, s := range statuses {
			switch {
			case s.State.Terminated != nil:
				parts = append(parts, fmt.Sprintf("%s terminated %s exit %d", s.Name, s.State.Terminated.Reason, s.State.Terminated.ExitCode))
			case s.State.Waiting != nil:
				parts = append(parts, fmt.Sprintf("%s waiting %s", s.Name, s.State.Waiting.Reason))
			case s.State.Running != nil:
				parts = append(parts, s.Name+" running")
			}
		}
	}
	return strings.Join(parts, "; ")
}

func short(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// live are the claims with a running process, in the roster's order.
func (r *liveRig) live() []*liveClaim {
	var out []*liveClaim
	for _, c := range r.claims {
		if c.state == stateRunning {
			out = append(out, c)
		}
	}
	return out
}

// known is every claim the daemon has not retired, but for skip.
func (r *liveRig) known(skip *liveClaim) []runtime.Known {
	var out []runtime.Known
	for _, c := range r.claims {
		if c == skip || c.state == stateNone || c.state == stateReleased {
			continue
		}
		out = append(out, runtime.Known{Claim: c.token, Locator: c.loc})
	}
	return out
}
