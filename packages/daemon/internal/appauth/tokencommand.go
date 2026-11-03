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
// installation the command may release), and TokenCommandReadOnly, "1" for a read-only token
// (ReadToken) and "" otherwise; an operator's command passes them on (`agent-secrets <NAME>
// ${LEGION_GITHUB_OWNER:+--github-owner "$LEGION_GITHUB_OWNER"}
// ${LEGION_GITHUB_READ_ONLY:+--github-read-only} -- sh -c 'printf %s "$<NAME>"'`). A read-only
// token holds what the command's read-only release gives: the broker's holds contents and
// metadata read and no pull requests read, where key mode's ReadToken also reads pull requests.
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
	// TokenCommandReadOnly is "1" in a token_command's environment when the daemon wants a
	// read-only token (ReadToken), and empty otherwise.
	TokenCommandReadOnly = "LEGION_GITHUB_READ_ONLY"
)

// releaseLimit bounds what a token_command may print: a release of a few installations is a few
// kilobytes.
const releaseLimit = 1 << 20

// releasedInstallation is one installation of a token_command's release.
type releasedInstallation struct {
	owner     string
	token     string
	expiresAt time.Time
}

// tokenCommandField is the configuration key of role's token_command, the name its failures carry.
func tokenCommandField(role AppRole) string {
	return "github_apps." + string(role) + ".token_command"
}

// runTokenCommand runs app's token_command for owner ("" for every installation) and readOnly, and
// returns the installations its release names, keyed by lowercased account login. A command that
// ctx ended is GitHub's or the broker's trouble (TransientError), as a request its deadline cut
// off is; one that exited non-zero, printed too much, or printed no release is the configuration's
// answer.
func (m *Manager) runTokenCommand(ctx context.Context, role AppRole, app config.GitHubApp, owner string, readOnly bool) (map[string]releasedInstallation, error) {
	field := tokenCommandField(role)
	command := exec.CommandContext(ctx, "sh", "-c", app.TokenCommand)
	procgroup.Configure(command)
	readOnlyValue := ""
	if readOnly {
		readOnlyValue = "1"
	}
	command.Env = append(slices.DeleteFunc(runtime.WithoutNATSSeeds(os.Environ()), func(entry string) bool {
		return strings.HasPrefix(entry, TokenCommandOwner+"=") || strings.HasPrefix(entry, TokenCommandReadOnly+"=")
	}), TokenCommandOwner+"="+owner, TokenCommandReadOnly+"="+readOnlyValue)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.MultiWriter(m.stderr, &stderr)
	err := command.Run()
	if ctx.Err() != nil {
		return nil, &TransientError{Err: fmt.Errorf("%s did not finish: %w", field, ctx.Err())}
	}
	if err != nil {
		var exit *exec.ExitError
		message := strings.TrimSpace(stderr.String())
		switch {
		case errors.As(err, &exit) && message != "":
			return nil, fmt.Errorf("%s failed (exit %d): %s", field, exit.ExitCode(), message)
		case errors.As(err, &exit):
			return nil, fmt.Errorf("%s failed (exit %d)", field, exit.ExitCode())
		default:
			return nil, fmt.Errorf("%s could not run: %w", field, err)
		}
	}
	if stdout.Len() > releaseLimit {
		return nil, fmt.Errorf("%s printed more than %d bytes, which no release is", field, releaseLimit)
	}
	return parseRelease(stdout.Bytes(), field, m.now())
}

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
		if owner == "" {
			return nil, fmt.Errorf("%s released an installation with no account login", field)
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

// mintFromCommand is mint in token mode: the command's release for owner, narrowed to read-only
// when readOnly, and the App's bot identity read under the token. A release without owner's
// installation, or a command that fails while the App's release of every installation lacks owner,
// is ErrNotInstalled, so a removed App is an answer about the owner, as in key mode.
func (m *Manager) mintFromCommand(ctx context.Context, role AppRole, app config.GitHubApp, owner string, readOnly bool) (Lease, error) {
	installations, err := m.runTokenCommand(ctx, role, app, owner, readOnly)
	if err != nil {
		var transient *TransientError
		if errors.As(err, &transient) {
			return Lease{}, err
		}
		if every, everyErr := m.runTokenCommand(ctx, role, app, "", false); everyErr == nil {
			if _, ok := every[strings.ToLower(owner)]; !ok {
				return Lease{}, fmt.Errorf("%w: the %s App's %s released no installation on %s", ErrNotInstalled, role, tokenCommandField(role), owner)
			}
		}
		return Lease{}, err
	}
	installation, ok := installations[strings.ToLower(owner)]
	if !ok {
		return Lease{}, fmt.Errorf("%w: the %s App's %s released no installation on %s", ErrNotInstalled, role, tokenCommandField(role), owner)
	}
	identity, err := m.identity(ctx, app.AppID, func(ctx context.Context) (GitIdentity, error) {
		return m.tokenIdentity(ctx, role, app.AppID, installation.token)
	})
	if err != nil {
		return Lease{}, err
	}
	return Lease{Token: installation.token, ExpiresAt: installation.expiresAt, Minted: m.now(), Identity: identity}, nil
}

// anyInstallation is one installation of the release of every installation app's token_command
// gives, the first by account login: AppLogin needs a token of any installation, no owner's.
func (m *Manager) anyInstallation(ctx context.Context, role AppRole, app config.GitHubApp) (releasedInstallation, error) {
	installations, err := m.runTokenCommand(ctx, role, app, "", false)
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
// GraphQL's viewer names the bot login, `<slug>[bot]`; GET /apps/<slug> must answer appID, so a
// command that releases another App's token is refused rather than acted as; and GET
// /users/<login> gives the bot user's id for its noreply email.
func (m *Manager) tokenIdentity(ctx context.Context, role AppRole, appID, token string) (GitIdentity, error) {
	login, err := m.viewerLogin(ctx, role, token)
	if err != nil {
		return GitIdentity{}, err
	}
	slug, ok := strings.CutSuffix(login, "[bot]")
	if !ok || slug == "" {
		return GitIdentity{}, fmt.Errorf("the %s App's %s released a token that acts as %s, not an App's bot", role, tokenCommandField(role), login)
	}
	if err := m.checkAppID(ctx, role, slug, appID, token); err != nil {
		return GitIdentity{}, err
	}
	return m.botIdentity(ctx, login, token)
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
	body, err := readBody(response, "read "+what)
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
	body, err := readBody(response, "read "+what)
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
// of every installation names repo's owner and that installation's repositories
// (GET /installation/repositories, every page) include repo. GET /repos/{owner}/{repo} would not
// do: an installation token reads any public repository, installed or not.
func (m *Manager) installedFromCommand(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	lease, err := m.ReadToken(ctx, role, repo.Owner())
	if errors.Is(err, ErrNotInstalled) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read whether the %s App is installed on %s: %w", role, repo, err)
	}
	what := fmt.Sprintf("read whether the %s App is installed on %s", role, repo)
	for page := 1; ; page++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet,
			m.baseURL+"/installation/repositories?per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil {
			return false, err
		}
		tokenHeaders(request, lease.Token)
		response, err := m.client.Do(request)
		if err != nil {
			return false, &TransientError{Err: fmt.Errorf("%s: %w", what, err)}
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return false, transient(response, refusalBody(response), fmt.Errorf("%s failed (%d)", what, response.StatusCode))
		}
		body, err := readBody(response, what)
		if err != nil {
			return false, err
		}
		var listing struct {
			Repositories []struct {
				FullName string `json:"full_name"`
			} `json:"repositories"`
		}
		if json.Unmarshal(body, &listing) != nil {
			return false, fmt.Errorf("%s: GitHub answered an invalid repository list", what)
		}
		for _, listed := range listing.Repositories {
			if strings.EqualFold(listed.FullName, repo.String()) {
				return true, nil
			}
		}
		if len(listing.Repositories) < 100 {
			return false, nil
		}
	}
}

// tokenHeaders sets an installation token's headers on request.
func tokenHeaders(request *http.Request, token string) {
	request.Header.Set("Authorization", "token "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
}
