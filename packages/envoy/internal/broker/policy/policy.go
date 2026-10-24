// Package policy decides who may have an agent secret from two tags on the secret itself in the
// secret store (AWS Secrets Manager's tags, or Google Secret Manager's annotations): owner
// (shared, a person's email, or a registered service) and tier (agent or human). Loader reads every secret under the namespace prefix into a Set, refusing by name each one
// whose tags or key the policy cannot serve, or reads one name alone (LoadOne); Current keeps the
// latest Set, rereads it on a ticker, and merges a single name's reread into it (RefreshOne);
// Set.Evaluate answers one requester's ask for one name.
//
// Who may ask follows from owner and tier alone:
//
//   - a person's agent-tier secret goes to that person's sessions without asking, and any other
//     session's or pod's request is an approval request to the person;
//   - a shared agent-tier secret goes to any session or pod;
//   - a human-tier secret needs its owner's approval, or anyone's if it is shared;
//   - a service's secret goes only to that service's own sessions, and anyone else's request is
//     refused;
//   - a secret a person withheld from a session, by revoking a grant of it the session got
//     without asking, is human tier to that session: it asks its owner, or anyone for a shared
//     secret, and a service's own secret is refused.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// The tags a namespace secret carries, and their fixed values.
const (
	// TagOwner names who owns the secret: OwnerShared, a person's lowercase email, or a registered
	// service's name.
	TagOwner = "owner"
	// TagTier is TierAgent or TierHuman.
	TagTier = "tier"
	// TagKind selects the token type a secret mints on release. An absent or empty tag leaves the
	// secret's stored value unchanged.
	TagKind = "kind"
	// KindGitHubApp mints one installation token for every installation of the stored GitHub App
	// on an account its TagGitHubOwners lists.
	KindGitHubApp = "github-app"
	// KindGoogleLogin exchanges the stored authorized-user refresh token for an access token.
	KindGoogleLogin = "google-login"
	// TagMaxGrant bounds a request's signed grant lifetime for this secret (ParseMaxGrant).
	TagMaxGrant = "max-grant"
	// TagGitHubOwners lists the GitHub accounts a github-app secret mints installation tokens for
	// (ParseGitHubOwners). A github-app secret without it is refused, since a public App can be
	// installed by any account, and an installation on an account it does not list gets no token.
	TagGitHubOwners = "github-owners"
	// KindSourcePrefix opens the Source of a kind secret (KindSource). The kind and store source
	// are frozen into a request, so changing or clearing the kind ends every live grant.
	KindSourcePrefix = "mint:"
	// TagRollbackGuard and RollbackGuardValue are the tag `agent-secrets secret` writes beside every
	// kind, and the value it writes. This broker never reads the tag. Brokers from
	// legion-envoy-v13.19.0 to the release before kinds read it as the service account a secret
	// mints for, and refuse a value that is not a service account's email (mint-target-malformed),
	// so a rollback to one of them refuses a kind secret rather than serving its stored App key or
	// refresh token. Brokers older than legion-envoy-v13.19.0 read no such tag and serve it; never
	// run one against a store that holds kind secrets.
	TagRollbackGuard   = "mint-service-account"
	RollbackGuardValue = "kind-secret"
	// OwnerShared is the owner value of a secret nobody owns alone.
	OwnerShared = "shared"
	// TierAgent is a secret its owner's agents use without asking.
	TierAgent = "agent"
	// TierHuman is a secret every request for which a person approves.
	TierHuman = "human"
)

// The outcomes Evaluate answers, stored as each requested name's decision.
const (
	Automatic = "automatic"
	Approval  = "approval"
	Deny      = "deny"
)

// Kinds is every value a secret's kind tag may name. The broker holds a minter for each.
var Kinds = []string{KindGitHubApp, KindGoogleLogin}

// ValidKind reports whether kind is one of Kinds.
func ValidKind(kind string) bool {
	return slices.Contains(Kinds, kind)
}

// ErrMaxGrantMalformed is a max-grant tag that is not a Go duration of at least one second.
var ErrMaxGrantMalformed = errors.New("max-grant is a duration of at least 1s")

// ParseMaxGrant reads a max-grant tag: a Go duration (time.ParseDuration) of at least one second,
// so 0 is free to mean that a secret sets none.
func ParseMaxGrant(raw string) (time.Duration, error) {
	maxGrant, err := time.ParseDuration(raw)
	if err != nil || maxGrant < time.Second {
		return 0, ErrMaxGrantMalformed
	}
	return maxGrant, nil
}

// ErrGitHubOwnersMissing is a github-owners tag that names no account.
var ErrGitHubOwnersMissing = errors.New("github-owners names at least one GitHub account")

// ErrGitHubOwnersComma is a github-owners tag that holds a comma: its logins are separated by
// spaces, as BROKER_SERVICES separates its entries, since an AWS tag value takes a space and no
// comma.
var ErrGitHubOwnersComma = errors.New(`github-owners separates GitHub logins with spaces, not commas ("<login> <login>")`)

// ErrGitHubOwnersLogin is a github-owners entry that cannot be a GitHub login: one that does not
// start with a letter or digit, holds anything but letters, digits and hyphens, or is longer than
// 39 characters.
var ErrGitHubOwnersLogin = errors.New("github-owners names GitHub logins: up to 39 letters, digits and hyphens, starting with a letter or digit")

// gitHubLogin is what a GitHub login can be, lowercased: a letter or digit, then letters, digits
// and hyphens. GitHub's sign-up rule today also refuses a trailing or doubled hyphen, but accounts
// made under its older rules keep them (`x-` is an organization from 2011), so this refuses only
// what no login can hold. A login is at most 39 characters (maxGitHubLogin), which the pattern does
// not check.
var gitHubLogin = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

const maxGitHubLogin = 39

// ParseGitHubOwners reads a github-owners tag: GitHub account logins separated by white space,
// compared without regard to case. It refuses a comma anywhere (ErrGitHubOwnersComma), an entry that
// is not a GitHub login (ErrGitHubOwnersLogin) and a list that names no account
// (ErrGitHubOwnersMissing). It answers them lowercased, sorted and joined by single spaces, so a
// Secret holding them stays comparable, two spellings of one list are one policy, and the value is
// one that both stores hold: an AWS tag value and a Secret Manager annotation both take a space,
// which a GitHub login never holds.
func ParseGitHubOwners(raw string) (string, error) {
	if strings.Contains(raw, ",") {
		return "", ErrGitHubOwnersComma
	}
	var owners []string
	for _, owner := range strings.Fields(raw) {
		owner = strings.ToLower(owner)
		if len(owner) > maxGitHubLogin || !gitHubLogin.MatchString(owner) {
			return "", fmt.Errorf("%w, not %q", ErrGitHubOwnersLogin, owner)
		}
		if !slices.Contains(owners, owner) {
			owners = append(owners, owner)
		}
	}
	if len(owners) == 0 {
		return "", ErrGitHubOwnersMissing
	}
	slices.Sort(owners)
	return strings.Join(owners, " "), nil
}

// KindNeedsGitHubOwners reports whether a secret of kind must carry a github-owners tag naming at
// least one account. Only a github-app secret reads the tag.
func KindNeedsGitHubOwners(kind string) bool {
	return kind == KindGitHubApp
}

// GitHubOwnersFor is the github-owners rule every reader and writer of a secret's tags applies:
// the loader, and `agent-secrets secret create` and `retag` before they write. For a kind that
// needs owners (KindNeedsGitHubOwners) it answers ParseGitHubOwners of raw, with its two errors;
// for any other kind the tag is not read, and it answers "".
func GitHubOwnersFor(kind, raw string) (string, error) {
	if !KindNeedsGitHubOwners(kind) {
		return "", nil
	}
	return ParseGitHubOwners(raw)
}

// KindSource is the Source of a kind secret: its kind frozen beside the store's own source.
// A store's source never starts with KindSourcePrefix (an AWS source starts with "arn:", a
// Secret Manager one with "projects/").
func KindSource(kind, storeSource string) string {
	return KindSourcePrefix + kind + ":" + storeSource
}

// ErrKindSourceMalformed is a source that opens with KindSourcePrefix but names no kind or no
// store source, which no KindSource built.
var ErrKindSourceMalformed = errors.New("a minted source names no kind or no store source")

// ParseKindSource splits a source KindSource built into its kind and store source. minted is false
// for a stored secret's source, which it answers whole as storeSource.
func ParseKindSource(source string) (kind, storeSource string, minted bool, err error) {
	rest, minted := strings.CutPrefix(source, KindSourcePrefix)
	if !minted {
		return "", source, false, nil
	}
	kind, storeSource, ok := strings.Cut(rest, ":")
	if !ok || kind == "" || storeSource == "" {
		return "", "", true, ErrKindSourceMalformed
	}
	return kind, storeSource, true, nil
}

// ErrUnknownSecret is a name no secret the policy serves carries: none exists under the namespace
// prefix, or the one that does was refused.
var ErrUnknownSecret = errors.New("no agent secret has this name")

// ReasonAbsent is LoadOne's answer for a name no secret carries: none exists under the prefix, or
// the one that does is scheduled for deletion. It is not a refusal, so it is never logged as
// RefusedMessage.
const ReasonAbsent = "absent"

type ownerKind int

const (
	ownerPerson ownerKind = iota
	ownerShared
	ownerService
)

// Secret is one namespace secret the policy serves.
type Secret struct {
	// Name is what a session asks for: the secret's name under the prefix, uppercased, each hyphen
	// an underscore (<prefix>deel-api-key is DEEL_API_KEY).
	Name string
	// Source is what the secret's value is read by: its ARN in Secrets Manager, or its resource
	// name in Secret Manager. A kind secret's is KindSource of its kind and that store source, so
	// the kind is frozen into every request (ParseKindSource reads it back).
	Source string
	// Owner is OwnerShared, a person's email, or a registered service.
	Owner string
	// Tier is TierAgent or TierHuman.
	Tier string
	// MaxGrant is the greatest grant lifetime a request may sign for this secret, or 0 when it sets
	// none (ParseMaxGrant refuses anything under a second). A value, never a pointer, so a Secret
	// compares equal to an unchanged reread of itself (Current.RefreshOne).
	MaxGrant time.Duration
	// GitHubOwners is a github-app secret's github-owners tag as ParseGitHubOwners answers it, and
	// "" for every other secret. A string, not a slice, for the same reason as MaxGrant.
	GitHubOwners string
	ownership    ownerKind
}

// GitHubOwnerLogins is GitHubOwners as a list of lowercase logins, nil when it is "".
func (secret Secret) GitHubOwnerLogins() []string {
	if secret.GitHubOwners == "" {
		return nil
	}
	return strings.Fields(secret.GitHubOwners)
}

// approver is who decides an approval request for secret: anyone signed in to Dispatch for a
// shared secret, its owner otherwise.
func (secret Secret) approver() string {
	if secret.ownership == ownerShared {
		return record.AnyoneApprover
	}
	return secret.Owner
}

// Set is the policy as one load read it, or as a single name's reread left it (NewSet builds both).
type Set struct {
	// Version is the SHA-256 of every served secret's name, owner, tier, source, max-grant and
	// github-owners: equal versions are equal policies. A request records the version it was decided
	// under, so a live grant is re-evaluated only once the policy changed.
	Version string
	Secrets map[string]Secret
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets. A secret with no max-grant and no github-owners writes the
// pre-kind digest bytes, "<Name>\t<Owner>\t<Tier>\t<Source>\n" (a kind secret's kind is in its
// Source); one with either adds both, as a fifth field (time.Duration's form, 0s for none) and a
// sixth (empty for none). Entries are ascending by the secret's name in the store (the slug) —
// the exact order Load wrote before NewSet existed, so deployed plain-secret digests do not shift.
// (Sorting by the uppercased Name would NOT be equivalent: '-' is 0x2D, '_' is 0x5F, so "a-b" <
// "a0" as slugs but "A0" < "A_B" as Names.)
func NewSet(secrets map[string]Secret) *Set {
	type bySlug struct {
		slug   string
		secret Secret
	}
	served := make([]bySlug, 0, len(secrets))
	for _, s := range secrets {
		served = append(served, bySlug{slug: slugOf(s.Name), secret: s})
	}
	slices.SortFunc(served, func(a, b bySlug) int { return strings.Compare(a.slug, b.slug) })
	digest := sha256.New()
	for _, s := range served {
		if s.secret.MaxGrant == 0 && s.secret.GitHubOwners == "" {
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source)
			continue
		}
		fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant, s.secret.GitHubOwners)
	}
	return &Set{Version: hex.EncodeToString(digest.Sum(nil)), Secrets: secrets}
}

// Requester is an enrolled session or pod as the policy sees it.
type Requester struct {
	// Operator is the email of the person whose machine login the session enrolled under; empty
	// for a pod.
	Operator string
	// Service is the service of the machine login that enrolled the session, set only when the
	// session is a pod whose verified service account is the one BROKER_SERVICES binds that service
	// to (requests.enrollmentRow.requester); empty otherwise.
	Service string
	// Withheld is the names the session's operator withheld from it by revoking a grant of them
	// the session got without asking (requests.Machine.RevokeByApprover). Evaluate answers each as
	// a human-tier secret.
	Withheld []string
}

// Decision is how the policy answers one requester's ask for one name.
type Decision struct {
	// Outcome is Automatic, Approval or Deny.
	Outcome string
	// Approver is who decides an Approval: the owner's email, or record.AnyoneApprover for a
	// shared secret.
	Approver string
	// Source is what the secret's value is read by (Secret.Source).
	Source string
}

// ServiceOwned reports whether the set serves name as a registered service's secret. Who is a
// service's own session rests on BROKER_SERVICES' accounts, which are not in Version, so a caller
// re-checks a grant of such a name on every use rather than only once Version moves.
func (s *Set) ServiceOwned(name string) bool {
	return s.Secrets[name].ownership == ownerService
}

// Evaluate answers r's ask for name. A name the set does not serve is ErrUnknownSecret. A name
// withheld from r (Requester.Withheld) is decided as a human-tier secret: what r would have got at
// once is an approval request to the owner, or to anyone for a shared secret, and a service's own
// secret, which no person approves, is denied.
func (s *Set) Evaluate(name string, r Requester) (Decision, error) {
	secret, ok := s.Secrets[name]
	if !ok {
		return Decision{}, ErrUnknownSecret
	}
	withheld := slices.Contains(r.Withheld, name)
	d := Decision{Source: secret.Source}
	switch {
	case secret.ownership == ownerService:
		d.Outcome = Deny
		if r.Service == secret.Owner && !withheld {
			d.Outcome = Automatic
		}
	case secret.Tier == TierAgent && !withheld && (secret.ownership == ownerShared || record.CanonicalLogin(r.Operator) == secret.Owner):
		d.Outcome = Automatic
	default:
		d.Outcome, d.Approver = Approval, secret.approver()
	}
	return d, nil
}
