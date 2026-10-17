package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// awsKeyARNPattern is a KMS key's ARN, whose last segment is the key id.
var awsKeyARNPattern = regexp.MustCompile(`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)

// ValidAWSKeyARN reports whether arn names a KMS key by its key ARN, the form AWS.KeyARN takes.
func ValidAWSKeyARN(arn string) bool {
	return awsKeyARNPattern.MatchString(arn)
}

// AWSKeyARNParts answers the region and account of a key ARN ValidAWSKeyARN accepts,
// arn:aws:kms:<region>:<account>:key/<key id>: the region and account every agent secret lives in.
func AWSKeyARNParts(arn string) (region, account string) {
	parts := strings.Split(arn, ":")
	return parts[3], parts[4]
}

// currentStage is the staging label GetSecretValue reads when it names no version.
const currentStage = "AWSCURRENT"

// HasCurrentVersion reports whether a secret's version stages (ListSecrets'
// SecretVersionsToStages, DescribeSecret's VersionIdsToStages) name a version labelled AWSCURRENT,
// the one GetSecretValue reads: the broker serves a secret only then, and the agent-secrets CLI
// shows it as having a value only then.
func HasCurrentVersion(versionsToStages map[string][]string) bool {
	for _, stages := range versionsToStages {
		if slices.Contains(stages, currentStage) {
			return true
		}
	}
	return false
}

// SecretsManagerAPI is the three Secrets Manager calls the AWS store reads with: ListSecrets for
// List, DescribeSecret for Describe and GetSecretValue for Read. Secrets Manager's own client and
// Local both implement it.
type SecretsManagerAPI interface {
	secretsmanager.ListSecretsAPIClient
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// AWS is the Store over AWS Secrets Manager: from Secrets Manager and KMS themselves in
// production, from a Local in local development and tests.
type AWS struct {
	// Client lists, describes and reads the secrets.
	Client SecretsManagerAPI
	// KMS resolves the aliases that point at KeyARN, read only when a secret names its key by an
	// alias: Secrets Manager reports a secret's key in whatever form it was set with.
	KMS kms.ListAliasesAPIClient
	// KeyARN is the agent-secrets key's ARN (BROKER_SECRETS_KMS_KEY_ARN); a secret encrypted with
	// any other key is not OnKey.
	KeyARN string
}

// awsListLag is how stale ListSecrets may be: AWS documents that it "might not reflect changes from
// the last five minutes" (ListSecrets API reference). DescribeSecret answers a write at once.
const awsListLag = 5 * time.Minute

// ListLag is ListSecrets' documented lag, five minutes.
func (AWS) ListLag() time.Duration { return awsListLag }

// maxSecretNameLength is the longest name Secrets Manager gives a secret. DescribeSecret answers a
// longer SecretId as invalid input (ValidationException, or InvalidParameterException once it is
// long enough), not as a secret it does not hold, so a caller would take it for a failed read.
const maxSecretNameLength = 512

// ValidName refuses a name past Secrets Manager's 512-character limit. Secrets Manager names are
// ASCII, so the length counts bytes.
func (a AWS) ValidName(id string) error {
	if len(id) > maxSecretNameLength {
		return fmt.Errorf("under the prefix it makes a %d-character secret name, past Secrets Manager's %d-character limit", len(id), maxSecretNameLength)
	}
	return nil
}

// List lists every secret under prefix with ListSecrets' name filter, a case-sensitive prefix
// match, reading every page. Whether a secret has a value is read from the same listing's
// SecretVersionsToStages, with no call per secret; ListSecrets leaves out a secret scheduled for
// deletion, since it is not asked to include one. A listing's OnKey reads the key's aliases at
// most once per List, the first time it is asked about a secret that names its key by one, so a
// secret the policy refuses for its name or tags costs no KMS call. A failed Secrets Manager call
// fails the whole List.
func (a AWS) List(ctx context.Context, prefix string) ([]Listing, error) {
	var listings []Listing
	keys := a.keys()
	pages := secretsmanager.NewListSecretsPaginator(a.Client, &secretsmanager.ListSecretsInput{
		Filters: []types.Filter{{Key: types.FilterNameStringTypeName, Values: []string{prefix}}},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list secrets under %s: %w", prefix, err)
		}
		for _, e := range page.SecretList {
			listings = append(listings, Listing{
				ID: aws.ToString(e.Name), Source: aws.ToString(e.ARN), Tags: TagMap(e.Tags),
				OnKey: a.onKey(aws.ToString(e.KmsKeyId), keys), HasCurrentValue: HasCurrentVersion(e.SecretVersionsToStages),
			})
		}
	}
	return listings, nil
}

// Describe reads one secret with DescribeSecret, which is read-after-write consistent where
// ListSecrets may lag a change by minutes: ErrNotFound for one Secrets Manager does not hold, and
// for one scheduled for deletion, which DescribeSecret still answers with its DeletedDate.
func (a AWS) Describe(ctx context.Context, id string) (Listing, error) {
	out, err := a.Client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(id)})
	var notFound *types.ResourceNotFoundException
	switch {
	case errors.As(err, &notFound):
		return Listing{}, ErrNotFound
	case err != nil:
		return Listing{}, fmt.Errorf("describe %s: %w", id, err)
	case out.DeletedDate != nil:
		return Listing{}, ErrNotFound
	}
	return Listing{
		ID: aws.ToString(out.Name), Source: aws.ToString(out.ARN), Tags: TagMap(out.Tags),
		OnKey: a.onKey(aws.ToString(out.KmsKeyId), a.keys()), HasCurrentValue: HasCurrentVersion(out.VersionIdsToStages),
	}, nil
}

// TagMap is a Secrets Manager secret's tags as a map, the last value of a repeated key winning.
func TagMap(tags []types.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// keyIdentifiers are the forms a secret's KmsKeyId may name the agent-secrets key in: its ARN, its
// key id, or one of the aliases that currently point at it (read once a List's or Describe's
// OnKey first meets an alias).
type keyIdentifiers struct {
	arn, id string
	aliases map[string]bool
}

// keys is the agent-secrets key's identifiers before any alias is read, shared by every listing of
// one List or Describe.
func (a AWS) keys() *keyIdentifiers {
	return &keyIdentifiers{arn: a.KeyARN, id: a.KeyARN[strings.LastIndex(a.KeyARN, "/")+1:]}
}

// onKey answers a listing's OnKey: whether kmsKeyID, a secret's KmsKeyId as Secrets Manager lists
// it, names the agent-secrets key. An empty one is the AWS-managed key.
func (a AWS) onKey(kmsKeyID string, keys *keyIdentifiers) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		switch {
		case kmsKeyID == "":
			return false, nil
		case kmsKeyID == keys.arn || kmsKeyID == keys.id:
			return true, nil
		case !strings.HasPrefix(kmsKeyID, "alias/") && !strings.Contains(kmsKeyID, ":alias/"):
			return false, nil
		}
		if keys.aliases == nil {
			aliases := map[string]bool{}
			pages := kms.NewListAliasesPaginator(a.KMS, &kms.ListAliasesInput{KeyId: aws.String(keys.arn)})
			for pages.HasMorePages() {
				page, err := pages.NextPage(ctx)
				if err != nil {
					return false, fmt.Errorf("list the aliases of %s: %w", keys.arn, err)
				}
				for _, al := range page.Aliases {
					aliases[aws.ToString(al.AliasName)] = true
					aliases[aws.ToString(al.AliasArn)] = true
				}
			}
			keys.aliases = aliases
		}
		return keys.aliases[kmsKeyID], nil
	}
}

// Read answers the secret's current value. A secret Secrets Manager does not hold
// (ResourceNotFoundException) and one scheduled for deletion are both ErrNotFound: a secret
// deleted with a recovery window, the console's and the CLI's default, stays there until the
// window passes and GetSecretValue refuses to read it with InvalidRequestException ("marked for
// deletion") rather than answering it absent, which is the same answer to its caller - the secret
// is no longer readable - and must not be the broker's own 500. InvalidRequestException is that
// state: GetSecretValue's other refusals name the request itself (InvalidParameterException) or the
// key (DecryptionFailure), and this one is the only request state a caller can reach.
func (a AWS) Read(ctx context.Context, source string) (string, error) {
	out, err := a.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(source)})
	var notFound *types.ResourceNotFoundException
	var deleting *types.InvalidRequestException
	if errors.As(err, &notFound) || errors.As(err, &deleting) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secrets manager %s: %w", source, err)
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", fmt.Errorf("secrets manager %s: value is not a non-empty string", source)
	}
	return *out.SecretString, nil
}
