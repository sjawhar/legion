package appauth

// The permission cut: what a role's token asks GitHub for, held against what the owner's account
// has approved for the App's installation. GitHub answers a request naming a permission the
// installation is not granted HTTP 422 and mints nothing, so a mint reads the grant first
// (grantedPermissions), asks for what the role's set and the grant have in common
// (narrowToGranted) and says once what the token therefore cannot do (sayShortfalls).

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

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
// token. checks and statuses are the check runs and commit statuses a tester posts, and differ by
// App: the review App's set asks for checks:write, its reviewer creating the check runs, and
// names no statuses permission at all, where the implement App's asks for checks:read, what its
// roles read of those check runs, and statuses:write, the commit status its own tester posts
// (docs/solutions/legion/one-role-keyed-table-decides-which-github-app-acts.md).
//
// The set says what the App's roles use, never what an installation holds: what an account has
// approved is the account's, and the cut takes each mint's request down to it (exchangeGranted),
// so an installation granted less than a set names is asked for less rather than refused.
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

// saidShortfall is the key one line of shortfallMessage is said under: the App role, the owner
// lowercased as a scope's is, and what the installation is granted of the permission. A second
// mint of any scope of that role and owner says nothing again, since the grant is the
// installation's and the line is about the installation.
type saidShortfall struct {
	role  AppRole
	owner string
	grantShortfall
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
