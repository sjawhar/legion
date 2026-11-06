package appauth

// The token exchange: POST /app/installations/{id}/access_tokens, the one call that mints a
// lease in key mode. Every request carries a body naming the repositories the token reaches and
// the permissions it holds, and the answer is held against both before a lease is served
// (narrowedTo).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// accessTokensBody is GitHub's POST /app/installations/{id}/access_tokens body: the repositories
// the token reaches and the permissions it holds, both of which every mint names, as the secrets
// broker's githubAccessTokensBody does for a narrowed release.
type accessTokensBody struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// mintedToken is GitHub's answer to a token exchange: the token, its expiry, and what GitHub says
// the token reaches and holds, which the mint holds against what it asked for (narrowedTo).
type mintedToken struct {
	Token               string            `json:"token"`
	ExpiresAt           string            `json:"expires_at"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	Repositories        []struct {
		Name string `json:"name"`
	} `json:"repositories"`
}

// narrowedTo reports that GitHub's answer describes a token reaching no more than asked names and
// holding no more than permissions does: a selection of named repositories, each of them one
// asked names (GitHub names them in its own case), and no permission the request did not ask for
// nor one above the access it asked at. GitHub applies the body and refuses what it cannot grant,
// so an answer past it means the body was dropped or widened between the daemon and GitHub,
// leaving a lease wider than the one asked for; it is refused rather than served, as the secrets
// broker refuses one its narrowing does not hold (heldBy). The refusal names what was asked for
// and the permission the answer named, never a value the answer carries that a request did not,
// which for this answer is the token.
//
// An answer naming no permission at all is refused as one naming no repository is: GitHub names
// every permission a token holds, and the token it mints for a request whose permissions were
// dropped on the way holds every permission the installation is granted — the owner-wide token
// this package exists to stop — which no later read of the answer could tell from a narrowed
// one.
//
// An access the answer names that this daemon cannot order (accessOrder) is refused too: it
// cannot be held to be no wider than the one asked for, and an answer naming it is no answer
// GitHub makes to a request that asked for a word of its own.
func (t mintedToken) narrowedTo(asked []string, permissions map[string]string) error {
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
	if len(t.Permissions) == 0 {
		return fmt.Errorf("GitHub App token exchange returned a token wider than the permissions asked for (%s): it names no permission",
			askedFor(asked, permissions))
	}
	for _, permission := range slices.Sorted(maps.Keys(t.Permissions)) {
		access := t.Permissions[permission]
		if rank := accessOrder[access]; rank == 0 || rank > accessOrder[permissions[permission]] {
			return fmt.Errorf("GitHub App token exchange returned a token wider than the permissions asked for (%s): it holds %s: %s",
				askedFor(asked, permissions), permission, access)
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
	return fmt.Sprintf("repositories %s; permissions %s", strings.Join(repos, ", "), strings.Join(named, ", "))
}

// exchange asks GitHub for installation's token over repos with permissions, naming both in the
// access_tokens body, which every request carries: a token names the repositories it reaches and
// the permissions it holds, and GitHub reads a request that names neither as the whole
// installation at every permission it is granted, which is the owner-wide token no caller here
// asks for. A request that names repositories is no proof of a narrowed token, so what GitHub
// says it minted is held against what was asked (narrowedTo) and a wider lease is refused rather
// than served.
func (m *Manager) exchange(ctx context.Context, jwt, installation string, repos []string, permissions map[string]string) (Lease, error) {
	encoded, err := json.Marshal(accessTokensBody{Repositories: repos, Permissions: permissions})
	if err != nil {
		return Lease{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/app/installations/"+url.PathEscape(installation)+"/access_tokens", bytes.NewReader(encoded))
	if err != nil {
		return Lease{}, err
	}
	m.appHeaders(request, jwt)
	request.Header.Set("Accept", "application/vnd.github+v3+json")
	request.Header.Set("Content-Type", "application/json")
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
			// Every other 422 is the narrowing this request carried: the installation reaching
			// fewer repositories than the request asked for, or being granted less than it asked
			// for of a permission — which a mint asks for as the installation was granted it a
			// moment before (exchangeGranted), so that half is a grant changed between the two
			// calls. GitHub answers the two the same way and its text is never quoted as a
			// cause, so the refusal names both halves of what was asked and an operator reads
			// which it was.
			return Lease{}, fmt.Errorf("%w (%s): %w", ErrNarrowingRefused, askedFor(repos, permissions), refused)
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
	if err := payload.narrowedTo(repos, permissions); err != nil {
		return Lease{}, err
	}
	return Lease{Token: payload.Token, ExpiresAt: expiresAt, Minted: m.now()}, nil
}
