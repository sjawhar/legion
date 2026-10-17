package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// RefusedMessage opens the one line logged, at ERROR on every load (Load for every secret, LoadOne
// for its one), for each secret under the prefix the policy refuses to serve, followed by
// name=<the secret's Secrets Manager name> and reason=<one of the Reason* values>. The deployment's
// alarm filters on it, so it does not change.
const RefusedMessage = "agent secret policy refused"

// LoadFailedMessage is logged, at ERROR, when a reload fails and the previous policy stays in
// force. The deployment's alarm filters on it too.
const LoadFailedMessage = "agent secret policy load failed; previous policy kept"

// Why a secret under the prefix is refused, as its RefusedMessage line's reason.
const (
	// ReasonNameMalformed: the name under the prefix is not lowercase letters, digits and single
	// hyphens starting with a letter, so it maps to no name a session can ask for.
	ReasonNameMalformed = "name-malformed"
	// ReasonOwnerTagMissing: the secret has no owner tag.
	ReasonOwnerTagMissing = "owner-tag-missing"
	// ReasonOwnerTagMalformed: the owner tag is not shared, a lowercase email, or a registered
	// service.
	ReasonOwnerTagMalformed = "owner-tag-malformed"
	// ReasonTierTagMissing: the secret has no tier tag.
	ReasonTierTagMissing = "tier-tag-missing"
	// ReasonTierTagMalformed: the tier tag is not agent or human.
	ReasonTierTagMalformed = "tier-tag-malformed"
	// ReasonServiceOwnerHumanTier: a service owns a human-tier secret, which nobody could approve.
	ReasonServiceOwnerHumanTier = "service-owner-human-tier"
	// ReasonNotOnAgentSecretsKey: the secret is encrypted with a key other than the agent-secrets
	// key, the AWS-managed key included.
	ReasonNotOnAgentSecretsKey = "not-on-agent-secrets-key"
	// ReasonNoCurrentValue: no version of the secret carries the AWSCURRENT staging label, the one
	// GetSecretValue reads: it was created without a value, or its only versions are pending or
	// previous. A grant of it would release nothing, so it is refused before anyone approves it.
	// Checked after every other reason, which each name something its owner must fix before a
	// value would make it servable.
	ReasonNoCurrentValue = "no-current-value"
)

// slugPattern is a secret's name under the prefix: the lowercase form of an environment-variable
// name, with each underscore a hyphen.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// emailPattern is an owner tag naming a person: their lowercase email, as their sign-in names them.
var emailPattern = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// ValidPersonOwner reports whether an owner tag names a person by lowercase email.
func ValidPersonOwner(owner string) bool {
	return emailPattern.MatchString(owner)
}

// Loader reads the policy from the secret store: every secret whose name starts with Prefix, with
// its owner and tier tags, whether it is on the agent-secrets key, and whether it has a current
// value.
type Loader struct {
	// Store lists, describes and reads the secrets: secrets.AWS over Secrets Manager and KMS (in
	// local development and tests, over a secrets.Local), or secrets.GCP over Secret Manager. Its
	// metadata may answer a write's previous state for up to its ListLag, for which Current keeps
	// and re-describes what a single-name read settled. Secrets Manager's DescribeSecret shows a
	// write at once; Secret Manager documents its GetSecret as eventually consistent too, so
	// there a reread settles a name only once Secret Manager does.
	Store secrets.Store
	// Prefix is the namespace every agent secret's name starts with: a Secrets Manager name
	// prefix ending in "/", or a Secret Manager one, projects/<number>/secrets/<id prefix>-.
	Prefix string
	// Services are the registered services a secret's owner tag may name: the broker's
	// BROKER_SERVICES. An owner tag naming any other service is refused as malformed.
	Services []string
}

// Load reads the namespace into a Set, logging one RefusedMessage line for each secret it refuses.
// A failed store call fails the whole load.
func (l Loader) Load(ctx context.Context) (*Set, error) {
	listings, err := l.Store.List(ctx, l.Prefix)
	if err != nil {
		return nil, err
	}
	// The store promises no listing order, and the RefusedMessage lines below are read in the
	// order they are logged: this sort is theirs alone. NewSet sorts the served secrets by slug
	// itself, so the Version's bytes do not rest on it.
	slices.SortFunc(listings, func(a, b secrets.Listing) int { return strings.Compare(a.ID, b.ID) })

	served := map[string]Secret{}
	for _, ls := range listings {
		// The namespace is the prefix as written: a name outside it is skipped, whatever the
		// lister answered.
		slug, ok := strings.CutPrefix(ls.ID, l.Prefix)
		if !ok {
			continue
		}
		secret, reason, err := l.secret(ctx, slug, ls)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			slog.Error(RefusedMessage, "name", ls.ID, "reason", reason)
			continue
		}
		served[secret.Name] = secret
	}
	return NewSet(served), nil
}

// Lookup is LoadOne's answer: Served with the Secret, or Reason — a Reason* refusal constant
// (logged as RefusedMessage, same line as Load) or ReasonAbsent (not logged).
type Lookup struct {
	Secret Secret
	Served bool
	Reason string
}

// secretName is the one place that answers whether name can name a secret under the prefix: its
// slug and its whole name in the store when it can, and ErrNameInvalid when it is not of
// namePattern's form or is a name the store refuses (Store.ValidName: past its length limit, or
// outside its character set), so no lookup is made for one.
func (l Loader) secretName(name string) (slug, id string, err error) {
	slug, err = NameToSlug(name)
	if err != nil {
		return "", "", err
	}
	id = l.Prefix + slug
	if err := l.Store.ValidName(id); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrNameInvalid, err)
	}
	return slug, id, nil
}

// LoadOne reads the one secret a session asks for as name, by the rules Load applies to each
// secret it lists, logging the same RefusedMessage line when it refuses it. A name no secret can
// carry (secretName) is ErrNameInvalid, with no call made; a failed store call is an error.
func (l Loader) LoadOne(ctx context.Context, name string) (Lookup, error) {
	slug, id, err := l.secretName(name)
	if err != nil {
		return Lookup{}, err
	}
	ls, err := l.Store.Describe(ctx, id)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return Lookup{Reason: ReasonAbsent}, nil
	case err != nil:
		return Lookup{}, err
	}
	secret, reason, err := l.secret(ctx, slug, ls)
	if err != nil {
		return Lookup{}, err
	}
	if reason != "" {
		slog.Error(RefusedMessage, "name", id, "reason", reason)
		return Lookup{Reason: reason}, nil
	}
	return Lookup{Secret: secret, Served: true}, nil
}

// secret reads one secret under the prefix, answering the reason it is refused when it is. It asks
// the store whether the secret is on the agent-secrets key only after every tag check, so a secret
// refused for its name or tags costs no key lookup; a failed lookup is an error.
func (l Loader) secret(ctx context.Context, slug string, ls secrets.Listing) (Secret, string, error) {
	if !slugPattern.MatchString(slug) {
		return Secret{}, ReasonNameMalformed, nil
	}
	s := Secret{Name: SlugToName(slug), Source: ls.Source}
	var ok bool
	if s.Owner, ok = ls.Tags[TagOwner]; !ok {
		return Secret{}, ReasonOwnerTagMissing, nil
	}
	switch {
	case s.Owner == OwnerShared:
		s.kind = ownerShared
	case ValidPersonOwner(s.Owner):
		s.kind = ownerPerson
	case slices.Contains(l.Services, s.Owner):
		s.kind = ownerService
	default:
		return Secret{}, ReasonOwnerTagMalformed, nil
	}
	if s.Tier, ok = ls.Tags[TagTier]; !ok {
		return Secret{}, ReasonTierTagMissing, nil
	}
	if s.Tier != TierAgent && s.Tier != TierHuman {
		return Secret{}, ReasonTierTagMalformed, nil
	}
	if s.kind == ownerService && s.Tier == TierHuman {
		return Secret{}, ReasonServiceOwnerHumanTier, nil
	}
	onKey, err := ls.OnKey(ctx)
	if err != nil {
		return Secret{}, "", err
	}
	if !onKey {
		return Secret{}, ReasonNotOnAgentSecretsKey, nil
	}
	if !ls.HasCurrentValue {
		return Secret{}, ReasonNoCurrentValue, nil
	}
	return s, "", nil
}
