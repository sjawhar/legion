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
	// KindGitHubApp mints one installation token for every installation of the stored GitHub App.
	KindGitHubApp = "github-app"
	// KindGoogleLogin exchanges the stored authorized-user refresh token for an access token.
	KindGoogleLogin = "google-login"
	// TagMaxGrant bounds a request's signed grant lifetime for this secret (ParseMaxGrant).
	TagMaxGrant = "max-grant"
	// KindSourcePrefix opens the Source of a kind secret (KindSource). The kind and store source
	// are frozen into a request, so changing or clearing the kind ends every live grant.
	KindSourcePrefix = "mint:"
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
	MaxGrant  time.Duration
	ownership ownerKind
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
	// Version is the SHA-256 of every served secret's name, owner, tier, source and max-grant:
	// equal versions are equal policies. A request records the version it was decided under, so a
	// live grant is re-evaluated only once the policy changed.
	Version string
	Secrets map[string]Secret
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets. A secret with no max-grant writes the pre-kind digest bytes,
// "<Name>\t<Owner>\t<Tier>\t<Source>\n" (a kind secret's kind is in its Source); one with a
// max-grant adds it as a fifth field. Entries are ascending by the secret's name in the store
// (the slug) — the exact order Load wrote before NewSet existed, so deployed plain-secret digests
// do not shift. (Sorting by the uppercased Name would NOT be equivalent: '-' is 0x2D, '_' is
// 0x5F, so "a-b" < "a0" as slugs but "A0" < "A_B" as Names.)
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
		if s.secret.MaxGrant == 0 {
			fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source)
			continue
		}
		fmt.Fprintf(digest, "%s\t%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.Source, s.secret.MaxGrant)
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
