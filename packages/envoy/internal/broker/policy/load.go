package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// RefusedMessage opens the one line logged, at ERROR on every load, for each secret under the
// prefix the policy refuses to serve, followed by name=<the secret's Secrets Manager name> and
// reason=<one of the Reason* values>. The deployment's alarm filters on it, so it does not change.
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
)

// slugPattern is a secret's name under the prefix: the lowercase form of an environment-variable
// name, with each underscore a hyphen.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// emailPattern is an owner tag naming a person: their lowercase email, as their sign-in names them.
var emailPattern = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// keyARNPattern is a KMS key's ARN, whose last segment is the key id.
var keyARNPattern = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)

// ValidKeyARN reports whether arn names a KMS key by its key ARN, the form Loader.KeyARN takes.
func ValidKeyARN(arn string) bool {
	return keyARNPattern.MatchString(arn)
}

// Loader reads the policy from Secrets Manager: every secret whose name starts with Prefix, with
// its owner and tier tags and the key it is encrypted with.
type Loader struct {
	// Secrets lists the namespace's secrets with their tags and key.
	Secrets secretsmanager.ListSecretsAPIClient
	// Aliases resolves the KMS aliases that point at KeyARN, read only when a secret names its key
	// by an alias: Secrets Manager reports a secret's key in whatever form it was set with.
	Aliases kms.ListAliasesAPIClient
	// Prefix is the namespace, ending in "/".
	Prefix string
	// KeyARN is the agent-secrets key's ARN; a secret encrypted with any other key is refused.
	KeyARN string
	// Services are the registered services a secret's owner tag may name. None is registered yet,
	// so an owner tag naming a service is refused as malformed.
	Services []string
}

// Load reads the namespace into a Set, logging one RefusedMessage line for each secret it refuses.
// A failed Secrets Manager or KMS call fails the whole load.
func (l Loader) Load(ctx context.Context) (*Set, error) {
	var entries []smtypes.SecretListEntry
	pages := secretsmanager.NewListSecretsPaginator(l.Secrets, &secretsmanager.ListSecretsInput{
		Filters: []smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{l.Prefix}}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list secrets under %s: %w", l.Prefix, err)
		}
		entries = append(entries, page.SecretList...)
	}
	slices.SortFunc(entries, func(a, b smtypes.SecretListEntry) int {
		return strings.Compare(aws.ToString(a.Name), aws.ToString(b.Name))
	})

	keys := keyIdentifiers{arn: l.KeyARN, id: l.KeyARN[strings.LastIndex(l.KeyARN, "/")+1:]}
	set := &Set{Secrets: map[string]Secret{}}
	digest := sha256.New()
	for _, e := range entries {
		id := aws.ToString(e.Name)
		// The namespace is the prefix as written: a name outside it is skipped, whatever the
		// lister answered.
		slug, ok := strings.CutPrefix(id, l.Prefix)
		if !ok {
			continue
		}
		secret, reason, err := l.secret(ctx, slug, e, &keys)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			slog.Error(RefusedMessage, "name", id, "reason", reason)
			continue
		}
		set.Secrets[secret.Name] = secret
		fmt.Fprintf(digest, "%s\t%s\t%s\t%s\n", secret.Name, secret.Owner, secret.Tier, secret.ARN)
	}
	set.Version = hex.EncodeToString(digest.Sum(nil))
	return set, nil
}

// secret reads one entry under the prefix, answering the reason it is refused when it is.
func (l Loader) secret(ctx context.Context, slug string, e smtypes.SecretListEntry, keys *keyIdentifiers) (Secret, string, error) {
	if !slugPattern.MatchString(slug) {
		return Secret{}, ReasonNameMalformed, nil
	}
	tags := map[string]string{}
	for _, t := range e.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	s := Secret{Name: strings.ToUpper(strings.ReplaceAll(slug, "-", "_")), ARN: aws.ToString(e.ARN)}
	var ok bool
	if s.Owner, ok = tags[TagOwner]; !ok {
		return Secret{}, ReasonOwnerTagMissing, nil
	}
	switch {
	case s.Owner == OwnerShared:
		s.kind = ownerShared
	case emailPattern.MatchString(s.Owner):
		s.kind = ownerPerson
	case slices.Contains(l.Services, s.Owner):
		s.kind = ownerService
	default:
		return Secret{}, ReasonOwnerTagMalformed, nil
	}
	if s.Tier, ok = tags[TagTier]; !ok {
		return Secret{}, ReasonTierTagMissing, nil
	}
	if s.Tier != TierAgent && s.Tier != TierHuman {
		return Secret{}, ReasonTierTagMalformed, nil
	}
	if s.kind == ownerService && s.Tier == TierHuman {
		return Secret{}, ReasonServiceOwnerHumanTier, nil
	}
	onKey, err := l.onKey(ctx, aws.ToString(e.KmsKeyId), keys)
	if err != nil {
		return Secret{}, "", err
	}
	if !onKey {
		return Secret{}, ReasonNotOnAgentSecretsKey, nil
	}
	return s, "", nil
}

// keyIdentifiers are the forms a secret's KmsKeyId may name the agent-secrets key in: its ARN, its
// key id, or one of the aliases that currently point at it (read once a load first meets an
// alias).
type keyIdentifiers struct {
	arn, id string
	aliases map[string]bool
}

// onKey reports whether kmsKeyID, a secret's KmsKeyId as Secrets Manager lists it, names the
// agent-secrets key. An empty one is the AWS-managed key.
func (l Loader) onKey(ctx context.Context, kmsKeyID string, keys *keyIdentifiers) (bool, error) {
	switch {
	case kmsKeyID == "":
		return false, nil
	case kmsKeyID == keys.arn || kmsKeyID == keys.id:
		return true, nil
	case !strings.HasPrefix(kmsKeyID, "alias/") && !strings.Contains(kmsKeyID, ":alias/"):
		return false, nil
	}
	if keys.aliases == nil {
		keys.aliases = map[string]bool{}
		pages := kms.NewListAliasesPaginator(l.Aliases, &kms.ListAliasesInput{KeyId: aws.String(keys.arn)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				return false, fmt.Errorf("list the aliases of %s: %w", keys.arn, err)
			}
			for _, a := range page.Aliases {
				keys.aliases[aws.ToString(a.AliasName)] = true
				keys.aliases[aws.ToString(a.AliasArn)] = true
			}
		}
	}
	return keys.aliases[kmsKeyID], nil
}
