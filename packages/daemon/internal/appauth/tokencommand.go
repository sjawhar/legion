package appauth

// Token mode: an App configured with github_apps.<role>.token_command, for a deployment whose App
// keys only a secrets broker holds. The Manager holds no key and signs no JWT for such an App: it
// runs the command, with `sh -c`, whenever it needs a lease (Token's refreshWindow governs when, as
// in key mode), and reads from its stdout the release the secrets broker's `agent-secrets <NAME>
// -- <cmd>` puts in <cmd>'s environment:
//
//	{"installations":{"<account login>":{"installation_id":<int>,"token":"ghs_…","expires_at":"<RFC3339>"}}}
//
// one entry per installation of the App the release covers. The command's environment names what
// the daemon wants: TokenCommandOwner, the account whose installation token it needs ("" for every
// installation the command may release), TokenCommandRepos, the repositories of that account the
// token may touch, separated by single spaces ("" for a token the daemon narrows to none), and
// TokenCommandReadOnly, "1" for a read-only token (ReadToken) and "" otherwise. An operator's
// command maps each onto the flag `agent-secrets` narrows the release by, one --github-repo per
// repository:
//
//	agent-secrets <NAME> ${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"} ${LEGION_GITHUB_REPOS:+$(printf ' --github-repo %s' $LEGION_GITHUB_REPOS)} ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s "$<NAME>"'
//
// Keeping to that narrowing is the command's, not the daemon's: a key-mode exchange holds GitHub's
// answer against what it asked for (narrowedTo), where a release carries nothing to hold it to, so
// a command that drops the variable hands every role a token over the whole installation. A
// read-only token holds what the command's read-only release gives: the broker's holds contents,
// metadata, pull requests and Actions read, key mode's ReadToken set, less the Actions read an
// installation that is not granted it does not hold. What a release leaves out is the command's
// too: the daemon reads what an installation is granted only where it mints with a key of its
// own (exchangeGranted), and asks a command for no permission at all.
//
// No token reaches a log or an error: the command's stdout is never quoted, and its stderr, which
// `agent-secrets` uses for the names and the request it waits on, goes to the daemon's stderr and
// into a failure's message.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const (
	// TokenCommandOwner names, in a token_command's environment, the account whose installation
	// token the daemon wants, empty for every installation the command may release.
	TokenCommandOwner = "LEGION_GITHUB_OWNER"
	// TokenCommandRepos names, in a token_command's environment, the repositories of
	// TokenCommandOwner's account the token may touch, separated by single spaces, which a
	// GitHub repository name holds none of (ghrepo.Parse). It is empty where the daemon asks
	// for no narrowing at all: the release of every installation AppLogin reads.
	TokenCommandRepos = "LEGION_GITHUB_REPOS"
	// TokenCommandReadOnly is "1" in a token_command's environment when the daemon wants a
	// read-only token (ReadToken), and empty otherwise.
	TokenCommandReadOnly = "LEGION_GITHUB_READ_ONLY"
)

// releaseLimit bounds what a token_command may print: a release of a few installations is a few
// kilobytes.
const releaseLimit = 1 << 20

// tokenCommandLimit bounds one run of a token_command. A run is not its caller's: a caller whose
// own deadline ends first returns ErrCommandPending while the run goes on, and the next caller
// asking the same thing joins it, so a grant that waits on a person's approval is one request,
// never killed and asked again. `agent-secrets` waits 30 minutes for a decision by default, then
// exits 75; the limit leaves it that.
var tokenCommandLimit = 35 * time.Minute

// exitPending is `agent-secrets`' exit status for a request still waiting on a person's approval
// when its wait ended: no answer about the App, so a later run may pass.
const exitPending = 75

// ErrCommandPending is a token_command still running when its caller's deadline ended, wrapped in
// a TransientError: most often a grant that waits on a person's approval. The run goes on, and the
// caller's next attempt joins it.
var ErrCommandPending = errors.New("token_command is still running")

// releasedInstallation is one installation of a token_command's release.
type releasedInstallation struct {
	owner     string
	token     string
	expiresAt time.Time
}

// commandRun is one run of a token_command, which every caller asking for the same owner and
// narrowing while it runs shares.
type commandRun struct {
	done          chan struct{}
	installations map[string]releasedInstallation
	err           error
}

// tokenCommandField is the configuration key of role's token_command, the name its failures carry.
func tokenCommandField(role AppRole) string {
	return "github_apps." + string(role) + ".token_command"
}

// runTokenCommand returns the release of app's token_command for the request's owner ("" for
// every installation), repositories and read, keyed by lowercased account login, from a run that
// outlives ctx (commandRun): ctx ending first returns ErrCommandPending, transient. A release of
// every installation records its accounts as the role's installations (recordOwners), the set a
// narrowed run that fails is read against.
//
// The run is keyed by the request's own scope (leaseScope), the key its lease is cached under:
// the repositories are part of what a lease may do, so a run narrowed to one repository must
// never answer a caller that asked for another, and one encoding of a scope serves both.
func (m *Manager) runTokenCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (map[string]releasedInstallation, error) {
	role, owner := request.role, request.owner
	key := request.scope()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("%s: the token manager is closed", tokenCommandField(role))
	}
	run, ok := m.commands[key]
	if !ok {
		run = &commandRun{done: make(chan struct{})}
		m.commands[key] = run
		m.runs.Add(1)
		go func() {
			defer m.runs.Done()
			bounded, cancel := context.WithTimeout(m.root, tokenCommandLimit)
			defer cancel()
			run.installations, run.err = m.execTokenCommand(bounded, app, request)
			if run.err == nil && owner == "" {
				m.recordOwners(role, run.installations)
			}
			m.mu.Lock()
			close(run.done)
			m.mu.Unlock()
			// A caller whose deadline ended may come back after the run finished: keep the result
			// for finishedRunKeep, or until a caller reads it, so that caller gets it and the
			// command is not run again.
			time.AfterFunc(finishedRunKeep, func() { m.forgetRun(key, run) })
		}()
	}
	m.mu.Unlock()
	select {
	case <-run.done:
		m.forgetRun(key, run)
		return run.installations, run.err
	case <-ctx.Done():
		return nil, &TransientError{Err: fmt.Errorf("%s for %q is still running after %v (a grant may wait on a person's approval): %w", tokenCommandField(role), owner, ctx.Err(), ErrCommandPending)}
	}
}

// finishedRunKeep is how long a finished token_command run's result waits for a caller whose own
// deadline ended before the run did: longer than the boot's longest wait between attempts
// (appMintRetry.Max, 30 s), far shorter than a released token's life.
var finishedRunKeep = 2 * time.Minute

// forgetRun drops run from the shared runs if it is still key's run.
func (m *Manager) forgetRun(key scope, run *commandRun) {
	m.mu.Lock()
	if m.commands[key] == run {
		delete(m.commands, key)
	}
	m.mu.Unlock()
}

// Close ends every token_command still running and waits for each to exit; a later call needing
// one is refused. The closed mark is set under the lock that accepts a run, so every run accepted
// before it is counted before Close waits. The daemon calls it as the workflow stops.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stop()
	m.runs.Wait()
}

// recordOwners records the accounts of a release of every installation as role's installations.
func (m *Manager) recordOwners(role AppRole, installations map[string]releasedInstallation) {
	owners := make(map[string]string, len(installations))
	for owner := range installations {
		owners[owner] = owner
	}
	m.mu.Lock()
	m.installations[role] = owners
	m.mu.Unlock()
}

// execTokenCommand runs app's token_command once, under ctx, for the request's owner,
// repositories and read. A run ctx ended, or that exited 75 (`agent-secrets`' request still
// waiting for approval), is transient; one that exited otherwise, printed too much, or printed
// no release is the configuration's answer. Its stderr reaches the daemon's and the error with
// every GitHub token redacted.
func (m *Manager) execTokenCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (map[string]releasedInstallation, error) {
	field := tokenCommandField(request.role)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(runCtx, "sh", "-c", app.TokenCommand)
	procgroup.Configure(command)
	readOnlyValue := ""
	if request.read {
		readOnlyValue = "1"
	}
	// Each of the three is set, empty where the daemon asks nothing by it, so a value the daemon
	// inherited never reads as one it meant.
	command.Env = append(slices.DeleteFunc(runtime.WithoutNATSSeeds(os.Environ()), func(entry string) bool {
		return strings.HasPrefix(entry, TokenCommandOwner+"=") || strings.HasPrefix(entry, TokenCommandRepos+"=") ||
			strings.HasPrefix(entry, TokenCommandReadOnly+"=")
	}), TokenCommandOwner+"="+request.owner, TokenCommandRepos+"="+strings.Join(request.repos, " "), TokenCommandReadOnly+"="+readOnlyValue)
	stdout := &boundedBuffer{limit: releaseLimit, overflow: cancel}
	var stderr bytes.Buffer
	relay := &redactingLines{out: m.stderr, keep: &stderr, limit: releaseLimit, overflow: cancel}
	command.Stdout = stdout
	command.Stderr = relay
	err := command.Run()
	relay.flush()
	if stdout.overflowed {
		return nil, fmt.Errorf("%s printed more than %d bytes, which no release is", field, releaseLimit)
	}
	if relay.overflowed {
		return nil, fmt.Errorf("%s wrote more than %d bytes to stderr", field, releaseLimit)
	}
	if ctx.Err() != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s did not finish within %s: %w", field, tokenCommandLimit, ctx.Err())}
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("%s could not run: %w", field, err)
		}
		failed := fmt.Errorf("%s failed (exit %d)", field, exit.ExitCode())
		if message := strings.TrimSpace(stderr.String()); message != "" {
			failed = fmt.Errorf("%s failed (exit %d): %s", field, exit.ExitCode(), message)
		}
		if exit.ExitCode() == exitPending {
			return nil, &TransientError{Err: failed}
		}
		return nil, failed
	}
	return parseRelease(stdout.buf.Bytes(), field, m.now())
}

// boundedBuffer keeps at most limit bytes of a command's stdout; the first write past it marks the
// output overflowed and calls overflow, which ends the command, so no broken command fills the
// daemon's memory.
type boundedBuffer struct {
	buf        bytes.Buffer
	limit      int
	overflow   func()
	overflowed bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.overflowed || b.buf.Len()+len(p) > b.limit {
		if !b.overflowed {
			b.overflowed = true
			b.overflow()
		}
		return 0, errors.New("token_command output exceeds its limit")
	}
	return b.buf.Write(p)
}

// tokenPattern matches the GitHub token forms (installation, OAuth, user-to-server, refresh,
// personal, fine-grained personal), which a command's stderr may carry and no log or error may.
var tokenPattern = regexp.MustCompile(`(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})`)

// redactingLines writes each whole line of a command's stderr to out, and to keep for the failure's
// message, with every GitHub token in it replaced, so `agent-secrets`' line naming a pending request
// reaches the daemon's log as it is written while a token never does; flush writes a last line with
// no newline. Past limit bytes in all it marks the stream overflowed, calls overflow, which ends the
// command, and relays nothing more.
type redactingLines struct {
	out        io.Writer
	keep       *bytes.Buffer
	limit      int
	overflow   func()
	overflowed bool
	seen       int
	pending    []byte
}

func (r *redactingLines) Write(p []byte) (int, error) {
	if r.overflowed {
		return 0, errors.New("token_command stderr exceeds its limit")
	}
	r.seen += len(p)
	if r.seen > r.limit {
		r.overflowed, r.pending = true, nil
		r.overflow()
		return 0, errors.New("token_command stderr exceeds its limit")
	}
	r.pending = append(r.pending, p...)
	for {
		end := bytes.IndexByte(r.pending, '\n')
		if end < 0 {
			break
		}
		r.emit(r.pending[:end+1])
		r.pending = r.pending[end+1:]
	}
	return len(p), nil
}

func (r *redactingLines) emit(line []byte) {
	clean := tokenPattern.ReplaceAll(line, []byte("[redacted]"))
	_, _ = r.out.Write(clean)
	r.keep.Write(clean)
}

func (r *redactingLines) flush() {
	if len(r.pending) > 0 && !r.overflowed {
		r.emit(r.pending)
	}
	r.pending = nil
}

// loginPattern is a GitHub account login: what a release may name an installation by, and the only
// owner text a refusal of a release quotes.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// parseRelease reads a token_command's stdout as the broker's release. Every refusal names the
// field and the account and never quotes the output, which holds tokens.
func parseRelease(output []byte, field string, now time.Time) (map[string]releasedInstallation, error) {
	var release struct {
		Installations map[string]struct {
			InstallationID json.RawMessage `json:"installation_id"`
			Token          string          `json:"token"`
			ExpiresAt      string          `json:"expires_at"`
		} `json:"installations"`
	}
	// encoding/json's syntax errors quote the offending input, which holds tokens: the error is
	// dropped and named.
	if json.Unmarshal(bytes.TrimSpace(output), &release) != nil || release.Installations == nil {
		return nil, fmt.Errorf(`%s printed no release (want {"installations":{"<account>":{"installation_id":…,"token":…,"expires_at":…}}})`, field)
	}
	installations := make(map[string]releasedInstallation, len(release.Installations))
	for owner, entry := range release.Installations {
		if !loginPattern.MatchString(owner) {
			return nil, fmt.Errorf("%s released an installation whose account is not a GitHub login", field)
		}
		if _, err := installationID(entry.InstallationID); err != nil {
			return nil, fmt.Errorf("%s released %s's installation with no installation id", field, owner)
		}
		expiresAt, err := time.Parse(time.RFC3339, entry.ExpiresAt)
		if err != nil || entry.Token == "" {
			return nil, fmt.Errorf("%s released %s's installation without a token and an RFC 3339 expires_at", field, owner)
		}
		if !expiresAt.After(now) {
			return nil, fmt.Errorf("%s released %s's installation with a token that expired at %s", field, owner, expiresAt.Format(time.RFC3339))
		}
		installations[strings.ToLower(owner)] = releasedInstallation{owner: owner, token: entry.Token, expiresAt: expiresAt}
	}
	return installations, nil
}

// brokerNoInstallation is the secrets broker's refusal of a release for an account the App has no
// installation on (packages/envoy/internal/broker/secrets/githubapp.go, GitHubApp.Mint), which
// `agent-secrets` prints on stderr and runTokenCommand's error carries.
const brokerNoInstallation = "has no installation on"

// brokerNarrowingRefused is the secrets broker's refusal of a narrowing GitHub answered 422
// (packages/envoy/internal/broker/secrets/minterr.go, ErrMintNarrowingRefused), carried the same
// way: a repository the request names that the installation does not reach, which is the key
// mode's ErrNarrowingRefused by another road. The broker's text never quotes GitHub's own and
// never a token.
const brokerNarrowingRefused = "the narrowing cannot be served"

// mintFromCommand is mint in token mode: the command's release for the request's owner, narrowed
// to the repositories it names and to read-only where it is the read token, and the token
// checked to be the configured App's on every release (tokenIdentity, since a later release
// could carry another App's token). A release without owner's installation is ErrNotInstalled,
// and so is a run that failed with the broker's refusal for an account with no installation, or
// failed for an owner the last release of every installation did not name: no second, wider
// release is asked for to decide it, so a read-only request never mints a write token, and one
// narrowed to a repository never mints a token over the installation.
//
// A run the broker refused for the narrowing itself (brokerNarrowingRefused) is
// ErrNarrowingRefused, the refusal a key-mode exchange answers a 422 with: the installation does
// not reach a repository the request named, and a caller that can act on that reads it the same
// way in either mode (ErrNarrowingRefused).
func (m *Manager) mintFromCommand(ctx context.Context, app config.GitHubApp, request mintRequest) (Lease, error) {
	role, owner := request.role, request.owner
	notInstalled := fmt.Errorf("%w: the %s App's %s released no installation on %s", ErrNotInstalled, role, tokenCommandField(role), owner)
	installations, err := m.runTokenCommand(ctx, app, request)
	if err != nil {
		var transient *TransientError
		switch {
		case errors.As(err, &transient):
			return Lease{}, err
		case strings.Contains(err.Error(), brokerNoInstallation):
			return Lease{}, fmt.Errorf("%w: %w", notInstalled, err)
		case strings.Contains(err.Error(), brokerNarrowingRefused):
			return Lease{}, fmt.Errorf("%w: the %s App's %s released no token for %s over %s: %w",
				ErrNarrowingRefused, role, tokenCommandField(role), owner, strings.Join(request.repos, ", "), err)
		}
		m.mu.Lock()
		known, recorded := m.installations[role]
		_, listed := known[strings.ToLower(owner)]
		m.mu.Unlock()
		if recorded && !listed {
			return Lease{}, fmt.Errorf("%w: %w", notInstalled, err)
		}
		return Lease{}, err
	}
	installation, ok := installations[strings.ToLower(owner)]
	if !ok {
		return Lease{}, notInstalled
	}
	identity, err := m.tokenIdentity(ctx, role, app.AppID, installation.token)
	if err != nil {
		return Lease{}, err
	}
	return Lease{Token: installation.token, ExpiresAt: installation.expiresAt, Minted: m.now(), Identity: identity}, nil
}

// anyInstallation is one installation of the release of every installation app's token_command
// gives, the first by account login: AppLogin needs a token of any installation, no owner's and
// no repository's.
//
// It is the one release the daemon asks for that names no repository, and it cannot be narrowed:
// `agent-secrets` refuses --github-repo and --github-read-only without --github-owner, since a
// narrowing names one account's token (its githubNarrowing), and this run knows no account — the
// release is what lists the installations themselves, and the login it is read for is read before
// any repository or owner of the project is known. What it is used for is read-only: the GraphQL
// viewer read that names the App's bot login and the GET /apps/<slug> that checks the App's id
// (tokenIdentity), and the token is never cached as a lease nor handed to a role.
func (m *Manager) anyInstallation(ctx context.Context, role AppRole, app config.GitHubApp) (releasedInstallation, error) {
	installations, err := m.runTokenCommand(ctx, app, mintRequest{role: role})
	if err != nil {
		return releasedInstallation{}, err
	}
	if len(installations) == 0 {
		return releasedInstallation{}, fmt.Errorf("%s released no installation of the %s App", tokenCommandField(role), role)
	}
	owners := make([]string, 0, len(installations))
	for owner := range installations {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	return installations[owners[0]], nil
}

// tokenIdentity is the bot identity of the App whose installation token token is, with no JWT:
// verifyTokenApp checks the token is appID's App's and names its bot login, run for every token,
// and GET /users/<login> gives the bot user's id for its noreply email, read once per App.
func (m *Manager) tokenIdentity(ctx context.Context, role AppRole, appID, token string) (GitIdentity, error) {
	login, err := m.verifyTokenApp(ctx, role, appID, token)
	if err != nil {
		return GitIdentity{}, err
	}
	identity, err := m.identity(ctx, appID, func(ctx context.Context) (GitIdentity, error) {
		return m.botIdentity(ctx, login, token)
	})
	if err != nil {
		return GitIdentity{}, err
	}
	if identity.Name != login {
		return GitIdentity{}, fmt.Errorf("the %s App's %s released a token that acts as %s, where the App acted as %s", role, tokenCommandField(role), login, identity.Name)
	}
	return identity, nil
}

// verifyTokenApp returns the bot login token acts as, refusing a token that is not the App appID's:
// GraphQL's viewer names the bot login, `<slug>[bot]`, and GET /apps/<slug> must answer appID, so a
// command that releases another App's token is refused rather than acted as.
func (m *Manager) verifyTokenApp(ctx context.Context, role AppRole, appID, token string) (string, error) {
	login, err := m.viewerLogin(ctx, role, token)
	if err != nil {
		return "", err
	}
	slug, ok := strings.CutSuffix(login, "[bot]")
	if !ok || slug == "" {
		return "", fmt.Errorf("the %s App's %s released a token that acts as %s, not an App's bot", role, tokenCommandField(role), login)
	}
	if err := m.checkAppID(ctx, role, slug, appID, token); err != nil {
		return "", err
	}
	return login, nil
}

// viewerLogin is the login GitHub's GraphQL viewer answers for token.
func (m *Manager) viewerLogin(ctx context.Context, role AppRole, token string) (string, error) {
	what := fmt.Sprintf("the %s App's bot login (GraphQL viewer)", role)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/graphql", strings.NewReader(`{"query":"{viewer{login}}"}`))
	if err != nil {
		return "", err
	}
	tokenHeaders(request, token)
	request.Header.Set("Content-Type", "application/json")
	response, err := m.client.Do(request)
	if err != nil {
		return "", &TransientError{Err: fmt.Errorf("read %s: %w", what, err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", transient(response, refusalBody(response), fmt.Errorf("read %s failed (%d)", what, response.StatusCode))
	}
	body, err := readBody(response, "read "+what, responseBound)
	if err != nil {
		return "", err
	}
	var answer struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Data.Viewer.Login == "" {
		return "", fmt.Errorf("read %s: GitHub answered no login", what)
	}
	return answer.Data.Viewer.Login, nil
}

// checkAppID reads GET /apps/<slug> and refuses an App whose id is not appID.
func (m *Manager) checkAppID(ctx context.Context, role AppRole, slug, appID, token string) error {
	what := fmt.Sprintf("the %s App %s (GET /apps/%s)", role, slug, slug)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/apps/"+url.PathEscape(slug), nil)
	if err != nil {
		return err
	}
	tokenHeaders(request, token)
	response, err := m.client.Do(request)
	if err != nil {
		return &TransientError{Err: fmt.Errorf("read %s: %w", what, err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return transient(response, refusalBody(response), fmt.Errorf("read %s failed (%d)", what, response.StatusCode))
	}
	body, err := readBody(response, "read "+what, responseBound)
	if err != nil {
		return err
	}
	var app struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &app) != nil {
		return fmt.Errorf("read %s: GitHub answered no App id", what)
	}
	id, err := installationID(app.ID)
	if err != nil {
		return fmt.Errorf("read %s: GitHub answered no App id", what)
	}
	if id != appID {
		return fmt.Errorf("the %s App's %s released a token of App %s (%s), not of the configured app_id %s", role, tokenCommandField(role), id, slug, appID)
	}
	return nil
}

// installedFromCommand is InstalledOn in token mode: the App is installed on repo when the release
// for repo's owner names an installation whose repositories (GET /installation/repositories, every
// page) include repo. GET /repos/{owner}/{repo} would not do: an installation token reads any
// public repository, installed or not. A cached lease GitHub answers 401 (an App uninstalled while
// its token lived) is dropped and minted once again, so an uninstall reads as not installed rather
// than an error until the lease expires.
func (m *Manager) installedFromCommand(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	for attempt := 0; ; attempt++ {
		installed, refused, err := m.listInstallation(ctx, role, repo)
		if !refused || attempt > 0 {
			return installed, err
		}
		m.Forget(role, repo.Owner())
	}
}

// listInstallation reads whether repo is among the repositories of its owner's installation under
// a read lease over that one repository; refused reports a 401, a token GitHub no longer honours.
//
// A lease refused for the narrowing (ErrNarrowingRefused) is that repository not installed: the
// read names repo alone, so the only repository the refusal can be about is repo itself — the
// App's installation on the owner does not reach it. Reading it as an error instead would leave
// the mapping's check recording the repository unread and running the command again on every
// backoff, where the answer is already in: the repository is refused, as it was before a lease
// named its repositories.
//
// The read is that answer's one source, so it drops whatever the Manager remembers of that
// scope's refusal first (ForgetNarrowingRefusal) and runs the command: the mapping's check is
// made precisely when an operator has changed something — the App's installation above all —
// and a read served from the five minutes a refusal is remembered for would tell that operator
// the App is not installed on a repository it was just installed on (lease,
// narrowingRefusalWindow, INFRA-513). A refusal the read itself gets is remembered as any
// mint's is: it is GitHub's answer of this moment, which the bundles refreshing behind the
// check would otherwise ask for again at once, and the next mapping read drops it again as this
// one did.
func (m *Manager) listInstallation(ctx context.Context, role AppRole, repo ghrepo.Repository) (installed, refused bool, err error) {
	m.ForgetNarrowingRefusal(role, repo.Owner(), []string{repo.Name()})
	lease, err := m.ReadToken(ctx, role, repo.Owner(), []string{repo.Name()})
	if errors.Is(err, ErrNotInstalled) || NarrowingRefused(err) {
		return false, false, nil
	}
	what := fmt.Sprintf("read whether the %s App is installed on %s", role, repo)
	if err != nil {
		return false, false, fmt.Errorf("%s: %w", what, err)
	}
	for page := 1; ; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			m.baseURL+"/installation/repositories?per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil {
			return false, false, err
		}
		tokenHeaders(request, lease.Token)
		response, err := m.client.Do(request)
		if err != nil {
			return false, false, &TransientError{Err: fmt.Errorf("%s: %w", what, err)}
		}
		if response.StatusCode == http.StatusUnauthorized {
			refusalBody(response)
			return false, true, fmt.Errorf("%s failed (401)", what)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return false, false, transient(response, refusalBody(response), fmt.Errorf("%s failed (%d)", what, response.StatusCode))
		}
		body, err := readBody(response, what, responseBound)
		if err != nil {
			return false, false, err
		}
		var listing struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return false, false, fmt.Errorf("%s: GitHub answered an invalid repository list", what)
		}
		for _, listed := range listing.Repositories {
			if strings.EqualFold(listed.FullName, repo.String()) {
				return true, false, nil
			}
		}
		if len(listing.Repositories) < 100 {
			return false, false, nil
		}
	}
}

// tokenHeaders sets an installation token's headers on request.
func tokenHeaders(request *http.Request, token string) {
	request.Header.Set("Authorization", "token "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
