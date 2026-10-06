// packages/envoy/cmd/agent-secrets/secret_aws.go
//
// The AWS side of the secret forms: the Secrets Manager and STS calls they make, under the
// person's own AWS sign-in, and the checks that refuse a read or a write under a sign-in in another
// account and a write under any sign-in but a person's own.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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

// secretStdin is where create and set read a secret's value from; tests replace it.
var secretStdin io.Reader = os.Stdin

// ssoRole matches a person's Identity Center sign-in and captures (account, session name). The
// deployment repository's Identity Center permission sets map the session name to the person's
// userName, which is their email.
var ssoRole = regexp.MustCompile(`^arn:aws:sts::([0-9]{12}):assumed-role/AWSReservedSSO_[^/]+/(.+)$`)

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
