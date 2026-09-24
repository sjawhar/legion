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
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// liveChecks are the harness's checks, in the order they run. namespace-clean, the last check of
// the proof, is the script's: it compares the namespace after the teardown, outside the harness.
var liveChecks = []string{
	"identity", "installed", "boot-refusal-negative", "image-probe", "root-ready", "gvisor",
	"adopt-working-copy", "worker-colocated", "suspend", "no-affinity", "resume",
	"same-agent-negative", "kill-pod", "stale-incarnation", "respawn-before-register",
	"concurrent-provision", "re-adopt", "orphan-sweep", "release-tree",
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
	if env.from != "" && (env.from == "identity" || env.from == "stale-incarnation" || !slices.Contains(liveChecks, env.from)) {
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

func (c *liveClaim) located() *runtime.Locator { return c.loc }

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

// implementTokens is the harness's ProvisionTokens: the implement App's installation token,
// minted in-process from the key the devbox's secret store holds (Sami's ruling on ask e3943412).
type implementTokens struct{ apps *appauth.Manager }

func (t implementTokens) Token(ctx context.Context, owner string) (string, error) {
	lease, err := t.apps.Token(ctx, appauth.Implement, owner)
	return lease.Token, err
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

	claims map[string]*liveClaim
	// What kill-pod recorded for stale-incarnation.
	killed struct {
		old, fresh runtime.Locator
		gone       observed
		resumedAt  int
	}
}

func TestStage4aSandboxRuntimeLive(t *testing.T) {
	env := readLiveEnv(t)
	r := newLiveRig(t, env)
	defer r.close()

	skipping := env.from != ""
	for _, name := range liveChecks {
		if skipping && name == env.from {
			skipping = false
		}
		if skipping && name != "identity" {
			fmt.Printf("CHECK %s: SKIPPED (development run from %s)\n", name, env.from)
			continue
		}
		r.run(name, r.checkFunc(name))
	}
	fmt.Println("stage 4a harness: every check passed")
}

func (r *liveRig) checkFunc(name string) func() error {
	return map[string]func() error{
		"identity":                r.checkIdentity,
		"installed":               r.checkInstalled,
		"boot-refusal-negative":   r.checkBootRefusal,
		"image-probe":             r.checkImageProbe,
		"root-ready":              r.checkRootReady,
		"gvisor":                  r.checkGVisor,
		"adopt-working-copy":      r.checkAdoptWorkingCopy,
		"worker-colocated":        r.checkWorkerColocated,
		"suspend":                 r.checkSuspend,
		"no-affinity":             r.checkNoAffinity,
		"resume":                  r.checkResume,
		"same-agent-negative":     r.checkSameAgentNegative,
		"kill-pod":                r.checkKillPod,
		"stale-incarnation":       r.checkStaleIncarnation,
		"respawn-before-register": r.checkRespawnBeforeRegister,
		"concurrent-provision":    r.checkConcurrentProvision,
		"re-adopt":                r.checkReAdopt,
		"orphan-sweep":            r.checkOrphanSweep,
		"release-tree":            r.checkReleaseTree,
	}[name]
}

func (r *liveRig) run(name string, check func() error) {
	fmt.Printf("== %s\n", name)
	started := time.Now()
	if err := check(); err != nil {
		fmt.Printf("CHECK %s: FAIL: %v\n", name, err)
		r.t.FailNow()
	}
	fmt.Printf("CHECK %s: PASS (%s)\n", name, time.Since(started).Round(time.Second))
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

	r.claims = map[string]*liveClaim{}
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
		r.claims[c.name] = &liveClaim{
			token: token, tree: c.tree, issue: c.issue, role: c.role, name: c.name,
			marker: ompSessionsDir + "/" + SandboxName(token) + ".marker",
		}
	}
	return r
}

func (r *liveRig) claim(name string) *liveClaim { return r.claims[name] }

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
	dir, err := workspace.Location(TreeRoot, r.env.repo, c.issue)
	if err != nil {
		panic(err)
	}
	return runtime.SpawnSpec{
		Claim: c.token, Project: r.env.claimProject, Tree: c.tree, Issue: c.issue, Role: c.role,
		Generation: c.gen, BootToken: c.bootToken, Env: map[string]string{"LEGION_E2E_MARKER": c.marker},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{r.prompt}}, Workspace: dir.Dir, ResumeSessionFile: resume,
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
	var prev runtime.Locator
	if c.last != nil {
		prev = *c.last
	}
	if err := r.startRuntimeOnce(); err != nil {
		return runtime.Locator{}, err
	}
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	loc, err := r.rt.Resume(r.ctx, prev, r.spec(c, file))
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

// ---- checks ------------------------------------------------------------------------------------

// identity: the runtime's client is the restricted Legion daemon identity, and nothing more.
func (r *liveRig) checkIdentity() error {
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Minute)
	defer cancel()
	review, err := r.kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("SelfSubjectReview through the runtime context: %w", err)
	}
	user := review.Status.UserInfo
	out, err := r.kubectl("auth", "whoami", "-o", "json")
	if err != nil {
		return err
	}
	var operator authnv1.SelfSubjectReview
	if err := json.Unmarshal([]byte(out), &operator); err != nil {
		return fmt.Errorf("the operator's whoami: %w", err)
	}
	note("runtime", "SelfSubjectReview: user %s, groups %v", user.Username, user.Groups)
	note("operator", "whoami: user %s", operator.Status.UserInfo.Username)
	if user.Username == operator.Status.UserInfo.Username {
		return fmt.Errorf("the runtime context authenticates as the operator (%s); refusing to start", user.Username)
	}
	if !regexp.MustCompile(`:assumed-role/[A-Za-z0-9+=,.@_-]*legion-daemon/`).MatchString(user.Username) {
		return fmt.Errorf("the runtime identity %s is not the assumed Legion daemon role", user.Username)
	}
	if !slices.Contains(user.Groups, "legion-daemon") {
		return fmt.Errorf("the runtime identity is not in group legion-daemon: %v", user.Groups)
	}

	_, err = r.kube.CoreV1().Secrets(r.env.namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if !apierrors.IsForbidden(err) {
		return fmt.Errorf("list secrets in %s through the runtime identity answered %v, want 403", r.env.namespace, err)
	}
	note("runtime", "list secrets -n %s: 403 Forbidden", r.env.namespace)

	// can-i --list, which is a SelfSubjectRulesReview, in every namespace: an RBAC grant anywhere
	// that the plan does not make fails the check, a stray RoleBinding in another namespace
	// included.
	out, err = r.kubectl("get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	namespaces := strings.Fields(out)
	var extra []string
	incomplete := ""
	for _, ns := range namespaces {
		rules, err := r.kube.AuthorizationV1().SelfSubjectRulesReviews().Create(ctx, &authzv1.SelfSubjectRulesReview{
			Spec: authzv1.SelfSubjectRulesReviewSpec{Namespace: ns},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("rules review in %s: %w", ns, err)
		}
		if rules.Status.Incomplete {
			incomplete = rules.Status.EvaluationError
		}
		for _, rule := range rules.Status.ResourceRules {
			for _, g := range expand(rule) {
				if !planGrants(ns, g) {
					extra = append(extra, ns+": "+g)
				}
			}
		}
		for _, rule := range rules.Status.NonResourceRules {
			if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
				extra = append(extra, fmt.Sprintf("%s: %v on %v", ns, rule.Verbs, rule.NonResourceURLs))
			}
		}
	}
	note("operator", "%d namespaces listed", len(namespaces))
	if len(extra) > 0 {
		return fmt.Errorf("the runtime identity holds %d grants the plan does not make: %s", len(extra), strings.Join(extra[:min(len(extra), 20)], "; "))
	}
	note("runtime", "rules review (can-i --list) in all %d namespaces: every resource grant is the plan's, every non-resource grant is get", len(namespaces))
	if incomplete != "" {
		note("runtime", "the API server marks the rules incomplete (%s); the access reviews below ask the webhook too", incomplete)
	}

	// Access reviews go through every authorizer, EKS's webhook included, which a rules review
	// cannot enumerate. Cluster-wide, and a positive control each for the two grants that must hold.
	type question struct {
		allow bool
		attrs authzv1.ResourceAttributes
	}
	questions := []question{
		{true, authzv1.ResourceAttributes{Namespace: r.env.namespace, Verb: "create", Group: "agents.x-k8s.io", Resource: "sandboxes"}},
		{true, authzv1.ResourceAttributes{Namespace: "agent-sandbox-system", Verb: "get", Group: "apps", Resource: "deployments", Name: "agent-sandbox-controller"}},
	}
	for _, resource := range []string{"users", "groups", "serviceaccounts", "uids"} {
		questions = append(questions, question{false, authzv1.ResourceAttributes{Verb: "impersonate", Resource: resource}})
	}
	questions = append(questions,
		question{false, authzv1.ResourceAttributes{Verb: "impersonate", Group: "authentication.k8s.io", Resource: "userextras"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "serviceaccounts", Subresource: "token"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "pods"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "pods", Subresource: "exec"}},
		question{false, authzv1.ResourceAttributes{Verb: "list", Resource: "secrets"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Resource: "secrets"}},
		question{false, authzv1.ResourceAttributes{Verb: "get", Resource: "persistentvolumeclaims"}},
		question{false, authzv1.ResourceAttributes{Verb: "list", Resource: "nodes"}},
		question{false, authzv1.ResourceAttributes{Verb: "create", Group: "agents.x-k8s.io", Resource: "sandboxes"}},
	)
	for _, resource := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
		for _, verb := range []string{"create", "update", "patch", "escalate", "bind"} {
			questions = append(questions, question{false, authzv1.ResourceAttributes{Verb: verb, Group: "rbac.authorization.k8s.io", Resource: resource}})
		}
	}
	allowed, denied := 0, 0
	for _, q := range questions {
		review, err := r.kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{
			Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &q.attrs},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("access review %+v: %w", q.attrs, err)
		}
		what := describeAttrs(q.attrs)
		if review.Status.Allowed != q.allow {
			return fmt.Errorf("access review %s answered allowed=%t, want %t (%s)", what, review.Status.Allowed, q.allow, review.Status.Reason)
		}
		if q.allow {
			allowed++
		} else {
			denied++
		}
	}
	note("runtime", "access reviews: %d positive controls allowed; %d denied cluster-wide (impersonation of every kind, serviceaccounts/token, pods and pods/exec, secrets list and create, PVC get, nodes, RBAC create/update/patch/escalate/bind, sandboxes outside %s)", allowed, denied, r.env.namespace)
	return nil
}

// expand is a resource rule as one "verb group/resource[ name]" per combination.
func expand(rule authzv1.ResourceRule) []string {
	groups, names := rule.APIGroups, rule.ResourceNames
	if len(groups) == 0 {
		groups = []string{""}
	}
	if len(names) == 0 {
		names = []string{""}
	}
	var out []string
	for _, verb := range rule.Verbs {
		for _, group := range groups {
			for _, resource := range rule.Resources {
				for _, name := range names {
					out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s/%s %s", verb, group, resource, name)))
				}
			}
		}
	}
	return out
}

// planGrants is the Stage 4 plan's Task 4.4 grant set, with the self-review endpoints every
// authenticated user has.
func planGrants(ns, grant string) bool {
	everywhere := []string{
		"create authentication.k8s.io/selfsubjectreviews",
		"create authorization.k8s.io/selfsubjectaccessreviews",
		"create authorization.k8s.io/selfsubjectrulesreviews",
		"get apiextensions.k8s.io/customresourcedefinitions sandboxes.agents.x-k8s.io",
	}
	if slices.Contains(everywhere, grant) {
		return true
	}
	switch ns {
	case "legion":
		var legion []string
		for _, verb := range []string{"create", "get", "list", "watch", "patch", "delete"} {
			legion = append(legion, verb+" agents.x-k8s.io/sandboxes")
		}
		for _, verb := range []string{"get", "list", "watch"} {
			legion = append(legion, verb+" /pods")
		}
		for _, verb := range []string{"create", "get", "update", "delete"} {
			legion = append(legion, verb+" /secrets")
		}
		legion = append(legion, "get agents.x-k8s.io/sandboxes/status", "get /pods/log", "list /events")
		return slices.Contains(legion, grant)
	case "agent-sandbox-system":
		return grant == "get apps/deployments agent-sandbox-controller"
	}
	return false
}

func describeAttrs(a authzv1.ResourceAttributes) string {
	resource := a.Resource
	if a.Group != "" {
		resource += "." + a.Group
	}
	if a.Subresource != "" {
		resource += "/" + a.Subresource
	}
	if a.Name != "" {
		resource += "/" + a.Name
	}
	scope := "cluster-wide"
	if a.Namespace != "" {
		scope = "-n " + a.Namespace
	}
	return fmt.Sprintf("%s %s %s", a.Verb, resource, scope)
}

// installed: Requirement 10's accepting path, under the shipped resourceNames grants.
func (r *liveRig) checkInstalled() error {
	if err := CheckInstalled(r.ctx, r.rc, productionInstall); err != nil {
		return err
	}
	note("runtime", "CheckInstalled(%s, %s/%s): nil — the CRD serves %s and the controller has an available replica",
		productionInstall.CRD, productionInstall.ControllerNamespace, productionInstall.ControllerName, sandboxGVR.Version)
	return nil
}

// boot-refusal-negative: a controller that is not there is refused by name, although the
// resourceNames grant makes the API answer 403 rather than 404.
func (r *liveRig) checkBootRefusal() error {
	ref := productionInstall
	ref.ControllerName = "legion-no-such-controller"
	err := CheckInstalled(r.ctx, r.rc, ref)
	if err == nil {
		return errors.New("CheckInstalled accepted a controller Deployment that does not exist")
	}
	message := err.Error()
	note("runtime", "CheckInstalled refused: %s", message)
	switch {
	case !strings.Contains(message, "agent-sandbox-system/legion-no-such-controller"):
		return errors.New("the refusal does not name the Deployment agent-sandbox-system/legion-no-such-controller")
	case !strings.Contains(message, "(403)"):
		return errors.New("the refusal is not the resourceNames grant's 403")
	case strings.Contains(message, "Sandbox CRD"):
		return errors.New("the refusal also blames the CRD, which is installed")
	}
	return nil
}

// image-probe: the probe Sandbox passes on the stage image and confirms the Go daemon API contract.
func (r *liveRig) checkImageProbe() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	_, digest, _ := strings.Cut(r.env.image, "@sha256:")
	name := probeName(r.env.project, digest)
	if err := r.recordSandbox(name); err != nil {
		return err
	}
	stateDir := filepath.Join(r.env.work, "probe-state")
	err := r.rt.ProbeImage(r.ctx, ImageProbe{
		Contract: api.GoDaemonAPIVersion, StateDir: stateDir, Budget: 10 * time.Minute,
		Retry: bootprobe.Retry{Initial: 15 * time.Second, Max: time.Minute, Attempts: 3}, APIServer: r.rc.Host,
	})
	if err != nil {
		return err
	}
	passed, ok := r.logs.find("sandbox runtime: the worker image passed its probe")
	if !ok {
		return errors.New("ProbeImage returned nil without logging a pass")
	}
	contract, ok := bootprobe.ConfirmedContract(passed["log"])
	if !ok || contract != api.GoDaemonAPIVersion {
		return fmt.Errorf("the probe log confirms contract %d (found %t), want %d: %s", contract, ok, api.GoDaemonAPIVersion, passed["log"])
	}
	note("runtime", "probe Sandbox %s passed: %s", passed["sandbox"], lastLine(passed["log"], bootprobe.OKPrefix))
	note("runtime", "go-daemon-api-version=%d parsed, the daemon's contract", contract)
	if err := r.poll(liveGoneLimit, "probe Sandbox "+name+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(name)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	note("runtime", "probe Sandbox %s deleted after the attempt", name)
	return nil
}

func lastLine(text, prefix string) string {
	for _, line := range slices.Backward(strings.Split(text, "\n")) {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *liveRig) startRuntimeOnce() error {
	if r.rt != nil {
		return nil
	}
	return r.startRuntime()
}

// root-ready: the root claim spawns, provisions its workspace, and registers at generation 1.
func (r *liveRig) checkRootReady() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	root := r.claim("root")
	since := time.Now()
	loc, err := r.spawn(root, true)
	if err != nil {
		return err
	}
	note("runtime", "Spawn(%s) returned incarnation %s", root.token, loc.Incarnation)
	reg, err := r.awaitRunning(root, since)
	if err != nil {
		return err
	}
	name := SandboxName(root.token)
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if string(pod.UID) != loc.Incarnation {
		return fmt.Errorf("pod %s is uid %s, the launch returned %s", name, pod.UID, loc.Incarnation)
	}
	note("runtime", "Sandbox %s Ready=True; pod uid %s equals the returned incarnation", name, pod.UID)
	log, err := r.initLog(name)
	if err != nil {
		return err
	}
	dir, _ := workspace.Location(TreeRoot, r.env.repo, root.issue)
	want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark
	if !strings.Contains(log, want) {
		return fmt.Errorf("the init log (pods/log) lacks %q: %s", want, strings.TrimSpace(log))
	}
	note("runtime", "init log (pods/log): %q", want)
	if reg.gen != 1 || reg.hash != tokenHash(root.bootToken) {
		return fmt.Errorf("registered at generation %d with token hash %s, want generation 1 with %s", reg.gen, short(reg.hash), short(tokenHash(root.bootToken)))
	}
	note("harness", "hello registered %s at generation 1, token sha256 %s… (the generation-1 token's)", root.token, reg.hash[:12])
	return nil
}

// gvisor: the root pod runs under gVisor, on the gvisor RuntimeClass.
func (r *liveRig) checkGVisor() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	kernel, err := r.exec(root, "uname", "-r")
	if err != nil {
		return err
	}
	name := SandboxName(root.token)
	class, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.spec.runtimeClassName}")
	if err != nil {
		return err
	}
	nodeName, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.spec.nodeName}")
	if err != nil {
		return err
	}
	host, err := r.kubectl("get", "node", nodeName, "-o", "jsonpath={.status.nodeInfo.kernelVersion}")
	if err != nil {
		return err
	}
	note("operator", "exec %s -- uname -r: %s; node %s's kernel: %s", name, kernel, nodeName, host)
	note("operator", "pod %s .spec.runtimeClassName: %s", name, class)
	switch {
	case class != gvisor:
		return fmt.Errorf("runtimeClassName is %q, want %s", class, gvisor)
	case !strings.HasSuffix(kernel, "-gvisor"):
		return fmt.Errorf("uname -r in the pod is %q, not gVisor's emulated kernel (…-gvisor)", kernel)
	case kernel == host:
		return fmt.Errorf("the pod reports the node's own kernel %s", host)
	}
	return nil
}

// adopt-working-copy: the root's shim sets its working copy's author, in the pod's workspace.
func (r *liveRig) checkAdoptWorkingCopy() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.rt.AdoptWorkingCopy(r.ctx, *root.loc, r.identity); err != nil {
		return err
	}
	note("runtime", "AdoptWorkingCopy(%s, %s <%s>): nil", root.token, r.identity.Name, r.identity.Email)
	author, err := r.exec(root, "sh", "-c", `cd "$LEGION_WORKSPACE" && jj log -r @ --no-graph -T author`)
	if err != nil {
		return err
	}
	note("operator", "exec: cd \"$LEGION_WORKSPACE\" && jj log -r @ --no-graph -T author: %s", author)
	if !strings.Contains(author, r.identity.Name) || !strings.Contains(author, r.identity.Email) {
		return fmt.Errorf("the working copy's author is %q, not %s <%s>", author, r.identity.Name, r.identity.Email)
	}
	return nil
}

// treeAffinity reports whether the pod requires the node of another pod of its tree.
func treeAffinity(p *corev1.Pod, tree string) bool {
	if p.Spec.Affinity == nil || p.Spec.Affinity.PodAffinity == nil {
		return false
	}
	for _, term := range p.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == corev1.LabelHostname && term.LabelSelector != nil && term.LabelSelector.MatchLabels[labelTree] == tree {
			return true
		}
	}
	return false
}

// worker-colocated: a worker spawned while the root runs requires, and gets, the root's node.
func (r *liveRig) checkWorkerColocated() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	since := time.Now()
	if _, err := r.spawn(worker, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(worker, since); err != nil {
		return err
	}
	pod, err := r.getPod(SandboxName(worker.token))
	if err != nil {
		return err
	}
	rootPod, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	if !treeAffinity(pod, worker.tree) {
		return fmt.Errorf("the worker pod carries no required podAffinity on %s=%s with topology %s: %+v", labelTree, worker.tree, corev1.LabelHostname, pod.Spec.Affinity)
	}
	if pod.Spec.NodeName != rootPod.Spec.NodeName {
		return fmt.Errorf("the worker runs on %s, the root on %s", pod.Spec.NodeName, rootPod.Spec.NodeName)
	}
	note("runtime", "worker pod %s: required podAffinity %s=%s, topology %s; node %s, the root's", pod.Name, labelTree, worker.tree, corev1.LabelHostname, pod.Spec.NodeName)
	return nil
}

// suspend: the worker's pod goes and its Sandbox and the tree volume stay; the runtime reports
// the recorded process Gone when asked, and Observe says nothing more about it that the supervisor
// would act on.
func (r *liveRig) checkSuspend() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	loc := *worker.loc
	if err := r.rt.Suspend(r.ctx, loc); err != nil {
		return err
	}
	returned := r.obs.mark()
	name := SandboxName(worker.token)
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeSuspended {
		return fmt.Errorf("Sandbox %s is %s after Suspend returned", name, s.mode())
	}
	if err := r.awaitPodGone(worker); err != nil {
		return err
	}
	worker.loc, worker.state = nil, stateSuspended
	note("runtime", "Sandbox %s operatingMode Suspended; pod %s gone", name, name)
	pvc := TreeClaimName(root.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	if phase != "Bound" {
		return fmt.Errorf("the tree PVC %s is %q", pvc, phase)
	}
	note("operator", "PVC %s: Bound", pvc)
	obs, err := r.rt.Probe(r.ctx, loc)
	if err != nil {
		return err
	}
	if obs.Kind != runtime.Gone || !sameLocator(obs.Locator, loc) {
		return fmt.Errorf("Probe(the recorded locator) answered %s for %s: %s", obs.Kind, obs.Locator.Incarnation, obs.Detail)
	}
	note("runtime", "Probe(recorded %s): gone — %s", short(loc.Incarnation), obs.Detail)
	time.Sleep(liveSettle)
	late := map[runtime.ObservationKind]int{}
	for _, o := range r.obs.since(returned) {
		if o.Locator.Claim != worker.token {
			continue
		}
		if (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess) && passesFence(worker, o.Observation) {
			return fmt.Errorf("Observe delivered %s for the suspended worker after Suspend returned, carrying the incarnation the claim holds: %s", o.Kind, o.Detail)
		}
		late[o.Kind]++
	}
	note("runtime", "Observe after Suspend returned, over %s: nothing the supervisor's fence would pass (late, each carrying the suspended locator the claim no longer holds: %d alive, %d gone, %d not_recorded_process, %d uncertain)",
		liveSettle, late[runtime.Alive], late[runtime.Gone], late[runtime.NotRecordedProcess], late[runtime.Uncertain])
	return nil
}

// passesFence is the supervisor's incarnation fence (supervise/machine.go, fence): an observation
// reaches the claim's machine only when the claim holds a locator and the observation carries its
// incarnation.
func passesFence(c *liveClaim, o runtime.Observation) bool {
	return c.loc != nil && o.Locator.Incarnation == c.loc.Incarnation
}

// no-affinity: with no pod of the tree scheduled, a worker and then the resumed root carry no
// affinity, schedule anywhere, and mount the tree volume.
func (r *liveRig) checkNoAffinity() error {
	root, second := r.claim("root"), r.claim("second")
	if err := r.ensureSuspended(r.claim("worker")); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.suspend(root); err != nil {
		return err
	}
	note("runtime", "root suspended; its pod gone")
	pods, err := r.kube.CoreV1().Pods(r.env.namespace).List(r.ctx, metav1.ListOptions{LabelSelector: labelProject + "=" + r.env.project + "," + labelTree + "=" + root.tree})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" && !terminal(&p) && p.DeletionTimestamp == nil {
			return fmt.Errorf("pod %s of the tree is still scheduled on %s", p.Name, p.Spec.NodeName)
		}
	}
	note("runtime", "no pod of tree %s is scheduled", root.tree)

	since := time.Now()
	if _, err := r.spawn(second, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	pod, err := r.getPod(SandboxName(second.token))
	if err != nil {
		return err
	}
	if treeAffinity(pod, second.tree) || (pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAffinity != nil) {
		return fmt.Errorf("the worker spawned with no tree pod scheduled carries an affinity: %+v", pod.Spec.Affinity)
	}
	claimName := ""
	for _, v := range pod.Spec.Volumes {
		if v.Name == treeVolume && v.PersistentVolumeClaim != nil {
			claimName = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claimName != TreeClaimName(root.token) {
		return fmt.Errorf("the worker mounts claim %q, not the tree's %s", claimName, TreeClaimName(root.token))
	}
	phase, err := r.kubectl("get", "pvc", claimName, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	note("runtime", "second worker pod %s: no affinity, node %s, Ready, mounts %s", pod.Name, pod.Spec.NodeName, claimName)
	note("operator", "PVC %s: %s", claimName, phase)
	if phase != "Bound" {
		return fmt.Errorf("the tree PVC is %q", phase)
	}
	if err := r.suspend(second); err != nil {
		return err
	}
	note("runtime", "second worker suspended; its pod gone")

	since = time.Now()
	if _, err := r.resume(root, root.marker); err != nil {
		return err
	}
	if _, err := r.awaitRunning(root, since); err != nil {
		return err
	}
	rootPod, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	if rootPod.Spec.Affinity != nil && rootPod.Spec.Affinity.PodAffinity != nil {
		return fmt.Errorf("the resumed root carries an affinity although no other tree pod is scheduled: %+v", rootPod.Spec.Affinity)
	}
	note("runtime", "root resumed as %s: no affinity, node %s, Ready", short(string(rootPod.UID)), rootPod.Spec.NodeName)
	return nil
}

// resume: the first worker comes back as a new incarnation of the same agent, beside the root.
func (r *liveRig) checkResume() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureSuspended(worker); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	old := *worker.last
	since := time.Now()
	loc, err := r.resume(worker, worker.marker)
	if err != nil {
		return err
	}
	reg, err := r.awaitRunning(worker, since)
	if err != nil {
		return err
	}
	if loc.Incarnation == old.Incarnation {
		return fmt.Errorf("the resumed incarnation %s is the old one", loc.Incarnation)
	}
	note("runtime", "Resume(prev %s) returned %s", short(old.Incarnation), short(loc.Incarnation))
	pod, err := r.getPod(SandboxName(worker.token))
	if err != nil {
		return err
	}
	if !treeAffinity(pod, worker.tree) {
		return fmt.Errorf("the resumed worker carries no tree affinity although the root runs: %+v", pod.Spec.Affinity)
	}
	note("runtime", "resumed pod requires the tree's node again (the root is Ready); node %s", pod.Spec.NodeName)
	if reg.hash != tokenHash(worker.bootToken) {
		return fmt.Errorf("the registration's token hash %s is not generation %d's", short(reg.hash), worker.gen)
	}
	note("harness", "hello registered %s at generation %d, token sha256 %s… (that generation's)", worker.token, reg.gen, reg.hash[:12])
	lines, err := r.markerLines(worker)
	if err != nil {
		return err
	}
	note("operator", "exec cat %s: %v", worker.marker, lines)
	if len(lines) != 2 || lines[0] != old.Incarnation || lines[1] != loc.Incarnation {
		return fmt.Errorf("the marker holds %v, want exactly [%s %s]", lines, old.Incarnation, loc.Incarnation)
	}
	return nil
}

// same-agent-negative: a resume naming a session the volume does not hold never starts a fresh
// agent; the init container refuses, and the runtime reports it Gone with the refusal.
func (r *liveRig) checkSameAgentNegative() error {
	second := r.claim("second")
	if err := r.ensureSuspended(second); err != nil {
		return err
	}
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	absent := ompSessionsDir + "/absent-" + SandboxName(second.token) + ".marker"
	mark := r.obs.mark()
	loc, err := r.resume(second, absent)
	if err != nil {
		return err
	}
	note("runtime", "Resume naming %s returned %s", absent, short(loc.Incarnation))
	gone, ok := r.obs.await(mark, liveRunningLimit, func(o runtime.Observation) bool {
		return sameLocator(o.Locator, loc) && o.Kind != runtime.Alive && o.Kind != runtime.Uncertain
	})
	if !ok {
		return fmt.Errorf("no final observation of %s within %s", loc.Incarnation, liveRunningLimit)
	}
	want := "Refusing to start " + second.issue + " fresh"
	if gone.Kind != runtime.Gone || !strings.Contains(gone.Detail, want) || !strings.Contains(gone.Detail, "init container "+initContainer) {
		return fmt.Errorf("observed %s, want gone quoting the init container's %q: %s", gone.Kind, want, gone.Detail)
	}
	note("runtime", "Observe: gone for %s — %s", short(loc.Incarnation), oneLine(gone.Detail))
	if regs := r.reg.registrations(second.token); len(regs) > 0 && regs[len(regs)-1].gen == second.gen {
		return errors.New("the refused incarnation registered a hello")
	}
	second.loc, second.state = nil, stateDead

	since := time.Now()
	fixed, err := r.resume(second, second.marker)
	if err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	lines, err := r.markerLines(second)
	if err != nil {
		return err
	}
	note("operator", "resumed correctly as %s; exec cat %s: %v", short(fixed.Incarnation), second.marker, lines)
	if len(lines) != 2 || lines[1] != fixed.Incarnation || slices.Contains(lines, loc.Incarnation) {
		return fmt.Errorf("the marker holds %v: want two agents, the last %s, and never the refused %s", lines, fixed.Incarnation, loc.Incarnation)
	}
	if err := r.suspend(second); err != nil {
		return err
	}
	note("runtime", "second worker suspended again, for the checks that need a suspended claim")
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// kill-pod: a worker killed in place is reported Gone with its exit, and Resume(prev=dead)
// relaunches it through Suspended.
func (r *liveRig) checkKillPod() error {
	worker := r.claim("worker")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	old := *worker.loc
	name := SandboxName(worker.token)
	mark := r.obs.mark()
	if _, err := r.exec(worker, "sh", "-c", "kill 1"); err != nil {
		return err
	}
	note("operator", "exec %s -- sh -c 'kill 1'", name)
	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == worker.token && o.Kind == runtime.Gone
	})
	if !ok {
		return fmt.Errorf("no gone for %s within %s", name, liveGoneLimit)
	}
	if !sameLocator(gone.Locator, old) || !strings.Contains(gone.Detail, string(old.Incarnation)) || !strings.Contains(gone.Detail, "main container "+mainContainer+" terminated") {
		return fmt.Errorf("the gone carries %s, want the old %s with the main container's exit: %s", gone.Locator.Incarnation, old.Incarnation, gone.Detail)
	}
	exit := regexp.MustCompile(`exit code (-?\d+)`).FindStringSubmatch(gone.Detail)
	if exit == nil {
		return fmt.Errorf("the gone names no exit code: %s", gone.Detail)
	}
	note("runtime", "Observe: gone for old %s, main container exit code %s — %s", short(old.Incarnation), exit[1], oneLine(firstLine(gone.Detail)))
	worker.loc, worker.state = nil, stateDead
	before, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if before.mode() != modeRunning {
		return fmt.Errorf("the dead pod's Sandbox is %s before the relaunch", before.mode())
	}
	resumedAt := r.obs.mark()
	since := time.Now()
	fresh, err := r.resume(worker, worker.marker)
	if err != nil {
		return err
	}
	if _, err := r.awaitRunning(worker, since); err != nil {
		return err
	}
	after, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if after.Generation-before.Generation != 2 {
		return fmt.Errorf("the Sandbox went from generation %d to %d; a relaunch through Suspended is two writes", before.Generation, after.Generation)
	}
	note("runtime", "Resume(prev=dead %s) returned %s; Sandbox generation %d → %d (Suspended, then the template and Running), the dead pod gone", short(old.Incarnation), short(fresh.Incarnation), before.Generation, after.Generation)
	r.killed.old, r.killed.fresh, r.killed.gone, r.killed.resumedAt = old, fresh, gone, resumedAt
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// stale-incarnation: across the kill and the relaunch, nothing about the old process reaches the
// new one.
func (r *liveRig) checkStaleIncarnation() error {
	if r.killed.fresh.Incarnation == "" {
		return errors.New("kill-pod did not run; stale-incarnation rides its relaunch")
	}
	worker := r.claim("worker")
	old, fresh := r.killed.old, r.killed.fresh
	time.Sleep(liveSettle)
	seen := 0
	for _, o := range r.obs.since(0) {
		if o.Locator.Incarnation != fresh.Incarnation {
			continue
		}
		seen++
		if o.Kind != runtime.Alive && o.Kind != runtime.Uncertain {
			return fmt.Errorf("an observation carrying the new %s is %s: %s", fresh.Incarnation, o.Kind, o.Detail)
		}
	}
	if seen == 0 {
		return fmt.Errorf("no observation carries the new %s after %s, so none could be judged", fresh.Incarnation, liveSettle)
	}
	note("runtime", "%d observations carry the new %s, every one alive or uncertain", seen, short(fresh.Incarnation))
	if r.killed.gone.Locator.Incarnation != old.Incarnation {
		return fmt.Errorf("the gone carried %s, not the old %s", r.killed.gone.Locator.Incarnation, old.Incarnation)
	}
	note("runtime", "the gone carried the old incarnation %s", short(old.Incarnation))
	if err := r.rt.Suspend(r.ctx, old); err != nil {
		return fmt.Errorf("Suspend(the old locator): %w", err)
	}
	name := SandboxName(worker.token)
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	uid, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return err
	}
	if s.mode() != modeRunning || string(pod.UID) != fresh.Incarnation || uid != fresh.Incarnation {
		return fmt.Errorf("Suspend(old) acted: Sandbox %s, pod uid %s (operator: %s), want Running and %s", s.mode(), pod.UID, uid, fresh.Incarnation)
	}
	note("runtime", "Suspend(old %s): nil; Sandbox still Running, pod still %s", short(old.Incarnation), short(string(pod.UID)))
	note("operator", "pod %s uid %s", name, uid)
	return nil
}

// respawn-before-register: a claim suspended before its first hello spawns again over its
// existing Sandbox, as a new incarnation on a rotated Secret.
func (r *liveRig) checkRespawnBeforeRegister() error {
	fresh := r.claim("fresh")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	first, err := r.spawn(fresh, false)
	if err != nil {
		return err
	}
	firstHash := tokenHash(fresh.bootToken)
	if err := r.suspend(fresh); err != nil {
		return err
	}
	if regs := r.reg.registrations(fresh.token); len(regs) > 0 {
		return fmt.Errorf("the claim registered before its first suspend: %+v", regs)
	}
	note("runtime", "Spawn returned %s; suspended before any hello (its token is withheld from the resolver: %d hellos refused, none registered)", short(first.Incarnation), r.reg.refusals(fresh.token))
	fresh.state = stateNone // the machine's view: never registered, so the next launch is a Spawn
	since := time.Now()
	second, err := r.spawn(fresh, true)
	if err != nil {
		return fmt.Errorf("the second Spawn over the existing Sandbox: %w", err)
	}
	reg, err := r.awaitRunning(fresh, since)
	if err != nil {
		return err
	}
	if second.Incarnation == first.Incarnation {
		return errors.New("the second Spawn returned the first incarnation")
	}
	out, err := r.kubectl("get", "secret", secretName(SandboxName(fresh.token)), "-o", "jsonpath={.data."+bootTokenKey+"}")
	if err != nil {
		return err
	}
	stored, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return err
	}
	storedHash := tokenHash(string(stored))
	switch {
	case storedHash == firstHash:
		return errors.New("the Secret still holds the first generation's boot token")
	case storedHash != tokenHash(fresh.bootToken):
		return errors.New("the Secret's boot token is neither generation's")
	case reg.hash != storedHash || reg.gen != 2:
		return fmt.Errorf("registered at generation %d with %s, want 2 with the Secret's %s", reg.gen, short(reg.hash), short(storedHash))
	}
	note("runtime", "second Spawn returned %s (new uid)", short(second.Incarnation))
	note("operator", "Secret %s: boot token sha256 %s… (generation 2's; generation 1's was %s…)", secretName(SandboxName(fresh.token)), storedHash[:12], firstHash[:12])
	note("harness", "hello registered at generation 2 with that token")
	return nil
}

// concurrent-provision: two claims of a new tree launched at once each provision their
// workspace, one after the other, from one clone.
func (r *liveRig) checkConcurrentProvision() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	root2, child := r.claim("root2"), r.claim("child2")
	since := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*liveClaim{root2, child} {
		if err := r.recordSandbox(SandboxName(c.token)); err != nil {
			return err
		}
		c.bootToken, c.gen = r.reg.mint(c.token, true)
		wg.Add(1)
		go func() {
			defer wg.Done()
			loc, err := r.rt.Spawn(r.ctx, r.spec(c, ""))
			if err == nil {
				c.loc, c.last, c.state = &loc, &loc, stateRunning
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	note("runtime", "Spawn(%s) and Spawn(%s) called at once", root2.token, child.token)
	type window struct{ start, end time.Time }
	windows := map[string]window{}
	for _, c := range []*liveClaim{root2, child} {
		if _, err := r.awaitRunning(c, since); err != nil {
			return err
		}
		name := SandboxName(c.token)
		log, err := r.initLog(name)
		if err != nil {
			return err
		}
		dir, _ := workspace.Location(TreeRoot, r.env.repo, c.issue)
		want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark
		if !strings.Contains(log, want) {
			return fmt.Errorf("%s's init log lacks %q: %s", c.name, want, strings.TrimSpace(log))
		}
		pod, err := r.getPod(name)
		if err != nil {
			return err
		}
		for _, s := range pod.Status.InitContainerStatuses {
			if s.Name == initContainer && s.State.Terminated != nil {
				windows[c.name] = window{s.State.Terminated.StartedAt.Time, s.State.Terminated.FinishedAt.Time}
			}
		}
		w := windows[c.name]
		note("runtime", "%s: init log %q; workspace-init ran %s → %s", c.name, want, w.start.UTC().Format(time.TimeOnly), w.end.UTC().Format(time.TimeOnly))
	}
	a, b := windows["root2"], windows["child2"]
	if a.start.IsZero() || b.start.IsZero() {
		return fmt.Errorf("an init container's terminated state is missing: %+v", windows)
	}
	if a.start.Before(b.end) && b.start.Before(a.end) {
		return fmt.Errorf("the two workspace-init runs overlapped: root2 %v–%v, child2 %v–%v", a.start, a.end, b.start, b.end)
	}
	note("runtime", "the two workspace-init runs did not overlap: the runtime serialized them")
	owner, repo, _ := strings.Cut(r.env.repo, "/")
	clone := TreeRoot + "/repos/github.com/" + owner + "/" + repo
	listing, err := r.exec(root2, "ls", "-A", filepath.Dir(clone))
	if err != nil {
		return err
	}
	entries := strings.Fields(listing)
	slices.Sort(entries)
	if !slices.Equal(entries, []string{repo, repo + ".lock"}) {
		return fmt.Errorf("%s holds %v, want one clone and its lock", filepath.Dir(clone), entries)
	}
	workspaces, err := r.exec(root2, "jj", "-R", clone, "workspace", "list")
	if err != nil {
		return err
	}
	for _, issue := range []string{root2.issue, child.issue} {
		if !strings.Contains(workspaces, strings.ToLower(issue)+":") {
			return fmt.Errorf("jj workspace list lacks %s: %s", strings.ToLower(issue), workspaces)
		}
	}
	fsck, err := r.exec(root2, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
	if err != nil {
		return fmt.Errorf("git fsck of the clone: %w", err)
	}
	note("operator", "exec ls -A %s: %v", filepath.Dir(clone), entries)
	note("operator", "exec jj -R %s workspace list: %s", clone, oneLine(workspaces))
	note("operator", "exec git fsck --connectivity-only: ok %s", oneLine(fsck))
	return nil
}

// re-adopt: a fresh runtime and listener take over every live claim, with nothing relaunched; a
// pod killed while no runtime ran is reported Gone with its recorded incarnation.
func (r *liveRig) checkReAdopt() error {
	victim := r.claim("fresh")
	for _, name := range []string{"root", "worker", "fresh", "root2", "child2"} {
		if err := r.ensureRunning(r.claim(name)); err != nil {
			return err
		}
	}
	if err := r.ensureSuspended(r.claim("second")); err != nil {
		return err
	}
	generations := map[string]int64{}
	for _, c := range r.live() {
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil {
			return err
		}
		generations[c.name] = s.Generation
	}
	r.stopRuntime()
	note("runtime", "listener and runtime closed")
	name := SandboxName(victim.token)
	if _, err := r.exec(victim, "sh", "-c", "kill 1"); err != nil {
		return err
	}
	if err := r.poll(liveGoneLimit, "pod "+name+" to end", func() (bool, error) {
		phase, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.status.phase}")
		return phase == string(corev1.PodFailed) || phase == string(corev1.PodSucceeded), err
	}); err != nil {
		return err
	}
	note("operator", "exec %s -- sh -c 'kill 1' while no runtime ran; the pod ended", name)

	restarted := time.Now()
	mark := r.obs.mark()
	if err := r.startRuntime(); err != nil {
		return err
	}
	var known []runtime.Known
	for _, c := range r.claims {
		if c.state == stateNone || c.state == stateReleased {
			continue
		}
		known = append(known, runtime.Known{Claim: c.token, Locator: c.located()})
	}
	if err := r.rt.ReconcileOrphans(r.ctx, known, time.Hour); err != nil {
		return err
	}
	note("runtime", "fresh listener and sandbox.New; ReconcileOrphans with %d known claims", len(known))

	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == victim.token && o.Kind != runtime.Alive
	})
	if !ok || gone.Kind != runtime.Gone || !sameLocator(gone.Locator, *victim.loc) {
		return fmt.Errorf("the killed claim was not reported gone with its recorded %s: %+v", victim.loc.Incarnation, gone)
	}
	note("runtime", "killed claim: gone, stamped %s — %s", short(gone.Locator.Incarnation), oneLine(firstLine(gone.Detail)))
	victim.loc, victim.state = nil, stateDead

	for _, c := range r.live() {
		loc := *c.loc
		alive, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool { return o.Locator.Claim == c.token })
		if !ok || alive.Kind != runtime.Alive || !sameLocator(alive.Locator, loc) {
			return fmt.Errorf("%s's first observation after re-adoption is %+v, want alive with its recorded %s", c.name, alive, loc.Incarnation)
		}
		reg, ok := r.reg.await(c.token, c.gen, restarted, 2*time.Minute)
		if !ok || reg.hash != tokenHash(c.bootToken) {
			return fmt.Errorf("%s's shim did not say hello again with its generation-%d token", c.name, c.gen)
		}
		uid, err := r.kubectl("get", "pod", SandboxName(c.token), "-o", "jsonpath={.metadata.uid}")
		if err != nil {
			return err
		}
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil {
			return err
		}
		if uid != loc.Incarnation || s.Generation != generations[c.name] {
			return fmt.Errorf("%s was relaunched: pod uid %s (recorded %s), Sandbox generation %d → %d", c.name, uid, loc.Incarnation, generations[c.name], s.Generation)
		}
		note("runtime", "%s: alive with recorded %s; Sandbox generation %d unchanged", c.name, short(loc.Incarnation), s.Generation)
		note("harness", "%s: hello again at generation %d, token sha256 %s…", c.name, reg.gen, reg.hash[:12])
		note("operator", "%s: pod uid %s", c.name, short(uid))
	}
	for _, o := range r.obs.since(mark) {
		if o.Kind == runtime.NotRecordedProcess || (o.Kind == runtime.Gone && o.Locator.Claim != victim.token) {
			return fmt.Errorf("re-adoption observed %s for %s: %s", o.Kind, o.Locator.Claim, o.Detail)
		}
	}
	return nil
}

// live are the claims with a running process, in a stable order.
func (r *liveRig) live() []*liveClaim {
	var out []*liveClaim
	for _, name := range []string{"root", "worker", "second", "fresh", "orphan", "root2", "child2"} {
		if c := r.claims[name]; c.state == stateRunning {
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
		out = append(out, runtime.Known{Claim: c.token, Locator: c.located()})
	}
	return out
}

// orphan-sweep: a Sandbox of the project that no claim records survives a sweep inside the grace
// and is deleted by one past it; a suspended claim's Sandbox survives both.
func (r *liveRig) checkOrphanSweep() error {
	orphan, suspended := r.claim("orphan"), r.claim("second")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	if err := r.ensureSuspended(suspended); err != nil {
		return err
	}
	if err := r.ensureRunning(orphan); err != nil {
		return err
	}
	name, keep := SandboxName(orphan.token), SandboxName(suspended.token)
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(orphan), time.Hour); err != nil {
		return err
	}
	for _, n := range []string{name, keep} {
		s, err := r.getSandbox(n)
		if err != nil || s.DeletionTimestamp != nil {
			return fmt.Errorf("Sandbox %s did not survive a sweep inside the grace: %v", n, err)
		}
	}
	note("runtime", "sweep with grace 1h, the orphan unrecorded: %s and the suspended %s survive", name, keep)
	time.Sleep(2 * time.Second)
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(orphan), time.Second); err != nil {
		return err
	}
	if err := r.poll(liveGoneLimit, "orphan Sandbox "+name+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(name)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	for _, c := range r.claims {
		if c == orphan || c.state == stateNone || c.state == stateReleased {
			continue
		}
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil || s.DeletionTimestamp != nil {
			return fmt.Errorf("the sweep past the grace took known claim %s's Sandbox: %v", c.name, err)
		}
	}
	note("runtime", "sweep with grace 1s: %s deleted; every known claim's Sandbox, the suspended %s included, survives", name, keep)
	orphan.loc, orphan.state = nil, stateReleased
	return nil
}

// release-tree: releasing every claim, a suspended one with no locator included, leaves nothing
// of the run: every Sandbox, every -boot Secret, and each tree volume go.
func (r *liveRig) checkReleaseTree() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	var released []string
	for _, name := range []string{"orphan", "fresh", "second", "worker", "child2", "root2", "root"} {
		c := r.claims[name]
		if c.state == stateNone || c.state == stateReleased {
			continue
		}
		loc := c.located()
		if err := r.rt.Release(r.ctx, c.token, loc, liveGrace); err != nil {
			return fmt.Errorf("Release(%s): %w", c.name, err)
		}
		how := "nil locator"
		if loc != nil {
			how = "locator " + short(loc.Incarnation)
		}
		released = append(released, c.name+" ("+how+")")
		c.loc, c.state = nil, stateReleased
	}
	note("runtime", "Released %s", strings.Join(released, ", "))
	selector := labelProject + "=" + r.env.project
	var left string
	err := r.poll(liveGoneLimit, "every object of the run to go", func() (bool, error) {
		out, err := r.kubectl("get", "sandboxes,secrets,pvc,pods", "-l", selector, "-o", "name")
		left = strings.TrimSpace(out)
		return left == "", err
	})
	if err != nil {
		return fmt.Errorf("%w; left: %s", err, oneLine(left))
	}
	note("operator", "kubectl get sandboxes,secrets,pvc,pods -l %s: none — every Sandbox, -boot Secret, and tree PVC (%s, %s) gone",
		selector, TreeClaimName(r.claim("root").token), TreeClaimName(r.claim("root2").token))
	return nil
}

// ---- the implement App ------------------------------------------------------------------------

// init resolves the implement App from the secret store through the same agent-tier read
// stage 3's private_key_command makes; the key stays in this process's memory.
func (r *liveRig) resolveApp() error {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(r.ctx, "secrets", r.env.appKeyName, "--", "sh", "-c", `printf %s "$`+r.env.appKeyName+`"`)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reading %s from the secret store: %v: %s", r.env.appKeyName, err, strings.TrimSpace(stderr.String()))
	}
	pem, err := decodeKey(stdout.String())
	if err != nil {
		return fmt.Errorf("%s: %w", r.env.appKeyName, err)
	}
	apps := appauth.New(config.GitHubApps{Implement: config.GitHubApp{AppID: r.env.appID, PrivateKey: pem}}, appauth.Options{})
	owner, _, _ := strings.Cut(r.env.repo, "/")
	lease, err := apps.Token(r.ctx, appauth.Implement, owner)
	if err != nil {
		return fmt.Errorf("minting the implement App's installation token for %s: %w", owner, err)
	}
	r.tokens, r.identity = implementTokens{apps: apps}, lease.Identity
	return nil
}

// decodeKey decodes the stored base64 of a PEM key, URL-safe or standard, padded or not.
func decodeKey(encoded string) (string, error) {
	cleaned := strings.NewReplacer("-", "+", "_", "/", "\n", "", "\r", "", " ", "").Replace(strings.TrimSpace(encoded))
	cleaned = strings.TrimRight(cleaned, "=")
	raw, err := base64.RawStdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", errors.New("not base64")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("-----BEGIN")) {
		return "", errors.New("does not decode to a PEM block")
	}
	return string(bytes.TrimSpace(raw)), nil
}
