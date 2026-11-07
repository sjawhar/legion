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

	"github.com/sjawhar/envoy/oidc"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
// `worker_rpc_timeout_seconds`, `probe_interval_seconds`, `worker_stop_timeout_seconds` or
// `pod_update_retask_hours` reaches the next decision with nothing rebuilt.
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
		PodUpdateRetask:       cfg.PodUpdateRetask,
	}
}

// sandboxOf is the Agent Sandbox runtime behind rt: rt itself, or the one the runtime holder
// currently holds. Everything that asks a question only the Sandbox runtime answers — the image
// probe, the repository caches, the pod at an address — asks through it, so a rebuild of the
// runtime moves the answer with it.
func sandboxOf(rt runtime.Runtime) (*sandbox.Runtime, bool) {
	if holder, held := rt.(heldRuntime); held {
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
	// live is the settings holder the daemon runs on, which register reads to record what a
	// holder's credentials said when that holder was built from them.
	live *config.Live
	// order is the registration order, and holders the registry by name: a holder the daemon did
	// not build — a test's replaced runtime, a supplied token source — is simply not registered,
	// and a change naming it is logged and does nothing.
	order   []string
	holders map[string]rebuilt
	// credentialDigests is what each credential holder was built from, by holder name: recorded
	// at registration and at every rebuild that succeeded (rebuilder.rebuild), and the guard that
	// keeps a wake of the file watcher from rebuilding a holder whose credentials have not moved
	// since (rereadCredentials).
	credentialDigests map[string]string
}

// newRebuilder is the rebuild observer of the daemon running on live, whose snapshot every
// registration reads the new holder's credentials from.
func newRebuilder(log *slog.Logger, live *config.Live) *rebuilder {
	return &rebuilder{log: log, live: live, holders: map[string]rebuilt{}, credentialDigests: map[string]string{}}
}

// register adds one holder. A second registration of the same name replaces the first.
//
// A holder built from a credential is registered having just been built from it, so what that
// credential says now is recorded here: without it the first reread after boot would count every
// one of them as moved and rebuild it — reconnecting NATS, migrating every durable consumer and
// dropping every App lease — on the first wake of the file watcher, which the kubelet's refresh
// of any watched directory, `agent_secrets.token_file`'s among them, is.
func (r *rebuilder) register(holders ...rebuilt) {
	for _, h := range holders {
		if h == nil {
			continue
		}
		if _, known := r.holders[h.holder()]; !known {
			r.order = append(r.order, h.holder())
		}
		r.holders[h.holder()] = h
		if credentials, reads := h.(credentialHolder); reads {
			r.credentialDigests[h.holder()] = credentials.credentials(r.live.Snapshot())
		}
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

// rebuild builds one holder again, records what its credentials said when it was built, and
// logs what it did. A failure is logged with the holder's name and leaves the old one serving:
// a daemon that cannot reach NATS with a new seed keeps reading events on the connection it
// has, and the operator sees why in the log.
//
// The digest is recorded here and at registration, and nowhere else: a holder is recorded as
// what it was built from, whichever route built it. It is read before the rebuild, so a
// credential that moved while the holder was being built is read again at the next reread
// rather than taken for the one it was built from, and a rebuild that failed records nothing,
// so the credentials it could not build on are tried again at the next reread. Recorded where a
// rebuild is decided instead — in the reread's own loop — the two drifted apart: one
// `dispatch_token_file` change rebuilt the client twice (the change's own rebuild, then the
// reread the watcher's settle runs), a route change of `github_apps.*` was rebuilt over again
// at the next unrelated wake, closing the manager that had served since — every lease dropped,
// every `token_command` waiting on a person killed — and a reread rebuild that failed was never
// retried.
func (r *rebuilder) rebuild(cfg *config.Config, holder rebuilt) {
	name := holder.holder()
	credentials, reads := holder.(credentialHolder)
	var built string
	if reads {
		built = credentials.credentials(cfg)
	}
	if err := holder.rebuild(cfg); err != nil {
		r.log.Error("legion settings: rebuilding a holder failed; the daemon keeps the one it has",
			"holder", name, "error", err)
		return
	}
	if reads {
		r.credentialDigests[name] = built
	}
	r.log.Info("legion settings: holder rebuilt", "holder", name)
}

// credentialHolder is a holder built from a credential that can move under the daemon with no
// setting changing: a file of this host's, read through internal/config's readers
// (config.ReadSecretPointer and its neighbours), or a value of the configuration itself. It
// says what its own rebuild would be built from as one string, beside that reader, so the
// daemon holds one list of what a holder reads rather than one here and one where it is read.
//
// What that string is, a reread compares with what the holder was built from
// (rereadCredentials), and the rebuilder records at every rebuild (rebuilder.rebuild). A
// holder's files are digested with credentialDigest; the App manager, whose private key is the
// key itself and whose command and secret are no file, digests the credentials it was built
// from instead, and that is why the comparison is a string rather than a list of files. It has
// to be compared at all because its rebuild closes the manager it replaces — every lease
// dropped, every `token_command` still waiting on a person's approval killed
// (appauth.Manager.Close) — and the file watcher wakes on the state of a whole directory, the
// one the kubelet rewrites a projected token in among them.
//
// A holder that is no credentialHolder is left alone by a reread: nothing it holds was read
// from a credential that can move under it.
type credentialHolder interface {
	rebuilt
	credentials(cfg *config.Config) string
}

// credentialDigest is what files say, as one string: each file's name beside the digest of its
// bytes, with a read failure folded in as its own text so a file that comes and goes is a change
// like any other. A file the configuration does not name is its own distinct mark, so a path
// appearing or being cleared moves the digest.
func credentialDigest(files ...string) string {
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

// drainBound is how long a rebuild waits for what it replaced to let go of the work inside it
// before it ends that thing anyway: the whole of the NATS rebuild's wait for the intake it
// ended to settle its messages (natsHolder.rebuild), and the margin the runtime rebuild adds to
// the bound of the runtime it replaced (runtimeDrainBound). It is longer than the ack wait a
// consumer is built with (`worker_rpc_timeout_seconds`, 5 s by default), so a message that is
// going to settle has its turn, and short enough that an intake that will never settle does not
// hold the settings change for ever: the daemon then closes the old connection and logs that it
// did, where the messages still in flight are redelivered by the durable consumers the new
// connection holds.
//
// It is no bound for a launch and the runtime rebuild does not take it for one. One Spawn or
// Resume try may rightfully run for the whole of the runtime's LaunchBound — the Sandbox
// runtime's is `worker_boot_timeout_seconds` several times over plus its provision bound, which
// is settled when that runtime is built and no caller can raise under it, since that timeout is
// class `rebuild` held by the runtime (config.Classes), so a change of it builds another runtime
// rather than lengthening the one a launch is inside — so that rebuild waits the replaced
// runtime's own bound with this as the margin beyond it (runtimeDrainBound), and only past that
// is the old runtime ended and the call inside it left with a cancelled context.
//
// That wait is taken under the settings holder's lock, which every other settings change takes:
// a rebuild runs either from an apply's observers (config.Live.Apply holds l.mu for the whole
// apply) or from the watcher's reread (config.Live.WhileSettled), so for as long as a rebuild
// drains, another route call and the file watcher's own settle wait behind it. That is the
// trade this daemon takes, and it is taken deliberately: the alternative — closing what was
// replaced from a goroutine of its own — would answer the operator `rebuilt` while the daemon
// still held the connection or the runtime it says it replaced, and would let the next rebuild
// of that holder start while the one before it was still letting go, with two drains of one
// holder in flight and no order between their ends. A drain is rare (nothing is in flight in
// the ordinary case, and the wait returns at once) and bounded, by the replaced runtime's own
// launch bound where that is the longer, which is the one wait that may hold a settings change
// for as long as a launch; a rebuild answering for work it has not finished would be neither.
const drainBound = 2 * time.Minute

// rereadCredentials builds every holder whose credentials have moved since that holder was
// built again, whatever the configuration says: the answer to a rotation of a file whose path
// did not change, which no settings change names, since the path is what the file holds and the
// path has not moved. It runs in the order the holders were registered, and over the holders
// that were built from a credential at all (credentialHolder).
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
// whose credentials read the same holds what they say already, so rebuilding it is work with no
// answer of its own. What each holder was built from is recorded as it is registered (register)
// and at every rebuild that succeeded (rebuilder.rebuild), so the first reread after boot
// rebuilds nothing, a holder a route change has just rebuilt is left alone at the next wake, and
// a rebuild that failed is tried again at the next reread.
func (r *rebuilder) rereadCredentials(cfg *config.Config) {
	for _, name := range r.order {
		holder, reads := r.holders[name].(credentialHolder)
		if !reads || r.credentialDigests[name] == holder.credentials(cfg) {
			continue
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

// credentials is the bearer this client is built with (`dispatch_token_file`, read again by
// every rebuild through readDispatchToken).
func (h *dispatchHolder) credentials(cfg *config.Config) string {
	return credentialDigest(cfg.DispatchTokenFile)
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

// credentials is the bearer this publisher is built with (`envoy_token_file`, read again by
// every rebuild through readEnvoyToken).
func (h *publisherHolder) credentials(cfg *config.Config) string {
	return credentialDigest(cfg.EnvoyTokenFile)
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

// credentials is no file of this host's: an App private key is the key itself, and its command
// and its secret are no file here (buildApps), so what a reread compares is the App credentials
// this manager was built from, digested. The comparison has to be made, because the rebuild
// closes the manager it replaces: every lease that manager granted is dropped and a
// `token_command` still waiting on a person's approval is killed (appauth/tokencommand.go's
// run). The watcher wakes on a whole directory and the kubelet rewrites the projected token in
// one of them, so without this the daemon would do that routinely, for nothing.
func (h *appsHolder) credentials(cfg *config.Config) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%+v", cfg.GitHubApps))
	return hex.EncodeToString(sum[:])
}

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

func (h *appsHolder) Token(ctx context.Context, role appauth.AppRole, repo ghrepo.Repository) (appauth.Lease, error) {
	return h.get().Token(ctx, role, repo)
}

func (h *appsHolder) ReadToken(ctx context.Context, role appauth.AppRole, owner string, repos []string) (appauth.Lease, error) {
	return h.get().ReadToken(ctx, role, owner, repos)
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

// credentials is the operator bearer the server compares against (`operator_token_file`, read
// again by every rebuild through readOperatorToken).
func (h *apiHolder) credentials(cfg *config.Config) string {
	return credentialDigest(cfg.OperatorTokenFile)
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
// lock, and every later read compares with the new one. It takes no other lock, reaches no
// consumer and cannot fail, which is why a daemon with a workflow registers it before the
// intake consumers and has rebuilder.observe build it first: a mapping read between the two
// rebuilds then works out what GitHub intake is to hear under the equivalence the consumers'
// filter is built with, never under one older than it (workflowRuntime.connect, refreshMapping).
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

// credentials are the launch secrets every spec carries and the operator's instructions, which
// a rebuild reads again (readLaunchSecrets, readInstructions).
func (h *specsHolder) credentials(cfg *config.Config) string {
	return credentialDigest(cfg.EnvoyTokenFile, cfg.NatsNkeySeedFile, cfg.InstructionsPath)
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

// keeperHolder is the controller keeper and the controller tick
// (config.HolderControllerKeeper): whether this daemon launches the project's controller itself
// (`controller`) and how often it wakes it (`controller_wake_interval_seconds`). A rebuild
// stores the new interval, which the controller tick reads as it starts its ticker, and starts
// or stops the keeper's own loop when the launch mode changed — the controller's claim is the
// store's either way, so neither start nor stop touches a running controller.
//
// The loop is started here and nowhere else (begin): the boot starts it through this holder
// under `controller: daemon`, and so does a change that moves the mode to the daemon, so there
// is one way the keeper runs and one place that knows whether it is running.
type keeperHolder struct {
	// wake is controller_wake_interval_seconds in nanoseconds, which the controller tick reads
	// as it starts its ticker: the one place the daemon holds it (workflowRuntime.tickController).
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
