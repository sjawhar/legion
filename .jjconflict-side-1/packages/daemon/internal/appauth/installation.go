package appauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Installations answers whether a GitHub App is installed on a repository. The daemon serves a
// repository of its project's mapping only once both Apps are.
type Installations interface {
	InstalledOn(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error)
}

var _ Installations = (*Manager)(nil)

// Logins answers each GitHub App's bot login under the App's own JWT, which needs no installation
// on any repository: the review App's is the login the workflow judges a push's pusher and a
// review's author by, known even while the daemon serves no repository.
type Logins interface {
	AppLogin(ctx context.Context, role AppRole) (string, error)
}

var _ Logins = (*Manager)(nil)

// AppLogin is role's App's bot login, "<slug>[bot]", read from GET /app under the App's JWT.
func (m *Manager) AppLogin(ctx context.Context, role AppRole) (string, error) {
	app, err := m.app(role)
	if err != nil {
		return "", err
	}
	jwt, err := GenerateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return "", err
	}
	slug, err := m.appSlug(ctx, jwt)
	if err != nil {
		return "", fmt.Errorf("read the %s App's login: %w", role, err)
	}
	return slug + "[bot]", nil
}

// InstalledOn reads GET /repos/{owner}/{repo}/installation under role's App JWT: GitHub answers
// the installation that covers the repository, 2xx, or 404 when the App has none there. An owner's
// installation may cover only some of its repositories, so the read is per repository. Any other
// answer is an error naming the repository and the App, a TransientError when it is GitHub's
// trouble.
func (m *Manager) InstalledOn(ctx context.Context, role AppRole, repo ghrepo.Repository) (bool, error) {
	app, err := m.app(role)
	if err != nil {
		return false, err
	}
	jwt, err := GenerateJWT(app.AppID, app.PrivateKey, m.now())
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.baseURL+"/repos/"+url.PathEscape(repo.Owner())+"/"+url.PathEscape(repo.Name())+"/installation", nil)
	if err != nil {
		return false, err
	}
	m.appHeaders(request, jwt)
	response, err := m.client.Do(request)
	if err != nil {
		return false, &TransientError{Err: fmt.Errorf("read whether the %s App is installed on %s: %w", role, repo, err)}
	}
	body := refusalBody(response)
	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		return true, nil
	case response.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, transient(response, body, fmt.Errorf("read whether the %s App is installed on %s failed (%d)", role, repo, response.StatusCode))
	}
}
