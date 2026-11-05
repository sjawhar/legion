package secrets

import "errors"

// MintRequest is one release's input to a minter: the secret's stored value, its name and
// whether the policy snapshot checked for this release holds it as human tier. The Azure
// minter requires that name to match its configured secret and HumanTier to be true.
// GitHubOwners is a github-app secret's github-owners tag as lowercase logins; every other
// kind leaves it nil. GitHubNarrowing is the narrowing a release asked for, nil for the
// owner-wide tokens; only the github-app minter reads it.
type MintRequest struct {
	Name            string
	HumanTier       bool
	Value           string
	GitHubOwners    []string
	GitHubNarrowing *GitHubNarrowing
}

// GitHubNarrowing narrows one github-app release to the installation of one account and, within
// it, to named repositories of that account, to read access, or to both: GitHub's
// POST /app/installations/{id}/access_tokens body. It can only narrow what the release would give
// without it: Owner must be an account the secret's github-owners lists, and the only permissions
// it names are contents: read, metadata: read, pull_requests: read and actions: read, which
// GitHub refuses past what the installation holds; an installation that does not hold actions
// gets the other three (githubapp.go's optionalReadOnlyPermission).
// policy.ParseGitHubNarrowing builds it from a caller's input.
type GitHubNarrowing struct {
	// Owner is the lowercase login of the one account whose installation the release mints for.
	Owner string
	// Repositories are names of Owner's repositories, without the owner, sorted and distinct: the
	// token reaches those repositories alone, but keeps every permission the App holds, its
	// organization permissions among them, unless ReadOnly is set too, since GitHub gives a token
	// whose body names no permissions all of the App's. Empty leaves the token every repository
	// the installation reaches.
	Repositories []string
	// ReadOnly limits the token to contents: read, metadata: read, pull_requests: read and
	// actions: read, less the Actions grant an installation does not hold. A machine that must
	// only read needs it, with or without Repositories.
	ReadOnly bool
}

// MaxNarrowedRepositories is the most repositories a GitHubNarrowing names. GitHub's
// access_tokens body takes 500, but its answer carries a full repository object for each one,
// about 5 KB, and the minter reads that answer to a cap sized for this many
// (accessTokensResponseLimit): a token GitHub minted whose answer the minter cannot read whole
// cannot be released or revoked.
const MaxNarrowedRepositories = 100

// The classes of a minter's failure. Every error a minter returns wraps exactly one, so the
// broker's log names the class an operator acts on without reading the message, which never
// holds the stored value or a token.
var (
	// ErrMintStoredValue: the secret's stored value cannot be minted from (it does not decode, its
	// key does not parse, it names no App id). Its owner stores a new value.
	ErrMintStoredValue = errors.New("the stored value cannot be minted from")
	// ErrMintRefused: the minter refuses the release (an Azure login that is not the configured
	// human-tier secret), or GitHub or Google refused (a 4xx other than GitHub's 422 to a narrowed
	// token, a Google login that needs a new sign-in, an App with no installation on a listed
	// account). A refusal of the narrowing a release asked for is ErrMintNarrowingRefused.
	ErrMintRefused = errors.New("the upstream refused the mint")
	// ErrMintNarrowingRefused: the narrowing the release asked for cannot be served: it names an
	// account the secret's github-owners tag does not list, which requests.Machine.Values refuses
	// before any minter runs and GitHubApp.Mint refuses again as a second barrier, or GitHub
	// answered the narrowed token request 422, which it does for a repository the installation
	// does not reach (a misspelled name among them) or a permission the App does not hold. The
	// caller fixes the narrowing; neither the App nor the secret is at fault.
	ErrMintNarrowingRefused = errors.New("the narrowing cannot be served")
	// ErrMintUpstream: GitHub or Google could not be asked or answered no usable reply (the network,
	// a 5xx, a rate limit, a body past its cap or one that does not decode, a token GitHub describes
	// as wider than the narrowing asked, which GitHubApp.Mint revokes at GitHub before it refuses),
	// or the broker's Azure projected token is unreadable, malformed or expired.
	// The broker's operator looks at the projected volume, egress and the upstream's status.
	ErrMintUpstream = errors.New("the upstream could not be asked")
)

// MintErrorClass names the class err wraps, for the broker's log line: "stored-value",
// "refused", "narrowing", "upstream", or "unknown" for an error that wraps none.
func MintErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrMintStoredValue):
		return "stored-value"
	case errors.Is(err, ErrMintNarrowingRefused):
		return "narrowing"
	case errors.Is(err, ErrMintRefused):
		return "refused"
	case errors.Is(err, ErrMintUpstream):
		return "upstream"
	default:
		return "unknown"
	}
}
