// Package secrets reads a granted secret's value from AWS Secrets Manager, and holds Local, the
// in-memory stand-in for the AWS APIs the broker reads.
package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

var ErrNotFound = errors.New("secret not found in the store")

type Reader interface {
	Read(ctx context.Context, source string) (string, error)
}

type smAPI interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// AWS reads a value with GetSecretValue: from Secrets Manager itself in production, from a Local
// in local development and tests.
type AWS struct{ Client smAPI }

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
