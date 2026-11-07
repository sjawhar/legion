// Package appauth mints GitHub installation tokens for Legion's two GitHub Apps.
package appauth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// refreshWindow is how long before a lease's expiry Token stops serving it from the cache and
// mints a fresh one. The daemon rewrites each role's gh token file from the cached lease, so a
// container sees a new token only after that rewrite reaches it, and a Kubernetes Secret volume
// reaches a running pod within about the kubelet's one-minute sync period. Twenty minutes leaves
// a container the new token at least some fifteen minutes before the old one expires, while GitHub
// keeps the old token valid until its own expiry, so a `gh` or `git` that started on the old one
// finishes on it.
const refreshWindow = 20 * time.Minute

// narrowingRefusalWindow is how long a Manager serves a narrowing GitHub already refused
// (ErrNarrowingRefused) from its own memory, asking GitHub nothing (lease). The refusal is
// GitHub's definitive answer about which repositories the installation reaches, and nothing the
// daemon does changes it: only an account adding the repository to the installation does, which
// the next ask past this window finds. Without the memory every refresh of every role of every
// live pod asks the refused scope again, and each of its repositories again under the probe a
// refusal starts (dirEntry), each exchange preceded by an uncached grant read — for one refused
// repository of an issue pod, six role bundles times two exchanges a minute — and a rate limit
// then fails every bundle of the project, the write leases included. Five minutes keeps that at
// one exchange per scope per five minutes, while leaving an account's fix to reach the daemon
// well inside the hour an installation token lasts.
const narrowingRefusalWindow = 5 * time.Minute

// AppRole names the GitHub App a Legion role acts as.
type AppRole string

const (
	Implement AppRole = "implement"
	Review    AppRole = "review"
)

// Roles is every App role, the Apps a Legion deployment configures one of each of.
var Roles = []AppRole{Implement, Review}

// GitIdentity is the bot identity that jj and git must use for work a role performs.
type GitIdentity = runtime.GitIdentity

// Lease is one installation-token lease, including its expiry, when it was minted and the App bot
// identity.
type Lease struct {
	Token     string
	ExpiresAt time.Time
	// Minted is when GitHub issued the token: one moments old may not be accepted yet.
	Minted   time.Time
	Identity GitIdentity
}

// TransientError is a GitHub answer that says nothing about the App or its installation, so the
// same request may pass when it is made again: no answer at all (a transport failure, including a
// request or a body read cut off by its deadline), a 5xx, or a rate limit (a 429, or a 403 that
// says it is one). Every other failure is GitHub's definitive answer, or one about the
// configuration, and asking again changes nothing.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// transient marks a response, whose body is body, as GitHub's trouble rather than an answer: a
// 5xx, or a rate limit (githubrest.RateLimited, which reads the body for a secondary limit GitHub
// names there alone).
func transient(response *http.Response, body []byte, err error) error {
	if response.StatusCode >= http.StatusInternalServerError || githubrest.RateLimited(response, body) {
		return &TransientError{Err: err}
	}
	return err
}

// refusalBody is the body GitHub sent with a refusal, read whole, and the response closed. One
// that cannot be read is empty: the status and the headers still say what the answer is.
func refusalBody(response *http.Response) []byte {
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return body
}

// responseBound caps what a mint reads of a GitHub answer that names no repository. GitHub has
// already acted by the time the answer arrives, so the bound is far above any answer it makes
// rather than a limit the daemon expects to meet.
const responseBound = 1 << 20

// repositoryAnswerBound is what a token exchange adds to responseBound for each repository it
// named, the answer carrying one repository object for each: the secrets broker measured those
// objects at 4.9 to 5.3 KB and bounds each at 64 KB on that measurement
// (packages/envoy/internal/broker/secrets/githubapp.go, githubRepositoryAnswerBound).
const repositoryAnswerBound = 64 << 10

// readBody reads a successful response's body whole, up to limit: a read that fails is a
// transport failure, and transient, where what the body then fails to parse as is GitHub's
// answer. A body past limit is no answer GitHub makes, and is refused naming the bound rather
// than parsed, so a body that never ends cannot take the daemon's memory with it; the refusal
// names the bound and never what the body held, which for a token exchange is the token.
func readBody(response *http.Response, what string, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	response.Body.Close()
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s: read the response: %w", what, err)}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: the response is longer than the %d bytes an answer holds", what, limit)
	}
	return body, nil
}

// Tokens mints a GitHub installation token for an App, narrowed to the repositories it names.
type Tokens interface {
	// Token is the write token for repo alone: the App role's installation token on repo's owner,
	// naming that one repository and the write permissions the App's roles use
	// (writePermissions). A zero Repository names none and is refused before anything is minted,
	// since a token naming no repository reaches every repository its owner's installation holds.
	Token(ctx context.Context, role AppRole, repo ghrepo.Repository) (Lease, error)
	// ReadToken is the App's installation token for owner, narrowed to repos — repository names
	// alone, as GitHub takes them inside one installation — and to reading: contents, pull
	// requests, Actions and metadata read. A pane holds one for every owner of the project but
	// the one its issue's repository is under (a configured rename's two owners count as that
	// one, GitHubCredential), one more for each other repository that owner serves, each over
	// that repository alone, a controller holds one for every owner it has, and the Kubernetes
	// runtime's provisioning token is one of the implement App for the issue's own owner over
	// the one repository its pod clones, which every issue pod's workspace-fetch and every
	// repository cache clones with (implementTokens, internal/daemon); there alone an
	// installation that refuses the narrowing is served the owner's own token instead
	// (ErrNarrowingRefused). An App on token_command gets what its command's read-only release
	// holds instead, which the command decides: the secrets broker's is the same four (its
	// readOnlyPermissions). With a key of its own the mint asks for what the installation is
	// granted of those four and leaves the rest out (exchangeGranted), so an installation whose
	// account has not granted the App Actions gets the other three rather than nothing. A call
	// naming no repository is refused before anything is minted, as Token's is.
	ReadToken(ctx context.Context, role AppRole, owner string, repos []string) (Lease, error)
}

// readPermissions narrows an installation token to reading a repository's contents, its pull
// requests, its Actions runs and its metadata: what a role needs of every repository of the
// project but its own issue's, what a controller needs of every one of them, and what the
// provisioning token takes of the issue's own repository, which a clone, a fetch and a
// repository cache only read (implementTokens, internal/daemon). A mint asks for what the
// installation is granted of this set, at the access it is granted (exchangeGranted), and
// GitHub mints a token with the permissions the request names.
//
// actions:read is here because reading a repository includes reading what its merges did: a
// deployment check of a merge reads the push run that carries it, that run's jobs and a job's
// log, and a role that cannot read them cannot tell a deploy that applied from one that was
// skipped. Measured on 2026-11-04 against a private repository whose installation holds
// actions:write: unnarrowed, actions/runs and actions/runs/{id}/jobs answered 200 and
// actions/jobs/{id}/logs answered 302 to a signed URL that answered 200; narrowed to contents,
// pull requests and metadata, all three answered 403 "Resource not accessible by integration".
// The narrowing was the whole refusal, and no App
// or installation setting was missing. It stays a read: actions:read cannot cancel, re-run or
// dispatch a workflow, and reaches no Actions secret or variable, which are permissions of their
// own.
//
// The set is the most a read lease holds, over the repositories the request names (ReadToken's
// repos) and not its installation's whole breadth: exchange sends those names beside these
// permissions, so a read lease reaches no repository its caller did not ask for, and a
// permission the installation is granted none of is left out of the request rather than minted.
var readPermissions = map[string]string{"contents": "read", "pull_requests": "read", "actions": "read", "metadata": "read"}

// Options makes a token manager testable. The zero value uses api.github.com, http.DefaultClient,
// the wall clock, and the daemon's own stderr for what a token_command writes there.
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
	Stderr     io.Writer
}

// Manager caches installation tokens by App and owner, and bot identities by App id. An App
// configured with a token_command is in token mode (tokencommand.go): the Manager holds no key and
// signs no JWT for it, and every lease comes from the command's release.
type Manager struct {
	apps    config.GitHubApps
	baseURL string
	client  *http.Client
	now     func() time.Time
	stderr  io.Writer

	mu                sync.Mutex
	leases            map[scope]Lease
	pending           map[scope]*pendingLease
	installations     map[AppRole]map[string]string
	identities        map[string]GitIdentity
	pendingIdentities map[string]*pendingIdentity
	commands          map[scope]*commandRun
	// keys holds each App private key parsed, by its PEM text (generateJWT).
	keys map[string]*rsa.PrivateKey

	// refusals holds, per scope, the narrowing GitHub refused and when it did (lease,
	// narrowingRefusalWindow): while the window holds, that scope is answered from here and
	// GitHub is asked nothing. A mint of the scope that succeeds drops its entry, as does a
	// Forget of its owner.
	refusals map[scope]refusedNarrowingAt

	// saidShortfalls holds every shortfall already said on stderr, each by the App role, the
	// owner whose installation is granted less than the role's set asks for, and the shortfall
	// itself (shortfallMessage).
	saidShortfalls map[saidShortfall]bool

	// root ends every token_command run on Close (stop), runs counts the runs still going, and
	// closed, set under mu, refuses a run after Close.
	root   context.Context
	stop   context.CancelFunc
	runs   sync.WaitGroup
	closed bool
}

type pendingLease struct {
	done  chan struct{}
	lease Lease
	err   error
}

// refusedNarrowingAt is one remembered narrowing refusal: GitHub's own refusal, served again to
// every caller of the scope while the window holds, and the Manager's clock reading when it was
// met.
type refusedNarrowingAt struct {
	err error
	at  time.Time
}

type pendingIdentity struct {
	done     chan struct{}
	identity GitIdentity
	err      error
}

// A Manager answers every interface a caller reaches it through, the optional ones included: a
// bundle takes the two Forgetters by type assertion (dirEntry), so a method renamed without its
// interface would leave the Manager silently un-forgotten rather than fail the build.
var (
	_ Tokens             = (*Manager)(nil)
	_ Forgetter          = (*Manager)(nil)
	_ RefusalForgetter   = (*Manager)(nil)
	_ ReadScopeForgetter = (*Manager)(nil)
)

// New makes an App token manager. Config.Load resolves command and secret key sources before
// constructing this manager; callers using LoadForValidation must resolve them themselves.
func New(apps config.GitHubApps, options Options) *Manager {
	baseURL := strings.TrimRight(options.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	root, stop := context.WithCancel(context.Background())
	return &Manager{
		root:              root,
		stop:              stop,
		apps:              apps,
		baseURL:           baseURL,
		client:            client,
		now:               now,
		stderr:            stderr,
		leases:            map[scope]Lease{},
		pending:           map[scope]*pendingLease{},
		installations:     map[AppRole]map[string]string{},
		identities:        map[string]GitIdentity{},
		pendingIdentities: map[string]*pendingIdentity{},
		commands:          map[scope]*commandRun{},
		keys:              map[string]*rsa.PrivateKey{},
		refusals:          map[scope]refusedNarrowingAt{},
		saidShortfalls:    map[saidShortfall]bool{},
	}
}

// Token returns the cached write lease for repo until fewer than twenty minutes remain
// (refreshWindow), otherwise minting a fresh installation token that names repo and no other
// repository, with the App role's write permissions (writePermissions). Concurrent requests for
// the same App, owner and repository share that one exchange. A zero Repository names no
// repository, which a token must: it is refused, and nothing is minted.
func (m *Manager) Token(ctx context.Context, role AppRole, repo ghrepo.Repository) (Lease, error) {
	if repo.IsZero() {
		return Lease{}, fmt.Errorf("the %s App's token names no repository to act on", role)
	}
	return m.lease(ctx, mintRequest{role: role, owner: repo.Owner(), repos: []string{repo.Name()}, permissions: writePermissions[role]})
}

// ReadToken is Token for a token narrowed to repos on owner and to readPermissions, cached under
// a scope of its own, so an owner's write and read tokens never stand in for each other. Each of
// those four permissions is asked for at what the installation is granted of it, and one it is
// granted none of is left out (exchangeGranted). In token mode it is the command's read-only
// release (TokenCommandReadOnly) instead, which the command decides; the secrets broker's is
// readPermissions' own set, narrowed the same way. A write request of the same owner is a run of
// its own either way: it carries its one repository as TokenCommandRepos and asks for no
// read-only release, so the two never stand in for each other. A call that names no repository
// is refused, and nothing is minted.
func (m *Manager) ReadToken(ctx context.Context, role AppRole, owner string, repos []string) (Lease, error) {
	if len(repos) == 0 {
		return Lease{}, fmt.Errorf("the %s App's read-only token for %s names no repository to read", role, owner)
	}
	return m.lease(ctx, mintRequest{role: role, owner: owner, repos: repos, read: true, permissions: readPermissions})
}

// Forgetter drops the leases a token source holds for an App role and owner, so the next one is
// minted: a token GitHub refused (401) is not served again from the cache.
type Forgetter interface {
	Forget(role AppRole, owner string)
}

// Forget drops every lease cached for role and owner, the write tokens' and the read tokens',
// whatever repositories each of them names, and every narrowing refusal remembered for them
// (refusals): the owner is what a refused token is known by, and a caller that met one holds no
// list of the scopes its role has minted for that owner, so the next ask of any of them reaches
// GitHub.
func (m *Manager) Forget(role AppRole, owner string) {
	forgotten := strings.ToLower(strings.TrimSpace(owner))
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.leases, func(key scope, _ Lease) bool {
		return key.role == role && key.owner == forgotten
	})
	maps.DeleteFunc(m.refusals, func(key scope, _ refusedNarrowingAt) bool {
		return key.role == role && key.owner == forgotten
	})
}

// RefusalForgetter drops the narrowing refusal a token source remembers for one read scope, so
// the next ask of that scope reaches GitHub rather than the memory
// (Manager.ForgetNarrowingRefusal). It is optional, as Forgetter is: a caller reaches it through
// a type assertion on its Tokens (dirEntry), so a test fake and a harness that implement Tokens
// alone keep working — a source that remembers no refusal has none to drop.
type RefusalForgetter interface {
	ForgetNarrowingRefusal(role AppRole, owner string, repos []string)
}

// ForgetNarrowingRefusal drops the narrowing refusal remembered for role's read lease over repos
// on owner (lease, narrowingRefusalWindow), and nothing else: no lease, and no other scope's
// refusal. It is for the caller that knows that one memory is stale — a directory's lease asked
// for again over the repositories that have just answered one at a time (dirEntry), the
// mapping's own installation read (listInstallation) — where Forget, which drops every lease and
// every refusal of the owner, would throw away tokens that are good and make every scope of that
// owner mint again.
//
// The scope is the one the ask itself is cached under: the repositories as a token request
// carries them (askedRepositories), which a blank name makes no scope of, and nothing is
// remembered under one.
func (m *Manager) ForgetNarrowingRefusal(role AppRole, owner string, repos []string) {
	asked, err := askedRepositories(role, owner, repos)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refusals, leaseScope(true, role, owner, asked))
}

// ReadScopeForgetter drops what a token source holds for one read scope — the lease it cached
// and the narrowing refusal it remembers, both — so the next ask of that scope is GitHub's
// answer of this moment and not an answer of the hour before it
// (Manager.ForgetReadScope). It is optional, as Forgetter and RefusalForgetter are: a caller
// reaches it through a type assertion on its Tokens (dirEntry), so a test fake and a harness
// that implement Tokens alone keep working — a source that caches nothing has nothing to drop,
// and every ask it is made is GitHub's answer already.
type ReadScopeForgetter interface {
	ForgetReadScope(role AppRole, owner string, repos []string)
}

// ForgetReadScope drops the lease cached for role's read lease over repos on owner and the
// narrowing refusal remembered for it, and nothing else: no write lease, and no other read
// scope. It is for the caller whose question is not what a token it holds may do but which
// repositories the installation reaches now, which a cached lease cannot answer — the probes a
// directory's lease is narrowed by, read as the installation's membership, when the ask over
// the repositories they said answered is refused all the same (dirEntry, INFRA-513). Forget
// would throw away every good token of the owner and ForgetNarrowingRefusal alone would leave
// the probe answered from the cache, which is the answer that misled it.
//
// The scope is the one the ask itself is cached under: the repositories as a token request
// carries them (askedRepositories), which a blank name makes no scope of, so nothing is held
// under one.
func (m *Manager) ForgetReadScope(role AppRole, owner string, repos []string) {
	asked, err := askedRepositories(role, owner, repos)
	if err != nil {
		return
	}
	key := leaseScope(true, role, owner, asked)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leases, key)
	delete(m.refusals, key)
}

// scope is one lease's identity, and the key every lease is cached, awaited and warned under:
// which kind of token it is, the App role it is minted as, the owner whose installation mints it
// and the repositories it reaches. The repositories are part of it because they are part of what
// the lease may do: a token for one repository must never be served to a caller that asked for
// another. They are held as one string so a scope compares as a value: lowercased and sorted, and
// joined by a comma, which no GitHub repository name holds (ghrepo.Parse).
type scope struct {
	read  bool
	role  AppRole
	owner string
	repos string
}

// leaseScope is the scope of role's lease for repos on owner, read or write. Its repositories are
// compared lowercased, as GitHub matches them inside one installation, so two callers that name
// one repository differently cased share the one lease rather than mint twice.
func leaseScope(read bool, role AppRole, owner string, repos []string) scope {
	lowered := make([]string, len(repos))
	for i, repo := range repos {
		lowered[i] = strings.ToLower(repo)
	}
	slices.Sort(lowered)
	return scope{read: read, role: role, owner: strings.ToLower(strings.TrimSpace(owner)), repos: strings.Join(slices.Compact(lowered), ",")}
}

// askedRepositories is names as a token request carries them: trimmed, sorted so one request's
// body reads like the next's, and each one named once. The names keep the case the caller wrote,
// which GitHub matches without regard to case inside an installation. A blank name asks for no
// repository and is refused: it would otherwise reach GitHub as a name nothing is served under.
func askedRepositories(role AppRole, owner string, names []string) ([]string, error) {
	asked := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return nil, fmt.Errorf("the %s App's token for %s names a repository with no name", role, owner)
		}
		asked = append(asked, trimmed)
	}
	slices.Sort(asked)
	return slices.Compact(asked), nil
}

// mintRequest is one ask for a lease, built by Token and ReadToken and carried whole through the
// mint: the App role the token is minted as, the owner whose installation mints it, the
// repositories it reaches, whether it is the read token, and the permissions GitHub is asked
// for. lease settles the repositories as a token request carries them (askedRepositories) before
// anything else reads them, so the request body, the cache key and the token-command run key all
// name the one set.
type mintRequest struct {
	role        AppRole
	owner       string
	repos       []string
	read        bool
	permissions map[string]string
}

// scope is the key the request's lease is cached and awaited under (leaseScope).
func (r mintRequest) scope() scope { return leaseScope(r.read, r.role, r.owner, r.repos) }

// lease serves the cached lease of the request's scope while more than refreshWindow remains,
// and otherwise mints one with the permissions the request names, sharing one exchange among
// concurrent callers. A caller that waited on another's mint, which ended only because that
// caller's own deadline did (a context error, or ErrCommandPending), asks again under its own
// context rather than take that caller's failure as its own.
//
// A narrowing GitHub refused is remembered per scope for narrowingRefusalWindow and served from
// that memory while it holds, GitHub asked nothing: it is GitHub's definitive answer about the
// repositories the installation reaches, where every refresh of every role of every live pod
// would otherwise ask it again. Only ErrNarrowingRefused is remembered — GitHub's trouble
// (TransientError) and every other failure keep asking as they always did — and a mint of the
// scope that succeeds drops what was remembered of it.
//
// The memory answers the ask that would repeat a refusal, never one that already knows better.
// A caller holding what the window cannot know — the mapping's installation read, whose whole
// business is to find the answer changed (listInstallation), and a directory's lease asked for
// again over the repositories that answered alone (dirEntry) — drops that one scope's memory
// before it asks (ForgetNarrowingRefusal), so what it is served is GitHub's answer now.
//
// What a cached answer is worth is worth saying, since a caller reads those asks as which
// repositories the installation reaches: a lease served from here says a token naming that scope
// was minted within the hour, not that GitHub reaches it now. An installation that drops a
// repository leaves every lease of it cached and good-looking until its expiry nears
// (refreshWindow), which is why a caller that reads a cached answer as membership and is then
// refused the ask over what it read drops those scopes whole — the lease with the refusal — and
// asks again (ForgetReadScope, dirEntry, INFRA-513).
//
// A mint that succeeds also drops every lease the cache holds that has expired. The cache is
// keyed by the repositories a lease reaches, so a mapping that changes over a long-lived
// daemon's life leaves entries no caller asks for again, and an expired one can never be served:
// a lease is served only while more than refreshWindow remains.
func (m *Manager) lease(ctx context.Context, request mintRequest) (Lease, error) {
	request.owner = strings.TrimSpace(request.owner)
	if request.owner == "" {
		return Lease{}, fmt.Errorf("GitHub App owner must not be empty")
	}
	asked, err := askedRepositories(request.role, request.owner, request.repos)
	if err != nil {
		return Lease{}, err
	}
	// Every door into a mint refuses a call that names no repository before it reaches here
	// (Token, ReadToken), so nothing today makes this guard answer. It stays: it is the one
	// place every mint passes through, and a token naming no repository reaches every
	// repository its owner's installation holds, which no caller may be served by accident.
	if len(asked) == 0 {
		return Lease{}, fmt.Errorf("the %s App's token for %s names no repository to reach", request.role, request.owner)
	}
	request.repos = asked
	cacheKey := request.scope()
	for {
		now := m.now()
		m.mu.Lock()
		if lease, ok := m.leases[cacheKey]; ok && lease.ExpiresAt.Sub(now) > refreshWindow {
			m.mu.Unlock()
			return lease, nil
		}
		if refused, ok := m.refusals[cacheKey]; ok {
			if now.Sub(refused.at) < narrowingRefusalWindow {
				m.mu.Unlock()
				return Lease{}, refused.err
			}
			delete(m.refusals, cacheKey)
		}
		if waiting, ok := m.pending[cacheKey]; ok {
			m.mu.Unlock()
			select {
			case <-waiting.done:
				if callerEnded(waiting.err) && ctx.Err() == nil {
					continue
				}
				return waiting.lease, waiting.err
			case <-ctx.Done():
				return Lease{}, ctx.Err()
			}
		}
		waiting := &pendingLease{done: make(chan struct{})}
		m.pending[cacheKey] = waiting
		m.mu.Unlock()

		lease, err := m.mint(ctx, request)
		minted := m.now()
		m.mu.Lock()
		switch {
		case err == nil:
			m.leases[cacheKey] = lease
			delete(m.refusals, cacheKey)
			maps.DeleteFunc(m.leases, func(_ scope, held Lease) bool { return !held.ExpiresAt.After(minted) })
		case NarrowingRefused(err):
			m.refusals[cacheKey] = refusedNarrowingAt{err: err, at: minted}
		}
		waiting.lease, waiting.err = lease, err
		delete(m.pending, cacheKey)
		close(waiting.done)
		m.mu.Unlock()
		return lease, err
	}
}

// NarrowingRefused reports GitHub refusing the narrowing a request named, and not GitHub's own
// trouble, which the same request may pass through a moment later (TransientError). An error
// that carries both is the second. No mint builds one today: the exchange returns a rate-limited
// 422 as a TransientError alone (exchange.go), and token mode returns its trouble before it
// builds the refusal (mintFromCommand), so excluding it here is a guard for a later producer. It
// is the one predicate every caller that acts on a refusal reads — a Manager deciding what to
// remember (refusals), a bundle deciding which repositories a directory holds (credential.go's
// dirEntry), the mapping's installation read (listInstallation), the provisioning adapter's
// fallback (internal/daemon's implementTokens) and the controller route's answer (internal/api)
// — so that none of them reads a rate limit as an answer about what an installation reaches.
func NarrowingRefused(err error) bool {
	var trouble *TransientError
	return errors.Is(err, ErrNarrowingRefused) && !errors.As(err, &trouble)
}

// callerEnded reports a failure that says only that the calling context ended: the context's own
// errors, or a token_command still running past it (ErrCommandPending).
func callerEnded(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCommandPending)
}

// mint mints the request's installation token. An installation the daemon found that answers the
// permissions read or the exchange 404 was removed since (errInstallationGone): it is forgotten
// and looked for once more, so a removed App answers ErrNotInstalled and an App installed there
// again mints on its new installation. A configured installation answering 404 is not installed
// either.
func (m *Manager) mint(ctx context.Context, request mintRequest) (Lease, error) {
	app, err := m.app(request.role)
	if err != nil {
		return Lease{}, err
	}
	if app.TokenCommand != "" {
		return m.mintFromCommand(ctx, app, request)
	}
	installation, err := m.installation(ctx, request.role, app, request.owner)
	if err != nil {
		return Lease{}, err
	}
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return Lease{}, err
	}
	lease, err := m.exchangeGranted(ctx, jwt, installation, request)
	if errors.Is(err, errInstallationGone) && configuredInstallation(app, request.owner) == "" {
		m.forgetInstallation(request.role, request.owner)
		if installation, err = m.installation(ctx, request.role, app, request.owner); err != nil {
			return Lease{}, err
		}
		lease, err = m.exchangeGranted(ctx, jwt, installation, request)
	}
	if errors.Is(err, errInstallationGone) {
		m.forgetInstallation(request.role, request.owner)
		return Lease{}, fmt.Errorf("%w: %s's installation %s on %s answers %w", ErrNotInstalled, request.role, installation, request.owner, err)
	}
	if err != nil {
		return Lease{}, err
	}
	identity, err := m.identity(ctx, app.AppID, func(ctx context.Context) (GitIdentity, error) {
		return m.fetchIdentity(ctx, jwt, lease.Token)
	})
	if err != nil {
		return Lease{}, err
	}
	lease.Identity = identity
	return lease, nil
}

func (m *Manager) app(role AppRole) (config.GitHubApp, error) {
	switch role {
	case Implement:
		return m.apps.Implement, nil
	case Review:
		return m.apps.Review, nil
	default:
		return config.GitHubApp{}, fmt.Errorf("role_not_configured: %s", role)
	}
}

func (m *Manager) installation(ctx context.Context, role AppRole, app config.GitHubApp, owner string) (string, error) {
	ownerKey := strings.ToLower(owner)
	if installation := configuredInstallation(app, owner); installation != "" {
		return installation, nil
	}
	m.mu.Lock()
	if installations := m.installations[role]; installations != nil {
		if installation := installations[ownerKey]; installation != "" {
			m.mu.Unlock()
			return installation, nil
		}
	}
	m.mu.Unlock()

	found, err := m.discoverInstallations(ctx, app)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.installations[role] == nil {
		m.installations[role] = map[string]string{}
	}
	for discoveredOwner, installation := range found {
		m.installations[role][strings.ToLower(discoveredOwner)] = installation
	}
	installation := m.installations[role][ownerKey]
	m.mu.Unlock()
	if installation == "" {
		return "", fmt.Errorf("%w: %s not installed on %s", ErrNotInstalled, role, owner)
	}
	return installation, nil
}

// configuredInstallation is the installation app's configuration names for owner, if any.
func configuredInstallation(app config.GitHubApp, owner string) string {
	for configuredOwner, installation := range app.Installations {
		if strings.EqualFold(configuredOwner, owner) && installation != "" {
			return installation
		}
	}
	return ""
}

// forgetInstallation drops the installation found for role on owner, for the next mint to look
// for it again.
func (m *Manager) forgetInstallation(role AppRole, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.installations[role], strings.ToLower(owner))
}

// errInstallationGone is a permissions read or a token exchange GitHub answers 404: the
// installation is no longer there.
var errInstallationGone = errors.New("the installation is gone")

// ErrNarrowingRefused is a token exchange GitHub answers 422, every one of them that is not a
// rate limit: a repository the installation does not reach, or a permission it is not granted —
// GitHub answers both the same way, and mints nothing rather than leaving one out. Every request
// names its repositories and its permissions (exchange), and a mint asks only for what the
// installation was granted a moment before (exchangeGranted), so a refusal here is the
// repositories, and the permissions half of it a grant that changed between the two calls. In
// token mode it is the secrets broker refusing the same narrowing (brokerNarrowingRefused).
//
// It is exported because its callers read it rather than fail on it: the daemon's provisioning
// adapter falls back on it (implementTokens in internal/daemon), where an installation that
// refuses a read token's narrowing is served the owner's own token instead; a role's gh bundle
// drops the repositories it names from a read lease and keeps the rest (mintBundle), and
// refuses a bundle whose own default owner it leaves out (bundleDefault), which the API's
// controller route answers as a refusal of its own (internal/api's controllerGitHubCredential);
// and a read of one repository refused is that repository not installed (listInstallation). The
// Manager remembers it per scope and serves it without asking GitHub again while the window
// holds (lease, narrowingRefusalWindow), which it does for no other refusal. No other refusal is
// read any of these ways.
var ErrNarrowingRefused = errors.New("the installation does not reach the repositories or hold the permissions asked for")

// ErrNotInstalled is a mint refused because the App has no installation on the owner: an answer
// about the owner, not GitHub's trouble, which a retry would only repeat.
var ErrNotInstalled = errors.New("github_app_not_installed")

func (m *Manager) discoverInstallations(ctx context.Context, app config.GitHubApp) (map[string]string, error) {
	jwt, err := m.generateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return nil, err
	}
	installations := map[string]string{}
	for page := 1; ; page++ {
		endpoint, err := url.Parse(m.baseURL + "/app/installations")
		if err != nil {
			return nil, fmt.Errorf("GitHub App installation URL: %w", err)
		}
		query := endpoint.Query()
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		m.appHeaders(request, jwt)
		response, err := m.client.Do(request)
		if err != nil {
			return nil, &TransientError{Err: fmt.Errorf("GitHub App installation discovery: %w", err)}
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return nil, transient(response, refusalBody(response), fmt.Errorf("GitHub App installation discovery failed (%d)", response.StatusCode))
		}
		body, err := readBody(response, "GitHub App installation discovery", responseBound)
		if err != nil {
			return nil, err
		}
		var payload []struct {
			ID      json.RawMessage `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("GitHub App installation discovery returned an invalid response")
		}
		for _, installation := range payload {
			id, err := installationID(installation.ID)
			if err != nil || installation.Account.Login == "" {
				return nil, fmt.Errorf("GitHub App installation discovery returned an invalid installation account")
			}
			installations[installation.Account.Login] = id
		}
		if len(payload) < 100 {
			return installations, nil
		}
	}
}

func installationID(raw json.RawMessage) (string, error) {
	var stringID string
	if err := json.Unmarshal(raw, &stringID); err == nil && stringID != "" {
		return stringID, nil
	}
	var numberID int64
	if err := json.Unmarshal(raw, &numberID); err == nil && numberID > 0 {
		return strconv.FormatInt(numberID, 10), nil
	}
	return "", fmt.Errorf("invalid installation id")
}

// identity is the bot identity of the App appID, read once by fetch and cached; concurrent callers
// share the one read.
func (m *Manager) identity(ctx context.Context, appID string, fetch func(context.Context) (GitIdentity, error)) (GitIdentity, error) {
	m.mu.Lock()
	if identity, ok := m.identities[appID]; ok {
		m.mu.Unlock()
		return identity, nil
	}
	if waiting, ok := m.pendingIdentities[appID]; ok {
		m.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.identity, waiting.err
		case <-ctx.Done():
			return GitIdentity{}, ctx.Err()
		}
	}
	waiting := &pendingIdentity{done: make(chan struct{})}
	m.pendingIdentities[appID] = waiting
	m.mu.Unlock()

	identity, err := fetch(ctx)
	m.mu.Lock()
	if err == nil {
		m.identities[appID] = identity
	}
	waiting.identity, waiting.err = identity, err
	delete(m.pendingIdentities, appID)
	close(waiting.done)
	m.mu.Unlock()
	return identity, err
}

func (m *Manager) fetchIdentity(ctx context.Context, jwt, installationToken string) (GitIdentity, error) {
	slug, err := m.appSlug(ctx, jwt)
	if err != nil {
		return GitIdentity{}, err
	}
	return m.botIdentity(ctx, slug+"[bot]", installationToken)
}

// botIdentity is the git identity of the App bot login, its noreply email naming the bot user's
// id, read from GET /users/<login> under installationToken.
func (m *Manager) botIdentity(ctx context.Context, login, installationToken string) (GitIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return GitIdentity{}, err
	}
	request.Header.Set("Authorization", "token "+installationToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
	response, err := m.client.Do(request)
	if err != nil {
		return GitIdentity{}, &TransientError{Err: fmt.Errorf("GitHub App bot identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return GitIdentity{}, transient(response, refusalBody(response), fmt.Errorf("GitHub App bot identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App bot identity lookup", responseBound)
	if err != nil {
		return GitIdentity{}, err
	}
	var bot struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &bot); err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	id, err := installationID(bot.ID)
	if err != nil {
		return GitIdentity{}, fmt.Errorf("GitHub App bot identity lookup returned an invalid bot user id")
	}
	return GitIdentity{
		Name:  login,
		Email: id + "+" + login + "@users.noreply.github.com",
	}, nil
}

// appSlug is the slug of the App jwt signs for, read from GET /app: its bot login is the slug's,
// "<slug>[bot]".
func (m *Manager) appSlug(ctx context.Context, jwt string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app", nil)
	if err != nil {
		return "", err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return "", &TransientError{Err: fmt.Errorf("GitHub App identity lookup: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", transient(response, refusalBody(response), fmt.Errorf("GitHub App identity lookup failed (%d)", response.StatusCode))
	}
	body, err := readBody(response, "GitHub App identity lookup", responseBound)
	if err != nil {
		return "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(body, &app); err != nil || app.Slug == "" {
		return "", fmt.Errorf("GitHub App identity lookup returned an invalid app slug")
	}
	return app.Slug, nil
}

func (m *Manager) appHeaders(request *http.Request, jwt string) {
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
