package policy

import (
	"context"
	"errors"
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

// currentStage is the staging label GetSecretValue reads when it names no version.
const currentStage = "AWSCURRENT"

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

// DescribeSecretAPIClient is the one Secrets Manager call LoadOne makes.
type DescribeSecretAPIClient interface {
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
}

// Loader reads the policy from Secrets Manager: every secret whose name starts with Prefix, with
// its owner and tier tags, the key it is encrypted with, and whether it has a current value.
type Loader struct {
	// Secrets lists the namespace's secrets with their tags, key and version stages.
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
	// Describer reads one secret's tags, key and version stages for LoadOne. DescribeSecret is
	// read-after-write consistent where ListSecrets may lag a change by minutes.
	Describer DescribeSecretAPIClient
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
	// Secrets Manager promises no listing order, and the RefusedMessage lines below are read in
	// the order they are logged: this sort is theirs alone. NewSet sorts the served secrets by slug
	// itself, so the Version's bytes do not rest on it.
	slices.SortFunc(entries, func(a, b smtypes.SecretListEntry) int {
		return strings.Compare(aws.ToString(a.Name), aws.ToString(b.Name))
	})

	keys := l.keys()
	served := map[string]Secret{}
	for _, e := range entries {
		ls := listingFromEntry(e)
		// The namespace is the prefix as written: a name outside it is skipped, whatever the
		// lister answered.
		slug, ok := strings.CutPrefix(ls.id, l.Prefix)
		if !ok {
			continue
		}
		secret, reason, err := l.secret(ctx, slug, ls, &keys)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			slog.Error(RefusedMessage, "name", ls.id, "reason", reason)
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

// maxSecretNameLength is the longest name Secrets Manager gives a secret. DescribeSecret answers a
// longer SecretId as invalid input (ValidationException, or InvalidParameterException once it is
// long enough), not as a secret it does not hold, so LoadOne would take it for a failed read; a
// name under the prefix past it can name no secret, and LoadOne refuses it before any call.
const maxSecretNameLength = 512

// secretName is the one place that answers whether name can name a secret under the prefix: its
// slug and its whole Secrets Manager name when it can, and ErrNameInvalid when it is not of
// namePattern's form or makes a name past maxSecretNameLength. Secrets Manager names are ASCII,
// so the length counts bytes.
func (l Loader) secretName(name string) (slug, id string, err error) {
	slug, err = NameToSlug(name)
	if err != nil {
		return "", "", err
	}
	id = l.Prefix + slug
	if len(id) > maxSecretNameLength {
		return "", "", fmt.Errorf("%w: under the prefix it makes a %d-character secret name, past Secrets Manager's %d-character limit", ErrNameInvalid, len(id), maxSecretNameLength)
	}
	return slug, id, nil
}

// LoadOne reads the one secret a session asks for as name, by the rules Load applies to each
// secret it lists, logging the same RefusedMessage line when it refuses it. A name no secret can
// carry (secretName) is ErrNameInvalid, with no call made; a failed Secrets Manager or KMS call is
// an error.
func (l Loader) LoadOne(ctx context.Context, name string) (Lookup, error) {
	slug, id, err := l.secretName(name)
	if err != nil {
		return Lookup{}, err
	}
	out, err := l.Describer.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	var notFound *smtypes.ResourceNotFoundException
	switch {
	case errors.As(err, &notFound):
		return Lookup{Reason: ReasonAbsent}, nil
	case err != nil:
		return Lookup{}, fmt.Errorf("describe %s: %w", id, err)
	}
	ls := listingFromDescribe(out)
	if ls.deleted {
		return Lookup{Reason: ReasonAbsent}, nil
	}
	keys := l.keys()
	secret, reason, err := l.secret(ctx, slug, ls, &keys)
	if err != nil {
		return Lookup{}, err
	}
	if reason != "" {
		slog.Error(RefusedMessage, "name", id, "reason", reason)
		return Lookup{Reason: reason}, nil
	}
	return Lookup{Secret: secret, Served: true}, nil
}

// listing is the per-secret fields both AWS answer shapes carry, ListSecrets' entry and
// DescribeSecret's output, so secret applies the tag, key and AWSCURRENT rules to either.
type listing struct {
	// id is the secret's whole Secrets Manager name, its prefix included.
	id, arn, kmsKeyID string
	tags              map[string]string
	// versions is each version's staging labels.
	versions map[string][]string
	// deleted is a secret scheduled for deletion, which DescribeSecret still answers.
	deleted bool
}

func listingFromEntry(e smtypes.SecretListEntry) listing {
	return listing{
		id: aws.ToString(e.Name), arn: aws.ToString(e.ARN), kmsKeyID: aws.ToString(e.KmsKeyId),
		tags: tagMap(e.Tags), versions: e.SecretVersionsToStages,
	}
}

func listingFromDescribe(out *secretsmanager.DescribeSecretOutput) listing {
	return listing{
		id: aws.ToString(out.Name), arn: aws.ToString(out.ARN), kmsKeyID: aws.ToString(out.KmsKeyId),
		tags: tagMap(out.Tags), versions: out.VersionIdsToStages, deleted: out.DeletedDate != nil,
	}
}

func tagMap(tags []smtypes.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// secret reads one secret under the prefix, answering the reason it is refused when it is.
func (l Loader) secret(ctx context.Context, slug string, ls listing, keys *keyIdentifiers) (Secret, string, error) {
	if !slugPattern.MatchString(slug) {
		return Secret{}, ReasonNameMalformed, nil
	}
	s := Secret{Name: SlugToName(slug), ARN: ls.arn}
	var ok bool
	if s.Owner, ok = ls.tags[TagOwner]; !ok {
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
	if s.Tier, ok = ls.tags[TagTier]; !ok {
		return Secret{}, ReasonTierTagMissing, nil
	}
	if s.Tier != TierAgent && s.Tier != TierHuman {
		return Secret{}, ReasonTierTagMalformed, nil
	}
	if s.kind == ownerService && s.Tier == TierHuman {
		return Secret{}, ReasonServiceOwnerHumanTier, nil
	}
	onKey, err := l.onKey(ctx, ls.kmsKeyID, keys)
	if err != nil {
		return Secret{}, "", err
	}
	if !onKey {
		return Secret{}, ReasonNotOnAgentSecretsKey, nil
	}
	if !hasCurrentVersion(ls.versions) {
		return Secret{}, ReasonNoCurrentValue, nil
	}
	return s, "", nil
}

// hasCurrentVersion reports whether a secret's version stages (ListSecrets'
// SecretVersionsToStages, DescribeSecret's VersionIdsToStages) name a version labelled AWSCURRENT.
func hasCurrentVersion(versionsToStages map[string][]string) bool {
	for _, stages := range versionsToStages {
		if slices.Contains(stages, currentStage) {
			return true
		}
	}
	return false
}

// keyIdentifiers are the forms a secret's KmsKeyId may name the agent-secrets key in: its ARN, its
// key id, or one of the aliases that currently point at it (read once a load first meets an
// alias).
type keyIdentifiers struct {
	arn, id string
	aliases map[string]bool
}

// keys is the agent-secrets key's identifiers before any alias is read: one per load, so a full
// load reads the key's aliases at most once.
func (l Loader) keys() keyIdentifiers {
	return keyIdentifiers{arn: l.KeyARN, id: l.KeyARN[strings.LastIndex(l.KeyARN, "/")+1:]}
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
