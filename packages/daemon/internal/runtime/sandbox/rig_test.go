package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	testNamespace = "legion"
	testProject   = "legion"
	testImage     = "ghcr.io/cybermonklabs/legion-worker@sha256:1d10089a0000000000000000000000000000000000000000000000000000beef"
	testTree      = "LEGION-208"
	// testSessionDSNKey is the providers Secret's key holding the session database's URL under the
	// postgres store (Options.SessionStore), which every runtime a test configures carries as a
	// postgres deployment's does.
	testSessionDSNKey = "SESSION_DSN"
)

var (
	rootToken   = claim.Token("legion-legion-legion-208-architect")
	workerToken = claim.Token("legion-legion-legion-208-tester")
	otherToken  = claim.Token("legion-legion-legion-208-reviewer")
	// childToken is a role of another issue of the same tree: its own issue pod.
	childToken = claim.Token("legion-legion-legion-209-implementer")
)

// testOptions are the options every test starts from: production's shape — a store that keeps
// sessions in a database included, which configure requires, here the postgres one with its URL key
// — both pods' reservations at the daemon's defaults (reservations), with budgets short enough that
// a wait the test expects to end never slows the suite.
func testOptions() Options {
	return Options{
		Namespace:    testNamespace,
		Project:      testProject,
		Store:        newFakeStore(),
		Image:        testImage,
		StorageClass: "gp2",
		IssueVolume:  resource.MustParse("20Gi"),
		Resources:    reservations(),
		SessionStore: ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: testSessionDSNKey},
		StreamURL:    "tcp://192.0.2.250:13371",
		DaemonURL:    "http://192.0.2.250:13370",
		EnvoyURL:     "http://192.0.2.250:9020",
		NATSURLs:     []string{"nats://192.0.2.250:4222"},
		Tools: Tools{
			GH: "/usr/local/bin/gh", Git: "/usr/bin/git", JJ: "/usr/local/bin/jj", Legion: "/opt/legion/bin/legion",
			AgentSecrets: "/opt/legion/bin/agent-secrets",
		},
		BootTimeout:           2 * time.Second,
		RegistrationIntervals: 3,
		BusyBackoff:           time.Millisecond,
		TerminationGrace:      200 * time.Millisecond,
		ProbeInterval:         time.Hour,
		AdoptTimeout:          time.Minute,
		Tokens:                staticTokens{},
		GitHubCredential:      staticCredential,
		ControllerCredential:  staticControllerCredential,
		GitHubDirs:            func(ghrepo.Repository, []appauth.AppRole) []string { return staticDirs },
		Conns:                 fake.NewConns(),
		RepositoryCache: RepositoryCache{
			Volume: resource.MustParse("20Gi"), Refresh: 10 * time.Minute, Retain: 168 * time.Hour,
			Resources: reservations().Controller,
		},
		Mapped: func() []ghrepo.Repository { return nil },
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
}

// staticTokens mints the same installation token for every repository.
type staticTokens struct{}

func (staticTokens) Token(_ context.Context, repo ghrepo.Repository) (string, error) {
	return "ghs_provision_" + repo.Owner(), nil
}

// staticCredentialExpiry is the lease expiry every rendered test credential carries.
var staticCredentialExpiry = time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)

// staticCredentialToken is the write token staticCredential renders for role: one per App, so a
// review role's files and an implement role's differ as the two Apps' tokens do.
func staticCredentialToken(role claim.Role) string {
	return "ghs_" + string(appauth.AppRoleFor(role)) + "_token"
}

// staticReadToken is the read-only token staticCredential renders for role on owner.
func staticReadToken(role claim.Role, owner string) string {
	return "ghs_" + string(appauth.AppRoleFor(role)) + "_read_" + owner
}

// staticCredential is the runtime's GitHubCredential in every test: a bundle of repo's owner —
// sjawhar when repo is the zero Repository, which is how staticControllerCredential builds the
// controller's — holding the role's App's write token (appauth.AppRoleFor), and acme holding its
// read token, rendered as gh's files with a fixed expiry; repo's owner is the bundle's default.
func staticCredential(_ context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
	app := string(appauth.AppRoleFor(role))
	owner := strings.ToLower(repo.Owner())
	if owner == "" {
		owner = "sjawhar"
	}
	return ghconfig.Bundle{Default: owner, Dirs: map[string]ghconfig.Rendered{
		"acme": ghconfig.Render(staticReadToken(role, "acme"), app, staticCredentialExpiry),
		owner:  ghconfig.Render(staticCredentialToken(role), app, staticCredentialExpiry),
	}}, nil
}

// staticControllerCredential is the runtime's ControllerCredential in every test: the bundle
// staticCredential renders for the controller, whose default is the owner it is asked for — the
// one the pod's Secret records, which its refresh asks for — and sjawhar, the first owner, when
// it is asked for none, which is what the controller's launch asks for, as
// appauth.ControllerCredential answers one.
func staticControllerCredential(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
	bundle, err := staticCredential(ctx, claim.RoleController, ghrepo.Repository{})
	if err != nil || defaultOwner == "" {
		return bundle, err
	}
	bundle.Default = strings.ToLower(defaultOwner)
	return bundle, nil
}

// noOwnerControllerCredential is the ControllerCredential of a project with no GitHub owner to
// mint for (runtime.ErrNoGitHubOwner: every mapped repository lacks an App): a controller's
// launch then writes no gh files at all and its gh acts as nobody, its gh directory an empty one.
// It is the controller's own function that refuses, the launch asking for no bundle of any role
// (relaunch's gitHubBundles).
func noOwnerControllerCredential(context.Context, string) (ghconfig.Bundle, error) {
	return ghconfig.Bundle{}, fmt.Errorf("the review App for controller: %w", runtime.ErrNoGitHubOwner)
}

// staticDirs are the directories of every bundle staticCredential renders for the test specs,
// whose repository's owner is sjawhar, sorted: the set testOptions serves as GitHubDirs too, so a
// pod a rig launches projects a gh directory for each of them and has drifted from none.
var staticDirs = []string{"acme", "sjawhar"}

// addedOwner is an owner the project's mapping gained after a pod was created: no owner of
// staticDirs, and sorted between them, so a pod built before it holds no directory of that
// name and a bundle rendered now holds one.
const addedOwner = "globex"

// servedDirs are the directories a daemon servingAddedOwner serves, sorted: staticDirs with
// addedOwner among them, which is what appauth.GitHubDirs answers once the mapping names a
// repository of that owner. It is derived, so a staticDirs this package edits moves it too.
var servedDirs = slices.Sorted(slices.Values(append(slices.Clone(staticDirs), addedOwner)))

// servingAddedOwner is a restarted daemon serving one owner more: GitHubDirs answers
// servedDirs, and every bundle it renders holds addedOwner's read-only files beside the
// directories staticCredential always renders, the controller's own among them (its launch and
// its refresh both ask ControllerCredential), as the daemon's own answers move together
// (appauth.GitHubDirs, appauth.GitHubCredential and appauth.ControllerCredential read one
// mapping). A pod created before it projects no directory for the new owner and never will
// (movedGhDirs).
func servingAddedOwner(o *Options) {
	o.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return servedDirs }
	credential := func(ctx context.Context, role claim.Role, repo ghrepo.Repository) (ghconfig.Bundle, error) {
		bundle, err := staticCredential(ctx, role, repo)
		if err != nil {
			return bundle, err
		}
		bundle.Dirs[addedOwner] = ghconfig.Render(staticReadToken(role, addedOwner), string(appauth.AppRoleFor(role)), staticCredentialExpiry)
		return bundle, nil
	}
	o.GitHubCredential = credential
	o.ControllerCredential = func(ctx context.Context, defaultOwner string) (ghconfig.Bundle, error) {
		bundle, err := credential(ctx, claim.RoleController, ghrepo.Repository{})
		if err != nil || defaultOwner == "" {
			return bundle, err
		}
		bundle.Default = strings.ToLower(defaultOwner)
		return bundle, nil
	}
}

// renamedDirs are the gh directories a daemon that holds the smoke repository's configured pair
// serves, sorted: the directory of the owner serving that repository now and the directory of
// the owner it moved away from, which every record opened before the transfer still names. Both
// owner-set states below answer it, and so does the owner predicate this branch replaces
// (ownerRuleOwners), so it is also what a pod created before this change projects a directory
// for.
var renamedDirs = slices.Sorted(slices.Values([]string{recordedSmoke.Owner(), servingSmoke.Owner()}))

// otherOfRecordedOwner is a second repository of the owner the pair moved the smoke repository
// away from: the one repository that separates state B from state A, and the whole of what the
// owner predicate read as the project owning that owner.
var otherOfRecordedOwner = ghrepo.MustParse(recordedSmoke.Owner() + "/other")

// servingRenamed is a daemon serving repos with the smoke pair configured, built as the daemon
// builds its own answers from one mapping (daemon's sandboxRuntime): the directories it serves,
// the gh bundles it renders, the provisioning tokens it mints and the mapping its cache sweep
// reads are one view of those repositories, each read one repository at a time, so a test that
// moves the mapping moves all four together.
func servingRenamed(repos ...ghrepo.Repository) func(*Options) {
	return func(o *Options) {
		served := func() []ghrepo.Repository { return repos }
		renames := ghrepo.MustRenames(smokeRename)
		// One record of what the mints found GitHub refuses, as the daemon builds one per
		// project and hands it to both credentials and the directory set alike.
		left := appauth.NewLeftOut()
		leases := &namedLeases{}
		o.Mapped = served
		o.Tokens = servingTokens{served: served, renames: renames}
		o.GitHubCredential = appauth.GitHubCredential(leases, served, renames, left, nil)
		o.ControllerCredential = appauth.ControllerCredential(leases, served, renames, left, nil)
		o.GitHubDirs = func(repo ghrepo.Repository, roles []appauth.AppRole) []string {
			return appauth.GitHubDirs(served, renames, left, roles, repo)
		}
	}
}

// servingTokens is the provisioning token source of a daemon that holds configured renames, as
// the daemon builds it (implementTokens over the mapping's own serving rule): every token is
// minted for the repository GitHub holds the recorded one under now, and names that owner as
// staticTokens names its own. The repository it is handed is resolved, and no address a caller
// builds from that repository is.
type servingTokens struct {
	served  func() []ghrepo.Repository
	renames ghrepo.Renames
}

func (s servingTokens) Token(_ context.Context, repo ghrepo.Repository) (string, error) {
	serving := s.renames.Serving(repo, func(candidate ghrepo.Repository) bool {
		return ghrepo.Serves(s.served(), candidate)
	})
	return "ghs_provision_" + serving.Owner(), nil
}

// launchOf is r's launch of spec as a relaunch that replaces the pod builds it: prepared, with each
// role's gh bundle minted (gitHubBundles), so the pod's gh volumes and its agents' GH_CONFIG_DIR
// name the bundle's directories.
func launchOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) launch {
	t.Helper()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.gitHubBundles(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// fakeStore is the runtime's durable state, in memory: every tree is live (its lifecycle open)
// unless closed says its cleanup confirmed, a claim of an issue runs a process only when running
// names it, in the tree it names, an issue's Sandbox belongs to a tree only when homes names it
// (to none otherwise), and a claim is stored unretired only when unretired names it. The store's
// own rule for a Sandbox's home is held by its tests (store.IssueSandboxHome); here a test sets it.
type fakeStore struct {
	mu        sync.Mutex
	closed    map[string]bool
	running   map[string]runningClaim
	homes     map[string]string
	unretired map[claim.Token]bool
	claims    []supervise.Claim
}

// runningClaim is a claim of an issue that runs a process (token:state), in tree.
type runningClaim struct{ tree, claim string }

func newFakeStore() *fakeStore {
	return &fakeStore{
		closed: map[string]bool{}, running: map[string]runningClaim{}, homes: map[string]string{},
		unretired: map[claim.Token]bool{},
	}
}

func (s *fakeStore) ClaimUnretired(_ context.Context, token claim.Token) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unretired[token], nil
}
func (s *fakeStore) Claims(context.Context) ([]supervise.Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.claims), nil
}

// admit records token's claim as stored and unretired, as the supervisor writes it before its
// launch begins (supervise.Machine.launchAfter).
func (s *fakeStore) admit(token claim.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unretired[token] = true
}

// retire records token's claim as retired.
func (s *fakeStore) retire(token claim.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.unretired, token)
}

func (s *fakeStore) IssueSandboxHome(_ context.Context, _, issue string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.homes[issue], nil
}

func (s *fakeStore) TreeLive(_ context.Context, _, tree string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed[tree], nil
}

// IssueRunsElsewhere filters as the store's query does: the issue's running claim is named only
// when its tree is not the tree asked about.
func (s *fakeStore) IssueRunsElsewhere(_ context.Context, _, issue, tree string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.running[issue]; ok && c.tree != tree {
		return c.claim, nil
	}
	return "", nil
}

// close records tree's cleanup as confirmed: what the tree left behind is then an orphan.
func (s *fakeStore) close(tree string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed[tree] = true
}

// testSpec is a launch of the claim on the tree, with its prompt files under dir.
func testSpec(t *testing.T, token claim.Token, role claim.Role, issue string) runtime.SpawnSpec {
	t.Helper()
	dir := t.TempDir()
	rolePrompt := filepath.Join(dir, "role.md")
	instructions := filepath.Join(dir, "instructions.md")
	if err := os.WriteFile(rolePrompt, []byte("You are the "+string(role)+".\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(instructions, []byte("Deployment instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.SpawnSpec{
		Claim: token, Project: "legion", Tree: testTree, Issue: issue, Role: role, Generation: 1,
		BootToken: "boot-" + string(token) + "-g1",
		Env:       map[string]string{"JJ_USER": "legion-implement[bot]", "JJ_EMAIL": "1+legion-implement[bot]@users.noreply.github.com"},
		Secrets:   map[string]string{"ENVOY_TOKEN": "envoy-bearer"},
		Prompt: runtime.PromptParts{
			RolePromptPaths: []string{rolePrompt}, Addressing: "Legion addressing: your role topic is `notifications.role." + string(token) + "`.",
			DeploymentInstructionsPath: instructions,
		},
		Repository:    ghrepo.MustParse("sjawhar/legion-smoke"),
		DefaultBranch: "main",
	}
}

func rootSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, rootToken, claim.RoleArchitect, testTree)
}

func workerSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, workerToken, claim.RoleTester, testTree)
}

// claudeWorkerSpec is workerSpec on the Claude Code backend: the tester, one of the roles Claude
// Code runs (runtime.Backends).
func claudeWorkerSpec(t *testing.T) runtime.SpawnSpec {
	spec := workerSpec(t)
	spec.Backend = runtime.BackendClaude
	return spec
}

// claudeTranscriptID is the session id of the transcript the Claude specs below resume, and
// claudeTranscript the path a role's Claude Code recorded it at: its own configuration directory
// (claudeConfigDir), `projects/<cwd>/<id>.jsonl`.
const claudeTranscriptID = "9f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"

func claudeTranscript(role claim.Role) string {
	return claudeConfigDir(role) + "/projects/--legion-workspaces-sjawhar-legion-smoke-legion-208--/" + claudeTranscriptID + ".jsonl"
}

// claudeResumeSpec is claudeWorkerSpec relaunched on the transcript a replaced pod's role
// recorded, the session a Claude claim records (packages/claude-envoy's legion-claim.ts).
func claudeResumeSpec(t *testing.T) runtime.SpawnSpec {
	spec := claudeWorkerSpec(t)
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", claudeTranscript(spec.Role)
	return spec
}

func childSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, childToken, claim.RoleImplementer, "LEGION-209")
}

// action is one API request either fake saw, in the one order both saw them.
type action struct {
	verb, resource, subresource, name string
	patch                             string
	// object is what a create sent.
	object k8sruntime.Object
}

func (a action) String() string {
	s := a.verb + " " + a.resource
	if a.subresource != "" {
		s += "/" + a.subresource
	}
	return s + " " + a.name
}

// rig is a runtime over client-go's dynamic and typed fakes, with a stand-in for the Agent
// Sandbox controller: while a Sandbox is Running and holds no pod it creates one it owns, and
// while it is Suspended it deletes the pod it owns — the two behaviours of v1.0.3's reconcilePod
// the runtime depends on (sandbox_controller.go:1256-1287, 1380-1453).
type rig struct {
	t     *testing.T
	ctx   context.Context
	stop  context.CancelFunc
	r     *Runtime
	dyn   *dynamicfake.FakeDynamicClient
	kube  *kubefake.Clientset
	conns *fake.Conns
	now   atomic.Pointer[time.Time]
	// store is the runtime's in-memory durable state, unless an option handed it another.
	store *fakeStore

	mu      sync.Mutex
	actions []action
	// feeds, when set, are the informers' feeds withBreakableFeeds serves.
	feeds *breakableFeeds
	uids  atomic.Int64

	// hold stops the controller creating pods; autoStart makes a pod it creates scheduled on a
	// node, past workspace-init, and Running at once.
	hold      atomic.Bool
	autoStart atomic.Bool
	// suspendDelay is how long a pod the controller deletes stays terminating first, in
	// nanoseconds. It is atomic because a test may shorten it while control() is running, to ask
	// for a pod it has been holding terminating to end now: g.suspendDelay.Store(0) in
	// TestPodDriftTellsTheRoundsOwnReplacementFromALaunchs (drift_test.go) ends the pod its
	// replacement is waiting out, as the cluster would.
	suspendDelay atomic.Int64
	// noController leaves every object as the test wrote it.
	noController bool
	// hooks, when set, sees the runtime's Sandbox gets and deletes (withSandboxHooks).
	hooks *sandboxHooks
	// limit, when set, is the client-side rate limiter the runtime's requests wait on
	// (withClientLimiter); secretGetsAsked counts each Secret get the runtime asked for, by name,
	// whether or not the limiter let it through.
	limit           flowcontrol.RateLimiter
	secretGetsAsked map[string]int
	// clients are the runtime's clients: the fakes themselves for its informers, and for its
	// requests the fakes behind whatever hooks and limiter the options asked for.
	clients runtimeClients
	// prober builds the runtime with newProber, the probe's configuration and the informers alone
	// (asProber); p is that Prober, whose runtime r also is.
	prober bool
	p      *Prober
	// opts are the Options the runtime was built from, for a second runtime over the same fakes.
	opts Options

	// commands are what each role's fake launcher was sent, in order; wanted are the roles whose
	// fake launcher connects again whenever their issue gets a new pod.
	commandsMu sync.Mutex
	commands   map[claim.Token][]shimwire.Frame
	wanted     map[claim.Token]bool
	// refuseBundle, when set, is the error every fake launcher of this rig answers each
	// launcher_bundle slice with, as a launcher refuses one that did not arrive as it was sent.
	// failRestore, when set, is the error its report of the restore carries instead, which is a
	// launcher that held the whole bundle and could not put it back.
	refuseBundle string
	failRestore  string
}

type rigOption func(*rig, *Options)

func withOptions(edit func(*Options)) rigOption { return func(_ *rig, o *Options) { edit(o) } }

// withoutController runs no controller stand-in: the objects stay as the test wrote them.
func withoutController() rigOption { return func(g *rig, _ *Options) { g.noController = true } }

// withRefusedBundle has every fake launcher refuse each slice of a workspace bundle with reason.
func withRefusedBundle(reason string) rigOption {
	return func(g *rig, _ *Options) { g.refuseBundle = reason }
}

// withFailedRestore has every fake launcher of this rig hold the whole bundle and then report
// that its restore failed with reason, as a launcher whose `git bundle unbundle` found no
// prerequisite commit does.
func withFailedRestore(reason string) rigOption {
	return func(g *rig, _ *Options) { g.failRestore = reason }
}

// testLaunchers is a launchers of its own, outside any rig, with no saved workspace bundles to
// carry: the tests that build one drive the launcher protocol itself.
func testLaunchers() *launchers {
	return newLaunchers(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// launcherIssue is the issue key a role token's pod is of, as the role Secret's annotation
// carries it to the real resolver (launcherCredential.issue); "" for the project controller's
// token, whose pod is of no issue.
func launcherIssue(token claim.Token) string {
	prefix, _, ok := token.Cut()
	if !ok {
		return ""
	}
	return strings.ToUpper(strings.TrimPrefix(prefix, "legion-"+testProject+"-"))
}

// sandboxHooks runs beforeDelete as the runtime asks to delete a Sandbox and afterGet once a get
// of one returns, outside the fake client's lock, which the fake holds across a whole call.
type sandboxHooks struct {
	beforeDelete, afterGet func(name string)
}

// withSandboxHooks hands the runtime a dynamic client that runs h around its Sandbox calls.
func withSandboxHooks(h *sandboxHooks) rigOption { return func(g *rig, _ *Options) { g.hooks = h } }

// hookedDynamic is a dynamic client that runs hooks around its Sandbox calls. Only a runtime's
// requests go through it: its informers list and watch on the fake itself, whose
// IsWatchListSemanticsUnSupported keeps them from streaming a watch-list.
type hookedDynamic struct {
	dynamic.Interface
	hooks *sandboxHooks
}

func (d hookedDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	if gvr != sandboxGVR {
		return d.Interface.Resource(gvr)
	}
	return hookedResource{d.Interface.Resource(gvr), d.hooks}
}

type hookedResource struct {
	dynamic.NamespaceableResourceInterface
	hooks *sandboxHooks
}

func (h hookedResource) Namespace(ns string) dynamic.ResourceInterface {
	return hookedSandboxes{h.NamespaceableResourceInterface.Namespace(ns), h.hooks}
}

type hookedSandboxes struct {
	dynamic.ResourceInterface
	hooks *sandboxHooks
}

func (h hookedSandboxes) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	h.hooks.beforeDelete(name)
	return h.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

func (h hookedSandboxes) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	u, err := h.ResourceInterface.Get(ctx, name, opts, subresources...)
	h.hooks.afterGet(name)
	return u, err
}

func newRig(t *testing.T, objects []k8sruntime.Object, options ...rigOption) *rig {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	var dynamicObjects []*unstructured.Unstructured
	var kubeObjects []k8sruntime.Object
	for _, object := range objects {
		if u, ok := object.(*unstructured.Unstructured); ok {
			dynamicObjects = append(dynamicObjects, u)
		} else {
			kubeObjects = append(kubeObjects, object)
		}
	}
	g := &rig{
		t: t, ctx: ctx, stop: stop, conns: fake.NewConns(),
		dyn: newDynamic(t, dynamicObjects...), kube: kubefake.NewClientset(kubeObjects...),
	}
	g.autoStart.Store(true)
	start := rigNow
	g.now.Store(&start)
	g.dyn.PrependReactor("patch", "sandboxes", k8stesting.ObjectReaction(generationTracker{g.dyn.Tracker()}))
	for _, client := range []*k8stesting.Fake{&g.dyn.Fake, &g.kube.Fake} {
		client.PrependReactor("*", "*", g.record)
	}
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	for _, option := range options {
		option(g, &opts)
	}
	g.store, _ = opts.Store.(*fakeStore)
	g.opts = opts
	var dyn dynamic.Interface = g.dyn
	var kube kubernetes.Interface = g.kube
	if g.hooks != nil {
		dyn = hookedDynamic{dyn, g.hooks}
	}
	if g.limit != nil {
		dyn, kube = limitedDynamic{dyn, g.limit}, limitedKube{kube, g.limit, g.askedSecret}
	}
	g.clients = runtimeClients{informers: kubeClients{dyn: g.dyn, kube: g.kube}, requests: kubeClients{dyn: dyn, kube: kube}}
	if g.prober {
		p, err := newProber(ctx, g.clients, opts)
		if err != nil {
			t.Fatal(err)
		}
		g.p, g.r = p, p.r
	} else {
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.start(ctx, g.clients); err != nil {
			t.Fatal(err)
		}
		g.r = r
	}
	if !g.noController {
		go g.control()
	}
	return g
}

// newDynamic is the dynamic fake holding objects, each under the resource an API server serves
// its kind at: the fake guesses "sandboxs" for a Sandbox it is handed at construction.
func newDynamic(t *testing.T, objects ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList", crdGVR: "CustomResourceDefinitionList",
	})
	for _, object := range objects {
		gvr := sandboxGVR
		if object.GetKind() == "CustomResourceDefinition" {
			gvr = crdGVR
		}
		if err := dyn.Tracker().Create(gvr, object, object.GetNamespace()); err != nil {
			t.Fatal(err)
		}
	}
	return dyn
}

// generationTracker stores what a Sandbox patch leaves as the API server does for a CRD with a
// status subresource, as the Sandbox's is: with metadata.generation bumped when the patch changed
// its spec, in the same write, hence one watch event, and left alone when it changed only metadata
// (an annotation). The fake's tracker leaves the generation alone.
type generationTracker struct{ k8stesting.ObjectTracker }

func (t generationTracker) Patch(gvr schema.GroupVersionResource, obj k8sruntime.Object, ns string, opts ...metav1.PatchOptions) error {
	if object, err := metaOf(obj); err == nil && gvr == sandboxGVR && t.specChanged(gvr, obj, ns, object.GetName()) {
		object.SetGeneration(object.GetGeneration() + 1)
	}
	return t.ObjectTracker.Patch(gvr, obj, ns, opts...)
}

// specChanged is whether obj, a patch's result, holds another spec than the stored object.
func (t generationTracker) specChanged(gvr schema.GroupVersionResource, obj k8sruntime.Object, ns, name string) bool {
	stored, err := t.ObjectTracker.Get(gvr, ns, name)
	if err != nil {
		return true
	}
	before, isBefore := stored.(*unstructured.Unstructured)
	after, isAfter := obj.(*unstructured.Unstructured)
	return !isBefore || !isAfter || !reflect.DeepEqual(before.Object["spec"], after.Object["spec"])
}

// advance moves the runtime's clock.
func (g *rig) advance(d time.Duration) {
	next := g.now.Load().Add(d)
	g.now.Store(&next)
}

// record logs every request, and gives what a create makes the uid and creation time an API
// server would: the fakes set neither.
func (g *rig) record(a k8stesting.Action) (bool, k8sruntime.Object, error) {
	entry := action{verb: a.GetVerb(), resource: a.GetResource().Resource, subresource: a.GetSubresource()}
	switch typed := a.(type) {
	case k8stesting.CreateAction:
		if object, err := metaOf(typed.GetObject()); err == nil {
			entry.name = object.GetName()
			if object.GetUID() == "" {
				object.SetUID(types.UID(fmt.Sprintf("uid-%s-%d", entry.resource, g.uids.Add(1))))
			}
			if created := object.GetCreationTimestamp(); created.IsZero() {
				object.SetCreationTimestamp(metav1.NewTime(*g.now.Load()))
			}
			entry.object = typed.GetObject().DeepCopyObject()
		}
	case k8stesting.PatchAction:
		entry.name, entry.patch = typed.GetName(), string(typed.GetPatch())
	case k8stesting.UpdateAction:
		if object, err := metaOf(typed.GetObject()); err == nil {
			entry.name = object.GetName()
		}
	case k8stesting.DeleteAction:
		entry.name = typed.GetName()
	case k8stesting.GetAction:
		entry.name = typed.GetName()
	}
	if entry.verb != "list" && entry.verb != "watch" {
		g.mu.Lock()
		g.actions = append(g.actions, entry)
		g.mu.Unlock()
	}
	return false, nil, nil
}

func metaOf(object k8sruntime.Object) (metav1.Object, error) {
	if u, ok := object.(*unstructured.Unstructured); ok {
		return u, nil
	}
	accessor, ok := object.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("%T has no metadata", object)
	}
	return accessor, nil
}

// writes are the requests that changed something, in order, as "verb resource name".
func (g *rig) writes() []action {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []action
	for _, a := range g.actions {
		if a.verb != "get" {
			out = append(out, a)
		}
	}
	return out
}

// requests are every request the runtime made, reads included, in order; the informers' lists and
// watches are never among them (record).
func (g *rig) requests() []action {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.actions)
}

func (g *rig) clearActions() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.actions = nil
}

// control is the controller stand-in, reconciling every Sandbox every few milliseconds: its pod,
// and the headless Service of a Sandbox that asks for one, which it makes as the controller does
// (cacheService) when no Service of the name exists and removes once its Sandbox is gone, as the
// garbage collector would. Its own requests go through the tracker directly, so they never appear
// among the runtime's.
func (g *rig) control() {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	terminating := map[types.UID]time.Time{}
	services := map[string]types.UID{}
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-tick.C:
		}
		list, err := g.dyn.Tracker().List(sandboxGVR, sandboxGVR.GroupVersion().WithKind("Sandbox"), testNamespace)
		if err != nil {
			continue
		}
		items, _ := list.(*unstructured.UnstructuredList)
		if items == nil {
			continue
		}
		live := map[types.UID]bool{}
		for i := range items.Items {
			s, err := decodeSandbox(&items.Items[i])
			if err != nil || s.DeletionTimestamp != nil {
				continue
			}
			live[s.UID] = true
			if s.Spec.Service != nil && *s.Spec.Service {
				if _, err := g.kube.Tracker().Get(servicesGVR, testNamespace, s.Name); err != nil {
					if g.kube.Tracker().Add(cacheService(s.Name, s.UID)) == nil {
						services[s.Name] = s.UID
					}
				}
			}
			pod := g.pod(s.Name)
			owned := ownedBy(pod, s.UID)
			switch {
			case owned && pod.DeletionTimestamp != nil:
				// A pod the cluster is terminating goes once its grace has passed, whatever its
				// Sandbox says by then. Only the suspended branch below used to remove one, so a
				// pod still terminating when its Sandbox came back to Running was in a state this
				// controller could not leave: it neither removed that pod nor made a replacement,
				// since a replacement needs none to be there. A launch whose own wait for the pod
				// to go accepts a terminating one - which production's does, because in a cluster
				// such a pod is on its way out - then waited for a pod this controller was never
				// going to create. That is what red this package's pod-drift row about 4 times in
				// 100 under load, at every bound it was given, with the controller reading
				// `generation 3, Running, pod uid-pod-13 terminating` for ever (INFRA-985).
				if started, ok := terminating[pod.UID]; ok && time.Since(started) >= time.Duration(g.suspendDelay.Load()) {
					_ = g.kube.Tracker().Delete(podsGVR, testNamespace, pod.Name)
				}
			case s.mode() == modeSuspended && owned:
				if _, ok := terminating[pod.UID]; !ok {
					terminating[pod.UID] = time.Now()
					g.update(pod, func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now })
				}
			case s.mode() == modeRunning && pod == nil && !g.hold.Load():
				g.createPod(s)
			}
		}
		for name, uid := range services {
			if !live[uid] {
				_ = g.kube.Tracker().Delete(servicesGVR, testNamespace, name)
				delete(services, name)
			}
		}
	}
}

var servicesGVR = corev1.SchemeGroupVersion.WithResource("services")

var podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

// createPod makes the pod a Running Sandbox stands for, as the controller and the API server
// between them do: a uid of its own, and the creation instant the API server stamps, which the
// runtime reads to decide whether a release's saved work is this pod's to restore
// (launcherSession.podCreated). The rig's clock is that stamp, so a test that advances it before
// a pod is replaced gets a replacement newer than the one it replaced.
func (g *rig) createPod(s *sandbox) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Name, Namespace: testNamespace, Labels: s.Spec.PodTemplate.Metadata.Labels,
			UID:               types.UID(fmt.Sprintf("uid-pod-%d", g.uids.Add(1))),
			CreationTimestamp: metav1.NewTime(*g.now.Load()),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: s.Name, UID: s.UID, Controller: new(true)}},
		},
		Spec:   s.Spec.PodTemplate.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if g.autoStart.Load() {
		pod.Spec.NodeName = "ip-192-0-2-7"
		pod.Status = runningStatusOf(pod.Spec)
	}
	// A new pod's role launchers are new processes, bound to its exact UID. The runtime waits
	// for their first state instead of treating the previous pod's connection as current.
	g.commandsMu.Lock()
	var reconnect []claim.Token
	for token := range g.wanted {
		if SandboxName(token) == s.Name {
			reconnect = append(reconnect, token)
		}
	}
	g.commandsMu.Unlock()
	_ = g.kube.Tracker().Add(pod)
	for _, token := range reconnect {
		g.connect(token)
	}
}

// runningStatus is a Running issue pod's status: workspace-init done and its one container running.
func runningStatus() corev1.PodStatus {
	return runningStatusOf(corev1.PodSpec{Containers: []corev1.Container{{Name: runtime.IssuePodContainer}}})
}

// runningStatusOf is the status of a pod of spec once Running: workspace-init done and each of its
// containers running.
func runningStatusOf(spec corev1.PodSpec) corev1.PodStatus {
	status := corev1.PodStatus{
		Phase: corev1.PodRunning,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: initContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}},
		}},
	}
	for _, c := range spec.Containers {
		status.ContainerStatuses = append(status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		})
	}
	return status
}

// reportLauncher registers a connected launcher for token reporting state, without a connection
// to drive: the observation mapping reads only the reported state.
func (g *rig) reportLauncher(token claim.Token, state shimwire.LauncherState) {
	g.t.Helper()
	pod := g.pod(SandboxName(token))
	if pod == nil {
		g.t.Fatal("cannot report launcher state without its pod")
	}
	s := g.r.launchers.accept(token, launcherIssue(token), pod.CreationTimestamp.Time, shimwire.LauncherHello{LauncherID: "test-" + string(token), PodUID: string(pod.UID)})
	s.mu.Lock()
	s.state = state
	close(s.ready)
	s.mu.Unlock()
}

// pod is the pod named name in the tracker, or nil.
func (g *rig) pod(name string) *corev1.Pod {
	object, err := g.kube.Tracker().Get(podsGVR, testNamespace, name)
	if err != nil {
		return nil
	}
	return object.(*corev1.Pod)
}

// update rewrites a pod in the tracker.
func (g *rig) update(pod *corev1.Pod, edit func(*corev1.Pod)) {
	next := pod.DeepCopy()
	edit(next)
	if err := g.kube.Tracker().Update(podsGVR, next, testNamespace); err != nil && !apierrors.IsNotFound(err) {
		g.t.Errorf("update pod %s: %v", pod.Name, err)
	}
}

// sandbox is the Sandbox named name in the tracker, or nil.
func (g *rig) sandbox(name string) *sandbox {
	object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
	if err != nil {
		return nil
	}
	s, err := decodeSandbox(object.(*unstructured.Unstructured))
	if err != nil {
		g.t.Fatal(err)
	}
	return s
}

// initFailed is the classification of loc's verdict that only this package reads: whether a failed
// init container of its pod is what ended the process there (observation.initFailed). No
// runtime.Observation carries it, so a test reads it from the mapping itself, over the same stores
// a Probe reads.
func (g *rig) initFailed(loc runtime.Locator) bool {
	g.t.Helper()
	v, err := g.r.view(loc.Sandbox.Name)
	if err != nil {
		g.t.Fatal(err)
	}
	return g.r.verdict(g.ctx, loc, v).initFailed
}

// secret is the Secret named name in the tracker, or nil.
func (g *rig) secret(name string) *corev1.Secret {
	object, err := g.kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, name)
	if err != nil {
		return nil
	}
	return object.(*corev1.Secret)
}

// eventually fails the test unless ok holds within a second.
func (g *rig) eventually(what string, ok func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spawn launches spec and fails the test on an error.
func (g *rig) spawn(spec runtime.SpawnSpec) runtime.Locator {
	g.t.Helper()
	g.launcher(spec.Claim)
	loc, err := g.r.Spawn(g.ctx, spec)
	if err != nil {
		g.t.Fatalf("spawn %s: %v", spec.Claim, err)
	}
	return loc
}

// launcher is the test rig's role-private launcher connection. It models only the launcher
// protocol — one child generation at a time, a stop of another generation refused — and records
// every command: child shims and their agent events remain the fake Conns this rig already owns.
func (g *rig) launcher(token claim.Token) {
	g.t.Helper()
	g.commandsMu.Lock()
	if g.wanted == nil {
		g.wanted = map[claim.Token]bool{}
	}
	g.wanted[token] = true
	g.commandsMu.Unlock()
	g.r.launchers.mu.Lock()
	_, exists := g.r.launchers.sessions[token]
	g.r.launchers.mu.Unlock()
	if exists || g.pod(SandboxName(token)) == nil {
		return
	}
	g.connect(token)
}

// connect is one fake launcher process's connection for token, replacing any earlier one.
func (g *rig) connect(token claim.Token) { g.connectRunning(token, 0) }

// connectRunning is a fake launcher connection for token whose launcher already runs generation
// running (0 for none), as one that survived a daemon restart reports its child at once.
func (g *rig) connectRunning(token claim.Token, running uint64) {
	server, client := net.Pipe()
	pod := g.pod(SandboxName(token))
	session := g.r.launchers.accept(token, launcherIssue(token), pod.CreationTimestamp.Time, shimwire.LauncherHello{LauncherID: "test-" + string(token), PodUID: string(pod.UID)})
	g.t.Cleanup(func() { _ = client.Close() })
	go session.ServeLauncher(server, bufio.NewReader(server), shimwire.NewWriter(server))
	go func() {
		writer := shimwire.NewWriter(client)
		first := shimwire.LauncherState{}
		if running != 0 {
			first.Child = &shimwire.LauncherChild{Generation: running, PID: 42}
		}
		_ = writer.WriteFrame(first)
		// held is the slices of each bundle sequence that arrived, as the real launcher holds
		// them: the whole of one is answered Held (internal/launcher's bundleRestore), and the
		// start that follows applies the bundle and reports what the restore did, which is what
		// the daemon drops its saved copy on. whole names the sequence awaiting that start, and
		// release the release whose save it carried, which the report names so that the daemon
		// deletes that release's bundle and no other.
		held := map[string]map[int]bool{}
		whole := ""
		var release int64
		reader := bufio.NewReader(client)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				return
			}
			g.commandsMu.Lock()
			if g.commands == nil {
				g.commands = map[claim.Token][]shimwire.Frame{}
			}
			g.commands[token] = append(g.commands[token], frame)
			g.commandsMu.Unlock()
			switch command := frame.(type) {
			case shimwire.LauncherStart:
				if running != 0 {
					_ = writer.WriteFrame(shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d is still running", running)})
					continue
				}
				// The real launcher restores the bundle inside the start, before it answers it
				// (internal/launcher's manager.start), so the report crosses first.
				if whole != "" {
					_ = writer.WriteFrame(shimwire.LauncherRestored{ID: whole, Release: release, OK: g.failRestore == "", Error: g.failRestore})
					whole, release = "", 0
				}
				running = command.Generation
				_ = writer.WriteFrame(shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation})
				_ = writer.WriteFrame(shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: command.Generation, PID: 42}})
			case shimwire.LauncherStop:
				if running != 0 && running != command.Generation {
					_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", running, command.Generation)})
					continue
				}
				stopped := running
				running = 0
				_ = writer.WriteFrame(shimwire.LauncherStopResult{ID: command.ID, OK: true})
				state := shimwire.LauncherState{}
				if stopped != 0 {
					state.LastExit = &shimwire.LauncherExit{Generation: stopped, Code: 143, Signal: "terminated"}
				}
				_ = writer.WriteFrame(state)
			case shimwire.LauncherBundle:
				if g.refuseBundle != "" {
					_ = writer.WriteFrame(shimwire.LauncherBundleResult{ID: command.ID, Index: command.Index, Error: g.refuseBundle})
					continue
				}
				if held[command.ID] == nil {
					held[command.ID] = map[int]bool{}
				}
				held[command.ID][command.Index] = true
				all := len(held[command.ID]) == command.Count
				if all {
					whole, release = command.ID, command.Release
				}
				_ = writer.WriteFrame(shimwire.LauncherBundleResult{
					ID: command.ID, Index: command.Index, OK: true, Held: all,
				})
			}
		}
	}()
}

// issueLaunchers connects a fake launcher for every role of token's issue.
func (g *rig) issueLaunchers(token claim.Token) {
	g.t.Helper()
	issue, _, ok := token.Cut()
	if !ok {
		g.t.Fatalf("%s names no role", token)
	}
	for _, sibling := range claim.Roles {
		g.launcher(claim.Token(issue + "-" + string(sibling)))
	}
}

// sent is every command token's fake launcher received.
func (g *rig) sent(token claim.Token) []shimwire.Frame {
	g.commandsMu.Lock()
	defer g.commandsMu.Unlock()
	return slices.Clone(g.commands[token])
}

// sandboxObject is a Sandbox as the API would hold it, for a test's starting state: an issue
// Sandbox as this runtime makes it, owning no claim template, since its disk is its own pod's
// generic ephemeral volume (issuePod.disk), and carrying a resourceVersion, as every object
// the API serves does. The project controller's Sandbox, which this runtime does give the `issue`
// template its working root is on, no test starts from: a test that needs one launches it
// (controllerSpec). A test of the cutover from the release before the pod-owned disk opts into
// that release's issue Sandbox by name (withSupersededDisk); one of the tree-volume layout's root
// or controller, into that layout's template (withTreeVolume).
func sandboxObject(t *testing.T, name, uid, mode string, labels map[string]string, conditions ...metav1.Condition) *unstructured.Unstructured {
	t.Helper()
	s := sandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, UID: types.UID(uid), Labels: labels, Generation: 2, ResourceVersion: "1",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC)),
		},
		Spec:   sandboxSpec{OperatingMode: mode, PodTemplate: podTemplate{Metadata: podMetadata{Labels: labels}}},
		Status: sandboxStatus{Conditions: conditions},
	}
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// withSupersededDisk is u as the release before the pod-owned disk made an issue's Sandbox: owning
// the one `issue` claim template, so the claim outlived every pod of the issue. It exists for the
// cutover alone — the boot census that names such a Sandbox rather than refusing it
// (treeVolumeLayout) and the issue's next launch that replaces it, Sandbox and claim together
// (supersededDisk, replaceSupersededSandbox) — and nothing else starts from this shape.
func withSupersededDisk(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	return withClaimTemplate(t, u, issueVolume)
}

// withClaimTemplate is u owning one volume claim template, named name and carrying u's labels.
func withClaimTemplate(t *testing.T, u *unstructured.Unstructured, name string) *unstructured.Unstructured {
	t.Helper()
	labels := map[string]any{}
	for key, value := range u.GetLabels() {
		labels[key] = value
	}
	template := map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}
	if err := unstructured.SetNestedSlice(u.Object, []any{template}, "spec", "volumeClaimTemplates"); err != nil {
		t.Fatal(err)
	}
	return u
}

// podObject is a pod the Sandbox with ownerUID owns ("" for none), as the API would hold it. Its
// spec carries the one piece every pod the runtime creates has and the mapping compares, the
// providers volume projecting the session database's URL key (providers, movedSessionStore): a
// stand-in without it holds a moved address, and every observation of it would be StaleAddress.
func podObject(name, uid, ownerUID string, labels map[string]string, status corev1.PodStatus) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID(uid), Labels: labels},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: providersVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ProvidersSecretName(testProject),
				Items:      []corev1.KeyToPath{{Key: testSessionDSNKey, Path: ompsessions.DSNFileName}},
			}},
		}}},
		Status: status,
	}
	if ownerUID != "" {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: types.UID(ownerUID), Controller: new(true),
		}}
	}
	return pod
}

// issueDiskName is the name Kubernetes gives the claim of an issue pod's own disk: the pod's name
// and the volume's (issueVolume), the pod being the claim's Sandbox name (SandboxName).
func issueDiskName(t claim.Token) string { return SandboxName(t) + "-" + issueVolume }

// claimLabels are the labels an issue's shared Sandbox carries.
func claimLabels(_ claim.Role) map[string]string {
	return map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree}
}

// sandboxLocator is a recorded role process locator in its claim's pod's one container.
func sandboxLocator(token claim.Token, uid string) runtime.Locator {
	role, ok := token.Role()
	if !ok {
		panic("test token has no role")
	}
	return runtime.Locator{
		Runtime:     runtime.RuntimeSandbox,
		Claim:       token,
		Incarnation: runtime.SandboxIncarnation(uid, 1),
		Sandbox:     &runtime.SandboxLocator{Namespace: testNamespace, Name: SandboxName(token), PodUID: uid, Container: podContainer(role), Generation: 1},
	}
}

// isMergePatch is whether a Sandbox patch body is a JSON merge patch, an object, rather than a JSON
// patch, an array of operations: a role's address record (recordAddresses) or the suspension that
// names a displaced pod (suspendDisplacing).
func isMergePatch(body string) bool {
	return strings.HasPrefix(strings.TrimSpace(body), "{")
}

// addressRecordPatch is whether a Sandbox patch body is a role's address record alone: a merge
// patch of metadata, which changes nothing of the pod.
func addressRecordPatch(body string) bool {
	var patch struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(body), &patch); err != nil || len(patch.Metadata.Annotations) != 1 {
		return false
	}
	for key := range patch.Metadata.Annotations {
		return strings.HasPrefix(key, "legion.dev/addresses-")
	}
	return false
}

// modePatched reports whether a patch body sets operatingMode to mode: a JSON patch's `add` at
// /spec/operatingMode, or a merge patch's spec.operatingMode.
func modePatched(t *testing.T, body, mode string) bool {
	t.Helper()
	if isMergePatch(body) {
		var patch struct {
			Spec struct {
				OperatingMode string `json:"operatingMode"`
			} `json:"spec"`
		}
		if err := json.Unmarshal([]byte(body), &patch); err != nil {
			t.Fatalf("merge patch %q: %v", body, err)
		}
		return patch.Spec.OperatingMode == mode
	}
	var ops []map[string]any
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("patch %q: %v", body, err)
	}
	for _, op := range ops {
		if op["path"] == "/spec/operatingMode" && op["value"] == mode {
			return true
		}
	}
	return false
}

// withLaggingSandboxInformer delays every watch event the Sandbox informer receives, as a store
// that has not caught up with an API write: the pod informer, undelayed, then runs ahead of it.
func withLaggingSandboxInformer(delay time.Duration) rigOption {
	return func(g *rig, _ *Options) {
		g.dyn.PrependWatchReactor("sandboxes", func(a k8stesting.Action) (bool, watch.Interface, error) {
			var opts metav1.ListOptions
			if w, ok := a.(k8stesting.WatchActionImpl); ok {
				opts = w.ListOptions
			}
			upstream, err := g.dyn.Tracker().Watch(sandboxGVR, a.GetNamespace(), opts)
			if err != nil {
				return true, nil, err
			}
			events := make(chan watch.Event)
			go func() {
				defer close(events)
				for ev := range upstream.ResultChan() {
					time.Sleep(delay)
					events <- ev
				}
			}()
			return true, watch.NewProxyWatcher(events), nil
		})
	}
}

// breakableFeeds is what withBreakableFeeds keeps of the informers' feeds: the watches it served,
// for refuseFeeds to end, the refusal their next lists and watches are answered with (nil for
// none), how many it answered so, and the resources whose next watch is answered with the
// resourceVersion it resumes from expired (expire).
type breakableFeeds struct {
	mu      sync.Mutex
	watches []watch.Interface
	refusal error
	refused int
	expire  map[string]bool
}

// expired is whether resource's next watch is answered with its resourceVersion expired, and clears
// it: a server that refused a watch for a while no longer serves the resourceVersion the informer
// watches again from, as a real API server answers 410 Expired, so the informer lists again. The
// fake's tracker cannot replay what changed meanwhile, deletions included, so a watch resumed from
// it would leave objects deleted in between in the store.
func (f *breakableFeeds) expired(resource string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	expired := f.expire[resource]
	delete(f.expire, resource)
	return expired
}

// refuse is the refusal a list or watch is answered with now, counted, or nil.
func (f *breakableFeeds) refuse() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusal != nil {
		f.refused++
	}
	return f.refusal
}

// withBreakableFeeds serves every Sandbox and pod list and watch the informers make through
// reactors refuseFeeds controls: a watch from the tracker, kept so refuseFeeds can end it as an API
// server ends a watch, and a list from the tracker, unless a refusal stands.
func withBreakableFeeds() rigOption {
	return func(g *rig, _ *Options) {
		g.feeds = &breakableFeeds{}
		for _, served := range []struct {
			fake     *k8stesting.Fake
			tracker  k8stesting.ObjectTracker
			resource string
		}{{&g.dyn.Fake, g.dyn.Tracker(), "sandboxes"}, {&g.kube.Fake, g.kube.Tracker(), "pods"}} {
			served.fake.PrependReactor("list", served.resource, func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				if err := g.feeds.refuse(); err != nil {
					return true, nil, err
				}
				return false, nil, nil
			})
			served.fake.PrependWatchReactor(served.resource, func(a k8stesting.Action) (bool, watch.Interface, error) {
				if err := g.feeds.refuse(); err != nil {
					return true, nil, err
				}
				if g.feeds.expired(served.resource) {
					w := watch.NewRaceFreeFake()
					status := apierrors.NewResourceExpired("too old resource version").Status()
					w.Error(&status)
					return true, w, nil
				}
				var opts metav1.ListOptions
				if w, ok := a.(k8stesting.WatchActionImpl); ok {
					opts = w.ListOptions
				}
				w, err := served.tracker.Watch(a.GetResource(), a.GetNamespace(), opts)
				if err != nil {
					return true, nil, err
				}
				g.feeds.mu.Lock()
				g.feeds.watches = append(g.feeds.watches, w)
				g.feeds.mu.Unlock()
				return true, w, nil
			})
		}
	}
}

// refuseFeeds answers the informers' next lists and watches with refusal, and with refusal set
// ends every watch withBreakableFeeds served, so the informers list and watch again; nil serves
// them again, the next watch of each resource after a refusal answered expired (expired). It
// returns how many lists and watches were refused so far.
func (g *rig) refuseFeeds(refusal error) int {
	g.feeds.mu.Lock()
	if refusal == nil && g.feeds.refusal != nil {
		g.feeds.expire = map[string]bool{"sandboxes": true, "pods": true}
	}
	g.feeds.refusal = refusal
	refused := g.feeds.refused
	watches := g.feeds.watches
	if refusal != nil {
		g.feeds.watches = nil
	}
	g.feeds.mu.Unlock()
	if refusal != nil {
		for _, w := range watches {
			w.Stop()
		}
	}
	return refused
}
