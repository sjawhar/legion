package secrets

import "errors"

// MintRequest is one release's input to a minter (GitHubApp.Mint, GoogleLogin.Mint): the secret's
// stored value, and the policy's facts about it a minter needs. GitHubOwners is a github-app
// secret's github-owners tag as lowercase logins; every other kind leaves it nil.
type MintRequest struct {
	Value        string
	GitHubOwners []string
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
