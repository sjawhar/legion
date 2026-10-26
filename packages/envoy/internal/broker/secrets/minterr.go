package secrets

import "errors"

// MintRequest is one release's input to a minter (GitHubApp.Mint, GoogleLogin.Mint): the secret's
// stored value, and the policy's facts about it a minter needs. GitHubOwners is a github-app
// secret's github-owners tag as lowercase logins; every other kind leaves it nil. GitHubNarrowing
// is the narrowing a github-app release asked for, nil for the owner-wide tokens; every other kind
// leaves it nil.
type MintRequest struct {
	Value           string
	GitHubOwners    []string
	GitHubNarrowing *GitHubNarrowing
}

// GitHubNarrowing narrows one github-app release to the installation of one account and, within
// it, to named repositories of that account, to read access, or to both: GitHub's
// POST /app/installations/{id}/access_tokens body. It can only narrow what the release would give
// without it: Owner must be an account the secret's github-owners lists, and the only permissions
// it names are contents: read and metadata: read, which GitHub refuses past what the installation
// holds.
type GitHubNarrowing struct {
	// Owner is the lowercase login of the one account whose installation the release mints for.
	Owner string
	// Repositories are names of Owner's repositories, without the owner, sorted and distinct: the
	// token reaches those alone. Empty leaves the token every repository the installation reaches.
	Repositories []string
	// ReadOnly limits the token to contents: read and metadata: read.
	ReadOnly bool
}

// The classes of a minter's failure (GitHubApp.Mint, GoogleLogin.Mint). Every error a minter
// returns wraps exactly one, so the broker's log names the class an operator acts on without
// reading the message, which never holds the stored value or a token.
var (
	// ErrMintStoredValue: the secret's stored value cannot be minted from (it does not decode, its
	// key does not parse, it names no App id). Its owner stores a new value.
	ErrMintStoredValue = errors.New("the stored value cannot be minted from")
	// ErrMintRefused: GitHub or Google answered and refused (a 4xx, a Google login that needs a new
	// sign-in, an App with no installation). Its owner fixes the App or signs in again.
	ErrMintRefused = errors.New("the upstream refused the mint")
	// ErrMintUpstream: GitHub or Google could not be asked or answered no usable reply (the network,
	// a 5xx, a body past its cap or one that does not decode). The broker's operator looks at
	// egress and the upstream's status.
	ErrMintUpstream = errors.New("the upstream could not be asked")
)

// MintErrorClass names the class err wraps, for the broker's log line: "stored-value",
// "refused", "upstream", or "unknown" for an error that wraps none.
func MintErrorClass(err error) string {
	switch {
	case errors.Is(err, ErrMintStoredValue):
		return "stored-value"
	case errors.Is(err, ErrMintRefused):
		return "refused"
	case errors.Is(err, ErrMintUpstream):
		return "upstream"
	default:
		return "unknown"
	}
}
