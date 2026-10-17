// packages/envoy/cmd/agent-secrets/secret_aws.go
//
// The AWS side of the secret forms: the Secrets Manager and STS calls they make, under the
// person's own AWS sign-in, and the checks that refuse a read or a write under a sign-in in another
// account and a write under any sign-in but a person's own.
package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// secretsAPI is the Secrets Manager calls the secret forms make: ListSecrets and DescribeSecret to
// read, and the five writes. *secretsmanager.Client and secrets.Local both satisfy it.
type secretsAPI interface {
	secretsmanager.ListSecretsAPIClient
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error)
	DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
	RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error)
}

// stsAPI is the one STS call the secret forms make: whose sign-in this is.
type stsAPI interface {
	GetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, o ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

var (
	_ secretsAPI = (*secretsmanager.Client)(nil)
	_ stsAPI     = (*sts.Client)(nil)
)

// awsClients loads the person's own sign-in (--profile, else AWS_PROFILE, else the default chain)
// pinned to the broker's region. Tests replace it.
var awsClients = func(ctx context.Context, profile, region string) (secretsAPI, stsAPI, error) {
	options := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if profile != "" {
		options = append(options, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, nil, fmt.Errorf("load your AWS sign-in: %w", err)
	}
	return secretsmanager.NewFromConfig(cfg), sts.NewFromConfig(cfg), nil
}

// ssoRole matches a person's Identity Center sign-in and captures (account, session name). The
// deployment repository's Identity Center permission sets map the session name to the person's
// userName, which is their email.
var ssoRole = regexp.MustCompile(`^arn:aws[a-z-]*:sts::([0-9]{12}):assumed-role/AWSReservedSSO_[^/]+/(.+)$`)

// requireAccount refuses, before any read or write, a sign-in in an account other than the
// broker's, naming both accounts: a read there would list another account's secrets as if they
// were the agent secrets. It answers the sign-in's ARN.
func requireAccount(ctx context.Context, st stsAPI, settings Settings) (arn string, err error) {
	id, err := st.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("read your AWS sign-in: %w", err)
	}
	arn, account := aws.ToString(id.Arn), aws.ToString(id.Account)
	if account != settings.AWSAccountID {
		return "", fmt.Errorf("your AWS sign-in %s is in account %s, but the agent secrets are in account %s: sign in to account %s (--profile or AWS_PROFILE names the sign-in)",
			arn, account, settings.AWSAccountID, settings.AWSAccountID)
	}
	return arn, nil
}

// requireWriteSignIn refuses, before any write, a sign-in in another account (requireAccount) or
// one that is a machine's own role (a devbox instance role, an assumed service role, an IAM user),
// naming the ARN it found; it answers the signed-in person's lowercased email for --owner me.
func requireWriteSignIn(ctx context.Context, st stsAPI, settings Settings) (email string, err error) {
	arn, err := requireAccount(ctx, st, settings)
	if err != nil {
		return "", err
	}
	m := ssoRole.FindStringSubmatch(arn)
	if m == nil || m[1] != settings.AWSAccountID {
		return "", fmt.Errorf("%s is not a person's Identity Center sign-in: a secret is written under your own sign-in to account %s (aws sso login, then --profile or AWS_PROFILE), never a machine's role",
			arn, settings.AWSAccountID)
	}
	return strings.ToLower(m[2]), nil
}

// checkSettingsKeyARN refuses, before any AWS call, settings whose aws_account_id or aws_region does
// not match the account and region in kms_key_arn. The broker derives both from the key ARN the same
// way (internal/broker/api handlers_secrets.go, secrets.AWSKeyARNParts), so the account the sign-in is
// checked against (requireAccount) and the region the SDK is pinned to are the key ARN's, not a
// separately-sent field a spoofed or plain-http broker could set on its own to steer a write.
func checkSettingsKeyARN(settings Settings) error {
	if !secrets.ValidAWSKeyARN(settings.KMSKeyARN) {
		return fmt.Errorf("the broker's settings name an invalid KMS key ARN %q (expected arn:aws:kms:<region>:<account>:key/<key id>)", settings.KMSKeyARN)
	}
	region, account := secrets.AWSKeyARNParts(settings.KMSKeyARN)
	if settings.AWSAccountID != account {
		return fmt.Errorf("the broker's settings name account %s, but its KMS key ARN %s is in account %s: the broker's settings are inconsistent", settings.AWSAccountID, settings.KMSKeyARN, account)
	}
	if settings.AWSRegion != region {
		return fmt.Errorf("the broker's settings name region %s, but its KMS key ARN %s is in region %s: the broker's settings are inconsistent", settings.AWSRegion, settings.KMSKeyARN, region)
	}
	return nil
}

// ownerTierTags is the owner and tier tags, which create and retag always send together
// (cmdSecretRetag says why).
func ownerTierTags(owner, tier string) []smtypes.Tag {
	return []smtypes.Tag{
		{Key: aws.String(policy.TagOwner), Value: aws.String(owner)},
		{Key: aws.String(policy.TagTier), Value: aws.String(tier)},
	}
}

// isAccessDenied reports whether err is AWS's AccessDeniedException, which the SDK answers as a
// generic API error: secretsmanager's types declare no type for it.
func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDeniedException"
}

// awsBackend is the secretBackend for store "" or secrets.StoreAWS: AWS Secrets Manager
// under the person's own AWS sign-in, pinned to the key ARN's account and region.
type awsBackend struct {
	settings Settings
	sm       secretsAPI
	st       stsAPI
}

func (b *awsBackend) id(slug string) string { return b.settings.SecretsPrefix + slug }

func (b *awsBackend) displayName(id string) string { return displayName(b.settings.SecretsPrefix, id) }

func (b *awsBackend) requireAccount(ctx context.Context) error {
	_, err := requireAccount(ctx, b.st, b.settings)
	return err
}

func (b *awsBackend) requireWriteSignIn(ctx context.Context) (string, error) {
	return requireWriteSignIn(ctx, b.st, b.settings)
}

func (b *awsBackend) list(ctx context.Context) ([]secretView, error) {
	views := []secretView{}
	pages := secretsmanager.NewListSecretsPaginator(b.sm, &secretsmanager.ListSecretsInput{
		Filters:                []smtypes.Filter{{Key: smtypes.FilterNameStringTypeName, Values: []string{b.settings.SecretsPrefix}}},
		IncludePlannedDeletion: aws.Bool(true),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list the agent secrets: %w", err)
		}
		for _, e := range page.SecretList {
			id := aws.ToString(e.Name)
			if !strings.HasPrefix(id, b.settings.SecretsPrefix) {
				continue // the name filter matched more than the namespace
			}
			tags := secrets.TagMap(e.Tags)
			views = append(views, secretView{
				Name: b.displayName(id), SecretName: id,
				Owner: tags[policy.TagOwner], Tier: tags[policy.TagTier],
				HasValue: secrets.HasCurrentVersion(e.SecretVersionsToStages),
				Created:  e.CreatedDate, LastChanged: e.LastChangedDate,
				Deleted: e.DeletedDate, EarliestPurge: earliestPurge(e.DeletedDate),
			})
		}
	}
	return views, nil
}

func (b *awsBackend) show(ctx context.Context, slug string) (secretView, error) {
	out, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(b.id(slug))})
	if err != nil {
		return secretView{}, err
	}
	tags := secrets.TagMap(out.Tags)
	return secretView{
		Name: b.displayName(aws.ToString(out.Name)), SecretName: aws.ToString(out.Name),
		Owner: tags[policy.TagOwner], Tier: tags[policy.TagTier],
		HasValue: secrets.HasCurrentVersion(out.VersionIdsToStages),
		Created:  out.CreatedDate, LastChanged: out.LastChangedDate,
		Deleted: out.DeletedDate, EarliestPurge: earliestPurge(out.DeletedDate),
		Versions: out.VersionIdsToStages,
	}, nil
}

func (b *awsBackend) create(ctx context.Context, slug, owner, tier, value string) error {
	_, err := b.sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(b.id(slug)),
		KmsKeyId:     aws.String(b.settings.KMSKeyARN),
		SecretString: aws.String(value),
		Tags:         ownerTierTags(owner, tier),
	})
	return err
}

func (b *awsBackend) set(ctx context.Context, slug, value string) error {
	_, err := b.sm.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(b.id(slug)), SecretString: aws.String(value),
	})
	return err
}

func (b *awsBackend) currentTags(ctx context.Context, slug string) (owner, tier string, err error) {
	held, err := b.sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(b.id(slug))})
	if err != nil {
		return "", "", err
	}
	tags := secrets.TagMap(held.Tags)
	return tags[policy.TagOwner], tags[policy.TagTier], nil
}

func (b *awsBackend) retag(ctx context.Context, slug, owner, tier string) error {
	_, err := b.sm.TagResource(ctx, &secretsmanager.TagResourceInput{
		SecretId: aws.String(b.id(slug)),
		Tags:     ownerTierTags(owner, tier),
	})
	return err
}

func (b *awsBackend) delete(ctx context.Context, slug string) (*time.Time, error) {
	out, err := b.sm.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(b.id(slug)), RecoveryWindowInDays: aws.Int64(recoveryWindowDays),
	})
	if err != nil {
		return nil, err
	}
	return out.DeletionDate, nil
}

func (b *awsBackend) restore(ctx context.Context, slug string) error {
	_, err := b.sm.RestoreSecret(ctx, &secretsmanager.RestoreSecretInput{SecretId: aws.String(b.id(slug))})
	return err
}

func (b *awsBackend) isAccessDenied(err error) bool { return isAccessDenied(err) }

// close is a no-op: the AWS SDK's HTTP-based clients hold no persistent connection worth
// releasing explicitly.
func (b *awsBackend) close() error { return nil }

var _ secretBackend = (*awsBackend)(nil)
