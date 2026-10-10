// Package policy decides who may have an agent secret from two tags on the secret itself in AWS
// Secrets Manager: owner (shared, a person's email, or a registered service) and tier (agent or
// human). Loader reads every secret under the namespace prefix into a Set, refusing by name each one
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

	"github.com/sjawhar/envoy/internal/broker/record"
)

// The tags a namespace secret carries, and their fixed values.
const (
	// TagOwner names who owns the secret: OwnerShared, a person's lowercase email, or a registered
	// service's name.
	TagOwner = "owner"
	// TagTier is TierAgent or TierHuman.
	TagTier = "tier"
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

// ErrUnknownSecret is a name no secret the policy serves carries: none exists under the namespace
// prefix, or the one that does was refused.
var ErrUnknownSecret = errors.New("no agent secret has this name")

// ReasonAbsent is LoadOne's answer for a name no secret carries: none exists under the prefix, or
// the one that does is scheduled for deletion. It is not a refusal, so it is never logged as
// RefusedMessage.
const ReasonAbsent = "absent"

// KeyARNParts answers the region and account of a key ARN ValidKeyARN accepts,
// arn:aws:kms:<region>:<account>:key/<key id>: the region and account every agent secret lives in.
func KeyARNParts(arn string) (region, account string) {
	parts := strings.Split(arn, ":")
	return parts[3], parts[4]
}

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
	// ARN is the secret's own ARN, which its value is read by.
	ARN string
	// Owner is OwnerShared, a person's email, or a registered service.
	Owner string
	// Tier is TierAgent or TierHuman.
	Tier string
	kind ownerKind
}

// approver is who decides an approval request for secret: anyone signed in to Dispatch for a
// shared secret, its owner otherwise.
func (secret Secret) approver() string {
	if secret.kind == ownerShared {
		return record.AnyoneApprover
	}
	return secret.Owner
}

// Set is the policy as one load read it, or as a single name's reread left it (NewSet builds both).
type Set struct {
	// Version is the SHA-256 of every served secret's name, owner, tier and ARN: equal versions are
	// equal policies. A request records the version it was decided under, so a live grant is
	// re-evaluated only once the policy changed.
	Version string
	Secrets map[string]Secret
}

// NewSet builds the one Set shape both Load and the single-name merges produce, keeping secrets
// (keyed by Name) as its Secrets: Version is the SHA-256 over "<Name>\t<Owner>\t<Tier>\t<ARN>\n"
// per served secret, ascending by the secret's Secrets Manager name (the slug) — the exact order
// and bytes Load wrote before NewSet existed, so deployed digests do not shift. (Sorting by the
// uppercased Name would NOT be equivalent: '-' is 0x2D, '_' is 0x5F, so "a-b" < "a0" as slugs but
// "A0" < "A_B" as Names.)
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
		fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", s.secret.Name, s.secret.Owner, s.secret.Tier, s.secret.ARN)
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
	// Source is the secret's ARN.
	Source string
}

// ServiceOwned reports whether the set serves name as a registered service's secret. Who is a
// service's own session rests on BROKER_SERVICES' accounts, which are not in Version, so a caller
// re-checks a grant of such a name on every use rather than only once Version moves.
func (s *Set) ServiceOwned(name string) bool {
	return s.Secrets[name].kind == ownerService
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
	d := Decision{Source: secret.ARN}
	switch {
	case secret.kind == ownerService:
		d.Outcome = Deny
		if r.Service == secret.Owner && !withheld {
			d.Outcome = Automatic
		}
	case secret.Tier == TierAgent && !withheld && (secret.kind == ownerShared || record.CanonicalLogin(r.Operator) == secret.Owner):
		d.Outcome = Automatic
	default:
		d.Outcome, d.Approver = Approval, secret.approver()
	}
	return d, nil
}
