// Package appauth mints GitHub installation tokens for Legion's two GitHub Apps.
package appauth

import (
	"bytes"
	"cmp"
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
	// one, GitHubCredential), a controller holds one for every owner it has, and the Kubernetes
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

// writePermissions is the permission set a role's write token is asked for, one per App role:
// what that App's roles do to the one repository the token names, and nothing else. It is what
// the mint asks for of an installation granted all of it; an installation granted less is asked
// for what it is granted (exchangeGranted), since GitHub answers a request naming a permission
// the installation is not granted HTTP 422, minting nothing rather than leaving that one out.
//
// Every entry has its user. workflows:write is a push that touches `.github/workflows`, which
// GitHub refuses from a token without it; contents:write is that push and every other, each role
// pushing its own commits; pull_requests:write is the pull request, its comments and its reviews;
// actions:write is `gh run rerun`, a re-run being a write even though the role only reads what it
// produces; metadata:read is every read of the repository itself, which GitHub requires of any
// token. checks:write and statuses:write are the check runs and commit statuses a tester posts,
// and differ by App: the review App creates check runs, and holds no statuses permission at all,
// while the implement App's installation holds checks:read alone and statuses:write
// (docs/solutions/legion/one-role-keyed-table-decides-which-github-app-acts.md).
//
// Issues, repository projects and packages are in neither set, though both Apps' installations
// hold them: Legion writes no GitHub issue and no project, and a comment on a pull request needs
// Issues write or Pull requests write (GitHub's own documentation of the create-comment endpoint,
// read 2026-11-06), which both sets hold.
var writePermissions = map[AppRole]map[string]string{
	Implement: {"actions": "write", "checks": "read", "contents": "write", "metadata": "read", "pull_requests": "write", "statuses": "write", "workflows": "write"},
	Review:    {"actions": "write", "checks": "write", "contents": "write", "metadata": "read", "pull_requests": "write"},
}

// accessOrder ranks GitHub's three accesses, each holding every one below it: a permission
// granted at write is granted its read, and one granted at read is not granted its write. A
// value outside these three is no access this daemon knows how to ask for, and ranks below all
// of them, so narrowToGranted leaves that permission out rather than ask for a word of GitHub's
// it cannot order.
var accessOrder = map[string]int{"read": 1, "write": 2, "admin": 3}

// grantShortfall is one permission a role's set asks for that the installation is not granted at
// the access asked: the permission, the access the set asks for, and the access the installation
// is granted, empty where it is granted none. It carries no token, and neither does the line it
// is said on (shortfallMessage).
type grantShortfall struct {
	permission string
	asked      string
	granted    string
}

// granting is what the installation is granted of the permission, as a line says it.
func (s grantShortfall) granting() string {
	if s.granted == "" {
		return "no " + s.permission
	}
	return s.permission + ": " + s.granted
}

// minting is what the mint's request carries of the permission: the access granted where that is
// one GitHub mints at, and nothing where the installation is granted none of it.
func (s grantShortfall) minting() string {
	if accessOrder[s.granted] == 0 {
		return "without it"
	}
	return "with " + s.permission + ": " + s.granted
}

// narrowToGranted is asked cut down to granted, and every shortfall between the two in one order
// whatever order a map ranges in: a permission granted at the access asked or above is asked for
// as asked names it, one granted below is asked for at the access granted, and one granted none
// is left out. It copies, since the sets it cuts are shared by every mint.
func narrowToGranted(asked, granted map[string]string) (map[string]string, []grantShortfall) {
	narrowed := make(map[string]string, len(asked))
	var short []grantShortfall
	for _, permission := range slices.Sorted(maps.Keys(asked)) {
		access, held := asked[permission], granted[permission]
		switch {
		case accessOrder[held] == 0:
			short = append(short, grantShortfall{permission: permission, asked: access, granted: held})
		case accessOrder[held] < accessOrder[access]:
			narrowed[permission] = held
			short = append(short, grantShortfall{permission: permission, asked: access, granted: held})
		default:
			narrowed[permission] = access
		}
	}
	return narrowed, short
}

// shortfallMessage is written to the daemon's stderr, once per App role, owner and shortfall,
// when a mint asks for less than the role's set names because the installation is granted less:
// the lease is served, and what the token cannot do is said once rather than at every mint. It
// takes the role, the owner, what the installation is granted (granting), the permission and the
// access asked for, what the request carries of it (minting), and the permission and access to
// grant. It carries no token.
const shortfallMessage = "appauth: the %s App's installation on %s is granted %s, where its token asks for %s: %s, so the token is minted %s; grant the App %s: %s and approve it on that account\n"

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
	commands          map[string]*commandRun
	// keys holds each App private key parsed, by its PEM text (generateJWT).
	keys map[string]*rsa.PrivateKey

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

// saidShortfall is the key one line of shortfallMessage is said under: the App role, the owner
// lowercased as a scope's is, and what the installation is granted of the permission. A second
// mint of any scope of that role and owner says nothing again, since the grant is the
// installation's and the line is about the installation.
type saidShortfall struct {
	role  AppRole
	owner string
	grantShortfall
}

type pendingLease struct {
	done  chan struct{}
	lease Lease
	err   error
}

type pendingIdentity struct {
	done     chan struct{}
	identity GitIdentity
	err      error
}

var _ Tokens = (*Manager)(nil)

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
		commands:          map[string]*commandRun{},
		keys:              map[string]*rsa.PrivateKey{},
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
// readPermissions' own set, narrowed the same way. The role's own owner's lease is unaffected
// either way, since Token asks the command for no narrowing at all. A call that names no
// repository is refused, and nothing is minted.
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
// whatever repositories each of them names: the owner is what a refused token is known by, and a
// caller that met one holds no list of the scopes its role has minted for that owner.
func (m *Manager) Forget(role AppRole, owner string) {
	forgotten := strings.ToLower(strings.TrimSpace(owner))
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.DeleteFunc(m.leases, func(key scope, _ Lease) bool {
		return key.role == role && key.owner == forgotten
	})
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
		if err == nil {
			m.leases[cacheKey] = lease
			maps.DeleteFunc(m.leases, func(_ scope, held Lease) bool { return !held.ExpiresAt.After(minted) })
		}
		waiting.lease, waiting.err = lease, err
		delete(m.pending, cacheKey)
		close(waiting.done)
		m.mu.Unlock()
		return lease, err
	}
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

// exchangeGranted is exchange over what the installation is granted: the request's permissions
// cut down to the access owner's account approved for the App (grantedPermissions,
// narrowToGranted), with a line on stderr for each one left out or lowered (sayShortfalls). A
// permission the installation is not granted is left out, where asking for it would make GitHub
// refuse the mint whole (a 422) and cost the role every other permission of its set, and the gh
// bundle that mint belongs to (GitHubCredential).
//
// A cut that leaves no permission at all refuses the mint instead: GitHub reads a request with
// no permissions as every permission the installation holds, so a body without them would mint
// wider than the role's set asks for rather than narrower. The refusal names the role, the
// owner, the repositories and the set asked for (askedFor), and carries no token.
//
// A 422 from the exchange after this cut is a repository the installation does not reach, or a
// grant that changed between the two calls (ErrNarrowingRefused): it stays the hard refusal it
// is.
func (m *Manager) exchangeGranted(ctx context.Context, jwt, installation string, request mintRequest) (Lease, error) {
	granted, err := m.grantedPermissions(ctx, jwt, installation)
	if err != nil {
		return Lease{}, err
	}
	permissions, short := narrowToGranted(request.permissions, granted)
	m.sayShortfalls(request.role, request.owner, short)
	if len(permissions) == 0 {
		return Lease{}, fmt.Errorf("the %s App's installation on %s is granted none of the permissions its token asks for (%s), and a token request naming no permission holds every permission the installation has",
			request.role, request.owner, askedFor(request.repos, request.permissions))
	}
	return m.exchange(ctx, jwt, installation, request.repos, permissions)
}

// grantedPermissions is what owner's account approved for the App's installation: GET
// /app/installations/{id} under the App's JWT answers the installation, whose permissions object
// maps each permission it is granted to the access it is granted at ("read", "write", "admin"),
// and names none of the ones it is granted nothing of. GitHub's trouble is transient, as every
// other read here is, and a 404 is the installation gone (errInstallationGone), which mint
// answers by looking for the installation once more.
//
// It is read before every exchange and never cached. A mint happens about three times an hour
// per scope, so the read costs little, and an account that approves a grant has it asked for at
// the next mint rather than at the next daemon restart, which a cache no approval invalidates
// would make it wait for.
func (m *Manager) grantedPermissions(ctx context.Context, jwt, installation string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/app/installations/"+url.PathEscape(installation), nil)
	if err != nil {
		return nil, err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return nil, &TransientError{Err: fmt.Errorf("GitHub App installation permissions read: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := refusalBody(response)
		failed := fmt.Errorf("GitHub App installation permissions read failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %w", errInstallationGone, failed)
		}
		return nil, transient(response, body, failed)
	}
	body, err := readBody(response, "GitHub App installation permissions read", responseBound)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("GitHub App installation permissions read returned an invalid response")
	}
	return payload.Permissions, nil
}

// sayShortfalls writes shortfallMessage for each of short the first time a mint of role for
// owner meets it, and nothing on every mint after it, whatever repositories that mint or the
// ones after it named: the grant is the installation's and the line is about the installation.
func (m *Manager) sayShortfalls(role AppRole, owner string, short []grantShortfall) {
	for _, shortfall := range short {
		key := saidShortfall{role: role, owner: strings.ToLower(strings.TrimSpace(owner)), grantShortfall: shortfall}
		m.mu.Lock()
		said := m.saidShortfalls[key]
		m.saidShortfalls[key] = true
		m.mu.Unlock()
		if said {
			continue
		}
		fmt.Fprintf(m.stderr, shortfallMessage, role, owner, shortfall.granting(),
			shortfall.permission, shortfall.asked, shortfall.minting(), shortfall.permission, shortfall.asked)
	}
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

// ErrNarrowingRefused is a token exchange GitHub answers 422 to a request that named
// repositories or permissions: a repository the installation does not reach, or a permission it
// is not granted — GitHub answers both the same way, and mints nothing rather than leaving one
// out. A mint asks only for what the installation was granted a moment before (exchangeGranted),
// so a refusal here is the repositories, and the permissions half of it a grant that changed
// between the two calls.
//
// It is exported because the daemon's provisioning adapter falls back on it (implementTokens in
// internal/daemon): an installation that refuses a read token's narrowing is served the owner's
// own token instead, and no other refusal is.
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

// accessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body: the repositories
// the token reaches and the permissions it holds, each left out when the request narrows nothing
// by it, as the secrets broker's githubAccessTokensBody is.
type accessTokensBody struct {
	Repositories []string          `json:"repositories,omitempty"`
	Permissions  map[string]string `json:"permissions,omitempty"`
}

// mintedToken is GitHub's answer to a token exchange: the token, its expiry, and what GitHub says
// the token reaches, which the mint holds against what it asked for (narrowedTo).
type mintedToken struct {
	Token               string `json:"token"`
	ExpiresAt           string `json:"expires_at"`
	RepositorySelection string `json:"repository_selection"`
	Repositories        []struct {
		Name string `json:"name"`
	} `json:"repositories"`
}

// narrowedTo reports that GitHub's answer describes a token reaching no more than asked names: a
// selection of named repositories, each of them one asked names (GitHub names them in its own
// case). GitHub applies the body and refuses what it cannot grant, so an answer past it means the
// body was dropped or widened between the daemon and GitHub, leaving a lease that reaches every
// repository of the installation; it is refused rather than served, as the secrets broker refuses
// one its narrowing does not hold (heldBy). The refusal names the repositories asked for, never a
// value the answer carries, which for this answer is the token. A request that named no
// repositories is not checked.
func (t mintedToken) narrowedTo(asked []string) error {
	if len(asked) == 0 {
		return nil
	}
	wider := func(because string) error {
		return fmt.Errorf("GitHub App token exchange returned a token wider than the %d repositories asked for (%s): %s",
			len(asked), strings.Join(asked, ", "), because)
	}
	if t.RepositorySelection != "selected" {
		return wider(`its repository selection is not "selected"`)
	}
	if len(t.Repositories) == 0 {
		return wider("it names no repository")
	}
	for _, granted := range t.Repositories {
		if !slices.ContainsFunc(asked, func(name string) bool { return strings.EqualFold(name, granted.Name) }) {
			return wider("it reaches a repository the request did not name")
		}
	}
	return nil
}

// askedFor is what a token request carried, for a refusal to say which half of it GitHub turned
// down: its repositories and its permissions, each named in a fixed order so one refusal reads
// like the next. A request body carries no token, so neither does this.
func askedFor(repos []string, permissions map[string]string) string {
	named := make([]string, 0, len(permissions))
	for _, permission := range slices.Sorted(maps.Keys(permissions)) {
		named = append(named, permission+": "+permissions[permission])
	}
	return fmt.Sprintf("repositories %s; permissions %s",
		cmp.Or(strings.Join(repos, ", "), "none"), cmp.Or(strings.Join(named, ", "), "none"))
}

// exchange asks GitHub for installation's token over repos with permissions, naming each in the
// access_tokens body and sending no body at all when it carries neither. A request that names
// repositories is no proof of a narrowed token, so what GitHub says it minted is held against
// what was asked (narrowedTo) and a wider lease is refused rather than served.
func (m *Manager) exchange(ctx context.Context, jwt, installation string, repos []string, permissions map[string]string) (Lease, error) {
	var requestBody io.Reader
	if len(repos) > 0 || len(permissions) > 0 {
		encoded, err := json.Marshal(accessTokensBody{Repositories: repos, Permissions: permissions})
		if err != nil {
			return Lease{}, err
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/app/installations/"+url.PathEscape(installation)+"/access_tokens", requestBody)
	if err != nil {
		return Lease{}, err
	}
	m.appHeaders(request, jwt)
	request.Header.Set("Accept", "application/vnd.github+v3+json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return Lease{}, &TransientError{Err: fmt.Errorf("GitHub App token exchange: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body := refusalBody(response)
		failed := fmt.Errorf("GitHub App token exchange failed (%d): %s", response.StatusCode, strings.TrimSpace(string(body)))
		if response.StatusCode == http.StatusNotFound {
			return Lease{}, fmt.Errorf("%w: %w", errInstallationGone, failed)
		}
		refused := transient(response, body, failed)
		if response.StatusCode == http.StatusUnprocessableEntity {
			// GitHub documents its rate limits on a 429 and a 403 (githubrest.RateLimited), but
			// the headers or the secondary-limit text of a limit on a 422 are that limit too,
			// and no verdict on what the request asked for: the same request may pass later, so
			// it is GitHub's trouble and never the installation's permissions.
			if githubrest.RateLimitSignals(response, body) {
				return Lease{}, &TransientError{Err: failed}
			}
			// Otherwise a 422 to a request that narrowed the token is the installation reaching
			// fewer repositories than the request asked for, or being granted less than it asked
			// for of a permission — which a mint asks for as the installation was granted it a
			// moment before (exchangeGranted), so that half is a grant changed between the two
			// calls. GitHub answers the two the same way and its text is never quoted as a
			// cause, so the refusal names both halves of what was asked and an operator reads
			// which it was. One to a request that narrowed nothing says nothing about either and
			// stays the refusal it is.
			if requestBody != nil {
				return Lease{}, fmt.Errorf("%w (%s): %w", ErrNarrowingRefused, askedFor(repos, permissions), refused)
			}
		}
		return Lease{}, refused
	}
	body, err := readBody(response, "GitHub App token exchange", responseBound+int64(len(repos))*repositoryAnswerBound)
	if err != nil {
		return Lease{}, err
	}
	var payload mintedToken
	if err := json.Unmarshal(body, &payload); err != nil {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	expiresAt, err := time.Parse(time.RFC3339, payload.ExpiresAt)
	if err != nil || payload.Token == "" {
		return Lease{}, fmt.Errorf("GitHub App token exchange returned an invalid response")
	}
	if err := payload.narrowedTo(repos); err != nil {
		return Lease{}, err
	}
	return Lease{Token: payload.Token, ExpiresAt: expiresAt, Minted: m.now()}, nil
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
