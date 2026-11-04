package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/oidc"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// liveBudgets is where every machine's decision reads the limits it is held to and the waits it
// times (supervise.Settings): the daemon's settings holder, read as the decision is made rather
// than copied into the machine when it was built, so a change of `launch_failure_limit`,
// `prompt_failure_limit`, `prompt_retire_limit`, `worker_boot_timeout_seconds`,
// `worker_rpc_timeout_seconds`, `probe_interval_seconds` or `worker_stop_timeout_seconds` reaches
// the next decision with nothing rebuilt.
type liveBudgets struct{ live *config.Live }

var _ supervise.Settings = liveBudgets{}

func (b liveBudgets) Limits() supervise.Limits {
	cfg := b.live.Snapshot()
	return supervise.Limits{
		LaunchFailures: cfg.LaunchFailureLimit,
		PromptFailures: cfg.PromptFailureLimit,
		PromptRetires:  cfg.PromptRetireLimit,
	}
}

func (b liveBudgets) Timeouts() supervise.Timeouts {
	cfg := b.live.Snapshot()
	return supervise.Timeouts{
		Boot:                  cfg.WorkerBootTimeout,
		RegistrationIntervals: cfg.WorkerBootRegistrationDeadlineIntervals,
		RPC:                   cfg.WorkerRPCTimeout,
		Probe:                 cfg.ProbeInterval,
		Stop:                  cfg.WorkerStopTimeout,
		LaunchBackoff:         launchBackoffFirst,
	}
}

// sandboxOf is the Agent Sandbox runtime behind rt: rt itself, or the one the runtime holder
// currently holds. Everything that asks a question only the Sandbox runtime answers — the image
// probe, the repository caches, the pod at an address — asks through it, so a rebuild of the
// runtime moves the answer with it.
func sandboxOf(rt runtime.Runtime) (*sandbox.Runtime, bool) {
	if holder, ok := rt.(*runtimeHolder); ok {
		rt = holder.get()
	}
	sandboxed, ok := rt.(*sandbox.Runtime)
	return sandboxed, ok
}

// rebuilt is one thing the daemon built from its configuration and can build again while it runs:
// a holder of the class table's `rebuild` and `launch` rows (config.Row.Holder). Each one knows
// its own name there, builds the new one from the configuration that has just settled, swaps it
// behind the pointer its readers go through, and drains or closes what it replaced. Nothing above
// a holder knows a rebuild happened: a reader holds the holder, never the thing inside it.
//
// A rebuild that fails returns its error and changes nothing: the old one keeps serving, the
// daemon keeps running, and the rebuilder logs what could not be built (rebuilder.rebuild).
type rebuilt interface {
	// holder is the name the class table gives it (config.HolderNATS and its neighbours).
	holder() string
	// rebuild builds this holder again from cfg and swaps it in.
	rebuild(cfg *config.Config) error
}

// rebuilder is the daemon's one observer of applied settings changes: it reads each change's
// holder (config.Change.Holder) and builds that holder again, once per apply however many of its
// paths changed, in the order the holders were registered. A change of a `decision` path with no
// holder rebuilds nothing — its readers read the new value at their next decision.
type rebuilder struct {
	log *slog.Logger
	// order is the registration order, and holders the registry by name: a holder the daemon did
	// not build — a test's replaced runtime, a supplied token source — is simply not registered,
	// and a change naming it is logged and does nothing.
	order   []string
	holders map[string]rebuilt
	// credentialDigests is what each credential holder's files said the last time
	// rereadCredentials looked, by holder name: the guard that keeps a wake of the file watcher
	// from rebuilding a holder whose files have not moved (rereadCredentials).
	credentialDigests map[string]string
}

func newRebuilder(log *slog.Logger) *rebuilder {
	return &rebuilder{log: log, holders: map[string]rebuilt{}, credentialDigests: map[string]string{}}
}

// register adds one holder. A second registration of the same name replaces the first.
func (r *rebuilder) register(holders ...rebuilt) {
	for _, h := range holders {
		if h == nil {
			continue
		}
		if _, known := r.holders[h.holder()]; !known {
			r.order = append(r.order, h.holder())
		}
		r.holders[h.holder()] = h
	}
}

// observe is what config.Live calls once every apply has settled: it is registered with
// Live.OnChange, and runs under the holder's lock, so it reads the configuration it is handed and
// never applies another change of its own.
//
// A change names the holder the caller is told was rebuilt (config.Change.Holder) and, where
// more than one thing holds the value, the rest on its row (config.Row.Also): `repository_renames`
// is held by the API server, the repository mapping, the intake consumers and the runtime's gh
// bundles, and every one of them is built again here, so no reader is left comparing names by
// the value the daemon booted on.
func (r *rebuilder) observe(cfg *config.Config, applied []config.Change) {
	named := map[string]struct{}{}
	for _, change := range applied {
		if change.Holder == "" {
			continue
		}
		named[change.Holder] = struct{}{}
		row, known := config.ClassOf(change.Path)
		if !known {
			continue
		}
		for _, also := range row.Also {
			named[also] = struct{}{}
		}
	}
	if len(named) == 0 {
		return
	}
	for _, name := range r.order {
		if _, asked := named[name]; asked {
			r.rebuild(cfg, r.holders[name])
		}
		delete(named, name)
	}
	for name := range named {
		r.log.Warn("legion settings: a changed setting names a holder this daemon did not build; nothing was rebuilt",
			"holder", name)
	}
}

// rebuild builds one holder again and logs what it did. A failure is logged with the holder's
// name and leaves the old one serving: a daemon that cannot reach NATS with a new seed keeps
// reading events on the connection it has, and the operator sees why in the log.
func (r *rebuilder) rebuild(cfg *config.Config, holder rebuilt) {
	name := holder.holder()
	if err := holder.rebuild(cfg); err != nil {
		r.log.Error("legion settings: rebuilding a holder failed; the daemon keeps the one it has",
			"holder", name, "error", err)
		return
	}
	r.log.Info("legion settings: holder rebuilt", "holder", name)
}

// credentialReader is a holder that reads a credential file of this host's through
// internal/config's readers (config.ReadSecretPointer and its neighbours): it names the files
// its own rebuild reads, beside that reader, so the daemon holds one list of what a holder reads
// rather than one here and one where it is read.
//
// A holder that reads credentials but names no file of this host's — the GitHub App manager,
// whose private key is the key itself and whose command and secret are no file — answers none
// and is rebuilt at every reread. A holder that is no credentialReader is left alone by a
// reread: nothing it holds was read from a file that can rotate under it.
type credentialReader interface {
	rebuilt
	credentialFiles(cfg *config.Config) []string
}

// credentialDigest is what files say, as one string: each file's name beside the digest of its
// bytes, with a read failure folded in as its own text so a file that comes and goes is a change
// like any other. A file the configuration does not name is its own distinct mark, so a path
// appearing or being cleared moves the digest.
func credentialDigest(files []string) string {
	sum := sha256.New()
	for _, file := range files {
		if file == "" {
			fmt.Fprint(sum, "\x00unnamed")
			continue
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(sum, "\x00error %s: %v", file, err)
			continue
		}
		fmt.Fprintf(sum, "\x00file %s %x", file, sha256.Sum256(contents))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// rereadCredentials builds every holder whose credential files have moved since the last reread
// again, whatever the configuration says: the answer to a rotation of a file whose path did not
// change, which no settings change names, since the path is what the file holds and the path has
// not moved. It runs in the order the holders were registered, and over the holders that read a
// credential file at all (credentialReader).
//
// The file watcher calls it, once the file arm has settled the configuration, and calls it
// under the settings holder's own lock (config.Live.WhileSettled), where an applied change's
// rebuilds run: a reread and a route change never build one holder at once, and the
// configuration it reads is the one the daemon is running on.
//
// The digest guard is what keeps an unrelated wake cheap. The watcher wakes on the state of a
// whole directory, so a projected token the kubelet refreshed beside a seed that did not move
// settles the configuration again; without the guard that wake would reconnect NATS and migrate
// every durable consumer, drop every App lease and rewrite the deployment instructions. A holder
// whose files read the same holds what they say already, so rebuilding it is work with no answer
// of its own; a holder that names no file (the GitHub App manager) is rebuilt as before.
func (r *rebuilder) rereadCredentials(cfg *config.Config) {
	for _, name := range r.order {
		holder, reads := r.holders[name].(credentialReader)
		if !reads {
			continue
		}
		if files := holder.credentialFiles(cfg); len(files) > 0 {
			digest := credentialDigest(files)
			moved := r.credentialDigests[name] != digest
			r.credentialDigests[name] = digest
			if !moved {
				continue
			}
		}
		r.rebuild(cfg, holder)
	}
}

// dispatchHolder is the Dispatch client every workflow call goes through (config.HolderDispatchClient).
// It is a dispatch.Client itself, so the workflow, the outbox and the API hold it and never the
// client inside it. A rebuild reads the bearer again (readDispatchToken) and swaps; the client it
// replaces holds no connection of its own — it is an HTTP client over the shared transport — so
// there is nothing to drain, and a call already in flight finishes on the client it started on.
type dispatchHolder struct {
	current atomic.Pointer[dispatch.Client]
}

var _ dispatch.Client = (*dispatchHolder)(nil)

// dispatchClient is the Dispatch client of one configuration and one bearer: boot's and the
// holder's rebuild both build it here, so the two can never differ in what they build.
func dispatchClient(cfg config.Config, token string) dispatch.Client {
	return dispatch.New(cfg.DispatchURL, token)
}

func newDispatchHolder(client dispatch.Client) *dispatchHolder {
	h := &dispatchHolder{}
	h.current.Store(&client)
	return h
}

func (h *dispatchHolder) holder() string { return config.HolderDispatchClient }

// credentialFiles is the bearer this client is built with (`dispatch_token_file`, read again by
// every rebuild through readDispatchToken).
func (h *dispatchHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.DispatchTokenFile}
}

func (h *dispatchHolder) rebuild(cfg *config.Config) error {
	token, err := readDispatchToken(*cfg)
	if err != nil {
		return err
	}
	client := dispatchClient(*cfg, token)
	h.current.Store(&client)
	return nil
}

// get is the client this call goes through.
func (h *dispatchHolder) get() dispatch.Client { return *h.current.Load() }

func (h *dispatchHolder) ListIssues(ctx context.Context, project string, statuses []string) ([]dispatch.IssueSummary, error) {
	return h.get().ListIssues(ctx, project, statuses)
}

func (h *dispatchHolder) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	return h.get().GetIssue(ctx, key)
}

func (h *dispatchHolder) SetStatus(ctx context.Context, key, status string) (dispatch.Issue, error) {
	return h.get().SetStatus(ctx, key, status)
}

func (h *dispatchHolder) HandOver(ctx context.Context, issue dispatch.Issue, status string) error {
	return h.get().HandOver(ctx, issue, status)
}

func (h *dispatchHolder) PostMessage(ctx context.Context, key, body string) error {
	return h.get().PostMessage(ctx, key, body)
}

func (h *dispatchHolder) MessageBodiesSince(ctx context.Context, key string, since time.Time) ([]string, error) {
	return h.get().MessageBodiesSince(ctx, key, since)
}

func (h *dispatchHolder) EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error) {
	return h.get().EventsAfter(ctx, key, after)
}

func (h *dispatchHolder) Approval(ctx context.Context, artifactID string) (dispatch.Approval, error) {
	return h.get().Approval(ctx, artifactID)
}

func (h *dispatchHolder) Handoff(ctx context.Context, id string) (dispatch.Handoff, error) {
	return h.get().Handoff(ctx, id)
}

func (h *dispatchHolder) BuildCommit(ctx context.Context) (string, error) {
	return h.get().BuildCommit(ctx)
}

func (h *dispatchHolder) ListRepoProjects(ctx context.Context) ([]dispatch.RepoProject, error) {
	return h.get().ListRepoProjects(ctx)
}

func (h *dispatchHolder) WebhookAppInstalled(ctx context.Context, repo string) (bool, error) {
	return h.get().WebhookAppInstalled(ctx, repo)
}

// publisherHolder is the Envoy notify publisher (config.HolderEnvoyPublisher), which the outbox
// publishes every notice through. It is a notify.Publisher itself. A rebuild reads the Envoy
// bearer again (readEnvoyToken) and swaps; the publisher it replaces is an HTTP client with
// nothing to drain, and a publish in flight finishes on the one it started on.
type publisherHolder struct {
	current atomic.Pointer[notify.Publisher]
}

var _ notify.Publisher = (*publisherHolder)(nil)

// envoyPublisher is the Envoy notify publisher of one configuration and one bearer: boot's and
// the holder's rebuild both build it here.
func envoyPublisher(cfg config.Config, token string) notify.Publisher {
	return notify.New(cfg.EnvoyURL, token)
}

func newPublisherHolder(publisher notify.Publisher) *publisherHolder {
	h := &publisherHolder{}
	h.current.Store(&publisher)
	return h
}

func (h *publisherHolder) holder() string { return config.HolderEnvoyPublisher }

// credentialFiles is the bearer this publisher is built with (`envoy_token_file`, read again by
// every rebuild through readEnvoyToken).
func (h *publisherHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.EnvoyTokenFile}
}

func (h *publisherHolder) rebuild(cfg *config.Config) error {
	token, err := readEnvoyToken(*cfg)
	if err != nil {
		return err
	}
	publisher := envoyPublisher(*cfg, token)
	h.current.Store(&publisher)
	return nil
}

func (h *publisherHolder) Publish(ctx context.Context, topic, message string, payload any, dedupeKey string) error {
	return (*h.current.Load()).Publish(ctx, topic, message, payload, dedupeKey)
}

// appsHolder is the GitHub App credential manager (config.HolderGitHubApps): the source every
// role's token is minted from. It is the four interfaces the workflow asks of a token source, so
// the workflow, the outbox, the API and the runtime hold it and never the manager inside it. A
// rebuild builds a manager from the new App credentials — the loader has already resolved each
// private key, its command and its secret — and swaps; the manager it replaces holds leases in
// memory alone, so dropping it is the drain, and the next mint asks GitHub under the new
// credential.
//
// A daemon whose token source was supplied rather than built (a workflow test's) registers no
// such holder: there is nothing the configuration would build again.
type appsHolder struct {
	current atomic.Pointer[appauth.Tokens]
}

var (
	_ appauth.Tokens        = (*appsHolder)(nil)
	_ appauth.Installations = (*appsHolder)(nil)
	_ appauth.Logins        = (*appsHolder)(nil)
	_ appauth.Forgetter     = (*appsHolder)(nil)
)

// buildApps is the App token manager of one configuration: boot's and the holder's rebuild both
// build it here.
func buildApps(cfg config.Config) appauth.Tokens {
	return appauth.New(cfg.GitHubApps, appauth.Options{})
}

func newAppsHolder(tokens appauth.Tokens) *appsHolder {
	h := &appsHolder{}
	h.current.Store(&tokens)
	return h
}

func (h *appsHolder) holder() string { return config.HolderGitHubApps }

// credentialFiles is none of this host's: an App private key is the key itself, and its command
// and its secret are no file here (buildApps), so the manager is built again at every reread.
func (h *appsHolder) credentialFiles(*config.Config) []string { return nil }

func (h *appsHolder) rebuild(cfg *config.Config) error {
	tokens := buildApps(*cfg)
	replaced := h.current.Swap(&tokens)
	// The manager the new one replaces owns a token_command's goroutine, so a rebuild that left
	// it open would leak one per changed App credential.
	if replaced != nil {
		closeTokens(*replaced)
	}
	return nil
}

// Close ends the App token manager this holder holds, so the daemon's stop ends every
// token_command still waiting on a grant (workflowRuntime.stop, closeTokens). Without it the
// holder — which is what the workflow holds, in place of the manager itself — would answer no
// Close and the stop would end no command at all.
func (h *appsHolder) Close() { closeTokens(h.get()) }

func (h *appsHolder) get() appauth.Tokens { return *h.current.Load() }

func (h *appsHolder) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return h.get().Token(ctx, role, owner)
}

func (h *appsHolder) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return h.get().ReadToken(ctx, role, owner)
}

func (h *appsHolder) InstalledOn(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (bool, error) {
	installs, ok := h.get().(appauth.Installations)
	if !ok {
		return false, errors.New("the GitHub App token source answers no App installation")
	}
	return installs.InstalledOn(ctx, role, repo)
}

func (h *appsHolder) AppLogin(ctx context.Context, role appauth.AppRole) (string, error) {
	logins, ok := h.get().(appauth.Logins)
	if !ok {
		return "", errors.New("the GitHub App token source answers no App login")
	}
	return logins.AppLogin(ctx, role)
}

func (h *appsHolder) Forget(role appauth.AppRole, owner string) {
	if forget, ok := h.get().(appauth.Forgetter); ok {
		forget.Forget(role, owner)
	}
}

// apiHolder is the settings the API server holds (config.HolderAPIServer): the operator bearer,
// the design gate, the repository equivalence, each project's merge rules and the settings
// route's own admission rule. Its socket is never rebound — the listener is boot's, and
// LEGION_LISTEN_FD may have handed the daemon one it never bound — so a rebuild reads the
// operator bearer again (readOperatorToken), builds the verifier again where the deployment
// moved it, and stores the new settings behind the pointer every request reads through
// (api.LiveSettings). Nothing drains: a request already in a route keeps the settings it began
// on.
type apiHolder struct {
	settings *api.LiveSettings
	// route is the settings route's projected-token arm as it now stands, and issuer and
	// audience what it was built for: a rebuild for some other setting keeps the verifier it has,
	// with the issuer's keys it has already read.
	route            api.SettingsRouteAuth
	issuer, audience string
}

func newAPIHolder(cfg config.Config, operatorToken string) *apiHolder {
	h := &apiHolder{}
	h.settings = api.NewLiveSettings(api.HeldSettings{
		OperatorToken:     operatorToken,
		DesignGate:        cfg.Gates.Design,
		RepositoryRenames: cfg.RepositoryRenames,
		MergeRules:        projectMergeRules(cfg),
		SettingsRoute:     h.settingsRoute(cfg),
	})
	return h
}

func (h *apiHolder) holder() string { return config.HolderAPIServer }

// credentialFiles is the operator bearer the server compares against (`operator_token_file`,
// read again by every rebuild through readOperatorToken).
func (h *apiHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.OperatorTokenFile}
}

func (h *apiHolder) rebuild(cfg *config.Config) error {
	token, err := readOperatorToken(*cfg)
	if err != nil {
		return err
	}
	h.settings.Store(api.HeldSettings{
		OperatorToken:     token,
		DesignGate:        cfg.Gates.Design,
		RepositoryRenames: cfg.RepositoryRenames,
		MergeRules:        projectMergeRules(*cfg),
		SettingsRoute:     h.settingsRoute(*cfg),
	})
	return nil
}

// settingsRoute is who the settings route admits besides the operator bearer, as cfg names them
// (`settings_route`): a verifier of the cluster's OIDC issuer for the audience the deployment's
// tokens are projected with, and the ServiceAccounts it takes. A deployment that names none
// leaves that route to the operator bearer alone.
//
// Building a verifier reads nothing: it discovers its issuer at the first token it verifies
// (oidc.NewLazyVerifier), so this runs under the settings holder's lock with no network call,
// and a daemon whose cluster issuer is unreachable still boots and still serves. A rebuild for
// another setting keeps the verifier already built, with whatever of the issuer's key set it has
// read; only a changed issuer or audience builds a new one.
func (h *apiHolder) settingsRoute(cfg config.Config) api.SettingsRouteAuth {
	route := cfg.SettingsRoute
	if !route.Configured() {
		h.route, h.issuer, h.audience = api.SettingsRouteAuth{}, "", ""
		return h.route
	}
	if h.route.Verifier == nil || h.issuer != route.OIDCIssuer || h.audience != route.Audience {
		h.route.Verifier = oidc.NewLazyVerifier(route.OIDCIssuer, route.Audience)
		h.issuer, h.audience = route.OIDCIssuer, route.Audience
	}
	h.route.ServiceAccounts = slices.Clone(route.ServiceAccounts)
	return h.route
}

// mappingHolder is the project's repository mapping as a holder of the deployment's repository
// equivalence (config.HolderRepositoryMapping, `repository_renames`): the rule the mapping
// compares two repository names with, which a change of that key must reach, as it reaches the
// API server's settings, the intake consumers and the gh bundles the runtime's launches hand
// each role (the row's Also).
//
// The mapping itself — which repositories Dispatch serves — is read from Dispatch and is no
// setting of this file's, so a rebuild replaces the equivalence alone, under the mapping's own
// lock, and every later read compares with the new one.
type mappingHolder struct{ repositories *repositories }

func newMappingHolder(repositories *repositories) *mappingHolder {
	return &mappingHolder{repositories: repositories}
}

func (h *mappingHolder) holder() string { return config.HolderRepositoryMapping }

func (h *mappingHolder) rebuild(cfg *config.Config) error {
	h.repositories.setRenames(cfg.RepositoryRenames)
	return nil
}

// workerStreamHolder is the worker stream listener's held settings (config.HolderWorkerStream):
// `worker_rpc_timeout_seconds`, the bound on every hello and every request. The socket is never
// rebound and no connection is dropped — every shim holds one — so the rebuild swaps the bound
// alone, and each connection the listener already holds reads the new one at its next request
// (stream.Listener.SetRPCTimeout).
type workerStreamHolder struct {
	listener *stream.Listener
}

func newWorkerStreamHolder(listener *stream.Listener) *workerStreamHolder {
	return &workerStreamHolder{listener: listener}
}

func (h *workerStreamHolder) holder() string { return config.HolderWorkerStream }

func (h *workerStreamHolder) rebuild(cfg *config.Config) error {
	if cfg.WorkerRPCTimeout <= 0 {
		return fmt.Errorf("worker_rpc_timeout_seconds must be positive, got %s", cfg.WorkerRPCTimeout)
	}
	h.listener.SetRPCTimeout(cfg.WorkerRPCTimeout)
	return nil
}

// specsHolder is the launch specs the supervisor builds each claim's process from
// (config.HolderLaunchSpecs). It is a supervise.Specs itself, held in the machines' Deps, so a
// rebuild reaches every machine without one being rebuilt: the next launch reads the new specs,
// which is what a `launch` row promises. A rebuild reads the launch secrets and the operator's
// instructions again (readLaunchSecrets, readInstructions), so a rotated credential reaches the
// next launch too; the specs it replaces hold nothing to drain.
type specsHolder struct {
	current atomic.Pointer[specs]
	// build is the specs of one configuration, over everything boot resolved that the
	// configuration does not carry (openSupervision's own specs literal).
	build func(cfg config.Config) (specs, error)
}

var _ supervise.Specs = (*specsHolder)(nil)

func newSpecsHolder(settled specs, build func(cfg config.Config) (specs, error)) *specsHolder {
	h := &specsHolder{build: build}
	h.current.Store(&settled)
	return h
}

func (h *specsHolder) holder() string { return config.HolderLaunchSpecs }

// credentialFiles are the launch secrets every spec carries and the operator's instructions,
// which a rebuild reads again (readLaunchSecrets, readInstructions).
func (h *specsHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.EnvoyTokenFile, cfg.NatsNkeySeedFile, cfg.InstructionsPath}
}

func (h *specsHolder) rebuild(cfg *config.Config) error {
	next, err := h.build(*cfg)
	if err != nil {
		return err
	}
	h.current.Store(&next)
	return nil
}

func (h *specsHolder) SpawnSpec(ctx context.Context, c supervise.Claim) (runtime.SpawnSpec, error) {
	return h.current.Load().SpawnSpec(ctx, c)
}

// runtimeHolder is the process runtime, Sandbox or tmux (config.HolderRuntime): everything a
// launch reads, and the pod shapes the Sandbox runtime caches when it is built
// (runtime/sandbox's podShape). It is a runtime.Runtime itself, held by the machines' Deps, the
// supervision and the API's tree releaser, so a rebuild reaches all of them behind one pointer
// and takes no pod down: the new runtime observes and adopts the processes the old one was
// supervising, which are the cluster's and the tmux server's, not the runtime's.
//
// Observe is the one call whose answer outlives it: supervision ranges over one channel for its
// whole life. The holder owns that channel and feeds it from whichever runtime is current, so a
// rebuild moves the feed across without supervision noticing.
type runtimeHolder struct {
	current atomic.Pointer[runtime.Runtime]
	// build makes a runtime from a configuration and answers the stop that ends it: the Sandbox
	// runtime's informers, its cache and credential refreshers and the tmux runtime's sweeps run
	// on a context of that runtime's own, so a rebuild can end the one it replaces rather than
	// leave it watching the cluster for the daemon's whole life.
	build func(cfg config.Config) (runtime.Runtime, context.CancelFunc, error)

	// mu guards the observation feed: out is the channel supervision ranges over, once it has
	// asked for one, observing the context it asked on, and stopFeed ends the feed from the
	// runtime being replaced. feeds numbers the feeds, so the one a rebuild replaced knows not to
	// close the channel the new one is writing to, and closed says the channel is already closed.
	// stopCurrent ends the runtime the holder holds now.
	mu          sync.Mutex
	out         chan runtime.Observation
	watching    context.Context
	stopFeed    context.CancelFunc
	stopCurrent context.CancelFunc
	feeds       uint64
	closed      bool
}

var _ runtime.Runtime = (*runtimeHolder)(nil)

// newRuntimeHolder holds rt, which stop ends, and builds its replacements with build.
func newRuntimeHolder(rt runtime.Runtime, stop context.CancelFunc, build func(cfg config.Config) (runtime.Runtime, context.CancelFunc, error)) *runtimeHolder {
	h := &runtimeHolder{build: build, stopCurrent: stop}
	h.current.Store(&rt)
	return h
}

func (h *runtimeHolder) holder() string { return config.HolderRuntime }

// rebuild builds the runtime again from the configuration that has just settled, moves
// supervision's feed onto it, swaps it in, and only then ends the one it replaced: everything
// that runtime runs of its own — the Sandbox runtime's three informers and its cache and
// credential refreshers — ends with it, so a deployment that changes a `runtime.*` key ten
// times is watching the cluster once and not ten times over.
//
// A failure changes nothing: the runtime that was built is stopped, the feed stays on the
// runtime the daemon is supervising with, and the rebuilder logs what could not be built.
func (h *runtimeHolder) rebuild(cfg *config.Config) error {
	next, stopNext, err := h.build(*cfg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.out != nil {
		// The feed moves to the new runtime before the old one's ends, so no observation is
		// missed between them; a duplicate observation of one process is what every probe
		// already answers.
		previous := h.stopFeed
		h.feeds++
		if err := h.feed(next); err != nil {
			h.feeds--
			stopNext()
			return err
		}
		if previous != nil {
			previous()
		}
	}
	h.current.Store(&next)
	replaced := h.stopCurrent
	h.stopCurrent = stopNext
	if replaced != nil {
		replaced()
	}
	return nil
}

func (h *runtimeHolder) get() runtime.Runtime { return *h.current.Load() }

// Observe is supervision's one channel of process observations, fed from whichever runtime is
// current. The caller's context is the feed's life; a second call answers the same channel.
func (h *runtimeHolder) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.out != nil {
		return h.out, nil
	}
	h.out, h.watching = make(chan runtime.Observation), ctx
	if err := h.feed(h.get()); err != nil {
		h.out, h.watching = nil, nil
		return nil, err
	}
	return h.out, nil
}

// feed starts forwarding rt's observations into the holder's channel until the rebuild that
// replaces rt, or the context Observe was given, ends. The caller holds mu.
//
// The last feed closes the channel as it ends, as a runtime's own Observe channel closes when
// its context ends: supervision ranges over it, and a channel that never closes would hold the
// daemon's stop until its budget ran out. A feed a rebuild replaced closes nothing — the feed
// that replaced it is writing to that same channel.
func (h *runtimeHolder) feed(rt runtime.Runtime) error {
	feeding, stop := context.WithCancel(h.watching)
	observations, err := rt.Observe(feeding)
	if err != nil {
		stop()
		return err
	}
	h.stopFeed = stop
	mine := h.feeds
	go func() {
		defer h.ended(mine)
		defer stop()
		for {
			select {
			case <-feeding.Done():
				return
			case observation, open := <-observations:
				if !open {
					return
				}
				select {
				case h.out <- observation:
				case <-feeding.Done():
					return
				}
			}
		}
	}()
	return nil
}

// ended closes the observation channel once the feed numbered mine was the last one.
func (h *runtimeHolder) ended(mine uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.feeds != mine || h.closed {
		return
	}
	h.closed = true
	close(h.out)
}

func (h *runtimeHolder) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	return h.get().Spawn(ctx, spec)
}

func (h *runtimeHolder) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	return h.get().Resume(ctx, prev, spec)
}

func (h *runtimeHolder) Suspend(ctx context.Context, loc runtime.Locator) error {
	return h.get().Suspend(ctx, loc)
}

func (h *runtimeHolder) Release(ctx context.Context, k runtime.Known) error {
	return h.get().Release(ctx, k)
}

func (h *runtimeHolder) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	return h.get().Probe(ctx, loc)
}

func (h *runtimeHolder) ReconcileOrphans(ctx context.Context, known []runtime.Known, grace time.Duration) error {
	return h.get().ReconcileOrphans(ctx, known, grace)
}

func (h *runtimeHolder) AdoptWorkingCopy(ctx context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	return h.get().AdoptWorkingCopy(ctx, loc, id)
}

func (h *runtimeHolder) ProvisionsWorkspaces() bool { return h.get().ProvisionsWorkspaces() }

func (h *runtimeHolder) ProvisionBound() time.Duration { return h.get().ProvisionBound() }

func (h *runtimeHolder) LaunchBound() time.Duration { return h.get().LaunchBound() }

func (h *runtimeHolder) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	return h.get().PodDrift(ctx, k)
}

func (h *runtimeHolder) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	return h.get().ReplacePod(ctx, k, due)
}

func (h *runtimeHolder) RescueUnpushed(ctx context.Context, k runtime.Known, request runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
	return h.get().RescueUnpushed(ctx, k, request)
}

func (h *runtimeHolder) CleanupTree(ctx context.Context, tree string) error {
	return h.get().CleanupTree(ctx, tree)
}

// keeperHolder is the controller keeper and the controller tick
// (config.HolderControllerKeeper): whether this daemon launches the project's controller itself
// (`controller`) and how often it wakes it (`controller_wake_interval_seconds`). A rebuild
// stores the new interval, which the tick reads at every turn, and starts or stops the keeper's
// own loop when the launch mode changed — the controller's claim is the store's either way, so
// neither start nor stop touches a running controller.
//
// The loop is started here and nowhere else (begin): the boot starts it through this holder
// under `controller: daemon`, and so does a change that moves the mode to the daemon, so there
// is one way the keeper runs and one place that knows whether it is running.
type keeperHolder struct {
	// wake is controller_wake_interval_seconds in nanoseconds, read by the controller tick at
	// every turn: the one place the daemon holds it (workflowRuntime.wakeInterval).
	wake atomic.Int64
	// launched is whether the daemon launches the controller itself, and start runs the keeper's
	// own loop until the context it is given ends; start is nil in a deployment with no keeper
	// to run (a test's workflow runtime built by hand).
	mu       sync.Mutex
	launched bool
	start    func() context.CancelFunc
	stop     context.CancelFunc
}

// newKeeperHolder is the holder of a daemon whose configuration wakes its controller every wake
// and launches it itself where launched; start runs the keeper's loop and answers the cancel
// that ends it (nil in a deployment with no keeper to run). It starts nothing: begin does, once
// the caller has registered the holder.
func newKeeperHolder(wake time.Duration, launched bool, start func() context.CancelFunc) *keeperHolder {
	h := &keeperHolder{launched: launched, start: start}
	h.wake.Store(int64(wake))
	return h
}

func (h *keeperHolder) holder() string { return config.HolderControllerKeeper }

// begin runs the keeper's loop where this daemon launches the controller itself, and nothing
// where it does not. It is the boot's one call, and a change of the mode starts and stops the
// same loop through the same start (rebuild).
func (h *keeperHolder) begin() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.launched && h.start != nil && h.stop == nil {
		h.stop = h.start()
	}
}

// launchedNow is whether the daemon launches the project's controller itself as its settings
// now stand, which the API reads at every request that turns on it
// (api.Options.ControllerLaunched).
func (h *keeperHolder) launchedNow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.launched
}

// interval is the wake the controller tick runs on now.
func (h *keeperHolder) interval() time.Duration { return time.Duration(h.wake.Load()) }

func (h *keeperHolder) rebuild(cfg *config.Config) error {
	if cfg.ControllerWakeInterval > 0 {
		h.wake.Store(int64(cfg.ControllerWakeInterval))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	launched := cfg.ControllerLaunch == config.ControllerLaunchDaemon
	if launched == h.launched || h.start == nil {
		return nil
	}
	h.launched = launched
	if !launched {
		if h.stop != nil {
			h.stop()
			h.stop = nil
		}
		return nil
	}
	h.stop = h.start()
	return nil
}

// natsHolder is the NATS connection and its durable consumers (config.HolderNATS). A rebuild
// opens a new connection with the configuration's URLs and seed, moves this project's durable
// consumers onto it, and only then ends the old connection's intake and closes it, waiting for
// the run it ended to settle its messages first (rebuild). No consumer is deleted and no
// position is lost — the consumers are durable, and the new ones resume where the old ones
// stopped.
type natsHolder struct {
	// open builds a connection and its consumers from a configuration.
	open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error)
	log  *slog.Logger

	mu         sync.Mutex
	conn       *nats.Conn
	consumers  *intake.Consumers
	generation uint64
	// cancelRun ends the intake loop's current generation; generation above is what a loop whose
	// run ended compares, to tell a rebuild's cancel from the daemon's stop (rebuilt). settled is
	// closed by that loop once its Run has returned, which is what a rebuild waits for before it
	// closes the connection that run was on; nil when no generation is running.
	cancelRun context.CancelFunc
	settled   chan struct{}
}

// drainBound is how long a rebuild waits for the intake it ended to settle its messages before
// it closes the connection anyway. It is longer than the ack wait a consumer is built with
// (`worker_rpc_timeout_seconds`), so a handler that is working has its turn, and short enough
// that a handler that will never return does not hold the settings change for ever: the daemon
// then closes the old connection and logs that it did, where the messages still in flight are
// redelivered by the durable consumers the new connection holds.
const drainBound = 2 * time.Minute

func newNATSHolder(conn *nats.Conn, consumers *intake.Consumers, log *slog.Logger, open func(cfg config.Config) (*nats.Conn, *intake.Consumers, error)) *natsHolder {
	if log == nil {
		log = slog.Default()
	}
	return &natsHolder{open: open, log: log, conn: conn, consumers: consumers}
}

func (h *natsHolder) holder() string { return config.HolderNATS }

// credentialFiles are the nkey seeds the connection authenticates as, which a rebuild reads
// again (readNATSConnection).
func (h *natsHolder) credentialFiles(cfg *config.Config) []string {
	return []string{cfg.NatsNkeySeedFile, cfg.NatsDaemonNkeySeedFile}
}

// connection and intake are the connection and the consumers as they now stand.
func (h *natsHolder) connection() *nats.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

func (h *natsHolder) intake() *intake.Consumers {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.consumers
}

// begin is one generation of the intake loop: the consumers to run, a context the next rebuild
// cancels, the generation's number, which the loop compares to tell a rebuild's cancel from the
// daemon's stop, and the settled the loop closes once its Run has returned.
func (h *natsHolder) begin(ctx context.Context) (*intake.Consumers, context.Context, uint64, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	running, cancel := context.WithCancel(ctx)
	h.cancelRun, h.settled = cancel, make(chan struct{})
	return h.consumers, running, h.generation, h.settled
}

// rebuilt says whether generation is behind the holder's: the loop's run was ended by a rebuild
// and the next one is the new connection's.
func (h *natsHolder) rebuilt(generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return generation != h.generation
}

// rebuild opens a connection with the configuration's URLs and seed, moves this project's
// durable consumers onto it, and only then ends the old connection's intake and closes it: the
// generation the intake loop runs (begin) is cancelled, and the connection is closed once that
// loop says its Run has returned, which is once every message it was holding has been acked or
// nak'd. An ack of a fact in flight therefore goes to the connection it was delivered on, never
// to a closed one, and a fact the handler had not finished is redelivered by the durable
// consumer the new connection holds. No consumer is deleted and no position is lost.
func (h *natsHolder) rebuild(cfg *config.Config) error {
	conn, consumers, err := h.open(*cfg)
	if err != nil {
		return err
	}
	h.mu.Lock()
	old, cancel, settled := h.conn, h.cancelRun, h.settled
	h.conn, h.consumers, h.generation = conn, consumers, h.generation+1
	h.cancelRun, h.settled = nil, nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if settled != nil {
		select {
		case <-settled:
		case <-time.After(drainBound):
			h.log.Warn("legion settings: the intake of the NATS connection being replaced did not settle; the daemon closes it and its durable consumers redeliver whatever was in flight",
				"holder", config.HolderNATS, "waited", drainBound)
		}
	}
	if old != nil {
		old.Close()
	}
	return nil
}

// close ends the connection the holder holds, which the daemon's stop calls.
func (h *natsHolder) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != nil {
		h.conn.Close()
		h.conn = nil
	}
}

// openNATS is how the daemon opens a connection and this project's durable consumers from a
// configuration: boot's own path (workflowRuntime.connect) and the holder's rebuild run it, so
// the two can never settle differently. It reads the daemon's nkey seed again (readNATSConnection),
// which is how a rotated seed reaches the connection.
func openNATS(ctx context.Context, w *workflowRuntime, cfg config.Config, lookup func(string) (string, bool), spec intake.ConsumerSpec) (*nats.Conn, *intake.Consumers, error) {
	nc, err := readNATSConnection(cfg, lookup)
	if err != nil {
		return nil, nil, err
	}
	conn, err := natsauth.Connect(cfg.NatsURLs, nc.seed, natsauth.LogEvents(w.log), natsauth.ReconnectForever())
	if err != nil {
		return nil, nil, fmt.Errorf("connect Envoy NATS: %w", err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("open Envoy JetStream: %w", natsauth.WithLastError(err, conn))
	}
	consumers, err := intake.OpenConsumers(ctx, js, w.pool, spec)
	if err != nil {
		conn.Close()
		return nil, nil, natsauth.WithLastError(err, conn)
	}
	return conn, consumers, nil
}
